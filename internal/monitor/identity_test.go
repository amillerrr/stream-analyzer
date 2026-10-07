package monitor

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// numberedPlaylist is a media playlist whose URIs carry the origin's
// segment number, like the origin's ...-seq=N.ts.
func numberedPlaylist(variant string, msn uint64, nums ...uint64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", msn)
	for _, n := range nums {
		fmt.Fprintf(&b, "#EXTINF:0.534,\n%s-begin=0-dur=5340000-seq=%d.ts\n", variant, n)
	}
	return []byte(b.String())
}

// An origin's renumbering pattern, with origin-numbered URIs: number 102
// is never produced (seq=103 sits at position 102), the playlist is later
// renumbered (MEDIA-SEQUENCE jumps to 103), and the media jumps 10 s at 104
// in both renditions. Each origin segment must be downloaded once and known
// by its origin number; the only timestamp faults are the real ones at 104;
// the other rendition must be compared at the same origin numbers.
func TestOriginNumbersIdentifySegments(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(twoVariantMaster))
	jumped := tstest.Base
	jumped.VideoStart += 900000
	jumped.AudioStart += 900000
	content := map[uint64][]byte{
		100: tstest.Base.Segment(0).Bytes(), 101: tstest.Base.Segment(1).Bytes(), 103: tstest.Base.Segment(2).Bytes(),
		104: jumped.Segment(3).Bytes(), 105: jumped.Segment(4).Bytes(), 106: jumped.Segment(5).Bytes(),
	}
	for _, v := range []string{"hi", "lo"} {
		for n, body := range content {
			o.sequence(fmt.Sprintf("/live/%s-begin=0-dur=5340000-seq=%d.ts", v, n), body)
		}
	}
	o.sequence("/live/hi.m3u8",
		numberedPlaylist("hi", 100, 100, 101, 103),
		numberedPlaylist("hi", 100, 100, 101, 103, 104),
		numberedPlaylist("hi", 100, 100, 101, 103, 104),
		numberedPlaylist("hi", 103, 103, 104, 105, 106))
	// The other rendition keeps the positional offset: position 103 is seq=104.
	o.sequence("/live/lo.m3u8", numberedPlaylist("lo", 100, 100, 101, 103, 104))
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.PostRoll, cfg.MergeWindow = 2e9, 2e9

	done := func(m *Monitor, recs map[uint64]SegmentRecord) bool {
		_, ok := recs[106]
		return ok && anyClosed(t, cfg.DataDir)
	}
	_, recs := run(t, cfg, done)

	o.mu.Lock()
	defer o.mu.Unlock()
	for n := range content {
		if hits := o.hits[fmt.Sprintf("/live/hi-begin=0-dur=5340000-seq=%d.ts", n)]; hits != 1 {
			t.Errorf("hi seq=%d downloaded %d times, want once", n, hits)
		}
		if r, ok := recs[n]; !ok || !strings.HasSuffix(r.URI, fmt.Sprintf("-seq=%d.ts", n)) {
			t.Errorf("record %d has URI %q, want the origin's seq=%d", n, r.URI, n)
		}
	}
	if _, ok := recs[102]; ok {
		t.Errorf("a record 102 exists, but the origin never produced number 102")
	}
	if g := recs[103].Gap; g != nil {
		t.Errorf("103 records a monitor gap %+v for the number the origin skipped", g)
	}
	r := onlyIncident(t, cfg)
	var got []string
	for _, f := range r.Faults {
		if f.Type == analysis.FaultVideoDTSGap || f.Type == analysis.FaultAudioPTSGap {
			got = append(got, fmt.Sprintf("%s@%d", f.Type, f.Seq))
			if f.URI != "hi-begin=0-dur=5340000-seq=104.ts" {
				t.Errorf("%s at %d carries URI %q", f.Type, f.Seq, f.URI)
			}
			if v := f.Renditions["1_640x360_500000"]; v != "reproduced" {
				t.Errorf("%s at %d: other rendition %q, want reproduced", f.Type, f.Seq, v)
			}
		}
	}
	slices.Sort(got)
	if want := []string{"audio_pts_gap@104", "video_dts_gap@104"}; !slices.Equal(got, want) {
		t.Errorf("timestamp faults %v, want %v", got, want)
	}
	for _, rr := range r.Renditions {
		for _, s := range rr.Segments {
			if s.Fetch != nil && !strings.HasSuffix(s.Fetch.URL, fmt.Sprintf("-seq=%d.ts", s.Seq)) {
				t.Errorf("rendition %s: segment %d was fetched from %s", rr.Label, s.Seq, s.Fetch.URL)
			}
		}
	}
}

// The worker compares a segment with the one the playlist lists before it,
// and books a monitor gap only for segments it never analyzed: numbers the
// origin skipped are not gaps.
func TestWorkerOrderFollowsThePlaylist(t *testing.T) {
	job := func(num uint64, prev string, prevNum uint64) segJob {
		return segJob{seg: hls.Segment{URI: fmt.Sprintf("s%d.ts", num)}, num: num, prevURI: prev, prevNum: prevNum}
	}
	type step struct {
		j     segJob
		st    handled
		want  analysis.Order
		label string
	}
	gap := func(from, to uint64) *analysis.Gap { return &analysis.Gap{From: from, To: to} }
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"origin skips a number", []step{
			{job(100, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(101, "s100.ts", 100), handledAnalyzed, analysis.Order{Adjacent: true}, "next"},
			{job(103, "s101.ts", 101), handledAnalyzed, analysis.Order{Adjacent: true}, "103 listed after 101"},
		}},
		{"two of our fetches fail", []step{
			{job(100, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(101, "s100.ts", 100), handledFailed, analysis.Order{Adjacent: true}, "fails"},
			{job(103, "s101.ts", 101), handledFailed, analysis.Order{Gap: gap(101, 101)}, "fails too"},
			{job(104, "s103.ts", 103), handledAnalyzed, analysis.Order{Gap: gap(101, 103)}, "after both"},
		}},
		{"refused segment is accounted for", []step{
			{job(100, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(101, "s100.ts", 100), handledUnavailable, analysis.Order{Adjacent: true}, "refused"},
			{job(102, "s101.ts", 101), handledAnalyzed, analysis.Order{}, "not compared, no gap"},
		}},
		{"window moved past entries never queued", []step{
			{job(100, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(110, "s109.ts", 109), handledAnalyzed, analysis.Order{Gap: gap(101, 109)}, "after the jump"},
		}},
		{"predecessor rewritten under the same number", []step{
			{job(100, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(102, "x101.ts", 101), handledAnalyzed, analysis.Order{Gap: gap(101, 101)}, "101 never analyzed"},
		}},
		{"the origin's window jumped past numbers it never listed", []step{
			{job(102, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{segJob{seg: hls.Segment{URI: "s110.ts"}, num: 110, skipped: gap(103, 109)}, handledAnalyzed, analysis.Order{}, "no monitor gap"},
		}},
		{"our fetch failed right before the origin's window jumped", []step{
			{job(101, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(102, "s101.ts", 101), handledFailed, analysis.Order{Adjacent: true}, "fails"},
			{segJob{seg: hls.Segment{URI: "s110.ts"}, num: 110, skipped: gap(103, 109)}, handledAnalyzed, analysis.Order{Gap: gap(102, 102)}, "only ours"},
		}},
		{"entry rewritten with no number missing", []step{
			{job(100, "", 0), handledAnalyzed, analysis.Order{}, "first"},
			{job(101, "x100.ts", 100), handledAnalyzed, analysis.Order{}, "not compared, no gap"},
		}},
	} {
		c := &Channel{}
		for _, s := range tc.steps {
			got := c.order(s.j)
			if got.Adjacent != s.want.Adjacent || (got.Gap == nil) != (s.want.Gap == nil) || got.Gap != nil && *got.Gap != *s.want.Gap {
				t.Errorf("%s, %s (%d): order %+v gap %v, want %+v gap %v", tc.name, s.label, s.j.num, got, got.Gap, s.want, s.want.Gap)
			}
			c.advance(s.j, s.st, got)
		}
	}
}

// dropFrames keeps the video AUs of s whose index keep accepts.
func dropFrames(s tstest.Segment, keep func(i int) bool) tstest.Segment {
	var aus []tstest.AU
	for i, au := range s.Video {
		if keep(i) {
			aus = append(aus, au)
		}
	}
	s.Video = aus
	return s
}

// The 512x288 rendition drops every other frame during black, so its
// segments step two frame slots at a time. A boundary where 15 frames are
// missing in every rendition (500.5 ms, just over the fault size) is the
// same fault in all of them: with FRAME-RATE=29.970 in the master
// playlist, each rendition is checked against a 29.97 fps frame, not its
// segments' own step, which would make it 467 ms there.
func TestRenditionAtHalfRateIsCheckedAgainstItsFrameRate(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,FRAME-RATE=29.970\nhi.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360,FRAME-RATE=29.970\nlo.m3u8\n"))
	tl := tstest.Timeline{VideoStart: 900000, AudioStart: 900000 + 2*tstest.FrameTicks + 1440, FramesPerSegment: 61, PCRLead: 63000}
	for k := range 5 {
		hi, lo := tl.Segment(k), tl.Segment(k)
		switch {
		case k == 3:
			// 15 frames missing at the boundary into segment 3; lo keeps
			// every other frame from the same first one.
			hi = dropFrames(hi, func(i int) bool { return i >= 15 })
			lo = dropFrames(lo, func(i int) bool { return i >= 15 && i%2 == 1 })
		default:
			lo = dropFrames(lo, func(i int) bool { return i%2 == 0 })
		}
		o.sequence(fmt.Sprintf("/live/hi%d.ts", 100+k), hi.Bytes())
		o.sequence(fmt.Sprintf("/live/lo%d.ts", 100+k), lo.Bytes())
	}
	pl := func(v string, n int) []byte {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:100\n")
		for k := range n {
			fmt.Fprintf(&b, "#EXTINF:2.035,\n%s%d.ts\n", v, 100+k)
		}
		return []byte(b.String())
	}
	o.sequence("/live/hi.m3u8", pl("hi", 3), pl("hi", 4), pl("hi", 5))
	o.sequence("/live/lo.m3u8", pl("lo", 5))
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.PostRoll, cfg.MergeWindow = 2e9, 2e9

	run(t, cfg, func(m *Monitor, recs map[uint64]SegmentRecord) bool {
		_, ok := recs[104]
		return ok && anyClosed(t, cfg.DataDir)
	})
	r := onlyIncident(t, cfg)
	var found bool
	for _, f := range r.Faults {
		if f.Type == analysis.FaultVideoDTSGap && f.Seq == 103 {
			found = true
			if v := f.Renditions["1_640x360_500000"]; v != "reproduced" {
				t.Errorf("video_dts_gap at 103: 640x360 at half rate says %q (%v), want reproduced", v, f.RenditionValues)
			}
		}
	}
	if !found {
		t.Fatalf("no video_dts_gap at 103: %+v", r.Faults)
	}
}
