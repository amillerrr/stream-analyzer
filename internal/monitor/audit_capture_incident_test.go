package monitor

// Scratch audit tests (incident capture races, verdicts, cap). Not part of
// the audited code.

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
)

// CAP: a segment whose .ts is linked into an open incident, but whose check
// (blackdetect) finishes after the incident closed, is left in the incident
// as a .ts with no .json sidecar and no entry in report.json.
func TestAuditIncidentCloseLeavesOrphanSegment(t *testing.T) {
	_, srv := newLiveOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.Blackdetect.Enabled = true
	var segs atomic.Int32
	m, err := New(Options{
		Config: cfg, Logger: testLogger(t), RetryDelay: 10 * time.Millisecond, OriginRetryDelay: 20 * time.Millisecond,
		BlackDetector: func(ctx context.Context, path string) ([]blackdetect.Interval, error) {
			select {
			case <-time.After(700 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, nil
		},
		OnSegment: func(string, SegmentRecord) { segs.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	waitFor(t, "first segment", 10*time.Second, func() bool { return segs.Load() > 0 })
	ch := m.channels[0]
	for range 3 {
		if _, _, err := m.incidents.manual(ch); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "close", 10*time.Second, func() bool { return m.incidents.openID(ch.name) == "none" })
		time.Sleep(1500 * time.Millisecond)
	}
	cancel()
	<-done

	re := regexp.MustCompile(`^seg_(\d+)\.(ts|json)$`)
	orphans := 0
	for _, dir := range incidentDirs(t, cfg) {
		r := readReport(t, dir)
		inReport := map[string]bool{}
		for _, s := range r.Segments {
			inReport[s.File] = true
		}
		entries, _ := os.ReadDir(filepath.Join(dir, "segments"))
		have := map[string]bool{}
		for _, e := range entries {
			have[e.Name()] = true
		}
		for _, e := range entries {
			mm := re.FindStringSubmatch(e.Name())
			if mm == nil || mm[2] != "ts" {
				continue
			}
			if !have["seg_"+mm[1]+".json"] {
				orphans++
				t.Errorf("%s: segments/%s has no seg_%s.json; in report.json: %v (status %s, close %s)",
					filepath.Base(dir), e.Name(), mm[1], inReport[e.Name()], r.Status, r.CloseReason)
			}
		}
	}
	t.Logf("orphan .ts files: %d", orphans)
}

// CAP: a stall whose other rendition could not be fetched at all (its
// playlist answers 404) is reported as "reproduced" there once the incident
// closes.
func TestAuditStallVerdictReproducedWithoutEvidence(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(twoVariantMaster))
	o.sequence("/live/hi.m3u8", playlistBody(400, -1, seg(400), seg(401), seg(402)))
	o.route("/live/lo.m3u8", func(_ int, w http.ResponseWriter) { http.Error(w, "gone", http.StatusNotFound) })
	o.segments(400, 3)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.StallTargetDurations = 3
	cfg.MaxIncident = 6 * time.Second

	var closed func(*Monitor, map[uint64]SegmentRecord) bool = func(*Monitor, map[uint64]SegmentRecord) bool {
		for _, d := range incidentDirs(t, cfg) {
			var r Report
			if b, err := os.ReadFile(filepath.Join(d, "report.json")); err == nil && json.Unmarshal(b, &r) == nil && r.Status == "closed" {
				return true
			}
		}
		return false
	}
	run(t, cfg, closed)
	r := onlyIncident(t, cfg)
	f := r.Faults[0]
	var rs []RenditionSegment
	for _, rr := range r.Renditions {
		rs = append(rs, rr.Segments...)
	}
	t.Logf("fault %s seq %d renditions %v; rendition segments %+v", f.Type, f.Seq, f.Renditions, rs)
	if f.Type == analysis.FaultStall && f.Renditions["1_640x360_500000"] == "reproduced" {
		t.Errorf("stall 'reproduced' in 1_640x360_500000, whose playlist was never fetched (404 every time)")
	}
}

// CAP: at shutdown a stall incident is closed with its rendition checks
// still pending, and every pending check becomes "reproduced".
func TestAuditStallVerdictAtShutdown(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, ch := m.incidents, m.channels[0]
	// A rendition that was never polled (the fetchers had no chance).
	r := &rendition{segs: map[uint64]*RenditionSegment{}, data: map[uint64]renditionData{}}
	inc := &incident{rend: []*rendition{r}, lastByType: map[string]time.Time{}}
	_ = in
	_ = ch
	r.want([]uint64{402, 403})
	inc.closed = true
	got, _ := inc.reproduction(FaultRecord{Type: analysis.FaultStall, Seq: 402})
	t.Logf("verdicts at close with nothing fetched: %v", got)
	if got[r.variant.Label()] == "reproduced" {
		t.Errorf("stall verdict %q for a rendition never checked", got[r.variant.Label()])
	}
}

// CAP: enforceCap deletes any directory under data/incidents in name order,
// including ones that are not incidents.
func TestAuditStorageCapDeletesNonIncidentDirectories(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.IncidentStorageBytes = 1500
	m := newTestMonitor(t, cfg, nil)
	root := filepath.Join(cfg.DataDir, "incidents")
	for _, name := range []string{"2026-shared-copies", "20260102T030405Z_channel6"} {
		os.MkdirAll(filepath.Join(root, name), 0o755)
		os.WriteFile(filepath.Join(root, name, "seg_1.ts"), make([]byte, 1000), 0o644)
	}
	m.incidents.enforceCap()
	var left []string
	for _, d := range incidentDirs(t, cfg) {
		left = append(left, filepath.Base(d))
	}
	t.Logf("left: %v", left)
	if !slices.Contains(left, "2026-shared-copies") {
		t.Errorf("a non-incident directory was deleted by the storage cap")
	}
}

// Concurrency stress: captures, faults, ticks, cap, health, recording and
// buffer pruning all at once under -race.
func TestAuditConcurrencyStress(t *testing.T) {
	_, srv := newLiveOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.Blackdetect.Enabled = true
	cfg.IncidentStorageBytes = 1 // cap always exceeded: enforceCap deletes every closed incident
	var segs atomic.Int32
	m, err := New(Options{
		Config: cfg, Logger: testLogger(t), RetryDelay: 10 * time.Millisecond, OriginRetryDelay: 20 * time.Millisecond,
		BlackDetector: func(ctx context.Context, path string) ([]blackdetect.Interval, error) {
			return []blackdetect.Interval{{Start: 0, End: 0.5, Duration: 0.5}}, nil
		},
		OnSegment: func(string, SegmentRecord) { segs.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	waitFor(t, "first segment", 10*time.Second, func() bool { return segs.Load() > 0 })
	ch := m.channels[0]
	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(f func()) {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				f()
				time.Sleep(3 * time.Millisecond)
			}
		})
	}
	loop(func() { m.incidents.manual(ch) })
	loop(func() {
		m.incidents.fault(ch, []analysis.Fault{{Type: analysis.FaultPTSPCRJump, Seq: 5, Values: map[string]any{"x": 1}}}, SegmentRecord{Seq: 5, URI: "x"})
	})
	loop(func() { m.incidents.tick(time.Now().Add(time.Hour)) })
	loop(func() { m.incidents.enforceCap() })
	loop(func() { ch.logHealth() })
	loop(func() { pruneBuffer(ch.dir, time.Now(), time.Millisecond, time.Time{}) })
	time.Sleep(3 * time.Second)
	close(stop)
	wg.Wait()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(cfg.DataDir, "incidents", "incidents.csv"))
	t.Logf("segments=%d csv rows=%d", segs.Load(), strings.Count(string(b), "\n")-1)
}
