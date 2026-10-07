package monitor

import (
	"slices"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
)

// EXT-X-ENDLIST says the origin has ended the stream: one stream_ended
// fault, and no stall however long the playlist stays the same. A playlist
// without it (the stream came back) ends that.
func TestEndlistEndsTheStreamNotAStall(t *testing.T) {
	parse := func(b []byte) *hls.Media {
		pl, err := hls.ParseMedia(b)
		if err != nil {
			t.Fatal(err)
		}
		return pl
	}
	live := parse(playlistBody(100, -1, seg(100), seg(101), seg(102)))
	ended := parse(append(playlistBody(100, -1, seg(100), seg(101), seg(102)), "#EXT-X-ENDLIST\n"...))
	at := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	meta := FetchMeta{Status: 200}
	var p pollState
	p.update(live, "http://h/p.m3u8", "p1", meta, at, 3)
	var types []string
	for i := range 3 {
		u := p.update(ended, "http://h/p.m3u8", "p2", meta, at.Add(time.Duration(i+1)*time.Second), 3)
		for _, f := range u.faults {
			types = append(types, f.Type)
		}
	}
	if !slices.Equal(types, []string{analysis.FaultStreamEnded}) {
		t.Errorf("faults %v, want one %s", types, analysis.FaultStreamEnded)
	}
	if f, ok := p.stall(at.Add(time.Hour), time.Second, 3, meta); ok {
		t.Errorf("%s an hour after EXT-X-ENDLIST", f.Type)
	}
	p.update(parse(playlistBody(101, -1, seg(101), seg(102), seg(103))), "http://h/p.m3u8", "p3", meta, at.Add(2*time.Hour), 3)
	if _, ok := p.stall(at.Add(3*time.Hour), time.Second, 3, meta); !ok {
		t.Error("no stall once the stream came back and then stopped")
	}
}

// A segment the playlist marks EXT-X-GAP has no media: it is not fetched,
// nothing is blamed on the origin, and it is logged as a gap_tagged event.
func TestGapSegmentIsNotFetched(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8", []byte("#EXTM3U\n#EXT-X-VERSION:8\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:100\n"+
		"#EXTINF:0.534,\ns100.ts\n#EXTINF:0.534,\ns101.ts\n#EXT-X-GAP\n#EXTINF:0.534,\nmissing.ts\n#EXTINF:0.534,\ns103.ts\n"))
	o.segments(100, 2)
	o.segments(103, 1)
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	_, recs := run(t, cfg, seen(103), func(o *Options) { o.InitialSegments = 4 })
	r := recs[102]
	if len(r.Faults) > 0 || r.Error != "" || !slices.ContainsFunc(r.Events, func(e analysis.Event) bool { return e.Type == analysis.EventGapTagged }) {
		t.Errorf("102: faults %v error %q events %+v, want only a %s event", r.Faults, r.Error, r.Events, analysis.EventGapTagged)
	}
	if recs[103].Gap != nil {
		t.Errorf("103 records a monitor gap %+v after the gap-tagged segment", recs[103].Gap)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if n := o.hits["/live/missing.ts"]; n > 0 {
		t.Errorf("the gap-tagged segment was requested %d times", n)
	}
}
