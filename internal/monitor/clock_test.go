package monitor

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
)

// report.json stores every time in UTC, like the ids, file names and CSV
// files, whatever the machine's zone.
func TestReportTimesAreUTC(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	pdt := time.FixedZone("PDT", -7*3600)
	clock := time.Date(2026, 1, 3, 4, 29, 57, 0, pdt)
	m := newTestMonitor(t, cfg, func() time.Time { return clock })
	m.incidents.fault(m.channels[0], []analysis.Fault{{Type: analysis.FaultPTSBehindPCR, Seq: 1}}, SegmentRecord{Seq: 1, URI: "a"})
	clock = clock.Add(2 * time.Minute)
	m.incidents.tick(clock)
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents %v", dirs)
	}
	b, err := os.ReadFile(filepath.Join(dirs[0], "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"opened_at", "last_fault_at", "detected_at", "closed_at"} {
		m := regexp.MustCompile(`"` + key + `": "([^"]+)"`).FindSubmatch(b)
		if m == nil {
			t.Errorf("no %s in report.json", key)
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, string(m[1])); err != nil || at.Location() != time.UTC {
			t.Errorf("%s = %s, want UTC", key, m[1])
		}
	}
}

// The machine sleeping (macOS stops the monotonic clock while it sleeps),
// or the wall clock being stepped, shows as wall time and monotonic time
// disagreeing between two health ticks.
func TestClockJump(t *testing.T) {
	t0 := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		wall, mono time.Duration
		want       time.Duration
	}{
		{"steady", time.Minute, time.Minute, 0},
		{"slept 9 minutes", 10 * time.Minute, time.Minute, 9 * time.Minute},
		{"clock stepped back", 50 * time.Second, time.Minute, -10 * time.Second},
	} {
		if got := clockJump(t0, 0, t0.Add(tc.wall), tc.mono); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
