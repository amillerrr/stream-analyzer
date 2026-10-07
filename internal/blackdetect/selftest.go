package blackdetect

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// The self-test's clips: H.264 made from lavfi sources with libx264 (see
// startup_test.go, which makes them again with -update).
//
//go:embed selftest/*.ts
var clips embed.FS

// selfTestCase is one decode of a clip: the video PID, how many frames,
// and which of them, in presentation order, are black.
type selfTestCase struct {
	file   string
	pid    uint16
	frames int
	black  func(i int) bool
	what   string // what it checks, for a failure
}

func frames(lo, hi int) func(int) bool { return func(i int) bool { return i >= lo && i < hi } }

var selfTestCases = []selfTestCase{
	{"limited.ts", 0x100, 30, frames(10, 20),
		"limited range (bt709) with B-frames: black frames 10-19 (luma 16) across the 33-bit PTS wrap, dark frames 20-22 (luma 40) not black"},
	{"full.ts", 0x100, 20, frames(5, 10),
		"full range: black frames 5-9 (luma 0), dark frames 10-14 (luma 30 of 255) not black; a decoder that drops the range tag makes them black"},
	{"twopid.ts", 0x100, 15, frames(0, 0),
		"two video PIDs: the picture on 0x100, never the black stream on 0x101"},
	{"twopid.ts", 0x101, 15, frames(0, 15),
		"two video PIDs: the black stream on 0x101"},
}

// SelfTest runs blackdetect as the monitor does, with ffmpeg's own default
// thresholds, on clips with known answers, and returns the H.264 decoder
// they went through. ffmpeg must decode every frame without an error, at
// the PTS the clip carries, and call black exactly the frames that are.
func SelfTest(ctx context.Context, o Options) (decoder string, err error) {
	dir, err := os.MkdirTemp("", "stream-analyzer-selftest-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ref := Default()
	o.Duration, o.PixelThreshold, o.PictureThreshold = 0, ref.PixelThreshold, ref.PictureThreshold
	for _, c := range selfTestCases {
		b, err := clips.ReadFile("selftest/" + c.file)
		if err != nil {
			return "", err
		}
		path := filepath.Join(dir, c.file)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return "", err
		}
		o.VideoPID = c.pid
		res, err := Detect(ctx, path, o)
		if err == nil {
			err = c.check(res, presentationPTS(b, c.pid))
		}
		if err != nil {
			return "", fmt.Errorf("clip %s, PID 0x%x (%s): %w", c.file, c.pid, c.what, err)
		}
		decoder = cmp.Or(decoder, res.Decoder)
	}
	return decoder, nil
}

func (c selfTestCase) check(res Result, pts []uint64) error {
	if len(pts) != c.frames {
		return fmt.Errorf("the clip has %d frames, want %d", len(pts), c.frames)
	}
	if n := res.LeadingErrors + res.DecodeErrors; n > 0 {
		return fmt.Errorf("ffmpeg logged %d decode errors (%s)", n, res.FirstDecodeError)
	}
	if len(res.Frames) != len(pts) {
		return fmt.Errorf("ffmpeg decoded %d frames of %d", len(res.Frames), len(pts))
	}
	for i, f := range res.Frames {
		if got := wrap33(f.PTS); got != pts[i] {
			return fmt.Errorf("frame %d came out at PTS %d, want %d", i, got, pts[i])
		}
		if f.Black != c.black(i) {
			return fmt.Errorf("frame %d (PTS %d): black %v, want %v", i, pts[i], f.Black, c.black(i))
		}
	}
	return nil
}

// presentationPTS lists the PTS of every PES on pid in b, in presentation
// order: one per frame in the clips.
func presentationPTS(b []byte, pid uint16) []uint64 {
	var out []uint64
	for off := 0; off+ts.PacketSize <= len(b); off += ts.PacketSize {
		p, err := ts.Decode(b[off : off+ts.PacketSize])
		if err != nil || p.PID != pid || !p.PayloadUnitStartIndicator {
			continue
		}
		if h, err := ts.ParsePESHeader(p.Payload); err == nil && h.HasPTS {
			out = append(out, h.PTS)
		}
	}
	if len(out) == 0 {
		return nil
	}
	ref := out[0]
	slices.SortFunc(out, func(a, b uint64) int { return cmp.Compare(ts.Diff(a, ref), ts.Diff(b, ref)) })
	return out
}

// wrap33 reduces a timestamp ffmpeg unwrapped to a 33-bit PTS.
func wrap33(t int64) uint64 {
	const w = int64(ts.Wrap)
	return uint64((t%w + w) % w)
}
