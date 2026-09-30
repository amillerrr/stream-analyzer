package monitor

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// Every playlist from the origin arrives gzipped: the monitor asks for
// gzip and reads it.
func TestGzippedPlaylistsAreRead(t *testing.T) {
	o, srv := newScriptOrigin(t)
	body := media(100, seg(100), seg(101), seg(102))
	o.route("/live/test.m3u8", func(_ int, w http.ResponseWriter) {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(body)
		zw.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(b.Bytes())
	})
	o.segments(100, 3)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	_, recs := run(t, cfg, seen(102))
	if r := recs[102]; r.Analysis == nil || len(r.Faults) > 0 {
		t.Errorf("102: analysis %v faults %v", r.Analysis != nil, r.Faults)
	}
}

// A 200 whose body is not MPEG-TS (an error page) is an invalid_segment,
// and the next segment is not compared with it.
func TestUnusableSegmentIsInvalid(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8", media(100, seg(100), seg(101), seg(102)))
	o.sequence("/live/s100.ts", tstest.Base.Segment(0).Bytes())
	o.sequence("/live/s101.ts", []byte("<html><body>Service Unavailable</body></html>"))
	o.sequence("/live/s102.ts", tstest.Base.Segment(2).Bytes())
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	_, recs := run(t, cfg, seen(102))
	if !slices.Equal(recs[101].Faults, []string{analysis.FaultInvalidSegment}) {
		t.Errorf("101 faults %v, want [invalid_segment]", recs[101].Faults)
	}
	if r := recs[102]; len(r.Faults) > 0 || r.Gap != nil {
		t.Errorf("102 after an unusable segment: faults %v gap %+v", r.Faults, r.Gap)
	}
}

// When the segment queue is full the segment is dropped, logged and
// counted in the health line.
func TestFullQueueDropsAreCounted(t *testing.T) {
	m := newTestMonitor(t, testConfig(t, "http://127.0.0.1:1/never.m3u8"), nil)
	c := m.channels[0]
	for range cap(c.jobs) {
		c.enqueue(segJob{num: 1})
	}
	c.enqueue(segJob{num: 2})
	c.enqueue(segJob{num: 3})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.st.queueDrops != 2 {
		t.Errorf("queue drops %d, want 2", c.st.queueDrops)
	}
}

// report.json lists at most maxListedFaults faults; the rest are counted
// and noted.
func TestFaultListIsCappedButCounted(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, c := m.incidents, m.channels[0]
	var faults []analysis.Fault
	for i := range maxListedFaults + 5 {
		faults = append(faults, analysis.Fault{Type: analysis.FaultPTSBehindPCR, Seq: uint64(i)})
	}
	in.fault(c, faults, SegmentRecord{Seq: 1, URI: "s1.ts"})
	in.shutdown()
	r := readReport(t, incidentDirs(t, cfg)[0])
	if len(r.Faults) != maxListedFaults || r.FaultCount != maxListedFaults+5 {
		t.Errorf("%d faults listed, %d counted; want %d and %d", len(r.Faults), r.FaultCount, maxListedFaults, maxListedFaults+5)
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "5 further faults") }) {
		t.Errorf("notes %v", r.Notes)
	}
}

// The health line is written every health_interval, not only at the end.
func TestHealthLinesAreWrittenEveryInterval(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.HealthInterval = 200 * time.Millisecond
	start := time.Now()
	run(t, cfg, func(*Monitor, map[uint64]SegmentRecord) bool { return time.Since(start) > 900*time.Millisecond })
	if rows := readHealthCSV(t, cfg); len(rows) < 1+4 { // the header, 4 periodic lines, the final one
		t.Errorf("%d health.csv rows after 0.9 s at 200 ms", len(rows))
	}
}
