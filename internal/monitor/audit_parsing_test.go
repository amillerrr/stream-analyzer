package monitor

// Scratch audit tests (parsing/config/security reviewer).

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
)

// countSegments wraps OnSegment to count how often each seq was processed.
func countSegments(mu *sync.Mutex, counts map[uint64]int) func(*Options) {
	return func(o *Options) {
		prev := o.OnSegment
		o.OnSegment = func(ch string, rec SegmentRecord) {
			mu.Lock()
			counts[rec.Seq]++
			mu.Unlock()
			prev(ch, rec)
		}
	}
}

func allFaults(t *testing.T, dirs []string) []FaultRecord {
	t.Helper()
	var out []FaultRecord
	for _, d := range dirs {
		out = append(out, readReport(t, d).Faults...)
	}
	return out
}

func faultTypes(fs []FaultRecord) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Type)
	}
	return out
}

// A 200 playlist body that ends in the middle of the last URI.
func TestAuditTruncatedPlaylistBecomesUnavailableFault(t *testing.T) {
	o, srv := newScriptOrigin(t)
	full := string(playlistBody(100, -1, seg(100), seg(101), seg(102), seg(103)))
	truncated := []byte(strings.TrimSuffix(full, "3.ts\n")) // last line "s10"
	o.sequence("/live/test.m3u8",
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		truncated,
		playlistBody(101, -1, seg(101), seg(102), seg(103), seg(104)))
	o.segments(100, 5)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(104))

	r := recs[103]
	o.mu.Lock()
	realHits := o.hits["/live/s103.ts"]
	o.mu.Unlock()
	t.Logf("seq 103: uri=%q faults=%v error=%q; real s103.ts fetched %d times", r.URI, r.Faults, r.Error, realHits)
	if slices.Contains(r.Faults, analysis.FaultUnavailable) || realHits == 0 {
		t.Errorf("a truncated playlist produced fault %v on 103 (URI %q) and the real segment was fetched %d times; incidents: %v",
			r.Faults, r.URI, realHits, faultTypes(allFaults(t, incidentDirs(t, cfg))))
	}
}

// A 200 playlist body that ends inside the EXT-X-MEDIA-SEQUENCE value.
func TestAuditPlaylistCutInsideMSNIsABackwardFault(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8",
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		[]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:10"),
		playlistBody(101, -1, seg(101), seg(102), seg(103)))
	o.segments(100, 4)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	run(t, cfg, seen(103))

	if fs := allFaults(t, incidentDirs(t, cfg)); len(fs) > 0 {
		t.Errorf("a playlist cut inside EXT-X-MEDIA-SEQUENCE raised %v: %s", faultTypes(fs), fs[0].Message)
	}
}

// "#EXTM3U" followed by an error page.
func TestAuditGarbageAfterEXTM3U(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8",
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		[]byte("#EXTM3U\n<html>\n<body>busy</body>\n</html>\n"),
		playlistBody(101, -1, seg(101), seg(102), seg(103)))
	o.segments(100, 4)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	var mu sync.Mutex
	counts := map[uint64]int{}

	run(t, cfg, func(_ *Monitor, recs map[uint64]SegmentRecord) bool {
		_, ok := recs[103]
		return ok
	}, countSegments(&mu, counts))

	mu.Lock()
	defer mu.Unlock()
	fs := allFaults(t, incidentDirs(t, cfg))
	var msgs []string
	for _, f := range fs {
		msgs = append(msgs, f.Type+": "+f.Message)
	}
	t.Logf("processed per seq: %v", counts)
	if len(fs) > 0 || counts[101] > 1 || counts[102] > 1 {
		t.Errorf("an error page after #EXTM3U raised faults:\n  %s\nand segments were processed %v times", strings.Join(msgs, "\n  "), counts)
	}
}

// One playlist with a huge EXT-X-TARGETDURATION (for example milliseconds
// instead of seconds) stops polling; a stall after it is never seen.
func TestAuditHugeTargetDurationStopsPolling(t *testing.T) {
	o, srv := newScriptOrigin(t)
	frozen := playlistBody(101, -1, seg(101), seg(102), seg(103))
	huge := []byte(strings.Replace(string(frozen), "#EXT-X-TARGETDURATION:1\n", "#EXT-X-TARGETDURATION:7000\n", 1))
	o.sequence("/live/test.m3u8",
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		huge,
		frozen) // the stream then stalls
	o.segments(100, 4)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.StallTargetDurations = 3 // stall after 3 s at the normal 1 s target

	_, _, stop := startMonitor(t, cfg)
	waitFor(t, "3 playlist fetches", 10*time.Second, func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.hits["/live/test.m3u8"] >= 3
	})
	time.Sleep(6 * time.Second)
	o.mu.Lock()
	n := o.hits["/live/test.m3u8"]
	o.mu.Unlock()
	stop()
	fs := allFaults(t, incidentDirs(t, cfg))
	t.Logf("playlist fetches after 6 s: %d; faults: %v", n, faultTypes(fs))
	if n <= 3 || !slices.Contains(faultTypes(fs), analysis.FaultStall) {
		t.Errorf("after one playlist with TARGETDURATION 7000 the monitor fetched no playlist for 6 s (%d fetches total) and saw no stall (faults %v)", n, faultTypes(fs))
	}
}

// EXT-X-GAP says the segment is missing; players skip it.
func TestAuditGapTaggedSegmentIsUnavailable(t *testing.T) {
	o, srv := newScriptOrigin(t)
	body := "#EXTM3U\n#EXT-X-VERSION:8\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:100\n" +
		"#EXTINF:0.534,\ns100.ts\n#EXTINF:0.534,\ns101.ts\n#EXT-X-GAP\n#EXTINF:0.534,\nmissing.ts\n#EXTINF:0.534,\ns103.ts\n"
	o.sequence("/live/test.m3u8", []byte(body))
	o.segments(100, 2)
	o.sequence("/live/s103.ts", nil)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(103))

	t.Logf("102: tags=%q faults=%v", recs[102].Tags, recs[102].Faults)
	if slices.Contains(recs[102].Faults, analysis.FaultUnavailable) {
		t.Errorf("a segment the playlist marks EXT-X-GAP raised %v", recs[102].Faults)
	}
}

// EXT-X-ENDLIST ends the stream; it is reported as a stall.
func TestAuditEndlistIsReportedAsStall(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8", append(playlistBody(100, -1, seg(100), seg(101), seg(102)), "#EXT-X-ENDLIST\n"...))
	o.segments(100, 3)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.StallTargetDurations = 3

	_, _, stop := startMonitor(t, cfg)
	waitFor(t, "an incident", 10*time.Second, func() bool { return len(incidentDirs(t, cfg)) > 0 })
	time.Sleep(200 * time.Millisecond)
	stop()
	fs := allFaults(t, incidentDirs(t, cfg))
	for _, f := range fs {
		t.Logf("%s: %s", f.Type, f.Message)
	}
	if slices.Contains(faultTypes(fs), analysis.FaultStall) {
		t.Errorf("EXT-X-ENDLIST (stream ended) is reported as %v with no mention of ENDLIST", faultTypes(fs))
	}
}

// A user_agent with a control character passes config validation, and then
// every request fails before it is sent: no playlist, no fault, ever.
func TestAuditUserAgentControlCharacter(t *testing.T) {
	_, srv := newOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.UserAgent = "stream-analyzer/1.0\n"
	m, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Logf("rejected: %v", err)
		return // such a user_agent is refused before any fetch
	}
	_, meta, err := m.fetch(context.Background(), srv.URL+"/x", true, time.Second)
	t.Logf("status=%d err=%v", meta.Status, err)
	if err != nil && meta.Status == 0 {
		t.Errorf("every fetch fails as a network error (status 0, so never a stall or fault): %v", err)
	}
}

// POST /capture has no Origin or Host check: a page in the user's browser
// (CSRF, a CORS "simple request" needs no preflight) or a DNS-rebinding
// host name reaches it.
func TestAuditCaptureAcceptsCrossOriginAndForeignHost(t *testing.T) {
	_, srv := newOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	m, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://rebind.attacker.example:8765/capture?channel=test", nil)
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	t.Logf("%d %s", rec.Code, rec.Body.String())
	m.incidents.shutdown()
	if rec.Code == http.StatusOK {
		t.Errorf("cross-site POST with Host %q opened an incident: %s", req.Host, strings.TrimSpace(rec.Body.String()))
	}
}

// Repeated manual captures keep one incident open without limit: a manual
// capture is exempt from max_incident, and each one extends the post-roll.
func TestAuditRepeatedCapturesHoldAnIncidentOpenPastMaxIncident(t *testing.T) {
	_, srv := newOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.PostRoll, cfg.MergeWindow, cfg.MaxIncident = time.Minute, time.Minute, 10*time.Minute
	var mu sync.Mutex
	now := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m, err := New(Options{Config: cfg, Logger: testLogger(t), Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	c := m.byName["test"]
	var id string
	for i := range 60 { // one capture every 50 s for 50 minutes
		mu.Lock()
		now = now.Add(50 * time.Second)
		mu.Unlock()
		got, _, err := m.incidents.manual(c)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			id = got
		}
		m.incidents.tick(clock())
	}
	open := m.incidents.openID("test")
	r := readReport(t, filepath.Join(cfg.DataDir, "incidents", id))
	m.incidents.shutdown()
	t.Logf("incident %s opened %s, still open at %s (status %q, %d faults)", id, r.OpenedAt.Format(time.TimeOnly), clock().Format(time.TimeOnly), r.Status, r.FaultCount)
	if open == id {
		t.Errorf("one incident stayed open for %v, past max_incident %v", clock().Sub(r.OpenedAt), cfg.MaxIncident)
	}
}

// listen: "" (for example to "turn the trigger off") must not listen on
// every interface, as net.Listen("tcp", "") does: it turns the manual
// trigger off.
func TestAuditEmptyListenBindsAllInterfaces(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Listen = ""
	var mu sync.Mutex
	var logs bytes.Buffer
	started := func(*Monitor, map[uint64]SegmentRecord) bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(logs.String(), "stream-analyzer started")
	}
	run(t, cfg, started, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil)) })
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logs.String(), "capture=off") {
		t.Errorf("listen \"\" did not turn the trigger off:\n%s", logs.String())
	}
}

var _ = json.Marshal
var _ = os.ReadFile

// Only configured names reach the incident code.
func TestAuditCaptureRejectsOtherNames(t *testing.T) {
	_, srv := newOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	m, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"channel=../test", "channel=test%00", "channel=TEST", "channel=test&channel=x", "channel=x&channel=test", "", "channel=%2e%2e", "channel=test/"} {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut} {
			rec := httptest.NewRecorder()
			m.Handler().ServeHTTP(rec, httptest.NewRequest(method, "/capture?"+q, strings.NewReader(strings.Repeat("x", 1<<20))))
			t.Logf("%-4s %-28q -> %d %s", method, q, rec.Code, strings.Join(strings.Fields(rec.Body.String()), " "))
		}
	}
	m.incidents.shutdown()
	entries, _ := os.ReadDir(filepath.Join(cfg.DataDir, "incidents"))
	for _, e := range entries {
		t.Logf("incident dir: %s", e.Name())
	}
}

// Channel names differing only in case pass validation but share one buffer
// directory on the default (case-insensitive) macOS filesystem.
func TestAuditCaseOnlyChannelNamesShareABufferDir(t *testing.T) {
	_, srv := newOrigin(t)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Channels = append(cfg.Channels, cfg.Channels[0])
	cfg.Channels[0].Name, cfg.Channels[1].Name = "channel1", "CHANNEL1"
	m, err := New(Options{Config: cfg, Logger: testLogger(t)})
	if err != nil {
		t.Logf("rejected: %v", err)
		return // such names are refused, so no two channels share a directory
	}
	a, b := m.channels[0].dir, m.channels[1].dir
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(a, "seg_1.ts"), []byte("A's segment"), 0o644)
	got, err := os.ReadFile(filepath.Join(b, "seg_1.ts"))
	t.Logf("%s and %s: reading B's seg_1.ts gives %q, %v", a, b, got, err)
	if err == nil {
		t.Errorf("channels %q and %q share buffer directory %s", cfg.Channels[0].Name, cfg.Channels[1].Name, a)
	}
}

// The origin sends no EXT-X-DISCONTINUITY-SEQUENCE and drops the
// EXT-X-DISCONTINUITY tag once its segment is first in the window. Started
// while a tagged segment is in the window but not among the newest three,
// the monitor never sees the tagged fault, only the "unexplained" change.
func TestAuditOriginDroppingTagOpensStandaloneIncident(t *testing.T) {
	o, srv := newScriptOrigin(t)
	first := playlistBody(100, -1, seg(100), discSeg(101), seg(102), seg(103), seg(104), seg(105))
	o.sequence("/live/test.m3u8",
		first, first,
		playlistBody(101, -1, seg(101), seg(102), seg(103), seg(104), seg(105), seg(106))) // tag dropped at the head
	o.segments(100, 7)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	run(t, cfg, seen(106))

	fs := allFaults(t, incidentDirs(t, cfg))
	for _, f := range fs {
		t.Logf("%s (reason %v) seq %d: %s", f.Type, f.Values["reason"], f.Seq, f.Message)
	}
	if len(fs) > 0 {
		t.Errorf("the origin's normal tag drop opened an incident: %v", faultTypes(fs))
	}
}
