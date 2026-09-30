package monitor

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// healthLines runs logHealth once on the only channel and returns the log
// line and the health.csv row it wrote.
func healthLines(t *testing.T, m *Monitor, logs *bytes.Buffer, mu *sync.Mutex) (string, [][]string) {
	t.Helper()
	m.channels[0].logHealth()
	mu.Lock()
	defer mu.Unlock()
	var line string
	for l := range strings.Lines(logs.String()) {
		if strings.Contains(l, "msg=health") {
			line = l
		}
	}
	return line, readHealthCSV(t, m.cfg)
}

func newLoggedMonitor(t *testing.T, cfgURL string, tweak func(*Options)) (*Monitor, *bytes.Buffer, *sync.Mutex) {
	t.Helper()
	cfg := testConfig(t, cfgURL)
	var mu sync.Mutex
	var logs bytes.Buffer
	o := Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil))}
	if tweak != nil {
		tweak(&o)
	}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return m, &logs, &mu
}

// Every failure that stops the monitor collecting evidence or checking
// the stream has a health.csv column and makes the health line a warning,
// even in a minute when segments arrived.
func TestFailuresMakeTheHealthLineAWarning(t *testing.T) {
	for _, tc := range []struct {
		column string
		fail   func(c *Channel)
	}{
		{"write_errors", func(c *Channel) { c.count(func(s *stats) { s.writeErrors++ }) }},
		{"blackdetect_errors", func(c *Channel) { c.count(func(s *stats) { s.blackErrors++ }) }},
		{"queue_drops", func(c *Channel) { c.count(func(s *stats) { s.queueDrops++ }) }},
		{"resolve_errors", func(c *Channel) { c.count(func(s *stats) { s.resolveErrors++ }) }},
		{"monitor_gaps", func(c *Channel) { c.count(func(s *stats) { s.gaps++ }) }},
	} {
		m, logs, mu := newLoggedMonitor(t, "http://127.0.0.1:1/never.m3u8", nil)
		c := m.channels[0]
		c.count(func(s *stats) { s.segments = 10 })
		tc.fail(c)
		line, rows := healthLines(t, m, logs, mu)
		if !strings.Contains(line, "level=WARN") {
			t.Errorf("%s: health line looks healthy: %s", tc.column, line)
		}
		if got := column(t, rows, 1, tc.column); got != "1" {
			t.Errorf("%s column = %q, want 1", tc.column, got)
		}
	}
}

// Free space in the data directory is on every health line; under
// min_free_gb the line is a warning.
func TestLowFreeSpaceMakesTheHealthLineAWarning(t *testing.T) {
	m, logs, mu := newLoggedMonitor(t, "http://127.0.0.1:1/never.m3u8", func(o *Options) {
		o.Config.MinFreeBytes = 5e9
		o.FreeSpace = func(string) (uint64, error) { return 3.2e9, nil }
	})
	m.channels[0].count(func(s *stats) { s.segments = 10 })
	line, rows := healthLines(t, m, logs, mu)
	if got := column(t, rows, 1, "free_gb"); got != "3.2" {
		t.Errorf("free_gb = %q, want 3.2", got)
	}
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "free_gb=3.2") {
		t.Errorf("3.2 GB free under a 5 GB minimum, but: %s", line)
	}
}

// A channel URL the origin never answers with a playlist is counted as
// resolve errors, not left at playlists=0 playlist_errors=0.
func TestUnresolvableChannelIsCounted(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.route("/live/master.m3u8", func(_ int, w http.ResponseWriter) { http.Error(w, "busy", http.StatusServiceUnavailable) })
	m, logs, mu := newLoggedMonitor(t, srv.URL+"/live/master.m3u8", nil)
	c := m.channels[0]
	c.resolve(t.Context())
	c.resolve(t.Context())
	line, rows := healthLines(t, m, logs, mu)
	if got := column(t, rows, 1, "resolve_errors"); got != "2" {
		t.Errorf("resolve_errors = %q, want 2 (%s)", got, line)
	}
}

// A segment whose file could not be written names no file: its record
// says why, and the health line counts the failure.
func TestUnwrittenSegmentNamesNoFile(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(100, entry{uri: "s100.ts"}))
	o.set("/live/s100.ts", tstest.Load(t, "cont_0.ts"))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	buf := filepath.Join(cfg.DataDir, "buffer", "test")
	os.MkdirAll(buf, 0o755)
	os.Chmod(buf, 0o555)
	t.Cleanup(func() { os.Chmod(buf, 0o755) })
	_, recs := run(t, cfg, seen(100))
	r := recs[100]
	if r.File != "" || r.SaveError == "" {
		t.Errorf("record file=%q save_error=%q: want no file and the error", r.File, r.SaveError)
	}
	if r.Analysis == nil {
		t.Error("the segment was not analyzed from memory")
	}
}

// A buffer directory removed while the monitor runs is made again.
func TestDeletedBufferDirectoryIsRecreated(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(100, entry{uri: "s100.ts"}))
	o.set("/live/s100.ts", tstest.Load(t, "cont_0.ts"))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	m, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	c := m.channels[0]
	if err := c.persist(kindSegments, "seg_1.ts", []byte("x")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := os.RemoveAll(c.dir); err != nil {
		t.Fatal(err)
	}
	if err := c.persist(kindSegments, "seg_2.ts", []byte("y")); err != nil {
		t.Errorf("write after the buffer directory was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "seg_2.ts")); err != nil {
		t.Error(err)
	}
}

// A panic while checking one segment is logged with its stack and counted,
// and the channel goes on with the next segment.
func TestPanicInASegmentIsRecovered(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(100, entry{uri: "s100.ts"}, entry{uri: "s101.ts"}))
	o.set("/live/s100.ts", tstest.Load(t, "cont_0.ts"))
	o.set("/live/s101.ts", tstest.Load(t, "cont_1.ts"))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled = true
	var mu sync.Mutex
	var logs bytes.Buffer
	var calls atomic.Int32
	run(t, cfg, seen(101), func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil))
		o.BlackDetector = func(context.Context, string) ([]blackdetect.Interval, error) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			return nil, nil
		}
	})
	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if !strings.Contains(out, "recovered from a panic") || !strings.Contains(out, "boom") || !strings.Contains(out, "goroutine") {
		t.Errorf("no panic with its stack in the log:\n%s", out)
	}
	// The line written when the monitor stopped.
	if got := column(t, readHealthCSV(t, cfg), 1, "panics"); got != "1" {
		t.Errorf("panics = %q, want 1", got)
	}
}

// Frame lateness is gone: on this packager (PCR = DTS - 800 ticks on every
// frame) it only repeated the DTS step. The PCR margins stay.
func TestFrameLatenessIsNotReported(t *testing.T) {
	if slices.Contains(healthColumns, "worst_frame_late_ms") {
		t.Error("health.csv still has worst_frame_late_ms")
	}
	for _, col := range []string{"min_pts_pcr_ms", "min_dts_pcr_ms"} {
		if !slices.Contains(healthColumns, col) {
			t.Errorf("health.csv lost %s", col)
		}
	}
	seg, err := analysis.Analyze(tstest.Base.Segment(0).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	b, err := marshalJSON(SegmentRecord{Seq: 1, Analysis: new(seg.Summary())})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "late") {
		t.Errorf("a segment record still reports lateness:\n%s", b)
	}
}

// health.csv's frame_gaps counts the minute's skipped frame slots, as the
// README says, not the number of gaps.
func TestHealthCountsSkippedFrameSlots(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	ch := m.channels[0]
	// 61783577: 7 slots missing in 2 gaps.
	ch.remember(SegmentRecord{Seq: 1, Events: []analysis.Event{{Type: analysis.EventFrameGap, Count: 2, Slots: 7, GapMs: 233.567}}})
	ch.logHealth()
	if got := column(t, readHealthCSV(t, cfg), 1, "frame_gaps"); got != "7" {
		t.Errorf("frame_gaps = %s, want 7 skipped slots", got)
	}
}
