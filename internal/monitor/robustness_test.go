package monitor

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

const twoVariantMaster = "#EXTM3U\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nhi.m3u8\n" +
	"#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360\nlo.m3u8\n"

// liveOrigin serves a live two-variant channel whose playlists advance one
// continuous Base segment per fetch, so polling sees a steady stream.
type liveOrigin struct {
	mu       sync.Mutex
	msn      uint64
	master   func(n int) (int, []byte) // status and body for the n-th master fetch (1-based)
	masterN  int
	outage   atomic.Bool // media playlists answer 503
	segments map[string][]byte
}

func newLiveOrigin(t *testing.T) (*liveOrigin, *httptest.Server) {
	o := &liveOrigin{msn: 1000, segments: map[string][]byte{}}
	o.master = func(int) (int, []byte) { return http.StatusOK, []byte(twoVariantMaster) }
	for k := range 200 {
		o.segments[fmt.Sprintf("/live/s%d.ts", 1000+k)] = tstest.Base.Segment(k).Bytes()
	}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return o, srv
}

func (o *liveOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch r.URL.Path {
	case "/live/master.m3u8":
		o.masterN++
		code, body := o.master(o.masterN)
		w.WriteHeader(code)
		w.Write(body)
	case "/live/hi.m3u8", "/live/lo.m3u8":
		if o.outage.Load() {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		o.msn++
		w.Write(media(o.msn-2, entry{uri: fmt.Sprintf("s%d.ts", o.msn-2)},
			entry{uri: fmt.Sprintf("s%d.ts", o.msn-1)}, entry{uri: fmt.Sprintf("s%d.ts", o.msn)}))
	default:
		if b, ok := o.segments[r.URL.Path]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}
}

// startMonitor runs a monitor in the background; stop() cancels it and
// waits for Run to return.
func startMonitor(t *testing.T, cfg config.Config) (m *Monitor, segments *atomic.Int32, stop func()) {
	t.Helper()
	segments = &atomic.Int32{}
	m, err := New(Options{
		Config: cfg, Logger: testLogger(t), RetryDelay: 10 * time.Millisecond, OriginRetryDelay: 20 * time.Millisecond,
		OnSegment: func(_ string, rec SegmentRecord) {
			if rec.Error == "" {
				segments.Add(1)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return m, segments, stop
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A CDN hiccup that answers the channel URL with a 200 HTML page must not
// make the channel treat its master playlist as a media playlist forever.
func TestBadChannelURLBodyAtStartupDoesNotStickTheChannel(t *testing.T) {
	o, srv := newLiveOrigin(t)
	o.master = func(n int) (int, []byte) {
		if n == 1 {
			return http.StatusOK, []byte("<html>temporarily unavailable</html>")
		}
		return http.StatusOK, []byte(twoVariantMaster)
	}
	_, segments, _ := startMonitor(t, testConfig(t, srv.URL+"/live/master.m3u8"))
	waitFor(t, "segments after the bad first response", 10*time.Second, func() bool { return segments.Load() > 0 })
}

// Mid-run: the media playlist fails long enough to trigger a re-resolve,
// the master answers that re-resolve with an empty 200, then the origin
// recovers. Monitoring must resume.
func TestChannelRecoversWhenReResolveGetsBadMaster(t *testing.T) {
	o, srv := newLiveOrigin(t)
	var emptyServed atomic.Bool
	o.master = func(int) (int, []byte) {
		if o.outage.Load() && !emptyServed.Load() {
			emptyServed.Store(true)
			return http.StatusOK, nil
		}
		return http.StatusOK, []byte(twoVariantMaster)
	}
	_, segments, _ := startMonitor(t, testConfig(t, srv.URL+"/live/master.m3u8"))
	waitFor(t, "initial segments", 10*time.Second, func() bool { return segments.Load() >= 2 })

	o.outage.Store(true)
	waitFor(t, "a re-resolve during the outage", 15*time.Second, emptyServed.Load)
	o.outage.Store(false)
	before := segments.Load()
	waitFor(t, "segments after recovery", 10*time.Second, func() bool { return segments.Load() > before+1 })
}

// Relative URIs resolve against where a redirect actually led, as a player
// would, and the fetch records the final URL.
func TestRedirectedPlaylistsResolveAgainstFinalURL(t *testing.T) {
	o, srv := newOrigin(t)
	mux := http.NewServeMux()
	mux.Handle("/", o)
	mux.Handle("/live/master.m3u8", http.RedirectHandler("/moved/master.m3u8", http.StatusFound))
	redirecting := httptest.NewServer(mux)
	t.Cleanup(redirecting.Close)
	_ = srv

	o.set("/moved/master.m3u8", []byte(twoVariantMaster))
	o.set("/moved/hi.m3u8", media(200, entry{uri: "s200.ts"}))
	o.set("/moved/s200.ts", tstest.Load(t, "cont_0.ts"))
	cfg := testConfig(t, redirecting.URL+"/live/master.m3u8")

	_, recs := run(t, cfg, seen(200))
	r := recs[200]
	if r.Error != "" || r.Fetch.URL != redirecting.URL+"/moved/s200.ts" {
		t.Errorf("segment fetch = %+v", r.Fetch)
	}
	metas, _ := filepath.Glob(filepath.Join(cfg.DataDir, "buffer", "test", "master_*.json"))
	if len(metas) == 0 {
		t.Fatal("no master fetch saved")
	}
	b, _ := os.ReadFile(metas[0])
	var meta FetchMeta
	json.Unmarshal(b, &meta)
	if meta.FinalURL != redirecting.URL+"/moved/master.m3u8" {
		t.Errorf("master final_url = %q", meta.FinalURL)
	}
}

// POST /capture racing with shutdown must never leave an incident open,
// and every capture answered 200 must belong to a closed incident.
func TestCaptureDuringShutdownLeavesNoOpenIncident(t *testing.T) {
	_, srv := newLiveOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = ln.Addr().String()
	ln.Close()
	// Old incidents make the storage-cap walk at shutdown slower, widening
	// the window a late capture could slip into.
	for i := range 150 {
		dir := filepath.Join(cfg.DataDir, "incidents", fmt.Sprintf("20200101T%06dZ_old", i), "segments")
		os.MkdirAll(dir, 0o755)
		for j := range 40 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", j)), []byte("x"), 0o644)
		}
	}
	_, segments, stop := startMonitor(t, cfg)
	waitFor(t, "the channel to start", 10*time.Second, func() bool { return segments.Load() > 0 })

	var mu sync.Mutex
	answered := map[string]bool{}
	quit := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			cl := &http.Client{Timeout: time.Second}
			for {
				select {
				case <-quit:
					return
				default:
				}
				resp, err := cl.Post("http://"+cfg.Listen+"/capture?channel=test", "", nil)
				if err != nil {
					continue
				}
				var body map[string]string
				if resp.StatusCode == http.StatusOK {
					b := make([]byte, 4096)
					n, _ := resp.Body.Read(b)
					json.Unmarshal(b[:n], &body)
					mu.Lock()
					answered[body["incident"]] = true
					mu.Unlock()
				}
				resp.Body.Close()
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	stop()
	close(quit)
	wg.Wait()

	for _, dir := range incidentDirs(t, cfg) {
		b, err := os.ReadFile(filepath.Join(dir, "report.json"))
		if err != nil {
			continue
		}
		var r Report
		json.Unmarshal(b, &r)
		if r.Status == "open" {
			t.Errorf("%s left open after Run returned", r.ID)
		}
		delete(answered, r.ID)
	}
	delete(answered, "")
	if len(answered) > 0 {
		t.Errorf("captures answered 200 for incidents with no report: %v", answered)
	}
}

// A manual capture merged late into an incident still gets its full
// post-roll, even past max_incident, and does not count as hitting the cap.
func TestManualCaptureNearTheCapGetsFullPostRoll(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.PostRoll, cfg.MergeWindow, cfg.MaxIncident = time.Minute, time.Minute, 5*time.Minute
	start := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	clock := start
	m := newTestMonitor(t, cfg, func() time.Time { return clock })
	in, ch := m.incidents, m.channels[0]

	in.fault(ch, []analysis.Fault{{Type: analysis.FaultPTSBehindPCR, Seq: 1}}, SegmentRecord{Seq: 1})
	clock = start.Add(4*time.Minute + 50*time.Second)
	if _, created, err := in.manual(ch); err != nil || created {
		t.Fatalf("manual capture: created=%v err=%v", created, err)
	}
	clock = start.Add(5*time.Minute + 30*time.Second)
	in.tick(clock)
	if in.openID(ch.name) == "none" {
		t.Fatal("closed at max_incident, 40 s into the capture's 60 s post-roll")
	}
	clock = start.Add(5*time.Minute + 51*time.Second)
	in.tick(clock)
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents = %v", dirs)
	}
	if r := readReport(t, dirs[0]); r.Status != "closed" || r.CloseReason != "post_roll_elapsed" {
		t.Errorf("closed as %s/%s, want closed/post_roll_elapsed", r.Status, r.CloseReason)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.suppress[ch.name]) != 0 {
		t.Errorf("fault types suppressed without hitting the cap: %v", in.suppress[ch.name])
	}
}

// Gaps inside the buffered window before the incident opened belong in the
// report's monitor_gaps too.
func TestReportListsMonitorGapsFromTheBufferedWindow(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, ch := m.incidents, m.channels[0]
	now := time.Now()
	os.MkdirAll(ch.dir, 0o755)
	ch.persistJSON(kindSegments, "seg_102.json", SegmentRecord{Seq: 102, URI: "s102.ts", Gap: &analysis.Gap{From: 101, To: 101}, Fetch: FetchMeta{RequestedAt: now}})
	rec := SegmentRecord{Seq: 103, URI: "s103.ts", Fetch: FetchMeta{RequestedAt: now}}
	ch.persistJSON(kindSegments, "seg_103.json", rec)
	in.fault(ch, []analysis.Fault{{Type: analysis.FaultVideoDTSGap, Seq: 103, PrevSeq: new(uint64(102))}}, rec)
	in.shutdown()

	r := readReport(t, incidentDirs(t, cfg)[0])
	if !slices.Equal(r.MonitorGaps, []analysis.Gap{{From: 101, To: 101}}) {
		t.Errorf("monitor_gaps = %+v", r.MonitorGaps)
	}
}

// Other-rendition verdicts must not claim "not_reproduced" when the check
// could not run there.
func TestRenditionVerdictsAreInconclusiveWhenChecksCouldNotRun(t *testing.T) {
	good := func(k int) *analysis.Segment {
		seg, err := analysis.Analyze(tstest.Base.Segment(k).Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return seg
	}
	r := &rendition{segs: map[uint64]*RenditionSegment{}, data: map[uint64]renditionData{}}
	r.want([]uint64{10, 11, 20, 21})
	for seq, d := range map[uint64]renditionData{
		10: {seg: good(0)},
		11: {seg: nil}, // bytes were not usable TS
		20: {seg: good(0)},
		21: {seg: good(1), blackErr: fmt.Errorf("ffmpeg exited 1")},
	} {
		r.segs[seq].Status = statusFetched
		r.data[seq] = d
	}
	r.recheck(analysis.DefaultThresholds(), nil)
	inc := &incident{rend: []*rendition{r}}

	for _, tc := range []struct {
		f    FaultRecord
		want string
	}{
		{FaultRecord{Type: analysis.FaultVideoDTSGap, Seq: 11, PrevSeq: new(uint64(10))}, "inconclusive"},
		{FaultRecord{Type: analysis.FaultInvalidSegment, Seq: 11}, "reproduced"},
		{FaultRecord{Type: analysis.FaultBlackVideo, Seq: 21}, "inconclusive"},
		{FaultRecord{Type: analysis.FaultVideoDTSGap, Seq: 21, PrevSeq: new(uint64(20))}, "not_reproduced"},
	} {
		if got, _ := inc.reproduction(tc.f); got[r.variant.Label()] != tc.want {
			t.Errorf("%s at %d: %q, want %q", tc.f.Type, tc.f.Seq, got[r.variant.Label()], tc.want)
		}
	}
}

// A segment that kept failing and then left the playlist keeps its last
// error, not just "expired".
func TestExpiredRenditionSegmentKeepsLastError(t *testing.T) {
	s := &RenditionSegment{Seq: 5, Status: statusFailed, Error: "HTTP 404 Not Found"}
	expire(s)
	if s.Status != statusExpired || s.Error != "left the playlist before it could be fetched; last error: HTTP 404 Not Found" {
		t.Errorf("expired segment = %+v", s)
	}
}
