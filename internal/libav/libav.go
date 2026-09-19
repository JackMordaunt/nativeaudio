//go:build linux && cgo

// Package libav decodes compressed audio by linking ffmpeg's libraries
// directly, rather than shelling out to the ffmpeg binary.
//
// The decode pipeline lives in C. libav is a struct-field API, and the
// layout of AVFrame, AVCodecContext and friends changes between major
// versions, so letting the headers describe them at build time is the
// only way to stay correct across the libavcodec versions distributions
// actually ship. Go sees five functions over an opaque handle.
//
// Output matches what the subprocess path produces: interleaved s16le
// at the source's own sample rate and channel count.
package libav

/*
#cgo pkg-config: libavformat libavcodec libavutil libswresample

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/channel_layout.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

// Size of the buffer avio reads the compressed input through. Nothing
// depends on it beyond amortising the callback.
#define NA_IO_BUFFER 4096

typedef struct na_dec {
	AVFormatContext *fmt;
	AVCodecContext  *codec;
	SwrContext      *swr;
	AVPacket        *pkt;
	AVFrame         *frame;
	AVIOContext     *avio;

	uint8_t *input;      // owned copy of the compressed bytes; memory source only.
	int64_t  input_size;
	int64_t  input_pos;

	int index;           // index of the audio stream being decoded.
	int sample_rate;
	int channels;

	uint8_t *out;        // converted PCM waiting to be collected.
	int      out_cap;
	int      out_len;
	int      out_off;

	int drained;         // demuxer exhausted and the decoder flushed.
} na_dec;

static int na_read_packet(void *opaque, uint8_t *buf, int size) {
	na_dec *d = (na_dec *)opaque;
	int64_t remaining = d->input_size - d->input_pos;
	if (remaining <= 0) {
		return AVERROR_EOF;
	}
	if ((int64_t)size > remaining) {
		size = (int)remaining;
	}
	memcpy(buf, d->input + d->input_pos, size);
	d->input_pos += size;
	return size;
}

// na_seek backs the seeking that mp4 and friends need to read a moov
// atom that trails the audio. It is why decoding from memory uses a
// custom AVIOContext rather than a pipe: the subprocess path cannot
// seek a pipe, which is the whole reason it writes a temporary file.
static int64_t na_seek(void *opaque, int64_t offset, int whence) {
	na_dec *d = (na_dec *)opaque;
	int64_t pos;
	if (whence == AVSEEK_SIZE) {
		return d->input_size;
	}
	switch (whence) {
	case SEEK_SET: pos = offset; break;
	case SEEK_CUR: pos = d->input_pos + offset; break;
	case SEEK_END: pos = d->input_size + offset; break;
	default: return -1;
	}
	if (pos < 0 || pos > d->input_size) {
		return -1;
	}
	d->input_pos = pos;
	return pos;
}

// na_init finds the audio stream and opens a decoder for it. The
// resampler is left until the first frame arrives, because a decoder is
// not obliged to report its sample format before it has decoded
// anything, while codecpar always knows the rate and channel count.
static int na_init(na_dec *d) {
	const AVCodec *codec = NULL;
	AVCodecParameters *par = NULL;
	int err;

	if ((err = avformat_find_stream_info(d->fmt, NULL)) < 0) {
		return err;
	}
	if ((err = av_find_best_stream(d->fmt, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0)) < 0) {
		return err;
	}
	d->index = err;
	par = d->fmt->streams[d->index]->codecpar;

	if (!(d->codec = avcodec_alloc_context3(codec))) {
		return AVERROR(ENOMEM);
	}
	if ((err = avcodec_parameters_to_context(d->codec, par)) < 0) {
		return err;
	}
	if ((err = avcodec_open2(d->codec, codec, NULL)) < 0) {
		return err;
	}
	if (!(d->pkt = av_packet_alloc()) || !(d->frame = av_frame_alloc())) {
		return AVERROR(ENOMEM);
	}

	d->sample_rate = par->sample_rate;
	d->channels = par->ch_layout.nb_channels;
	return 0;
}

// na_convert resamples the decoded frame to interleaved s16le, building
// the resampler on first use from the frame's own description.
static int na_convert(na_dec *d) {
	int channels, want, need, got;
	uint8_t *plane[1];
	int err;

	if (!d->swr) {
		AVChannelLayout out;
		memset(&out, 0, sizeof(out));
		if ((err = av_channel_layout_copy(&out, &d->frame->ch_layout)) < 0) {
			return err;
		}
		err = swr_alloc_set_opts2(&d->swr,
			&out, AV_SAMPLE_FMT_S16, d->frame->sample_rate,
			&d->frame->ch_layout, (enum AVSampleFormat)d->frame->format, d->frame->sample_rate,
			0, NULL);
		av_channel_layout_uninit(&out);
		if (err < 0) {
			return err;
		}
		if ((err = swr_init(d->swr)) < 0) {
			return err;
		}
	}

	channels = d->frame->ch_layout.nb_channels;
	if ((want = swr_get_out_samples(d->swr, d->frame->nb_samples)) < 0) {
		return want;
	}
	need = want * channels * 2;
	if (need > d->out_cap) {
		uint8_t *grown = av_realloc(d->out, need);
		if (!grown) {
			return AVERROR(ENOMEM);
		}
		d->out = grown;
		d->out_cap = need;
	}

	plane[0] = d->out;
	got = swr_convert(d->swr, plane, want, (const uint8_t **)d->frame->extended_data, d->frame->nb_samples);
	if (got < 0) {
		return got;
	}
	d->out_len = got * channels * 2;
	d->out_off = 0;
	return 0;
}

// na_next refills the pending buffer, returning AVERROR_EOF once the
// input is exhausted and the decoder has been flushed.
static int na_next(na_dec *d) {
	int err;

	for (;;) {
		err = avcodec_receive_frame(d->codec, d->frame);
		if (err == 0) {
			if ((err = na_convert(d)) < 0) {
				return err;
			}
			if (d->out_len == 0) {
				continue;
			}
			return 0;
		}
		if (err == AVERROR_EOF) {
			return AVERROR_EOF;
		}
		if (err != AVERROR(EAGAIN)) {
			return err;
		}
		if (d->drained) {
			return AVERROR_EOF;
		}

		err = av_read_frame(d->fmt, d->pkt);
		if (err == AVERROR_EOF) {
			d->drained = 1;
			// A null packet tells the decoder to emit whatever it has
			// buffered, which is where the tail of the audio comes from.
			avcodec_send_packet(d->codec, NULL);
			continue;
		}
		if (err < 0) {
			return err;
		}
		if (d->pkt->stream_index != d->index) {
			av_packet_unref(d->pkt);
			continue;
		}
		err = avcodec_send_packet(d->codec, d->pkt);
		av_packet_unref(d->pkt);
		if (err < 0 && err != AVERROR(EAGAIN)) {
			return err;
		}
	}
}

static int na_read(na_dec *d, uint8_t *buf, int size) {
	int n, err;

	if (d->out_off >= d->out_len) {
		if ((err = na_next(d)) < 0) {
			return err;
		}
	}
	n = d->out_len - d->out_off;
	if (n > size) {
		n = size;
	}
	memcpy(buf, d->out + d->out_off, n);
	d->out_off += n;
	return n;
}

static na_dec *na_alloc(void) {
	return (na_dec *)av_mallocz(sizeof(na_dec));
}

static int na_open_file(na_dec *d, const char *path) {
	int err = avformat_open_input(&d->fmt, path, NULL, NULL);
	if (err < 0) {
		return err;
	}
	return na_init(d);
}

static int na_open_memory(na_dec *d, const uint8_t *data, int64_t size) {
	uint8_t *iobuf;
	int err;

	if (!(d->input = av_malloc(size))) {
		return AVERROR(ENOMEM);
	}
	memcpy(d->input, data, size);
	d->input_size = size;

	if (!(iobuf = av_malloc(NA_IO_BUFFER))) {
		return AVERROR(ENOMEM);
	}
	if (!(d->avio = avio_alloc_context(iobuf, NA_IO_BUFFER, 0, d, na_read_packet, NULL, na_seek))) {
		av_free(iobuf);
		return AVERROR(ENOMEM);
	}
	if (!(d->fmt = avformat_alloc_context())) {
		return AVERROR(ENOMEM);
	}
	d->fmt->pb = d->avio;
	d->fmt->flags |= AVFMT_FLAG_CUSTOM_IO;

	if ((err = avformat_open_input(&d->fmt, NULL, NULL, NULL)) < 0) {
		return err;
	}
	return na_init(d);
}

static void na_free(na_dec *d) {
	if (!d) {
		return;
	}
	if (d->frame) av_frame_free(&d->frame);
	if (d->pkt) av_packet_free(&d->pkt);
	if (d->swr) swr_free(&d->swr);
	if (d->codec) avcodec_free_context(&d->codec);
	// A failed avformat_open_input has already freed the context and
	// nulled it, so this covers both paths.
	if (d->fmt) avformat_close_input(&d->fmt);
	if (d->avio) {
		// Custom IO owns its buffer; closing the format context does not
		// reclaim it, and avio may have swapped it for a larger one.
		av_freep(&d->avio->buffer);
		avio_context_free(&d->avio);
	}
	if (d->input) av_freep(&d->input);
	if (d->out) av_freep(&d->out);
	av_free(d);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"unsafe"
)

// Format describes the PCM a Stream produces.
type Format struct {
	SampleRate     int
	Channels       int
	BytesPerSample int
}

// Startup prepares the library. libav needs no global initialisation
// any more, so this only quietens its logging: libav writes diagnostics
// to stderr by default, and a library has no business doing that to its
// host. Decode failures are reported through error values instead.
func Startup() error {
	C.av_log_set_level(C.AV_LOG_QUIET)
	return nil
}

// Shutdown releases global state. There is none to release.
func Shutdown() error { return nil }

// Stream decodes incrementally, yielding interleaved s16le PCM.
type Stream struct {
	mu     sync.Mutex
	dec    *C.na_dec
	format Format
}

// Open decodes compressed audio held in memory.
//
// The bytes are copied into memory libav owns, which keeps them out of
// reach of the Go collector while the C callbacks read them. Compressed
// audio is roughly an order of magnitude smaller than the PCM it
// becomes, so the copy costs little next to what streaming saves.
func Open(compressed []byte) (*Stream, error) {
	if len(compressed) == 0 {
		return nil, errors.New("libav: no audio to decode")
	}
	dec := C.na_alloc()
	if dec == nil {
		return nil, errors.New("libav: allocating decoder")
	}
	rc := C.na_open_memory(dec, (*C.uint8_t)(unsafe.Pointer(&compressed[0])), C.int64_t(len(compressed)))
	return finish(dec, rc, "decoding audio from memory")
}

// OpenFile decodes the audio file at path. libav reads the file itself,
// so unlike the Media Foundation backend nothing is buffered up front.
func OpenFile(path string) (*Stream, error) {
	dec := C.na_alloc()
	if dec == nil {
		return nil, errors.New("libav: allocating decoder")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	rc := C.na_open_file(dec, cpath)
	return finish(dec, rc, fmt.Sprintf("decoding %q", path))
}

// finish turns an open result into a Stream, releasing the decoder if
// the open failed or produced audio this package cannot represent.
func finish(dec *C.na_dec, rc C.int, what string) (*Stream, error) {
	if rc < 0 {
		C.na_free(dec)
		return nil, fmt.Errorf("libav: %s: %s", what, averr(rc))
	}
	channels := int(dec.channels)
	if channels < 1 || channels > 2 {
		C.na_free(dec)
		return nil, fmt.Errorf("libav: can only handle {1,2} channels got %d", channels)
	}
	return &Stream{
		dec: dec,
		format: Format{
			SampleRate: int(dec.sample_rate),
			Channels:   channels,
			// Always 2: the resampler is configured for s16 regardless
			// of what the source held.
			BytesPerSample: 2,
		},
	}, nil
}

// Format describes the PCM this Stream produces. It is known as soon as
// the Stream is opened, before any audio is read.
func (s *Stream) Format() Format { return s.format }

// Read fills p with decoded PCM, returning io.EOF once the audio is
// exhausted.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dec == nil {
		return 0, io.EOF
	}
	n := C.na_read(s.dec, (*C.uint8_t)(unsafe.Pointer(&p[0])), C.int(len(p)))
	switch {
	case n == C.int(eof):
		return 0, io.EOF
	case n < 0:
		return 0, fmt.Errorf("libav: decoding: %s", averr(n))
	}
	return int(n), nil
}

// Close releases the decoder. It is safe to call more than once, and
// abandoning a Stream before io.EOF is fine as long as it is closed.
//
// Nothing can wedge here the way Media Foundation can: the decode runs
// on the calling goroutine, so there is no worker to wait for and
// nothing in flight once Read has returned.
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dec == nil {
		return nil
	}
	C.na_free(s.dec)
	s.dec = nil
	return nil
}

// eof is libav's AVERROR_EOF, computed once rather than spelled out,
// since the macro is a byte-order-dependent FourCC.
var eof = C.int(C.AVERROR_EOF)

// averr renders a libav error code the way ffmpeg would report it.
func averr(code C.int) string {
	buf := make([]byte, C.AV_ERROR_MAX_STRING_SIZE)
	if C.av_strerror(code, (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf))) < 0 {
		return fmt.Sprintf("error %d", int(code))
	}
	if i := bytesIndexZero(buf); i >= 0 {
		buf = buf[:i]
	}
	return string(buf)
}

func bytesIndexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
