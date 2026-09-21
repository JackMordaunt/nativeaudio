//go:build darwin && cgo

package nativeaudio

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AudioToolbox -framework AVFoundation

#include <AudioToolbox/AudioToolbox.h>
#include <AVFoundation/AVFoundation.h>
#include <stdint.h>

// handleToPointer widens a runtime/cgo handle into the void* these
// AudioToolbox interfaces take for caller data.
//
// The cast belongs on this side. A cgo handle is an integer, and turning
// an integer into a pointer in Go is the very thing the unsafe pointer
// rules forbid.
static void *handleToPointer(uintptr_t handle) {
	return (void *)handle;
}

// Pre-declare exported Go functions to make them visible in the
// C pseudo package.

OSStatus InputDataProc(
	AudioConverterRef               inAudioConverter,
	UInt32 *                        ioNumberDataPackets,
	AudioBufferList *               ioData,
	AudioStreamPacketDescription ** outDataPacketDescription,
	void *                          inUserData
);

OSStatus AudioFileReadProcImpl(
	void *inClientData,
	SInt64 inPosition,
	UInt32 requestCount,
	void *buffer,
	UInt32 *actualCount
);

SInt64 AudioFileGetSizeProcImpl(
	void *inClientData
);
*/
import "C"
import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/cgo"
	"sync"
	"sync/atomic"
	"unsafe"
)

func start() error {
	return nil
}

func end() error {
	return nil
}

// macStream decodes incrementally through an AudioConverter.
//
// The converter was always a pull loop: each FillComplexBuffer asks the
// input proc for as much compressed audio as it needs and hands back a
// chunk of PCM. Buffering the whole decode was only a matter of running
// that loop to completion up front, so streaming is the same loop driven
// one chunk at a time rather than a second pipeline.
type macStream struct {
	mu     sync.Mutex
	pinner runtime.Pinner

	inputFile *AudioFile
	converter *AudioConverter
	icHandle  cgo.Handle

	format Format

	// chunk is the scratch the converter fills. pending is the part of it
	// Read has not handed over yet, so a caller reading in small pieces
	// does not cost a conversion per call.
	chunk   []byte
	pending []byte

	packetsPerChunk C.UInt32
	drained         bool
	closed          bool
}

// newMacStream builds the AudioToolbox pipeline over compressed audio
// held in memory. The returned stream owns everything allocated here and
// must be closed.
func newMacStream(buf []byte) (_ *macStream, err error) {
	// Enough output packets to keep the converter busy without holding
	// much: one output packet is a frame, so this is 40KB of stereo, a
	// quarter second at 44.1kHz. The buffered decode used the same number
	// to favour throughput, and at this size it costs nothing to stream.
	s := &macStream{packetsPerChunk: 10000}

	// Everything below is released by Close, including when the setup
	// fails partway and the caller never receives the stream.
	defer func() {
		if err != nil {
			s.Close()
		}
	}()

	// Allocate an "AudioFile" backed by a byte slice. The handle inside
	// keeps buf reachable for as long as AudioToolbox can call back, so
	// neither the slice nor its data needs pinning.
	inputFile, err := OpenAudioFileBuffer(buf)
	if err != nil {
		return nil, fmt.Errorf("opening file with callbacks: %w", err)
	}

	s.inputFile = inputFile
	s.pinner.Pin(inputFile)

	// Query the input format.
	var inputDescription C.AudioStreamBasicDescription

	if _, err := inputFile.GetProperty(
		C.kAudioFilePropertyDataFormat,
		C.UInt32(unsafe.Sizeof(inputDescription)),
		unsafe.Pointer(&inputDescription),
	); err != nil {
		return nil, fmt.Errorf("querying for property kAudioFilePropertyDataFormat: %w", err)
	}

	var inputUsesPacketDescriptions C.Boolean

	// For variable encodings, the bytes and frames per packet are found
	// during decoding rather than defined globally.
	if inputDescription.mBytesPerPacket == 0 || inputDescription.mFramesPerPacket == 0 {
		inputUsesPacketDescriptions = _true
	}

	// Define the output format.
	outputDescription := C.AudioStreamBasicDescription{
		mSampleRate:       inputDescription.mSampleRate,
		mChannelsPerFrame: inputDescription.mChannelsPerFrame,
		mFormatID:         C.kAudioFormatLinearPCM,
		mFormatFlags:      C.kAudioFormatFlagIsSignedInteger | C.kAudioFormatFlagIsPacked,
		mBytesPerPacket:   2 * inputDescription.mChannelsPerFrame,
		mFramesPerPacket:  1,
		mBytesPerFrame:    2 * inputDescription.mChannelsPerFrame,
		mBitsPerChannel:   16,
	}

	// Allocate the AudioConverter with our input and output formats.
	// This handles the conversion between audio formats.
	audioConverter, err := NewAudioConverter(&inputDescription, &outputDescription)
	if err != nil {
		return nil, fmt.Errorf("creating audio converter: %w", err)
	}

	s.converter = audioConverter

	magicCookieSize, err := inputFile.GetPropertySize(C.kAudioFilePropertyMagicCookieData)
	if err != nil {
		return nil, fmt.Errorf("getting magic cookie property: %w", err)
	}

	// If a magic cookie exists in the input, set it on the AudioConverter.
	//
	// The magic cookie is a completely opaque piece of data, written and read only
	// by the codec itself. A magic cookie is only present for codecs that require it;
	// this API will return NULL if one does not exist. This API is specific to audio
	// format descriptions, and will return NULL if called with a non-audio format
	// description.
	//
	// https://developer.apple.com/documentation/coremedia/1489508-cmaudioformatdescriptiongetmagic
	if magicCookieSize > 0 {
		magicCookie := make([]byte, 0, magicCookieSize)

		s.pinner.Pin(unsafe.SliceData(magicCookie))

		if _, err := inputFile.GetProperty(
			C.kAudioFilePropertyMagicCookieData,
			magicCookieSize,
			unsafe.Pointer(unsafe.SliceData(magicCookie)),
		); err != nil {
			return nil, fmt.Errorf("getting magic cookie: %w", err)
		}

		if err := audioConverter.SetProperty(
			C.kAudioConverterDecompressionMagicCookie,
			magicCookieSize,
			unsafe.Pointer(unsafe.SliceData(magicCookie)),
		); err != nil {
			return nil, fmt.Errorf("setting magic cookie: %w", err)
		}
	}

	var maxInputPacketSize C.UInt32

	if _, err := inputFile.GetProperty(
		C.kAudioFilePropertyMaximumPacketSize,
		C.UInt32(unsafe.Sizeof(maxInputPacketSize)),
		unsafe.Pointer(&maxInputPacketSize),
	); err != nil {
		return nil, fmt.Errorf("getting maximum packet size from input: %w", err)
	}

	// Allocate the InputContext.
	// This is a structure that we define and use within the [InputDataProc].
	// It gets passed in as a void pointer.
	ic := NewInputContext(
		inputFile,
		inputDescription,
		maxInputPacketSize,
		inputUsesPacketDescriptions,
	)

	s.pinner.Pin(unsafe.SliceData(ic.mPacketDescriptions))

	// The converter keeps this caller data across callbacks, so it goes
	// across as a handle rather than as a Go pointer.
	s.icHandle = cgo.NewHandle(ic)

	s.chunk = make([]byte, int(s.packetsPerChunk)*int(outputDescription.mBytesPerPacket))
	s.pinner.Pin(unsafe.SliceData(s.chunk))

	s.format = Format{
		SampleRate:     int(outputDescription.mSampleRate),
		Channels:       int(outputDescription.mChannelsPerFrame),
		BytesPerSample: int(outputDescription.mBitsPerChannel / 8),
	}

	return s, nil
}

// Format describes the PCM this stream produces, known before any audio
// is read.
func (s *macStream) Format() Format { return s.format }

// Read fills p with decoded PCM, returning io.EOF once the input is
// exhausted.
func (s *macStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return 0, io.EOF
	}

	for len(s.pending) == 0 {
		if s.drained {
			return 0, io.EOF
		}
		if err := s.fill(); err != nil {
			return 0, err
		}
	}

	n := copy(p, s.pending)
	s.pending = s.pending[n:]

	return n, nil
}

// fill runs one turn of the converter loop.
func (s *macStream) fill() error {
	numPackets := s.packetsPerChunk

	// Initialize AudioBufferList with a single buffer because we are
	// working with interleaved PCM samples. mDataByteSize is an in-out
	// variable, and will contain the number of bytes copied to the
	// buffer after the call to FillComplexBuffer.
	abl := C.AudioBufferList{
		mNumberBuffers: 1,
		mBuffers: [1]C.AudioBuffer{{
			mNumberChannels: C.UInt32(s.format.Channels),
			mDataByteSize:   C.UInt32(len(s.chunk)), // in: capacity, out: length
			mData:           unsafe.Pointer(unsafe.SliceData(s.chunk)),
		}},
	}

	if err := s.converter.FillComplexBuffer(
		(C.AudioConverterComplexInputDataProc)(C.InputDataProc),
		C.handleToPointer(C.uintptr_t(s.icHandle)),
		&numPackets,
		&abl,
		nil,
	); err != nil {
		return fmt.Errorf("filling buffer: %w", err)
	}

	s.pending = s.chunk[:abl.mBuffers[0].mDataByteSize]

	// A short fill means the input ran out. This is the same signal the
	// buffered loop used to decide it had reached the end, and the chunk
	// carrying it still holds audio, so it is served before io.EOF.
	if numPackets < s.packetsPerChunk {
		s.drained = true
	}

	return nil
}

// Close releases the pipeline. It is safe to call more than once, and
// abandoning a stream before io.EOF is fine as long as it is closed.
func (s *macStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true
	s.pending = nil

	// Order matters here. The converter can call back into the input
	// file, so it is torn down first; the handle those callbacks resolve
	// stays valid until it cannot be reached; and nothing is unpinned
	// while AudioToolbox could still be holding a pointer to it.
	//
	// AudioFile.Dispose is not idempotent, which is what the closed flag
	// above is guarding.
	if s.converter != nil {
		s.converter.Dispose()
	}
	if s.inputFile != nil {
		s.inputFile.Dispose()
	}
	if s.icHandle != 0 {
		s.icHandle.Delete()
	}

	s.pinner.Unpin()

	return nil
}

const (
	_false = C.Boolean(0)
	_true  = C.Boolean(1)
)

// AudioConverter is responsible for converting between audio formats.
type AudioConverter struct {
	ref      C.AudioConverterRef
	disposed atomic.Bool
}

func NewAudioConverter(inSourceFormat, inDestinationFormat *C.AudioStreamBasicDescription) (*AudioConverter, error) {
	ac := &AudioConverter{}
	if err := C.AudioConverterNew(inSourceFormat, inDestinationFormat, &ac.ref); err != C.noErr {
		return nil, fmt.Errorf("AudioConverterNew: %v", err)
	}
	return ac, nil
}

func (ac *AudioConverter) FillComplexBuffer(
	inInputDataProc C.AudioConverterComplexInputDataProc,
	inInputDataProcUserData unsafe.Pointer,
	ioOutputDataPacketSize *C.UInt32,
	outOutputData *C.AudioBufferList,
	outPacketDescription *C.AudioStreamPacketDescription,
) error {
	if err := C.AudioConverterFillComplexBuffer(
		ac.ref,
		inInputDataProc,
		inInputDataProcUserData,
		ioOutputDataPacketSize,
		outOutputData,
		outPacketDescription,
	); err != C.noErr {
		return fmt.Errorf("AudioConverterFillComplexBuffer: %v", err)
	}
	return nil
}

func (ac *AudioConverter) SetProperty(
	inPropertyID C.AudioFilePropertyID,
	inDataSize C.UInt32,
	inPropertyData unsafe.Pointer,
) error {
	if err := C.AudioConverterSetProperty(ac.ref, inPropertyID, inDataSize, inPropertyData); err != C.noErr {
		return fmt.Errorf("AudioConverterSetProperty: %v", err)
	}
	return nil
}

func (ac *AudioConverter) Dispose() {
	if ac.disposed.Swap(true) {
		return
	}
	if ac.ref != nil {
		C.AudioConverterDispose(ac.ref)
	}
}

type AudioFile struct {
	id         C.AudioFileID
	nextPacket C.SInt64

	// data holds the Go buffer this file reads from, when it was opened
	// from one. AudioToolbox keeps the caller data across callbacks, so
	// it cannot be a Go pointer; a handle is an opaque integer the
	// runtime resolves back for us. Disposing the file releases it.
	data cgo.Handle
}

func OpenAudioFile(path string) (*AudioFile, error) {
	inputFileURL := C.CFURLCreateFromFileSystemRepresentation(
		C.kCFAllocatorDefault,
		(*C.UInt8)(unsafe.Pointer(unsafe.StringData(path))),
		C.long(len(path)),
		_false,
	)

	runtime.KeepAlive(path)

	var fileID C.AudioFileID

	if err := C.AudioFileOpenURL(inputFileURL, C.kAudioFileReadPermission, 0, &fileID); err != C.noErr {
		return nil, fmt.Errorf("AudioFileOpenURL: %v", err)
	}

	C.CFRelease(C.CFTypeRef(inputFileURL))

	af := &AudioFile{id: fileID}

	return af, nil
}

// OpenAudioFileBuffer allocates an AudioFile that operates on a
// set of callbacks instead of a true file. This can be used to supply
// data from arbitrary sources.
//
// The particular implementation here simply wraps a Go byte slice and
// copies the data into the out buffer.
//
// This could be made lazy by wrapping an [io.Reader] instead.
//
// Make sure that [buf] is pinned.
func OpenAudioFileBuffer(buf []byte) (*AudioFile, error) {
	var outAudioFile C.AudioFileID

	handle := cgo.NewHandle(buf)

	// WriteProc and SetSizeProc must be nil, otherwise AudioToolbox considers
	// it a writeable file, which restricts what formats it can accept.
	if err := C.AudioFileOpenWithCallbacks(
		C.handleToPointer(C.uintptr_t(handle)),
		(C.AudioFile_ReadProc)(C.AudioFileReadProcImpl),
		nil,
		(C.AudioFile_GetSizeProc)(C.AudioFileGetSizeProcImpl),
		nil,
		0,
		&outAudioFile,
	); err != C.noErr {
		handle.Delete()
		return nil, fmt.Errorf("AudioFileOpenWithCallbacks: %v", err)
	}

	return &AudioFile{id: outAudioFile, data: handle}, nil
}

func (af *AudioFile) ID() C.AudioFileID {
	return af.id
}

func (af *AudioFile) Dispose() {
	C.AudioFileClose(af.id)
	if af.data != 0 {
		af.data.Delete()
		af.data = 0
	}
}

func (af *AudioFile) NextPacket() C.SInt64 {
	return af.nextPacket
}

func (af *AudioFile) ReadPackets(
	ioNumBytes *C.UInt32,
	outPacketDescriptions *C.AudioStreamPacketDescription,
	ioNumPackets *C.UInt32,
	outBuffer unsafe.Pointer,
) error {
	if err := C.AudioFileReadPacketData(
		af.id,
		_false,
		ioNumBytes,
		outPacketDescriptions,
		af.nextPacket,
		ioNumPackets,
		outBuffer,
	); err != C.noErr {
		return fmt.Errorf("AudioFileReadPacketData: %v", err)
	}
	af.nextPacket += C.SInt64(*ioNumPackets)
	return nil
}

func (af *AudioFile) WritePackets(
	inNumBytes C.UInt32,
	inPacketDescriptions *C.AudioStreamPacketDescription,
	inNumPackets C.UInt32,
	inBuffer unsafe.Pointer,
) error {
	if err := C.AudioFileWritePackets(
		af.id,
		_false,
		inNumBytes,
		inPacketDescriptions,
		af.nextPacket,
		&inNumPackets,
		inBuffer,
	); err != C.noErr {
		return fmt.Errorf("AudioFileWritePackets: %v", err)
	}
	af.nextPacket += C.SInt64(inNumPackets)
	return nil
}

func (af *AudioFile) GetProperty(
	inPropertyID C.AudioFilePropertyID,
	inDataSize C.UInt32,
	outPropertyData unsafe.Pointer,
) (C.UInt32, error) {
	dataSize := inDataSize
	if err := C.AudioFileGetProperty(af.id, inPropertyID, &dataSize, outPropertyData); err != C.noErr {
		return 0, fmt.Errorf("AudioFileGetProperty: %v", err)
	}
	return dataSize, nil
}

func (af *AudioFile) GetPropertySize(inPropertyID C.AudioFilePropertyID) (C.UInt32, error) {
	var (
		size       C.UInt32
		isWritable C.UInt32
	)
	if err := C.AudioFileGetPropertyInfo(af.id, inPropertyID, &size, &isWritable); err != C.noErr {
		if err == C.kAudioFileUnsupportedPropertyError {
			return 0, nil
		}
		return 0, fmt.Errorf("AudioFileGetPropertyInfo: %v", err)
	}
	return size, nil
}

// InputContext is smuggled into the [InputDataProc].
type InputContext struct {
	mInputFile                   *AudioFile
	mMaxInputPacketSize          C.UInt32
	mInputUsesPacketDescriptions C.Boolean
	mInputDescription            C.AudioStreamBasicDescription
	mPacketDescriptions          []C.AudioStreamPacketDescription
}

func NewInputContext(
	inputFile *AudioFile,
	inputDescription C.AudioStreamBasicDescription,
	maxInputPacketSize C.UInt32,
	inputUsesPacketDescriptions C.Boolean,
) *InputContext {
	return &InputContext{
		mInputFile:                   inputFile,
		mInputDescription:            inputDescription,
		mMaxInputPacketSize:          maxInputPacketSize,
		mInputUsesPacketDescriptions: inputUsesPacketDescriptions,
		mPacketDescriptions:          make([]C.AudioStreamPacketDescription, 0, 8),
	}
}

func (ic *InputContext) PacketsRead() C.SInt64 {
	return ic.mInputFile.NextPacket()
}

// InputDataProc reads audio packets from the input file.
//
//export InputDataProc
func InputDataProc(
	inAudioConverter C.AudioConverterRef,
	ioNumberDataPackets *C.UInt32,
	ioData *C.AudioBufferList,
	outDataPacketDescription **C.AudioStreamPacketDescription,
	inUserData unsafe.Pointer,
) C.OSStatus {
	ic := cgo.Handle(uintptr(inUserData)).Value().(*InputContext)

	// Only variable bitrate input carries packet descriptions. Constant
	// bitrate input has none, and there the out-parameter arrives
	// uninitialised, so reading it back to pass along, as this used to,
	// hands the file reader whatever happened to be on the stack.
	//
	// Nothing caught it because the only fixture was AAC, which is
	// variable. The first constant bitrate input, a plain WAV, wedged the
	// converter until the test timeout.
	var packetDescriptions *C.AudioStreamPacketDescription

	if ic.mInputUsesPacketDescriptions == _true {
		// Cap the number of data packets to the capacity of the slice.
		if int(*ioNumberDataPackets) > cap(ic.mPacketDescriptions) {
			*ioNumberDataPackets = C.UInt32(cap(ic.mPacketDescriptions))
		}
		packetDescriptions = unsafe.SliceData(ic.mPacketDescriptions)
	}

	if outDataPacketDescription != nil {
		*outDataPacketDescription = packetDescriptions
	}

	if err := ic.mInputFile.ReadPackets(
		&ioData.mBuffers[0].mDataByteSize,
		packetDescriptions,
		ioNumberDataPackets,
		ioData.mBuffers[0].mData,
	); err != nil {
		return unwrapOSStatus(err)
	}

	return C.noErr
}

// AudioFileReadProcImpl copies data from a Go byte slice to the
// output buffer.
//
//export AudioFileReadProcImpl
func AudioFileReadProcImpl(
	inClientData unsafe.Pointer,
	inPosition C.SInt64,
	requestCount C.UInt32,
	buffer unsafe.Pointer,
	actualCount *C.UInt32,
) C.OSStatus {
	pos := int(inPosition)
	req := int(requestCount)
	end := pos + req

	inBuf := cgo.Handle(uintptr(inClientData)).Value().([]byte)

	// Assuming the the out buffer is sized to contain the requested number of bytes.
	// This is not memory we control.
	outBuf := unsafe.Slice((*byte)(buffer), req)

	dst := outBuf[:req]

	// The requested amount is allowed to exceed the actual size of the
	// audio data, and a read can start beyond the end of it, so clamp
	// both ends. Clamping only the end would slice with the start past
	// the finish and panic inside a C callback, which takes the process
	// with it.
	if pos < 0 {
		pos = 0
	}
	if pos > len(inBuf) {
		pos = len(inBuf)
	}
	if end > len(inBuf) {
		end = len(inBuf)
	}
	if end < pos {
		end = pos
	}

	src := inBuf[pos:end]

	n := copy(dst, src)

	*actualCount = C.UInt32(n)

	return C.noErr
}

//export AudioFileGetSizeProcImpl
func AudioFileGetSizeProcImpl(
	inClientData unsafe.Pointer,
) C.SInt64 {
	inBuf := cgo.Handle(uintptr(inClientData)).Value().([]byte)

	// The length, not the capacity. A slice built by append or returned
	// by io.ReadAll usually has room to spare, and reporting that as the
	// file size tells AudioToolbox there is more audio than there is. It
	// then keeps asking for data past the end, gets short reads reported
	// as success, and never stops: a generated WAV hung the decode for
	// five minutes before this was found.
	return C.SInt64(len(inBuf))
}

// unwrapOSStatus extracts the [OSStatus] from an [error] for conforming to C
// function signatures.
func unwrapOSStatus(err error) C.OSStatus {
	var errOSStatus ErrOSStatus
	if errors.As(err, &errOSStatus) {
		return C.OSStatus(errOSStatus)
	}
	panic(fmt.Errorf("unwrapping OSStatus from %v", err))
}

type ErrOSStatus C.OSStatus

func (e ErrOSStatus) Error() string {
	return fmt.Sprintf("%v", C.OSStatus(e))
}

// openStream decodes incrementally through AudioToolbox.
func openStream(by []byte) (*Stream, error) {
	ms, err := newMacStream(by)
	if err != nil {
		return nil, err
	}
	return &Stream{r: ms, format: ms.Format()}, nil
}

// openStreamFile buffers the compressed file, which is small next to the
// PCM the stream avoids holding, then decodes it incrementally.
func openStreamFile(path string) (*Stream, error) {
	by, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading input file: %w", err)
	}
	return openStream(by)
}
