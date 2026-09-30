package monitor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The storage cap bounds everything the monitor keeps: the rolling buffer
// and open incidents count toward it, although only closed incidents are
// deleted to meet it.
func TestStorageCapCountsTheBufferAndOpenIncidents(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.IncidentStorageBytes = 3500
	m := newTestMonitor(t, cfg, nil)
	write := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "seg_1.ts"), make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(cfg.DataDir, "incidents")
	write(filepath.Join(cfg.DataDir, "buffer", "test"))
	for _, name := range []string{"20260101T000000Z_a", "20260102T000000Z_b", "20260103T000000Z_open"} {
		write(filepath.Join(root, name, "segments"))
	}
	m.incidents.open["test"] = &incident{dir: filepath.Join(root, "20260103T000000Z_open")}

	m.incidents.enforceCap() // 4000 bytes: the buffer, two closed incidents and an open one

	var left []string
	for _, d := range incidentDirs(t, cfg) {
		left = append(left, filepath.Base(d))
	}
	if want := []string{"20260102T000000Z_b", "20260103T000000Z_open"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

// A channel prunes its buffer as it starts: files a previous run left a day
// ago are gone before an incident could copy them as its pre-roll, while
// those from a restart a minute ago stay.
func TestBufferIsPrunedAtStart(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	c := newTestMonitor(t, cfg, nil).channels[0]
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]time.Duration{"seg_1.ts": 26 * time.Hour, "seg_2.ts": time.Minute} {
		path := filepath.Join(c.dir, name)
		os.WriteFile(path, []byte("x"), 0o644)
		at := time.Now().Add(-age)
		os.Chtimes(path, at, at)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.run(ctx)
	if _, err := os.Stat(filepath.Join(c.dir, "seg_1.ts")); err == nil {
		t.Error("a day-old buffer file survived the start")
	}
	if _, err := os.Stat(filepath.Join(c.dir, "seg_2.ts")); err != nil {
		t.Errorf("a minute-old buffer file is gone: %v", err)
	}
}

// A segment that was never fetched (its URI didn't resolve) is logged at
// the time it was handled, not at year 1.
func TestUnfetchedSegmentIsLoggedNow(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	at := time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)
	c := newTestMonitor(t, cfg, func() time.Time { return at }).channels[0]
	c.logEvents(SegmentRecord{Seq: 5, SCTE35: []string{"#EXT-X-CUE-IN"}})
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "scte35.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "2026-09-30T18:00:00.000Z,test,5,") {
		t.Errorf("scte35.csv:\n%s", b)
	}
}

// When the incidents directory can't be read the storage cap can't be
// applied, and the log says so instead of nothing.
func TestStorageCapThatCannotRunSaysSo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any directory")
	}
	m, logs, mu := newLoggedMonitor(t, "http://127.0.0.1:1/never.m3u8", nil)
	if err := os.MkdirAll(m.incidentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(m.incidentDir, 0o000)
	t.Cleanup(func() { os.Chmod(m.incidentDir, 0o755) })
	m.incidents.enforceCap()
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "storage cap") {
		t.Errorf("no log line about the storage cap:\n%s", logs.String())
	}
}
