package monitor

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/report"
	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

const singleVariantMaster = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nhi.m3u8\n"

// scriptOrigin serves each path from a function of how many times that
// path has been requested (1-based); anything unscripted is a 404.
type scriptOrigin struct {
	mu     sync.Mutex
	hits   map[string]int
	routes map[string]func(n int, w http.ResponseWriter)
}

func newScriptOrigin(t *testing.T) (*scriptOrigin, *httptest.Server) {
	o := &scriptOrigin{hits: map[string]int{}, routes: map[string]func(int, http.ResponseWriter){}}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return o, srv
}

func (o *scriptOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.hits[r.URL.Path]++
	n, route := o.hits[r.URL.Path], o.routes[r.URL.Path]
	o.mu.Unlock()
	if route == nil {
		http.NotFound(w, r)
		return
	}
	route(n, w)
}

func (o *scriptOrigin) route(path string, f func(n int, w http.ResponseWriter)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.routes[path] = f
}

// sequence serves bodies[n-1], repeating the last one.
func (o *scriptOrigin) sequence(path string, bodies ...[]byte) {
	o.route(path, func(n int, w http.ResponseWriter) { w.Write(bodies[min(n, len(bodies))-1]) })
}

// segments serves /live/s<seq>.ts for seq in [first, first+count) as
// consecutive Base segments.
func (o *scriptOrigin) segments(first uint64, count int) {
	for k := range count {
		body := tstest.Base.Segment(k).Bytes()
		o.sequence(fmt.Sprintf("/live/s%d.ts", first+uint64(k)), body)
	}
}

// playlistBody is a live media playlist of 0.534 s segments s<seq>.ts;
// dsn < 0 omits EXT-X-DISCONTINUITY-SEQUENCE.
func playlistBody(msn uint64, dsn int, entries ...entry) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", msn)
	if dsn >= 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", dsn)
	}
	for _, e := range entries {
		if e.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:0.534,\n%s\n", e.uri)
	}
	return []byte(b.String())
}

func seg(seq uint64) entry     { return entry{uri: fmt.Sprintf("s%d.ts", seq)} }
func discSeg(seq uint64) entry { return entry{uri: fmt.Sprintf("s%d.ts", seq), disc: true} }

func onlyIncident(t *testing.T, cfg config.Config) Report {
	t.Helper()
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents = %v, want exactly 1", dirs)
	}
	return readReport(t, dirs[0])
}

// --- 1. discontinuities --------------------------------------------------

// The window slides one segment and EXT-X-DISCONTINUITY-SEQUENCE goes 5 -> 6,
// but no tagged segment left: segment 301's discontinuity number changed.
func TestUnexplainedDiscontinuitySequenceChangeOpensIncident(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8",
		playlistBody(300, 5, seg(300), seg(301), seg(302)),
		playlistBody(301, 6, seg(301), seg(302), seg(303)))
	o.segments(300, 4)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")

	run(t, cfg, seen(303))

	r := onlyIncident(t, cfg)
	f := r.Faults[0]
	if f.Type != analysis.FaultPlaylistViolation || f.Values["reason"] != "discontinuity_sequence" ||
		f.Seq != 301 || f.Values["was"] != float64(5) || f.Values["now"] != float64(6) {
		t.Errorf("fault = %+v", f)
	}
}

// Segment 300 carries EXT-X-DISCONTINUITY and leaves the window; the DSN
// goes 5 -> 6 exactly as that explains. 300 is never fetched (only the
// newest three of the first playlist are), so nothing may fire.
func TestExplainedDiscontinuitySequenceChangeIsNotAFault(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8",
		playlistBody(300, 5, discSeg(300), seg(301), seg(302), seg(303)),
		playlistBody(301, 6, seg(301), seg(302), seg(303), seg(304)))
	o.segments(300, 5)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")

	run(t, cfg, seen(304))

	if dirs := incidentDirs(t, cfg); len(dirs) != 0 {
		t.Errorf("incidents opened: %v", dirs)
	}
}

// --- 2. stalls -------------------------------------------------------------

// The playlist stops advancing: after 3 target durations (3 s) a stall
// incident opens and stays open while the stall lasts, then closes after
// the post-roll once a new segment appears.
func TestStallOpensIncidentHeldOpenUntilSegmentsResume(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	var resumed atomic.Bool
	o.route("/live/hi.m3u8", func(_ int, w http.ResponseWriter) {
		if resumed.Load() {
			w.Write(playlistBody(400, -1, seg(400), seg(401), seg(402), seg(403)))
			return
		}
		w.Write(playlistBody(400, -1, seg(400), seg(401), seg(402)))
	})
	o.segments(400, 4)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.StallTargetDurations = 3

	_, segments, stop := startMonitor(t, cfg)
	// The directory appears a moment before report.json is first written.
	waitFor(t, "a stall incident", 10*time.Second, func() bool {
		dirs := incidentDirs(t, cfg)
		if len(dirs) != 1 {
			return false
		}
		_, err := os.Stat(filepath.Join(dirs[0], "report.json"))
		return err == nil
	})
	dir := incidentDirs(t, cfg)[0]
	r := readReport(t, dir)
	f := r.Faults[0]
	if f.Type != analysis.FaultStall || f.Seq != 402 || f.Values["stalled_for_s"].(float64) < 3 {
		t.Fatalf("stall fault = %+v", f)
	}

	time.Sleep(2 * time.Second) // far past the 300 ms post-roll
	if readReport(t, dir).Status != "open" {
		t.Fatal("stall incident closed while the stall was still going on")
	}

	resumed.Store(true)
	waitFor(t, "segment 403", 10*time.Second, func() bool { return segments.Load() >= 4 })
	waitFor(t, "the incident to close after the stall", 10*time.Second, func() bool { return readReport(t, dir).Status == "closed" })
	stop()
	r = readReport(t, dir)
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "stall ended") }) {
		t.Errorf("notes = %v, want a stall-ended note", r.Notes)
	}
	if !slices.ContainsFunc(r.Segments, func(s SegmentRecord) bool { return s.Seq == 403 }) {
		t.Error("the segment that ended the stall is not in the report")
	}
}

// During a stall nothing new arrives, so the buffer's last 3 minutes of
// wall time hold no segments. The 3 minutes before the last segment must
// survive pruning; stall-time playlists older than the window go.
func TestBufferPruneKeepsTheWindowBeforeTheLastSegment(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	lastSegment := now.Add(-10 * time.Minute)
	ages := map[string]time.Duration{
		"seg_0.ts":             14 * time.Minute, // 4 min before the last segment
		"seg_1.ts":             12 * time.Minute, // 2 min before the last segment
		"seg_2.ts":             10 * time.Minute, // the last segment
		"playlist_mid.m3u8":    5 * time.Minute,  // stall time, older than the window
		"playlist_recent.m3u8": 10 * time.Second,
	}
	for name, age := range ages {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	if err := pruneBuffer(dir, now, 3*time.Minute, lastSegment); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"playlist_recent.m3u8", "seg_1.ts", "seg_2.ts"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

// --- 3. origin errors ---------------------------------------------------------

// 501 is listed in every playlist and answers 404 every time: the stream
// is missing it, which is a fault, and 502 is not a monitor gap.
func TestListedSegmentThatKeeps404ingIsUnavailable(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(500, seg(500), seg(501), seg(502)))
	o.set("/live/s500.ts", tstest.Load(t, "cont_0.ts"))
	o.set("/live/s502.ts", tstest.Load(t, "cont_2.ts"))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(502))

	r := recs[501]
	if !slices.Equal(r.Faults, []string{analysis.FaultUnavailable}) || r.Fetch.Status != http.StatusNotFound || r.Fetch.Attempts != 2 {
		t.Errorf("501: faults=%v status=%d attempts=%d", r.Faults, r.Fetch.Status, r.Fetch.Attempts)
	}
	if recs[502].Gap != nil {
		t.Errorf("502 reports a monitor gap %+v; 501 was accounted for", recs[502].Gap)
	}
	rep := onlyIncident(t, cfg)
	if rep.Faults[0].Type != analysis.FaultUnavailable || rep.Faults[0].Values["status"] != float64(404) {
		t.Errorf("incident fault = %+v", rep.Faults[0])
	}
}

// 501 answers 503 once and then works: counted for the health line, no fault.
func TestOriginErrorThatRecoversOnRetryIsCountedNotFaulted(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8", playlistBody(500, -1, seg(500), seg(501), seg(502)))
	o.segments(500, 3)
	good := tstest.Base.Segment(1).Bytes()
	o.route("/live/s501.ts", func(n int, w http.ResponseWriter) {
		if n == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write(good)
	})
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(502))

	r := recs[501]
	if r.Error != "" || len(r.Faults) != 0 || r.Fetch.Attempts != 2 || !slices.Equal(r.Fetch.FailedStatuses, []int{503}) {
		t.Errorf("501: error=%q faults=%v attempts=%d failed=%v", r.Error, r.Faults, r.Fetch.Attempts, r.Fetch.FailedStatuses)
	}
	// The health line written when the monitor stopped counts it.
	if got := column(t, readHealthCSV(t, cfg), 1, "origin_errors_recovered"); got != "1" {
		t.Errorf("origin_errors_recovered = %s, want 1", got)
	}
	if dirs := incidentDirs(t, cfg); len(dirs) != 0 {
		t.Errorf("incidents opened: %v", dirs)
	}
}

// 501 404s, but by then the playlist has moved past it: we were too slow,
// which stays a monitor gap.
func TestSegmentThatLeftTheWindowIsAMonitorGap(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	moved := make(chan struct{})
	var once sync.Once
	o.route("/live/hi.m3u8", func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Write(playlistBody(500, -1, seg(500), seg(501), seg(502)))
			return
		}
		w.Write(playlistBody(502, -1, seg(502), seg(503), seg(504)))
		once.Do(func() { close(moved) })
	})
	o.segments(500, 5)
	o.route("/live/s501.ts", func(_ int, w http.ResponseWriter) {
		select {
		case <-moved:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(100 * time.Millisecond) // let the poller take in the new window
		http.NotFound(w, nil)
	})
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")

	_, recs := run(t, cfg, seen(504))

	if r := recs[501]; r.Error == "" || len(r.Faults) != 0 {
		t.Errorf("501: error=%q faults=%v, want a fetch error and no fault", r.Error, r.Faults)
	}
	if g := recs[502].Gap; g == nil || g.From != 501 || g.To != 501 {
		t.Errorf("502 gap = %+v, want 501..501", g)
	}
	if dirs := incidentDirs(t, cfg); len(dirs) != 0 {
		t.Errorf("incidents opened: %v", dirs)
	}
}

func TestMediaSequenceGoingBackwardOpensIncident(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8",
		playlistBody(600, -1, seg(600), seg(601), seg(602)),
		playlistBody(599, -1, seg(599), seg(600), seg(601)),
		playlistBody(600, -1, seg(600), seg(601), seg(602)))
	o.segments(599, 4)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")

	run(t, cfg, func(*Monitor, map[uint64]SegmentRecord) bool { return len(incidentDirs(t, cfg)) == 1 })

	f := onlyIncident(t, cfg).Faults[0]
	if f.Type != analysis.FaultMediaSequenceBackward || f.Values["prev_msn"] != float64(600) || f.Values["msn"] != float64(599) {
		t.Errorf("fault = %+v", f)
	}
}

// --- 4. black trigger -------------------------------------------------------

// fakeBlack returns canned blackdetect results by segment file, given in
// seconds from the segment's first frame and reported, as ffmpeg -copyts
// does, in stream time.
func fakeBlack(runs map[uint64][]blackdetect.Interval) func(*Options) {
	return func(o *Options) {
		o.BlackDetector = func(_ context.Context, path string) ([]blackdetect.Interval, error) {
			var seq uint64
			fmt.Sscanf(filepath.Base(path), "seg_%d.ts", &seq)
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			seg, err := analysis.Analyze(b)
			if err != nil {
				return nil, err
			}
			first := float64(seg.Video.MinPTS()) / ts.Hz
			var out []blackdetect.Interval
			for _, iv := range runs[seq] {
				out = append(out, blackdetect.Interval{Start: first + iv.Start, End: first + iv.End, Duration: iv.Duration})
			}
			return out, nil
		}
	}
}

// A 0.3 s black run is below trigger_min (1 s): recorded on the segment and
// in any report, but it opens nothing. The discontinuity on 702 opens an
// incident whose report still lists that run.
func TestShortBlackRunsAreRecordedButOpenNoIncident(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(700, seg(700), seg(701), discSeg(702)))
	for k := range 3 {
		o.set(fmt.Sprintf("/live/s%d.ts", 700+k), tstest.Base.Segment(k).Bytes())
	}
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled = true
	short := blackdetect.Interval{Start: 0.1, End: 0.4, Duration: 0.3}

	_, recs := run(t, cfg, seen(702), fakeBlack(map[uint64][]blackdetect.Interval{700: {short}}))

	if r := recs[700]; len(r.Black) != 1 || r.Black[0].Start != 0.1 || r.Black[0].End != 0.4 || r.Black[0].Duration != 0.3 || len(r.Faults) != 0 {
		t.Errorf("700: black=%+v faults=%v", r.Black, r.Faults)
	}
	rep := onlyIncident(t, cfg)
	if !slices.Equal(rep.FaultTypes, []string{analysis.FaultDiscontinuity}) {
		t.Errorf("fault types = %v, want only discontinuity", rep.FaultTypes)
	}
	if b := rep.BlackRuns; len(b) != 1 || b[0].Seq != 700 || b[0].Start != 0.1 || b[0].End != 0.4 || b[0].Duration != 0.3 || b[0].StartPTS == 0 {
		t.Errorf("black_runs = %+v, want 700's run at 0.1-0.4 s with its PTS", b)
	}
}

// Black from 0.2 s to the end of 700 (0.334 s), all of 701 (0.534 s) and
// the first 0.3 s of 702 is one run of 1.168 s: it reaches trigger_min only
// on 702, and only when the parts are joined.
func TestBlackRunAcrossSegmentsReachingTriggerMinOpensIncident(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(700, seg(700), seg(701), seg(702)))
	for k := range 3 {
		o.set(fmt.Sprintf("/live/s%d.ts", 700+k), tstest.Base.Segment(k).Bytes())
	}
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled = true

	_, recs := run(t, cfg, seen(702), fakeBlack(map[uint64][]blackdetect.Interval{
		700: {{Start: 0.2, End: 0.534, Duration: 0.334}},
		701: {{Start: 0, End: 0.534, Duration: 0.534}},
		702: {{Start: 0, End: 0.3, Duration: 0.3}},
	}))

	if len(recs[700].Faults) != 0 || len(recs[701].Faults) != 0 {
		t.Errorf("faults before the run reached 1 s: 700=%v 701=%v", recs[700].Faults, recs[701].Faults)
	}
	if !slices.Equal(recs[702].Faults, []string{analysis.FaultBlackVideo}) {
		t.Fatalf("702 faults = %v, want [black_video]", recs[702].Faults)
	}
	f := onlyIncident(t, cfg).Faults[0]
	if got := f.Values["longest_run_s"].(float64); math.Abs(got-1.168) > 0.001 {
		t.Errorf("longest_run_s = %v, want 1.168", got)
	}
	if got := f.Values["joined_previous_s"].(float64); math.Abs(got-0.868) > 0.001 {
		t.Errorf("joined_previous_s = %v, want 0.868", got)
	}
}

// --- 5. timing margin ---------------------------------------------------------

func TestHealthReportsSmallestPCRMarginsAndWritesCSV(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	var logs bytes.Buffer
	m, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ch := m.channels[0]
	margins := func(seq uint64, pts, dts float64) SegmentRecord {
		return SegmentRecord{Seq: seq, Analysis: &analysis.Summary{
			Video: &analysis.VideoSummary{PTSPCRMinMs: new(pts), DTSPCRMinMs: new(dts)},
		}}
	}
	ch.remember(margins(1, 50, 30))
	ch.remember(margins(2, 42, 35))
	ch.count(func(s *stats) { s.originRecovered++ })
	ch.logHealth()
	ch.remember(margins(3, 60, 55))
	ch.logHealth()

	for _, want := range []string{"min_pts_pcr_ms=42", "min_dts_pcr_ms=30", "origin_errors_recovered=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("health log lacks %s:\n%s", want, logs.String())
		}
	}
	f, err := os.Open(filepath.Join(cfg.DataDir, "health.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 3 {
		t.Fatalf("health.csv rows = %v, %v", rows, err)
	}
	for i, want := range []struct{ pts, dts, recovered string }{{"42", "30", "1"}, {"60", "55", "0"}} {
		if column(t, rows, i+1, "channel") != "test" || column(t, rows, i+1, "min_pts_pcr_ms") != want.pts ||
			column(t, rows, i+1, "min_dts_pcr_ms") != want.dts || column(t, rows, i+1, "origin_errors_recovered") != want.recovered {
			t.Errorf("row %d = %v", i+1, rows[i+1])
		}
	}
}

// --- round 3 ------------------------------------------------------------------

// Any HTTP refusal of a listed segment, not just 404 or 5xx, is the origin
// failing to serve it: unavailable after the retry, and not a monitor gap.
func TestListedSegmentAnsweringAny4xxIsUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			o, srv := newScriptOrigin(t)
			o.sequence("/live/test.m3u8", playlistBody(500, -1, seg(500), seg(501), seg(502)))
			o.segments(500, 3)
			o.route("/live/s501.ts", func(_ int, w http.ResponseWriter) { http.Error(w, "refused", status) })
			cfg := testConfig(t, srv.URL+"/live/test.m3u8")

			_, recs := run(t, cfg, seen(502))

			r := recs[501]
			if !slices.Equal(r.Faults, []string{analysis.FaultUnavailable}) || r.Fetch.Status != status ||
				r.Fetch.Attempts != 2 || !slices.Equal(r.Fetch.FailedStatuses, []int{status, status}) {
				t.Errorf("501: faults=%v status=%d attempts=%d failed=%v", r.Faults, r.Fetch.Status, r.Fetch.Attempts, r.Fetch.FailedStatuses)
			}
			if recs[502].Gap != nil {
				t.Errorf("502 reports a monitor gap %+v", recs[502].Gap)
			}
		})
	}
}

// A 429 that clears on the retry is counted for the health line, not faulted.
func TestTransient4xxCuredByRetryIsCounted(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8", playlistBody(500, -1, seg(500), seg(501), seg(502)))
	o.segments(500, 3)
	good := tstest.Base.Segment(1).Bytes()
	o.route("/live/s501.ts", func(n int, w http.ResponseWriter) {
		if n == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write(good)
	})
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(502))

	if r := recs[501]; r.Error != "" || len(r.Faults) != 0 || !slices.Equal(r.Fetch.FailedStatuses, []int{429}) {
		t.Errorf("501: error=%q faults=%v failed=%v", r.Error, r.Faults, r.Fetch.FailedStatuses)
	}
	if got := column(t, readHealthCSV(t, cfg), 1, "origin_errors_recovered"); got != "1" {
		t.Errorf("origin_errors_recovered = %s, want 1", got)
	}
}

func readHealthCSV(t *testing.T, cfg config.Config) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(cfg.DataDir, "health.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Short black runs (d or longer, under trigger_min) are counted once each,
// when they end: 700's run inside the segment; 701's run to its end, which
// ends at the boundary because 702 starts clean. The run from 703's tail
// through 704 into 705 reaches 1.068 s: long, not counted as short.
func TestShortBlackRunsAreCountedPerMinute(t *testing.T) {
	o, srv := newOrigin(t)
	var entries []entry
	for k := range 6 {
		entries = append(entries, seg(700+uint64(k)))
		o.set(fmt.Sprintf("/live/s%d.ts", 700+k), tstest.Base.Segment(k).Bytes())
	}
	o.set("/live/test.m3u8", media(700, entries...))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled = true

	allSix := func(o *Options) { o.InitialSegments = 6 } // the first playlist lists all six
	_, recs := run(t, cfg, seen(705), allSix, fakeBlack(map[uint64][]blackdetect.Interval{
		700: {{Start: 0.1, End: 0.4, Duration: 0.3}},
		701: {{Start: 0.3, End: 0.534, Duration: 0.234}},
		703: {{Start: 0.1, End: 0.534, Duration: 0.434}},
		704: {{Start: 0, End: 0.534, Duration: 0.534}},
		705: {{Start: 0, End: 0.1, Duration: 0.1}},
	}))

	for seq := uint64(700); seq <= 704; seq++ {
		if recs[seq].Seq != seq || len(recs[seq].Faults) != 0 {
			t.Errorf("%d: processed=%v faults=%v", seq, recs[seq].Seq == seq, recs[seq].Faults)
		}
	}
	if !slices.Equal(recs[705].Faults, []string{analysis.FaultBlackVideo}) {
		t.Errorf("705 faults = %v, want [black_video]", recs[705].Faults)
	}
	rows := readHealthCSV(t, cfg) // the line written when the monitor stopped
	if got := column(t, rows, 1, "short_black_runs"); got != "2" {
		t.Errorf("short_black_runs = %s, want 2", got)
	}
}

// A health.csv written with other columns (an older version) is moved
// aside, not appended to, so every file has one header that fits its rows.
func TestHealthCSVWithOtherColumnsIsRotated(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	old := "time_utc,channel,min_pts_pcr_ms\n2026-09-28T17:45:20Z,test,42.244\n"
	os.MkdirAll(cfg.DataDir, 0o755)
	os.WriteFile(filepath.Join(cfg.DataDir, "health.csv"), []byte(old), 0o644)
	m := newTestMonitor(t, cfg, nil)
	m.channels[0].logHealth()

	rows := readHealthCSV(t, cfg)
	if len(rows) != 2 || !slices.Equal(rows[0], healthColumns) {
		t.Errorf("health.csv = %v, want the current header and one row", rows)
	}
	moved, _ := filepath.Glob(filepath.Join(cfg.DataDir, "health.*.csv"))
	if len(moved) != 1 {
		t.Fatalf("rotated files = %v, want 1", moved)
	}
	if b, _ := os.ReadFile(moved[0]); string(b) != old {
		t.Errorf("rotated file changed: %q", b)
	}
}

// --- round 4: irregular segments ---------------------------------------------

func readEventsCSV(t *testing.T, cfg config.Config) [][]string {
	t.Helper()
	return readDataCSV(t, cfg, "events.csv")
}

// readDataCSV reads a CSV file in the data directory.
func readDataCSV(t *testing.T, cfg config.Config, name string) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(cfg.DataDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Irregular segments go to data/events.csv, one row per event, and are
// counted in health.csv; none of them opens an incident. 801 carries a
// SCTE-35 cue, so the frame gap in 802 is flagged as next to one.
func TestIrregularSegmentsAreLoggedToEventsCSV(t *testing.T) {
	o, srv := newOrigin(t)
	segs := map[uint64]tstest.Segment{}
	for k := range 6 {
		segs[800+uint64(k)] = tstest.Base.Segment(k)
	}
	gap := segs[802]
	gap.Video = append(gap.Video[:9:9], gap.Video[10:]...) // a skipped frame slot
	segs[802] = gap
	retimed := segs[803]
	for i := 5; i < len(retimed.Audio); i++ {
		retimed.Audio[i].PTS -= 80
	}
	segs[803] = retimed
	for _, seq := range []uint64{804, 805} { // audio 1000 ticks late from 804 on
		s := segs[seq]
		for i := range s.Audio {
			s.Audio[i].PTS += 1000
		}
		segs[seq] = s
	}
	var entries []entry
	for seq := uint64(800); seq <= 805; seq++ {
		e := seg(seq)
		if seq == 801 {
			e.tags = []string{"#EXT-X-CUE-OUT:30"}
		}
		entries = append(entries, e)
		o.set(fmt.Sprintf("/live/s%d.ts", seq), segs[seq].Bytes())
	}
	o.set("/live/test.m3u8", media(800, entries...))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(805), func(o *Options) { o.InitialSegments = 6 })

	if dirs := incidentDirs(t, cfg); len(dirs) != 0 {
		t.Errorf("incidents opened: %v", dirs)
	}
	if !slices.Equal(eventNames(recs[802].Events), []string{analysis.EventFrameGap}) {
		t.Errorf("802 events = %v", eventNames(recs[802].Events))
	}
	rows := readEventsCSV(t, cfg)
	if len(rows) != 4 {
		t.Fatalf("events.csv = %v, want a header and 3 rows", rows)
	}
	for i, want := range []struct{ seq, typ, video, audio, gap, scte string }{
		{"802", "frame_gap", "15", "26", "33.367", "true"},
		{"803", "audio_retimed", "16", "24", "-0.889", "false"},
		{"804", "audio_gap", "16", "26", "12", "false"},
	} {
		got := struct{ seq, typ, video, audio, gap, scte string }{
			column(t, rows, i+1, "seq"), column(t, rows, i+1, "type"), column(t, rows, i+1, "video_frames"),
			column(t, rows, i+1, "audio_frames"), column(t, rows, i+1, "gap_ms"), column(t, rows, i+1, "scte35"),
		}
		if got != want || column(t, rows, i+1, "channel") != "test" || column(t, rows, i+1, "time_utc") == "" {
			t.Errorf("row %d = %v, want %+v", i+1, rows[i+1], want)
		}
	}
	health := readHealthCSV(t, cfg) // the line written when the monitor stopped
	if column(t, health, 1, "frame_gaps") != "1" || column(t, health, 1, "audio_retimed") != "1" {
		t.Errorf("health frame_gaps=%s audio_retimed=%s, want 1 and 1",
			column(t, health, 1, "frame_gaps"), column(t, health, 1, "audio_retimed"))
	}
}

func eventNames(es []analysis.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

// --- round 5: SCTE-35 tag types ----------------------------------------------

// Each events.csv row names the SCTE-35 tag on its own segment, and every
// tagged segment gets a row in data/scte35.csv: 901 starts a break (OUT),
// 902 is inside it (CONT) and 903 is the first segment after it (IN). 904
// has no tag, but the one before it has, so its scte35 flag is still true.
func TestSCTE35TagTypesAreLogged(t *testing.T) {
	o, srv := newOrigin(t)
	cues := map[uint64][]string{
		901: {"#EXT-X-CUE-OUT:120.000"},
		902: {"#EXT-X-CUE-OUT-CONT:ElapsedTime=0.534,Duration=120.000"},
		903: {"#EXT-X-CUE-IN"},
	}
	var entries []entry
	for k := range 5 {
		seq := 900 + uint64(k)
		s := tstest.Base.Segment(k)
		if seq > 900 {
			s.Video = append(s.Video[:9:9], s.Video[10:]...) // a skipped frame slot
		}
		e := seg(seq)
		e.tags = cues[seq]
		entries = append(entries, e)
		o.set(fmt.Sprintf("/live/s%d.ts", seq), s.Bytes())
	}
	o.set("/live/test.m3u8", media(900, entries...))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	run(t, cfg, seen(904), func(o *Options) { o.InitialSegments = 5 })

	events := readEventsCSV(t, cfg)
	var got []string
	for i := 1; i < len(events); i++ {
		got = append(got, strings.Join([]string{column(t, events, i, "seq"), column(t, events, i, "type"),
			column(t, events, i, "scte35"), column(t, events, i, "scte35_tag")}, " "))
	}
	if want := []string{"901 frame_gap true OUT", "902 frame_gap true CONT", "903 frame_gap true IN", "904 frame_gap true "}; !slices.Equal(got, want) {
		t.Errorf("events.csv rows = %q, want %q", got, want)
	}

	rows := readDataCSV(t, cfg, "scte35.csv")
	if want := []string{"time_utc", "channel", "seq", "scte35_tag", "extinf", "tags"}; !slices.Equal(rows[0], want) {
		t.Fatalf("scte35.csv header = %v, want %v", rows[0], want)
	}
	got = nil
	for _, r := range rows[1:] {
		if r[0] == "" || r[1] != "test" {
			t.Errorf("row %v: want a time and channel test", r)
		}
		got = append(got, strings.Join(r[2:], " "))
	}
	want := []string{
		"901 OUT 0.534 #EXT-X-CUE-OUT:120.000",
		"902 CONT 0.534 #EXT-X-CUE-OUT-CONT:ElapsedTime=0.534,Duration=120.000",
		"903 IN 0.534 #EXT-X-CUE-IN",
	}
	if !slices.Equal(got, want) {
		t.Errorf("scte35.csv rows = %q, want %q", got, want)
	}
}

// The report reads what the monitor writes. A break runs from 1002
// (CUE-OUT) to 1006 (CUE-IN); 1002 skips a frame slot, and so does 1008 in
// programming, which by then is also a frame short of the usual 16.
func TestReportReadsWhatTheMonitorWrites(t *testing.T) {
	o, srv := newOrigin(t)
	cues := map[uint64][]string{
		1002: {"#EXT-X-CUE-OUT:2.136"},
		1003: {"#EXT-X-CUE-OUT-CONT:ElapsedTime=0.534,Duration=2.136"},
		1004: {"#EXT-X-CUE-OUT-CONT:ElapsedTime=1.068,Duration=2.136"},
		1005: {"#EXT-X-CUE-OUT-CONT:ElapsedTime=1.602,Duration=2.136"},
		1006: {"#EXT-X-CUE-IN"},
	}
	var entries []entry
	for k := range 10 {
		seq := 1000 + uint64(k)
		s := tstest.Base.Segment(k)
		if seq == 1002 || seq == 1008 {
			s.Video = append(s.Video[:9:9], s.Video[10:]...) // a skipped frame slot
		}
		e := seg(seq)
		e.tags = cues[seq]
		entries = append(entries, e)
		o.set(fmt.Sprintf("/live/s%d.ts", seq), s.Bytes())
	}
	o.set("/live/test.m3u8", media(1000, entries...))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	from := time.Now().Add(-time.Minute)

	run(t, cfg, seen(1009), func(o *Options) { o.InitialSegments = 10 })

	var out bytes.Buffer
	if err := report.Run(&out, report.Options{DataDir: cfg.DataDir, From: from, To: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for l := range strings.Lines(out.String()) {
		lines = append(lines, strings.Join(strings.Fields(l), " "))
	}
	for _, want := range []string{
		"monitored 10 segments, 0 monitor gaps",
		"splice points 1 CUE-OUT, 1 CUE-IN; 1 of 2 with an irregular segment",
		"breaks 1",
		"break length 2 s declared; 2 s from CUE-OUT to CUE-IN", // 2.136 s, 4 x 0.534 s
		"faults none",
		"incidents none",
		// 1001-1003 and 1005-1007 at a splice, 1004 in the break.
		"all monitored 10 6 60% 1 10% 3 30%",
		"frame_gap 2 1 50% 0 0% 1 50%",
		"odd_length 1 0 0% 0 0% 1 100%",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
	for _, want := range []string{"CUE-OUT seq 1002 0: frame_gap", "CUE-IN seq 1006 none"} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, want) }) {
			t.Errorf("report lacks a splice point ending %q:\n%s", want, out.String())
		}
	}
}

// When the monitor stops it writes a last health line for each channel, so
// the part of a minute it stopped in isn't lost: here the whole run, since
// the tests' health interval is an hour.
func TestFinalHealthLineWhenTheMonitorStops(t *testing.T) {
	o, srv := newOrigin(t)
	var entries []entry
	for k := range 3 {
		seq := 700 + uint64(k)
		entries = append(entries, seg(seq))
		o.set(fmt.Sprintf("/live/s%d.ts", seq), tstest.Base.Segment(k).Bytes())
	}
	o.set("/live/test.m3u8", media(700, entries...))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	logs := &lockedBuffer{}

	run(t, cfg, seen(702), func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(logs, nil)) })

	rows := readHealthCSV(t, cfg)
	if len(rows) != 2 || column(t, rows, 1, "segments") != "3" || column(t, rows, 1, "seq") != "702" {
		t.Errorf("health.csv = %v, want one final row counting segments 700-702", rows)
	}
	var health []string
	for l := range strings.Lines(logs.String()) {
		if strings.Contains(l, "msg=health") {
			health = append(health, l)
		}
	}
	if len(health) != 1 || !strings.Contains(health[0], "level=INFO") || !strings.Contains(health[0], "final=true") {
		t.Errorf("health log lines = %q, want one INFO line with final=true", health)
	}
}

// lockedBuffer is a log destination the monitor's goroutines can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
