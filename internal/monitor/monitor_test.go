package monitor

import (
	"bytes"
	"context"
	"encoding/csv"
	json "encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// origin is a fake HLS origin: path -> body, anything else is a 404.
// Paths marked with drop have their connection closed without an answer.
type origin struct {
	mu    sync.Mutex
	files map[string][]byte
	drops map[string]bool
}

func newOrigin(t *testing.T) (*origin, *httptest.Server) {
	o := &origin{files: map[string][]byte{}, drops: map[string]bool{}}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return o, srv
}

func (o *origin) set(path string, body []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[path] = body
}

func (o *origin) drop(path string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drops[path] = true
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	body, ok := o.files[r.URL.Path]
	drop := o.drops[r.URL.Path]
	o.mu.Unlock()
	if drop {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(r.URL.Path, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		w.Header().Set("Content-Type", "video/MP2T")
	}
	w.Write(body)
}

type entry struct {
	uri  string
	disc bool
	tags []string // extra tag lines before the segment, such as SCTE-35 cues
}

// media builds a live media playlist of 0.534 s segments (the fixtures'
// real duration) starting at msn.
func media(msn uint64, entries ...entry) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", msn)
	for _, e := range entries {
		if e.disc {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		for _, tag := range e.tags {
			b.WriteString(tag + "\n")
		}
		fmt.Fprintf(&b, "#EXTINF:0.534,\n%s\n", e.uri)
	}
	return []byte(b.String())
}

func testConfig(t *testing.T, url string) config.Config {
	c := config.Default()
	c.DataDir = t.TempDir()
	c.Listen = "127.0.0.1:0"
	c.LogFile = ""
	c.PostRoll = 300 * time.Millisecond
	c.MergeWindow = 300 * time.Millisecond
	c.MaxIncident = 30 * time.Second
	c.HealthInterval = time.Hour
	c.Blackdetect.Enabled = false // synthetic fixtures carry no decodable video
	c.StallTargetDurations = 1000 // static test playlists would otherwise stall
	c.MinFreeBytes = 0            // health doesn't depend on this machine's free space
	c.Channels = []config.Channel{{Name: "test", URL: url}}
	return c
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// run starts the monitor and stops it once done() holds, failing after 15 s.
// It returns the segment records seen, by sequence number.
func run(t *testing.T, cfg config.Config, done func(m *Monitor, recs map[uint64]SegmentRecord) bool, tweaks ...func(*Options)) (*Monitor, map[uint64]SegmentRecord) {
	t.Helper()
	var mu sync.Mutex
	recs := map[uint64]SegmentRecord{}
	opts := Options{
		Config:           cfg,
		Logger:           testLogger(t),
		RetryDelay:       10 * time.Millisecond,
		OriginRetryDelay: 20 * time.Millisecond,
		OnSegment: func(channel string, rec SegmentRecord) {
			mu.Lock()
			defer mu.Unlock()
			recs[rec.Seq] = rec
		},
	}
	for _, tweak := range tweaks {
		tweak(&opts)
	}
	m, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- m.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		ok := done(m, recs)
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-errc
			t.Fatalf("timed out; records so far: %v", slices.Sorted(maps.Keys(recs)))
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return m, maps.Clone(recs)
}

func seen(seq uint64) func(*Monitor, map[uint64]SegmentRecord) bool {
	return func(_ *Monitor, recs map[uint64]SegmentRecord) bool { _, ok := recs[seq]; return ok }
}

func incidentDirs(t *testing.T, cfg config.Config) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.DataDir, "incidents"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(cfg.DataDir, "incidents", e.Name()))
		}
	}
	return dirs
}

func readReport(t *testing.T, dir string) Report {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func readCSV(t *testing.T, cfg config.Config) [][]string {
	t.Helper()
	f, err := os.Open(filepath.Join(cfg.DataDir, "incidents", "incidents.csv"))
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

func column(t *testing.T, rows [][]string, row int, name string) string {
	t.Helper()
	i := slices.Index(rows[0], name)
	if i < 0 {
		t.Fatalf("no %q column in %v", name, rows[0])
	}
	return rows[row][i]
}

// Seq 101's connection is dropped every time: our fetch failed (a monitor
// gap), so 100 and 102 must not be compared, even though their timestamps
// are a segment apart, and no incident may open.
func TestSkippedSegmentIsAMonitorGapNotAnIncident(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(100, entry{uri: "s100.ts"}, entry{uri: "s101.ts"}, entry{uri: "s102.ts"}))
	o.set("/live/s100.ts", tstest.Load(t, "cont_0.ts"))
	o.drop("/live/s101.ts")
	o.set("/live/s102.ts", tstest.Load(t, "cont_2.ts"))
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")

	_, recs := run(t, cfg, seen(102))

	if r := recs[101]; r.Error == "" || r.Fetch.Status != 0 || len(r.Faults) != 0 {
		t.Errorf("101: error=%q status=%d faults=%v, want a network error and no fault", r.Error, r.Fetch.Status, r.Faults)
	}
	r := recs[102]
	if r.Gap == nil || r.Gap.From != 101 || r.Gap.To != 101 {
		t.Errorf("102 gap = %+v, want 101..101", r.Gap)
	}
	if len(r.Faults) != 0 || len(recs[100].Faults) != 0 {
		t.Errorf("faults: 100=%v 102=%v", recs[100].Faults, r.Faults)
	}
	if dirs := incidentDirs(t, cfg); len(dirs) != 0 {
		t.Errorf("incidents opened: %v", dirs)
	}

	buf := filepath.Join(cfg.DataDir, "buffer", "test")
	for _, name := range []string{
		segmentFile(100, "s100.ts") + ".ts", segmentFile(100, "s100.ts") + ".json",
		segmentFile(101, "s101.ts") + ".json", segmentFile(102, "s102.ts") + ".ts",
	} {
		if _, err := os.Stat(filepath.Join(buf, name)); err != nil {
			t.Errorf("buffer: %v", err)
		}
	}
	metas, _ := filepath.Glob(filepath.Join(buf, "playlist_*_msn100.json"))
	if len(metas) == 0 {
		t.Fatal("no saved playlist fetch")
	}
	b, _ := os.ReadFile(metas[0])
	var meta FetchMeta
	if err := json.Unmarshal(b, &meta); err != nil || meta.Status != 200 || meta.RequestedAt.IsZero() ||
		meta.Headers.Get("Content-Type") != "application/vnd.apple.mpegurl" {
		t.Errorf("playlist meta = %+v, %v", meta, err)
	}
}

func jumpOrigin(t *testing.T, disc bool) (config.Config, []byte, []byte) {
	o, srv := newOrigin(t)
	o.set("/live/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nhi.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360\nlo.m3u8\n"))
	before, after := tstest.Load(t, "cont_0.ts"), tstest.Load(t, "jump_1.ts")
	for _, v := range []string{"hi", "lo"} {
		o.set("/live/"+v+".m3u8", media(200, entry{uri: v + "200.ts"}, entry{uri: v + "201.ts", disc: disc}))
		o.set("/live/"+v+"200.ts", before)
		o.set("/live/"+v+"201.ts", after)
	}
	return testConfig(t, srv.URL+"/live/master.m3u8"), before, after
}

// jump_1 starts 10 s after cont_0 ends with no EXT-X-DISCONTINUITY: a stream
// fault. The incident must carry the evidence, the other rendition's copies
// of 200 and 201, and a CSV row.
func TestTimestampJumpOpensIncidentWithEvidence(t *testing.T) {
	cfg, before, after := jumpOrigin(t, false)
	// Long enough for the other rendition to be fetched even on a loaded
	// machine: at 1 s this failed 4 of 42 runs at background priority.
	cfg.PostRoll = 3 * time.Second

	closed := func(*Monitor, map[uint64]SegmentRecord) bool {
		for _, d := range incidentDirs(t, cfg) {
			var r Report
			if b, err := os.ReadFile(filepath.Join(d, "report.json")); err == nil && json.Unmarshal(b, &r) == nil && r.Status == "closed" {
				return true
			}
		}
		return false
	}
	run(t, cfg, closed)

	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 || !regexp.MustCompile(`/\d{8}T\d{6}Z_test$`).MatchString(dirs[0]) {
		t.Fatalf("incident dirs = %v", dirs)
	}
	dir := dirs[0]
	r := readReport(t, dir)
	if r.Channel != "test" || r.CloseReason != "post_roll_elapsed" || r.Stream.Rendition != "0_1280x720_2000000" {
		t.Errorf("report header = %+v", r)
	}
	if !slices.Contains(r.FaultTypes, analysis.FaultVideoDTSGap) || !slices.Contains(r.FaultTypes, analysis.FaultAudioPTSGap) {
		t.Errorf("fault types = %v", r.FaultTypes)
	}
	var video *FaultRecord
	for i := range r.Faults {
		if r.Faults[i].Type == analysis.FaultVideoDTSGap {
			video = &r.Faults[i]
		}
	}
	if video == nil || video.Seq != 201 || video.PrevSeq == nil || *video.PrevSeq != 200 || video.Values["first_dts"] != float64(1848048) {
		t.Fatalf("video fault = %+v", video)
	}
	if got := video.Renditions["1_640x360_500000"]; got != "reproduced" {
		t.Errorf("other rendition = %q, want reproduced", got)
	}
	var segs []uint64
	for _, s := range r.Segments {
		segs = append(segs, s.Seq)
	}
	if !slices.Contains(segs, 200) || !slices.Contains(segs, 201) {
		t.Errorf("report segments = %v", segs)
	}

	for name, want := range map[string][]byte{
		"segments/" + segmentFile(200, "hi200.ts") + ".ts":                    before,
		"segments/" + segmentFile(201, "hi201.ts") + ".ts":                    after,
		"renditions/1_640x360_500000/" + segmentFile(201, "lo201.ts") + ".ts": after,
		"renditions/1_640x360_500000/" + segmentFile(200, "lo200.ts") + ".ts": before,
	} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, %v", name, len(got), err)
		}
	}
	for _, pattern := range []string{"playlists/playlist_*.m3u8", "playlists/playlist_*.json", "playlists/master_*.m3u8", "segments/" + segmentFile(201, "hi201.ts") + ".json"} {
		if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) == 0 {
			t.Errorf("no %s in the incident", pattern)
		}
	}

	rows := readCSV(t, cfg)
	if len(rows) != 2 {
		t.Fatalf("csv rows = %v", rows)
	}
	if column(t, rows, 1, "channel") != "test" || column(t, rows, 1, "status") != "closed" ||
		column(t, rows, 1, "fault_types") != "audio_pts_gap;video_dts_gap" || column(t, rows, 1, "id") != filepath.Base(dir) {
		t.Errorf("csv row = %v", rows[1])
	}
}

// A new segment tagged EXT-X-DISCONTINUITY opens a "discontinuity" incident,
// but the timestamp jump behind the tag is not a continuity fault.
func TestTaggedDiscontinuityOpensDiscontinuityIncident(t *testing.T) {
	cfg, _, _ := jumpOrigin(t, true)
	_, recs := run(t, cfg, seen(201))
	if !recs[201].Discontinuity || !slices.Equal(recs[201].Faults, []string{analysis.FaultDiscontinuity}) {
		t.Errorf("201: disc=%v faults=%v, want [discontinuity]", recs[201].Discontinuity, recs[201].Faults)
	}
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents = %v, want 1", dirs)
	}
	r := readReport(t, dirs[0])
	if !slices.Equal(r.FaultTypes, []string{analysis.FaultDiscontinuity}) || r.Faults[0].Values["reason"] != "tag" {
		t.Errorf("fault types %v, first fault %+v", r.FaultTypes, r.Faults[0])
	}
}

func newTestMonitor(t *testing.T, cfg config.Config, now func() time.Time) *Monitor {
	t.Helper()
	m, err := New(Options{Config: cfg, Logger: testLogger(t), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManualCaptureOpensThenMerges(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.MergeWindow = time.Minute
	m := newTestMonitor(t, cfg, nil)
	h := m.Handler()

	post := func(target string) (int, map[string]string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765"+target, nil))
		var body map[string]string
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, first := post("/capture?channel=test")
	if code != http.StatusOK || first["status"] != "opened" || first["incident"] == "" {
		t.Fatalf("first capture: %d %v", code, first)
	}
	code, second := post("/capture?channel=test")
	if code != http.StatusOK || second["status"] != "merged" || second["incident"] != first["incident"] {
		t.Errorf("second capture: %d %v", code, second)
	}
	if code, _ := post("/capture?channel=nope"); code != http.StatusNotFound {
		t.Errorf("unknown channel: %d", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/capture?channel=test", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}

	m.incidents.shutdown()
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents = %v", dirs)
	}
	r := readReport(t, dirs[0])
	if len(r.Faults) != 2 || r.Faults[0].Type != analysis.FaultManual || r.CloseReason != "shutdown" {
		t.Errorf("report = %+v", r)
	}
	if rows := readCSV(t, cfg); len(rows) != 2 {
		t.Errorf("csv rows = %v", rows)
	}
}

// A fault that never stops closes its incident at max_incident and is then
// suppressed on that channel until it has been quiet for merge_window; a
// different fault type still opens an incident meanwhile.
func TestSustainedFaultIsCappedThenSuppressed(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.PostRoll, cfg.MergeWindow, cfg.MaxIncident = time.Minute, time.Minute, 5*time.Minute
	clock := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	m := newTestMonitor(t, cfg, func() time.Time { return clock })
	in, ch := m.incidents, m.channels[0]
	fault := func(typ string, seq uint64) {
		in.fault(ch, []analysis.Fault{{Type: typ, Seq: seq}}, SegmentRecord{Seq: seq})
		in.tick(clock)
	}

	start := clock
	for i := range 70 { // black every 6 s for 7 minutes
		clock = start.Add(time.Duration(i) * 6 * time.Second)
		fault(analysis.FaultBlackVideo, uint64(i))
	}
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("after 7 min of black: %d incidents, want 1", len(dirs))
	}
	if r := readReport(t, dirs[0]); r.Status != "closed" || r.CloseReason != "max_duration" {
		t.Errorf("first incident = %s/%s", r.Status, r.CloseReason)
	}

	clock = clock.Add(6 * time.Second)
	fault(analysis.FaultPTSPCRJump, 100)
	if n := len(incidentDirs(t, cfg)); n != 2 {
		t.Fatalf("a new fault type during suppression: %d incidents, want 2", n)
	}

	clock = clock.Add(3 * time.Minute) // black quiet for longer than merge_window
	in.tick(clock)
	fault(analysis.FaultBlackVideo, 200)
	if n := len(incidentDirs(t, cfg)); n != 3 {
		t.Errorf("black after a quiet spell: %d incidents, want 3", n)
	}
	in.shutdown()
}

func TestStorageCapDeletesOldestClosedIncidents(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.IncidentStorageBytes = 2500
	m := newTestMonitor(t, cfg, nil)
	root := filepath.Join(cfg.DataDir, "incidents")
	for _, name := range []string{"20251231T000000Z_open", "20260101T000000Z_a", "20260102T000000Z_b", "20260103T000000Z_c"} {
		dir := filepath.Join(root, name, "segments")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "seg_1.ts"), make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The oldest is still open, so it must survive.
	m.incidents.open["open"] = &incident{dir: filepath.Join(root, "20251231T000000Z_open")}

	m.incidents.enforceCap()

	var left []string
	for _, d := range incidentDirs(t, cfg) {
		left = append(left, filepath.Base(d))
	}
	if want := []string{"20251231T000000Z_open", "20260103T000000Z_c"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

func TestPruneOlderKeepsRecentFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{"old.ts": 5 * time.Minute, "new.ts": 0} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	if err := pruneBuffer(dir, now, 3*time.Minute, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.ts")); !os.IsNotExist(err) {
		t.Error("old.ts survived")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.ts")); err != nil {
		t.Error("new.ts was removed")
	}
}

func TestRecoverMarksInterruptedIncidents(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	dir := filepath.Join(cfg.DataDir, "incidents", "20260101T000000Z_test")
	os.MkdirAll(dir, 0o755)
	b, _ := json.Marshal(Report{ID: filepath.Base(dir), Channel: "test", Status: "open",
		Faults: []FaultRecord{{Type: analysis.FaultPTSBehindPCR, Seq: 7}}})
	os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)

	m := newTestMonitor(t, cfg, nil)
	m.incidents.recover()

	if r := readReport(t, dir); r.Status != "interrupted" {
		t.Errorf("status = %q", r.Status)
	}
	rows := readCSV(t, cfg)
	if len(rows) != 2 || column(t, rows, 1, "status") != "interrupted" || column(t, rows, 1, "fault_types") != "pts_behind_pcr" {
		t.Errorf("csv = %v", rows)
	}
}
