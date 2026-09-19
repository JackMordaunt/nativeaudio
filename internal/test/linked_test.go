//go:build linux && cgo

package test

import (
	"bytes"
	"testing"

	"git.sr.ht/~jackmordaunt/nativeaudio"
)

// TestLinkedMatchesSubprocess compares the linked backend against the
// ffmpeg binary installed on the same machine.
//
// The reference PCM checked into this directory ages: it was produced by
// one ffmpeg, and a later one decodes the same file to a different amount
// of trailing silence. This comparison cannot age, because both sides
// come from the same install and move together. It is also the only
// check that would notice the linked backend drifting away from the tool
// it is meant to be equivalent to, which a frozen reference stops being
// able to see once the two have diverged for unrelated reasons.
func TestLinkedMatchesSubprocess(t *testing.T) {
	for _, name := range []string{"compressed.m4a", "mixkit-game-level-music-689.wav"} {
		t.Run(name, func(t *testing.T) {
			reference, rf, err := nativeaudio.FFmpegLoad(name)
			if err != nil {
				t.Fatalf("decoding with the ffmpeg binary: %v", err)
			}
			linked, lf, err := newDecoder(t).DecodeFile(name)
			if err != nil {
				t.Fatalf("decoding with the linked backend: %v", err)
			}
			if lf != rf {
				t.Fatalf("format mismatch: linked %+v, ffmpeg %+v", lf, rf)
			}
			// Worth reporting but not worth failing over: the binary is
			// free to apply defaults the library API does not, so the two
			// are held to the same tolerance as any other pair of decoders.
			t.Logf("byte-identical: %v (linked %d bytes, ffmpeg %d bytes)",
				bytes.Equal(linked, reference), len(linked), len(reference))
			if !equal(t, linked, reference, lf.Channels*lf.BytesPerSample) {
				t.Fatalf("the linked backend disagrees with the ffmpeg binary")
			}
		})
	}
}
