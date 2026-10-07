package blackdetect

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	return path
}

// A 2 s clip at 25 fps whose frames from t=0.5 to t=1.0 are black, muxed
// with PTS starting at 1001.4 s: times are the stream's own (PTS in
// seconds), so the black run is 1001.92-1002.44 s (first and last frame
// boundaries), whatever the file's start.
func TestDetectFindsBlackInStreamTime(t *testing.T) {
	ff := requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "black.ts")
	gen := exec.CommandContext(t.Context(), ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=25:duration=2,drawbox=color=black:t=fill:enable='between(t,0.5,1)',format=yuv420p",
		"-c:v", "mpeg2video", "-output_ts_offset", "1000", "-f", "mpegts", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v\n%s", err, out)
	}

	res, err := Detect(t.Context(), path, Default())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Intervals; len(got) != 1 || math.Abs(got[0].Start-1001.92) > 0.001 || math.Abs(got[0].End-1002.44) > 0.001 {
		t.Errorf("intervals = %+v, want one at 1001.92-1002.44", got)
	}

	// d is honored: a 0.52 s run is shorter than d=0.6.
	o := Default()
	o.Duration = 0.6
	if res, err := Detect(t.Context(), path, o); err != nil || len(res.Intervals) != 0 {
		t.Errorf("with d=0.6: %+v, %v; want none", res.Intervals, err)
	}
}

func TestDetectReportsFFmpegFailure(t *testing.T) {
	requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "not-video.ts")
	if err := os.WriteFile(path, []byte("<html>502 Bad Gateway</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Detect(t.Context(), path, Default()); err == nil {
		t.Error("expected an error for a file ffmpeg cannot decode")
	}
}

// Only the video PID the TS parser analyzed is decoded. A PID the file
// doesn't carry is an error, never a decode of whatever video ffmpeg would
// pick instead.
func TestDetectDecodesOnlyTheGivenPID(t *testing.T) {
	ff := requireFFmpeg(t)
	path := filepath.Join(t.TempDir(), "one.ts")
	gen := exec.CommandContext(t.Context(), ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=black:size=64x64:rate=25:duration=1",
		"-c:v", "mpeg2video", "-f", "mpegts", path)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v\n%s", err, out)
	}
	o := Default()
	o.FFmpeg = ff
	o.VideoPID = 0x100
	if _, err := Detect(t.Context(), path, o); err != nil {
		t.Fatalf("PID 0x100: %v", err)
	}
	o.VideoPID = 0x101
	if iv, err := Detect(t.Context(), path, o); err == nil || !strings.Contains(err.Error(), "0x101") {
		t.Errorf("PID 0x101 is not in the file: intervals %+v, error %v", iv, err)
	}
}

// Detect returns every frame ffmpeg decoded, with its exact PTS (90 kHz
// ticks, as -copyts gives them) and whether blackdetect called it black.
// A run that lasts to the end of the file and one that ends one frame
// before it print the same black_end (the last frame's PTS); the frames
// tell them apart.
func TestDetectReportsEveryDecodedFrame(t *testing.T) {
	ff := requireFFmpeg(t)
	for _, tc := range []struct {
		name      string
		lastBlack bool
		enable    string
	}{
		{"black to the end", true, "gte(n,20)"},
		{"black until one frame before the end", false, "between(n,20,23)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 25 frames at 25 fps from PTS 1000 s (ffmpeg's mpegts muxer adds
			// 1.4 s): frame n at (1001.4 + n/25) s.
			path := filepath.Join(t.TempDir(), "clip.ts")
			gen := exec.CommandContext(t.Context(), ff, "-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc=size=64x64:rate=25:duration=1,drawbox=color=black:t=fill:enable='"+tc.enable+"',format=yuv420p",
				"-c:v", "mpeg2video", "-output_ts_offset", "1000", "-f", "mpegts", path)
			if out, err := gen.CombinedOutput(); err != nil {
				t.Skipf("cannot generate test clip: %v\n%s", err, out)
			}
			o := Default()
			o.FFmpeg, o.VideoPID = ff, 0x100
			res, err := Detect(t.Context(), path, o)
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Frames) != 25 {
				t.Fatalf("%d frames, want 25: %+v", len(res.Frames), res.Frames)
			}
			for n, f := range res.Frames {
				want := Frame{PTS: 90126000 + int64(n)*3600, Black: n >= 20 && (n <= 23 || tc.lastBlack)}
				if f != want {
					t.Errorf("frame %d = %+v, want %+v", n, f, want)
				}
			}
			if len(res.Intervals) != 1 {
				t.Errorf("intervals %+v, want one", res.Intervals)
			}
		})
	}
}

// Detect reports the pixel format and color range blackdetect judged the
// frames in, and Range overrides what the stream signals: luma 30 is black
// in limited range (at most 37 at pix_th 0.10) and not in full range (at
// most 25).
func TestDetectColorRange(t *testing.T) {
	ff := requireFFmpeg(t)
	dir := t.TempDir()
	// Limited range, luma 30 (MPEG-2 can't signal full range).
	limited := filepath.Join(dir, "tv.ts")
	cmd := exec.CommandContext(t.Context(), ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=64x64:r=25:d=0.4,format=yuv420p,geq=lum=30:cb=128:cr=128",
		"-c:v", "mpeg2video", "-qscale:v", "1", "-color_range", "tv", "-f", "mpegts", limited)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v\n%s", err, out)
	}
	// Full range: the self-test clip, whose frame 10 has luma 30.
	full := filepath.Join(dir, "full.ts")
	b, err := clips.ReadFile("selftest/full.ts")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, b, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, force, wantRange string
		frame                        int
		black                        bool
	}{
		{"limited, as signaled", limited, "", "tv", 0, true},
		{"limited, forced full", limited, "full", "pc", 0, false},
		{"full, as signaled", full, "", "pc", 10, false},
		{"full, forced limited", full, "limited", "tv", 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Default()
			o.FFmpeg, o.VideoPID, o.Duration, o.Range = ff, 0x100, 0, tc.force
			res, err := Detect(t.Context(), tc.path, o)
			if err != nil {
				t.Fatal(err)
			}
			if res.ColorRange != tc.wantRange || res.PixFmt == "" {
				t.Errorf("pix_fmt %q, color range %q; want range %q", res.PixFmt, res.ColorRange, tc.wantRange)
			}
			if got := len(res.Frames) > tc.frame && res.Frames[tc.frame].Black; got != tc.black {
				t.Errorf("frame %d black %v, want %v", tc.frame, got, tc.black)
			}
		})
	}
}
