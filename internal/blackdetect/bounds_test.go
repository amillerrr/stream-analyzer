package blackdetect

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	ff := fakeFFmpeg(t, `i=0
while [ $i -lt 200000 ]; do echo "[h264 @ 0x1] error while decoding MB $i 3, bytestream -5" >&2; i=$((i+1)); done
echo "[blackdetect @ 0x2] black_start:1.5 black_end:2.5 black_duration:1" >&2
echo "frame=  181 fps=0.0" >&2
`)
	var s logSink
	o := Default()
	o.FFmpeg = ff
	iv, err := detectWith(t.Context(), "seg.ts", o, &s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(iv, []Interval{{Start: 1.5, End: 2.5, Duration: 1}}) {
		t.Errorf("intervals %+v", iv)
	}
	if n := s.kept(); n > 64<<10 {
		t.Errorf("kept %d bytes of ffmpeg's log", n)
	}
}

// A child that keeps ffmpeg's stderr open after ffmpeg exits can't hold
// Detect: it returns within its wait delay.
func TestDetectDoesNotWaitForAStrayChild(t *testing.T) {
	ff := fakeFFmpeg(t, "sleep 20 &\necho 'black_start:0 black_end:1 black_duration:1' >&2\n")
	o := Default()
	o.FFmpeg = ff
	start := time.Now()
	iv, err := Detect(t.Context(), "seg.ts", o)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Detect took %v (err %v)", took, err)
	}
	// ffmpeg itself succeeded, so its result stands.
	if err != nil || len(iv) != 1 {
		t.Errorf("intervals %+v, err %v", iv, err)
	}
}
