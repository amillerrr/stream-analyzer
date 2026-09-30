package monitor

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// Real ffmpeg output: a 25 fps clip black from 0.5 s to 1.5 s, cut into
// 1 s MPEG-TS segments. The run is one run of 1.0 s on screen: it joins
// across the boundary in PTS, and the part in the first segment lasts one
// frame past that segment's last frame, where ffmpeg ends it.
func TestBlackRunJoinsAcrossRealSegments(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	gen := exec.CommandContext(t.Context(), ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=25:duration=2,drawbox=color=black:t=fill:enable='between(t,0.5,1.49)',format=yuv420p",
		"-c:v", "mpeg2video", "-g", "25", "-force_key_frames", "expr:gte(t,n_forced*1)",
		"-f", "segment", "-segment_time", "1", "-segment_format", "mpegts", filepath.Join(dir, "seg_%d.ts"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clips: %v\n%s", err, out)
	}
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Blackdetect.TriggerMin = 0.9
	ch := newTestMonitor(t, cfg, nil).channels[0]
	var got analysis.Fault
	for seq := range uint64(2) {
		path := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", seq))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		seg, err := analysis.Analyze(b)
		if err != nil {
			t.Fatal(err)
		}
		iv, err := blackdetect.Detect(t.Context(), path, cfg.Blackdetect.Options)
		if err != nil {
			t.Fatal(err)
		}
		runs := placeBlack(seg, iv, seg.Video.FrameTicks)
		t.Logf("segment %d: ffmpeg %+v -> %+v", seq, iv, runs)
		if f, ok := ch.blackTrigger(seq, true, seg, runs); ok {
			got = f
		}
	}
	if got.Type == "" {
		t.Fatal("no black_video fault for a 1.0 s run with trigger_min 0.9 s")
	}
	if l := got.Values["longest_run_s"].(float64); math.Abs(l-1.0) > 0.005 {
		t.Errorf("longest_run_s = %.3f, want 1.000 (%s)", l, got.Message)
	}
}

// A black run over frames the encoder dropped is time on screen, but not
// black pictures: each run records its black frames and the time with no
// frame at all, and the fault reports both.
func TestBlackRunSeparatesBlackFramesFromMissingFrames(t *testing.T) {
	// No reordering (PTS = DTS + 2 frames), and every other frame dropped:
	// 8 frames on slots 0, 2, ... 14.
	s := tstest.Base.Segment(0)
	for i := range s.Video {
		s.Video[i].PTS = ts.Add(s.Video[i].DTS, 2*tstest.FrameTicks)
	}
	half := dropFrames(s, func(i int) bool { return i%2 == 0 })
	seg, err := analysis.Analyze(half.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	first := float64(seg.Video.MinPTS()) / ts.Hz
	last := float64(seg.Video.MaxPTS()) / ts.Hz
	runs := placeBlack(seg, []blackdetect.Interval{{Start: first, End: last, Duration: last - first}}, tstest.FrameTicks)
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	r := runs[0]
	// On screen from slot 0 to the end of slot 14: 15 slots, 7 of them
	// with no frame.
	if r.Frames != 8 || math.Abs(r.Duration-0.5005) > 1e-6 || math.Abs(r.NoFrameS-0.233567) > 1e-6 {
		t.Errorf("run %+v: want 8 black frames over 0.500500 s, 0.233567 s of it with no frame", r)
	}

	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Blackdetect.TriggerMin = 0.5
	ch := newTestMonitor(t, cfg, nil).channels[0]
	ch.variant = &hls.Variant{FrameRate: "29.970"}
	f, ok := ch.blackTrigger(1, true, seg, runs)
	if !ok {
		t.Fatal("no fault")
	}
	if f.Values["black_frames"] != 8 || f.Values["black_frames_s"] != 0.267 || f.Values["no_frame_s"] != 0.234 {
		t.Errorf("fault values %v", f.Values)
	}
	if !strings.Contains(f.Message, "0.267 s of black frames") || !strings.Contains(f.Message, "0.234 s with no frame") {
		t.Errorf("message %q does not separate black frames from missing frames", f.Message)
	}
}

// ffmpeg is asked for every black run, however short, and d applies to
// whole runs: 0.08 s at the end of one segment and 0.52 s at the start of
// the next are one run of 0.60 s.
func TestShortBlackRunAtABoundaryStillJoins(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	gen := exec.CommandContext(t.Context(), ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=25:duration=2,drawbox=color=black:t=fill:enable='between(t,0.91,1.5)',format=yuv420p",
		"-c:v", "mpeg2video", "-g", "25", "-force_key_frames", "expr:gte(t,n_forced*1)",
		"-f", "segment", "-segment_time", "1", "-segment_format", "mpegts", filepath.Join(dir, "seg_%d.ts"))
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clips: %v\n%s", err, out)
	}
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Blackdetect.Enabled = true
	cfg.Blackdetect.TriggerMin = 0.55 // d stays 0.1
	m := newTestMonitor(t, cfg, nil)
	ch := m.channels[0]
	var got analysis.Fault
	for seq := range uint64(2) {
		path := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", seq))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		seg, err := analysis.Analyze(b)
		if err != nil {
			t.Fatal(err)
		}
		iv, err := m.detectBlack(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		runs := placeBlack(seg, iv, seg.Video.FrameTicks)
		t.Logf("segment %d: %+v", seq, runs)
		if f, ok := ch.blackTrigger(seq, true, seg, runs); ok {
			got = f
		}
	}
	if got.Type == "" {
		t.Fatal("0.08 s + 0.52 s of black across a boundary raised no fault with trigger_min 0.55 s")
	}
	if l := got.Values["longest_run_s"].(float64); math.Abs(l-0.60) > 0.005 {
		t.Errorf("longest_run_s = %.3f, want 0.600", l)
	}
}
