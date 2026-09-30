package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// originPlaylist is a playlist the way the origin writes them:
// URIs numbered ...-seq=N.ts and no EXT-X-DISCONTINUITY-SEQUENCE. A number
// in disc gets EXT-X-DISCONTINUITY.
func originPlaylist(target int, msn uint64, disc []uint64, nums ...uint64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", target, msn)
	for _, n := range nums {
		if slices.Contains(disc, n) {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:0.534,\nhi-begin=0-dur=5340000-seq=%d.ts\n", n)
	}
	return []byte(b.String())
}

// The origin's playlist violations, in the order it makes them
// around a splice: it skips a number, renumbers the playlist once that
// entry reaches the head, changes EXT-X-TARGETDURATION, and drops
// EXT-X-DISCONTINUITY as its entry becomes first. Each is one
// playlist_violation fault naming the entry, except the target duration
// change and the dropped tag, which are its normal practice (it sends no
// EXT-X-DISCONTINUITY-SEQUENCE): those are noted once in origin_notes.csv
// and listed in the open incident's origin_notes, and the dropped tag is
// not reported as a second discontinuity.
func TestOriginPlaylistViolations(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	for k, n := range []uint64{100, 101, 103, 104, 105, 106} {
		o.sequence(fmt.Sprintf("/live/hi-begin=0-dur=5340000-seq=%d.ts", n), tstest.Base.Segment(k).Bytes())
	}
	d := []uint64{103}
	o.sequence("/live/hi.m3u8",
		originPlaylist(1, 100, nil, 100, 101),
		originPlaylist(1, 100, d, 100, 101, 103),        // 103 follows 101: 102 never used
		originPlaylist(1, 100, d, 100, 101, 103, 104),   //
		originPlaylist(2, 102, d, 101, 103, 104, 105),   // renumbered (+1), TARGETDURATION 1 -> 2
		originPlaylist(2, 103, nil, 103, 104, 105, 106)) // 103's tag dropped at the head
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.PostRoll, cfg.MergeWindow = 3e9, 3e9

	run(t, cfg, func(m *Monitor, recs map[uint64]SegmentRecord) bool {
		_, ok := recs[106]
		return ok && anyClosed(t, cfg.DataDir)
	}, func(o *Options) { o.InitialSegments = 10 })

	r := onlyIncident(t, cfg)
	var got []string
	for _, f := range r.Faults {
		got = append(got, fmt.Sprintf("%s:%v@%d", f.Type, f.Values["reason"], f.Seq))
		if f.Type == analysis.FaultPlaylistViolation && !strings.HasPrefix(f.URI, "hi-begin=") {
			t.Errorf("%v fault has URI %q", f.Values["reason"], f.URI)
		}
	}
	want := []string{
		"playlist_violation:skipped_number@103",
		"discontinuity:tag@103",
		"playlist_violation:renumbered@101",
	}
	if !slices.Equal(got, want) {
		t.Errorf("faults %v, want %v", got, want)
	}
	var noted []string
	for _, n := range r.OriginNotes {
		noted = append(noted, fmt.Sprintf("%s@%d", n.Reason, n.Seq))
	}
	if want := []string{"target_duration_changed@101", "discontinuity_tag_dropped@103"}; !slices.Equal(noted, want) {
		t.Errorf("origin notes %v, want %v", noted, want)
	}
	notes, err := os.ReadFile(filepath.Join(cfg.DataDir, "origin_notes.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(notes), ",hi-begin=0-dur=5340000-seq=103.ts,"+hls.ViolationDiscontinuityTagDropped+","); n != 1 {
		t.Errorf("origin_notes.csv has %d %s rows for 103, want 1:\n%s", n, hls.ViolationDiscontinuityTagDropped, notes)
	}
}
