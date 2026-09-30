package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// taggedPlaylist is an origin playlist whose entries may carry one tag
// each (by origin number).
func taggedPlaylist(target int, msn uint64, tags map[uint64]string, nums ...uint64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", target, msn)
	for _, n := range nums {
		if tag, ok := tags[n]; ok {
			b.WriteString(tag + "\n")
		}
		fmt.Fprintf(&b, "#EXTINF:6.006,\nhi-begin=0-dur=60060000-seq=%d.ts\n", n)
	}
	return []byte(b.String())
}

// noted returns the reasons a playlist update notes, and fails the test on
// a fault.
func noted(t *testing.T, prev, cur []byte) []string {
	t.Helper()
	parse := func(b []byte) *hls.Media {
		pl, err := hls.ParseMedia(b)
		if err != nil {
			t.Fatal(err)
		}
		return pl
	}
	at := time.Date(2026, 9, 30, 18, 44, 0, 0, time.UTC)
	meta := FetchMeta{Status: 200}
	var p pollState
	p.update(parse(prev), "http://h/p.m3u8", "p1", meta, at, 3)
	u := p.update(parse(cur), "http://h/p.m3u8", "p2", meta, at.Add(time.Second), 3)
	for _, f := range u.faults {
		t.Errorf("fault %s (%v): %s", f.Type, f.Values["reason"], f.Message)
	}
	var reasons []string
	for _, v := range u.notes {
		reasons = append(reasons, v.Reason)
	}
	return reasons
}

// The origin raises EXT-X-TARGETDURATION while a long black or filler
// segment is listed: its normal practice, noted, not a fault.
func TestTargetDurationChangeIsNoted(t *testing.T) {
	got := noted(t, taggedPlaylist(7, 100, nil, 100, 101, 102), taggedPlaylist(8, 100, nil, 100, 101, 102, 103))
	if !slices.Equal(got, []string{hls.ViolationTargetDuration}) {
		t.Errorf("notes %v, want %s", got, hls.ViolationTargetDuration)
	}
}

// Each EXT-X-TARGETDURATION change is counted in health.csv and listed in
// the report of the incident whose evidence covers it, with its values: one
// noted before the incident opened is marked pre_roll.
func TestTargetDurationChangesAreCountedAndListed(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	for k, n := range []uint64{100, 101, 102, 103, 104} {
		o.sequence(fmt.Sprintf("/live/hi-begin=0-dur=5340000-seq=%d.ts", n), tstest.Base.Segment(k).Bytes())
	}
	d := []uint64{103}
	o.sequence("/live/hi.m3u8",
		originPlaylist(1, 100, nil, 100, 101),
		originPlaylist(2, 100, nil, 100, 101, 102),    // TARGETDURATION 1 -> 2, before any incident
		originPlaylist(2, 100, d, 100, 101, 102, 103), // a discontinuity opens one
		originPlaylist(1, 101, d, 101, 102, 103, 104)) // TARGETDURATION 2 -> 1, while it is open
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.PostRoll, cfg.MergeWindow = 3e9, 3e9

	run(t, cfg, func(m *Monitor, recs map[uint64]SegmentRecord) bool {
		_, ok := recs[104]
		return ok && anyClosed(t, cfg.DataDir)
	}, func(o *Options) { o.InitialSegments = 10 })

	r := onlyIncident(t, cfg)
	for _, f := range r.Faults {
		if f.Type != analysis.FaultDiscontinuity {
			t.Errorf("fault %s (%v): want only the discontinuity", f.Type, f.Values["reason"])
		}
	}
	var listed []string
	for _, n := range r.OriginNotes {
		listed = append(listed, fmt.Sprintf("%s pre_roll=%v %v->%v", n.Reason, n.PreRoll, n.Values["prev_target_s"], n.Values["target_s"]))
		if n.URI == "" || n.DetectedAt.IsZero() || n.Values["playlist"] == nil {
			t.Errorf("note %+v lacks its entry, time or playlist", n)
		}
	}
	want := []string{"target_duration_changed pre_roll=true 1->2", "target_duration_changed pre_roll=false 2->1"}
	if !slices.Equal(listed, want) {
		t.Errorf("origin notes %v, want %v", listed, want)
	}
	if got := column(t, readHealthCSV(t, cfg), 1, "target_duration_changes"); got != "2" {
		t.Errorf("health.csv target_duration_changes = %q, want 2", got)
	}
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "origin_notes.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), ","+hls.ViolationTargetDuration+","); n != 2 {
		t.Errorf("origin_notes.csv has %d target duration rows, want 2:\n%s", n, b)
	}
}

// The origin sometimes re-tags the entry that becomes first at the head of
// a break (CUE-OUT becoming CUE-OUT-CONT, channel7 61800449 on
// 2026-09-30): its practice, noted, not a rewritten-entry fault.
func TestFirstEntryCueRewriteIsNoted(t *testing.T) {
	got := noted(t, taggedPlaylist(7, 100, map[uint64]string{101: "#EXT-X-CUE-OUT:120.000"}, 100, 101, 102),
		taggedPlaylist(7, 101, map[uint64]string{101: "#EXT-X-CUE-OUT-CONT:ElapsedTime=0.034,Duration=120.000"}, 101, 102, 103))
	if !slices.Equal(got, []string{hls.ViolationFirstEntryCues}) {
		t.Errorf("notes %v, want %s", got, hls.ViolationFirstEntryCues)
	}
}
