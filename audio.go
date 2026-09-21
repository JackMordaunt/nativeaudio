// Package nativeaudio decodes compressed audio into PCM using the
// decoder each operating system already ships, falling back to ffmpeg
// where there is no native API to call.
//
//	Windows: Media Foundation
//	  macOS: AudioToolbox
//	  Linux: ffmpeg's libraries, linked
//
// Built without cgo, and on any other operating system, the ffmpeg
// binary is run as a subprocess instead.
//
// Output is always signed 16-bit little-endian PCM, which is directly
// playable and is what the common Go audio stacks expect. The play
// subpackage is a thin convenience over that for callers who just want
// to hear a file.
package nativeaudio

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrClosed is returned when a Decoder is used after Close.
var ErrClosed = errors.New("nativeaudio: decoder is closed")

// Decoder decodes compressed audio into PCM.
//
// Create one with New and release it with Close. A Decoder owns
// whatever platform state the backend requires, which is why it is a
// value rather than a set of package functions: two independent parts
// of a program can hold their own without one tearing down the other.
//
// A Decoder is safe for concurrent use. Decodes may run in parallel,
// and Close waits for those in flight, and for any open Stream to be
// closed, before releasing platform state.
type Decoder struct {
	mu      sync.RWMutex
	closed  bool
	streams sync.WaitGroup
	limits  Limits
}

// Option configures a Decoder at construction.
type Option func(*Decoder)

// WithLimits bounds what a single decode may consume. See [Limits].
func WithLimits(l Limits) Option {
	return func(d *Decoder) { d.limits = l }
}

// New creates a Decoder, initialising any platform state the backend
// needs. Call Close when you are finished with it.
func New(opts ...Option) (*Decoder, error) {
	if err := start(); err != nil {
		return nil, fmt.Errorf("initialising platform decoder: %w", err)
	}
	d := &Decoder{limits: DefaultLimits()}
	for _, opt := range opts {
		opt(d)
	}
	return d, nil
}

// Close releases the platform state held by the Decoder. It is
// idempotent, and any further use of the Decoder returns ErrClosed.
func (d *Decoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	// Wait for open streams before tearing down platform state, which
	// their decoder objects are still using.
	d.streams.Wait()
	if err := end(); err != nil {
		return fmt.Errorf("shutting down platform decoder: %w", err)
	}
	return nil
}

// DecodeFile decodes the audio file at path, returning s16le PCM and
// the format needed to play it back correctly.
func (d *Decoder) DecodeFile(path string) (pcm []byte, format Format, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, format, ErrClosed
	}
	return d.drain(openStreamFile(path))
}

// Decode decodes compressed audio held in memory, returning s16le PCM
// and the format needed to play it back correctly.
func (d *Decoder) Decode(compressed []byte) (pcm []byte, format Format, err error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, format, ErrClosed
	}
	return d.drain(openStream(compressed))
}

// drain reads a stream to completion under the decoder's limits, always
// closing it. Routing the buffered API through the streaming one is what
// lets limits apply to both.
func (d *Decoder) drain(s *Stream, err error) ([]byte, Format, error) {
	if err != nil {
		return nil, Format{}, err
	}
	defer s.Close()
	pcm, err := io.ReadAll(newLimited(s.r, d.limits))
	if err != nil {
		return nil, s.format, err
	}
	return pcm, s.format, nil
}

// Format describes the PCM a decode produced, and is everything needed
// to play it back correctly.
type Format struct {
	SampleRate     int // samples per second.
	Channels       int // number of channels.
	BytesPerSample int // bytes per sample; always 2, for the s16le output this package produces.
}

// Stream decodes compressed audio held in memory, returning PCM through
// an [io.Reader] rather than a single buffer.
//
// The returned Stream must be closed. Every native backend decodes
// incrementally; only the subprocess fallback decodes up front and
// serves from memory, because ffmpeg cannot read every container this
// package supports from a pipe.
func (d *Decoder) Stream(compressed []byte) (*Stream, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	s, err := openStream(compressed)
	if err != nil {
		return nil, err
	}
	s.r = newLimited(s.r, d.limits)
	d.track(s)
	return s, nil
}

// StreamFile decodes the audio file at path, returning PCM through an
// [io.Reader] rather than a single buffer.
//
// The returned Stream must be closed. The Linux backend and the
// subprocess fallback read the file directly, so the PCM is never held
// whole; Windows and macOS read the compressed file into memory first,
// which is small next to the PCM it avoids buffering.
func (d *Decoder) StreamFile(path string) (*Stream, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return nil, ErrClosed
	}
	s, err := openStreamFile(path)
	if err != nil {
		return nil, err
	}
	s.r = newLimited(s.r, d.limits)
	d.track(s)
	return s, nil
}

// track registers an open Stream so Close can wait for it. The caller
// holds at least a read lock, which is what keeps this from racing the
// Wait in Close.
func (d *Decoder) track(s *Stream) {
	d.streams.Add(1)
	s.done = d.streams.Done
}
