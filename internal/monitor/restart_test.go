package monitor

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

func named(prefix string, msn uint64, n int) []byte {
	var es []entry
	for k := range n {
		es = append(es, entry{uri: fmt.Sprintf("%s%d.ts", prefix, msn+uint64(k))})
	}
	return media(msn, es...)
}

// One playlist whose MEDIA-SEQUENCE went back is not yet a restart: it may
// be a stale copy from another cache. The old timeline resumes, and the new
// one's segments are never fetched.
func TestOneBackwardPlaylistIsNotARestart(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8", named("s", 1000, 3), named("r", 10, 3), named("s", 1001, 3))
	o.segments(1000, 4)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(1003))
	o.mu.Lock()
	defer o.mu.Unlock()
	if n := o.hits["/live/r10.ts"] + o.hits["/live/r11.ts"] + o.hits["/live/r12.ts"]; n > 0 {
		t.Errorf("segments of a single backward playlist were fetched %d times", n)
	}
	if r := recs[1003]; r.Gap != nil || len(r.Faults) > 0 {
		t.Errorf("1003 after the old timeline resumed: gap %+v faults %v", r.Gap, r.Faults)
	}
}

// Two playlists on the same new timeline confirm a restart: the channel
// starts over there, and nothing on it is compared with the old timeline.
func TestTwoConsistentBackwardPlaylistsAreARestart(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8", named("s", 1000, 3), named("r", 10, 3), named("r", 11, 3), named("r", 12, 3))
	o.segments(1000, 3)
	restarted := tstest.Base
	restarted.VideoStart += 50 * 90000 // the new timeline is 50 s on
	restarted.AudioStart += 50 * 90000
	for k := range 5 {
		o.sequence(fmt.Sprintf("/live/r%d.ts", 10+k), restarted.Segment(k).Bytes())
	}
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(14))
	for _, n := range []uint64{12, 13, 14} {
		if r := recs[n]; slices.Contains(r.Faults, analysis.FaultVideoDTSGap) || slices.Contains(r.Faults, analysis.FaultAudioPTSGap) {
			t.Errorf("r%d on the new timeline was compared with the old one: %v", n, r.Faults)
		}
	}
}

// A listed segment the origin refuses is unavailable only if a fresh
// playlist still lists it. After a sleep or an outage, the one the monitor
// last saw may be long out of date, and the segment has simply left.
func TestRefusedSegmentThatLeftThePlaylistIsNotUnavailable(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.route("/live/hi.m3u8", func(_ int, w http.ResponseWriter) {
		o.mu.Lock()
		gone := o.hits["/live/s501.ts"] >= 2 // refused twice: the window has moved on
		o.mu.Unlock()
		if gone {
			w.Write(named("s", 503, 3))
			return
		}
		w.Write(named("s", 500, 3))
	})
	o.route("/live/s501.ts", func(_ int, w http.ResponseWriter) { http.NotFound(w, nil) })
	o.segments(500, 1)
	o.segments(502, 4)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(503))
	if slices.Contains(recs[501].Faults, analysis.FaultUnavailable) {
		t.Errorf("501 left the playlist before it could be fetched, but it is %v", recs[501].Faults)
	}
}
