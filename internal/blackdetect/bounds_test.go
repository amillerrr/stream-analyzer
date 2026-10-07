package blackdetect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeFFmpeg writes a shell script standing in for ffmpeg.
func fakeFFmpeg(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// ffmpeg's log is read as it comes: however much a broken file makes it
// write, only the black lines and the last few lines are kept, and the
// black runs are all found.
func TestDetectKeepsLittleOfALongLog(t *testing.T) {
	// Frames at 25 fps from 1 s, black from 1.5 s to 2.5 s.
	frames := frameLines(frames25(50), func(i int) bool { return i >= 13 && i < 38 })
	ff := fakeFFmpeg(t, `i=0
while [ $i -lt 200000 ]; do echo "[h264 @ 0x1] [error] error while decoding MB $i 3, bytestream -5" >&2; i=$((i+1)); done
cat >&2 <<'LOG'
`+frames+`[blackdetect @ 0x2] [info] black_start:1.52 black_end:2.52 black_duration:1
[info] frame=   50 fps=0.0
LOG
`)
	var s logSink
	o := Default()
	o.FFmpeg = ff
	res, err := detectWith(t.Context(), "seg.ts", o, &s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Intervals, []Interval{{Start: 1.52, End: 2.52, Duration: 1}}) {
		t.Errorf("intervals %+v", res.Intervals)
	}
	if n := s.kept(); n > 64<<10 {
		t.Errorf("kept %d bytes of ffmpeg's log", n)
	}
}

// A child that keeps ffmpeg's stderr open after ffmpeg exits can't hold
// Detect: it returns within its wait delay.
func TestDetectDoesNotWaitForAStrayChild(t *testing.T) {
	ff := fakeFFmpeg(t, "sleep 20 &\ncat >&2 <<'LOG'\n"+frameLines(frames25(26), func(i int) bool { return i < 25 })+
		"[blackdetect @ 0x2] [info] black_start:1 black_end:2 black_duration:1\nLOG\n")
	o := Default()
	o.FFmpeg = ff
	start := time.Now()
	res, err := Detect(t.Context(), "seg.ts", o)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Detect took %v (err %v)", took, err)
	}
	// ffmpeg itself succeeded, so its result stands.
	if err != nil || len(res.Intervals) != 1 {
		t.Errorf("intervals %+v, err %v", res.Intervals, err)
	}
}

// Decoder errors are counted apart before and after the first frame ffmpeg
// decoded: before it they are frames dropped for want of a keyframe. A
// demuxer's errors are not decode errors.
func TestDetectCountsDecodeErrorsAfterTheFirstFrame(t *testing.T) {
	ff := fakeFFmpeg(t, `cat >&2 <<'LOG'
[h264 @ 0x1] [error] non-existing PPS 0 referenced
[h264 @ 0x1] [error] no frame!
[Parsed_metadata_3 @ 0x2] [info] frame:0    pts:1000    pts_time:0.0111111
[Parsed_metadata_3 @ 0x2] [info] sa.frame=1
[h264 @ 0x1] [error] error while decoding MB 3 2, bytestream -5
[mpegts @ 0x3] [error] Packet corrupt (stream = 1, dts = 5).
[vist#0:0/h264 @ 0x4] [dec:h264 @ 0x5] [error] Decoding error: Invalid data found when processing input
[h264 @ 0x1] [warning] mmco: unref short failure
[Parsed_metadata_3 @ 0x2] [info] frame:1    pts:4003    pts_time:0.0444778
[Parsed_metadata_3 @ 0x2] [info] sa.frame=1
LOG
`)
	o := Default()
	o.FFmpeg = ff
	res, err := Detect(t.Context(), "seg.ts", o)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Frames) != 2 || res.LeadingErrors != 2 || res.DecodeErrors != 2 ||
		res.FirstDecodeError != "[h264 @ 0x1] [error] error while decoding MB 3 2, bytestream -5" {
		t.Errorf("result %+v: want 2 frames, 2 errors before the first and 2 after", res)
	}
}

// frameLines prints the metadata filter's lines for frames at the given
// PTS (90 kHz ticks), marking the first black frame of each run and the
// first frame after it, as ffmpeg does.
func frameLines(pts []int64, black func(i int) bool) string {
	var b strings.Builder
	in := false
	for i, p := range pts {
		fmt.Fprintf(&b, "[Parsed_metadata_3 @ 0x2] [info] frame:%-4d pts:%-7d pts_time:%g\n", i, p, float64(p)/90000)
		b.WriteString("[Parsed_metadata_3 @ 0x2] [info] sa.frame=1\n")
		switch {
		case black(i) && !in:
			fmt.Fprintf(&b, "[Parsed_metadata_3 @ 0x2] [info] lavfi.black_start=%g\n", float64(p)/90000)
		case !black(i) && in:
			fmt.Fprintf(&b, "[Parsed_metadata_3 @ 0x2] [info] lavfi.black_end=%g\n", float64(p)/90000)
		}
		in = black(i)
	}
	return b.String()
}

// Frames at 25 fps from 1 s: frame i at 90000 + 3600 i.
func frames25(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = 90000 + 3600*int64(i)
	}
	return out
}

// Lines Detect must not guess at: anything blackdetect prints that isn't
// a black run, a frame with no timestamp, and runs the frames don't show.
// Each is an error, so the segment counts as not checked.
func TestDetectRejectsOutputItCannotRead(t *testing.T) {
	blackFrom := func(lo, hi int) func(int) bool { return func(i int) bool { return i >= lo && i < hi } }
	for _, tc := range []struct{ name, log string }{
		{"unknown blackdetect line", frameLines(frames25(10), blackFrom(2, 4)) +
			"[blackdetect @ 0x1] [info] black_start:1.08 black_end:1.16 black_duration:0.08\n" +
			"[blackdetect @ 0x1] [info] black frames: 2\n"},
		{"frame with no timestamp", "[Parsed_metadata_3 @ 0x2] [info] frame:0    pts:NOPTS   pts_time:NOPTS\n"},
		{"a run the frames don't show", frameLines(frames25(10), blackFrom(2, 4)) +
			"[blackdetect @ 0x1] [info] black_start:1.2 black_end:1.28 black_duration:0.08\n"},
		{"frames show a run ffmpeg didn't print", frameLines(frames25(10), blackFrom(2, 4))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := fakeFFmpeg(t, "cat >&2 <<'LOG'\n"+tc.log+"LOG\n")
			o := Default()
			o.FFmpeg, o.Duration = ff, 0
			res, err := Detect(t.Context(), "seg.ts", o)
			if _, ok := errors.AsType[*FormatError](err); !ok {
				t.Errorf("result %+v, error %v; want a FormatError", res, err)
			}
		})
	}
}

// What ffmpeg really prints is read: times before 7.0 have 6 significant
// digits, a run near the 33-bit wrap has negative times, and a run that
// lasts to the end of the file ends at the last frame.
func TestDetectReadsWhatFFmpegPrints(t *testing.T) {
	near := []int64{-9009, -6006, -3003, 0, 3003}
	for _, tc := range []struct{ name, log string }{
		{"coarse times", frameLines([]int64{4889109000, 4889112003, 4889115006, 4889118009}, func(i int) bool { return i == 1 || i == 2 }) +
			"[blackdetect @ 0x1] [info] black_start:54323.5 black_end:54323.5 black_duration:0.0667333\n"},
		{"negative times", frameLines(near, func(i int) bool { return i >= 1 && i <= 2 }) +
			"[blackdetect @ 0x1] [info] black_start:-0.0667333 black_end:0 black_duration:0.0667333\n"},
		{"run to the end", frameLines(frames25(10), func(i int) bool { return i >= 7 }) +
			"[blackdetect @ 0x1] [info] black_start:1.28 black_end:1.36 black_duration:0.08\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := fakeFFmpeg(t, "cat >&2 <<'LOG'\n"+tc.log+"LOG\n")
			o := Default()
			o.FFmpeg, o.Duration = ff, 0
			if res, err := Detect(t.Context(), "seg.ts", o); err != nil || len(res.Intervals) != 1 {
				t.Errorf("result %+v, error %v; want one run", res, err)
			}
		})
	}
}
