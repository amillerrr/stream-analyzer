package blackdetect

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseLog(t *testing.T) {
	log := `Input #0, mpegts, from 'seg.ts':
[Parsed_blackdetect_0 @ 0x147706b20] black_start:0.52 black_end:1.04 black_duration:0.52
[null @ 0x15a129c00] Application provided invalid, non monotonically increasing dts
[blackdetect @ 0x7f8] black_start:5.9 black_end:6.006 black_duration:0.106
frame=  181 fps=0.0 q=-0.0 Lsize=N/A time=00:00:06.03
`
	got := ParseLog(log)
	want := []Interval{{Start: 0.52, End: 1.04, Duration: 0.52}, {Start: 5.9, End: 6.006, Duration: 0.106}}
	if !slices.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if got := ParseLog("frame=181 fps=0.0\n"); len(got) != 0 {
		t.Errorf("no black lines: got %+v", got)
	}
}

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

	got, err := Detect(t.Context(), path, Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || math.Abs(got[0].Start-1001.92) > 0.001 || math.Abs(got[0].End-1002.44) > 0.001 {
		t.Errorf("intervals = %+v, want one at 1001.92-1002.44", got)
	}

	// d is honored: a 0.52 s run is shorter than d=0.6.
	o := Default()
	o.Duration = 0.6
	if got, err := Detect(t.Context(), path, o); err != nil || len(got) != 0 {
		t.Errorf("with d=0.6: %+v, %v; want none", got, err)
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
