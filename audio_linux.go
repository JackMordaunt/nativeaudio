//go:build linux && cgo

package nativeaudio

import (
	"git.sr.ht/~jackmordaunt/nativeaudio/internal/libav"
)

func start() error {
	return libav.Startup()
}

func end() error {
	return libav.Shutdown()
}

// openStream decodes incrementally through libav.
//
// Unlike the subprocess path this needs no temporary file: a custom
// AVIOContext gives the demuxer a seekable view over the bytes, which
// is what containers like mp4 want and what a pipe cannot offer.
func openStream(compressed []byte) (*Stream, error) {
	ls, err := libav.Open(compressed)
	if err != nil {
		return nil, err
	}
	return adapt(ls), nil
}

// openStreamFile decodes the file incrementally, letting libav read it
// rather than buffering the compressed bytes first.
func openStreamFile(path string) (*Stream, error) {
	ls, err := libav.OpenFile(path)
	if err != nil {
		return nil, err
	}
	return adapt(ls), nil
}

// adapt wraps a libav stream in the package's own Stream type.
func adapt(ls *libav.Stream) *Stream {
	f := ls.Format()
	return &Stream{
		r: ls,
		format: Format{
			SampleRate:     f.SampleRate,
			Channels:       f.Channels,
			BytesPerSample: f.BytesPerSample,
		},
	}
}
