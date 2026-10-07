package analysis

// Detection-logic audit (scratch). Each test logs what the chain reports for
// a constructed case; t.Error marks the behavior the audit considers wrong.

import (
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// six-second segments like the origin's: 181 frames at 29.97 fps (3n+1 so
// the synthetic IBBP GOP closes), audio in 2-frame PES.
var six = tstest.Timeline{
	VideoStart:       900000,
	AudioStart:       900000 + 2*tstest.FrameTicks + 1440,
	FramesPerSegment: 181,
	PCRLead:          63000,
}

func analyzeSeg(t *testing.T, s tstest.Segment) *Segment {
	t.Helper()
	seg, err := Analyze(s.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

func shiftVideo(s *tstest.Segment, from int, d int64) {
	for i := from; i < len(s.Video); i++ {
		s.Video[i].DTS = ts.Add(s.Video[i].DTS, d)
		s.Video[i].PTS = ts.Add(s.Video[i].PTS, d)
		if s.Video[i].PCR != nil {
			s.Video[i].PCR = new(ts.Add(*s.Video[i].PCR, d))
		}
	}
}

func evTypes(es []Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

// A -16.7 ms (half-frame) video DTS step inside a segment is invisible; the
// same step at a segment boundary is a video_dts_gap fault.
func TestAuditDetInternalHalfFrameStepIsInvisible(t *testing.T) {
	const half = tstest.FrameTicks / 2
	// Inside: segment 1 steps back half a frame at frame 90; everything after
	// (including segment 2) stays on the shifted timeline.
	c := NewChain(DefaultThresholds())
	s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
	shiftVideo(&s1, 90, -half)
	shiftVideo(&s2, 0, -half)
	c.Add(Input{Seq: 100, ExtInf: 6.039, Segment: analyzeSeg(t, s0)})
	r1 := c.Add(Input{Seq: 101, ExtInf: 6.039, Segment: analyzeSeg(t, s1)})
	r2 := c.Add(Input{Seq: 102, ExtInf: 6.039, Segment: analyzeSeg(t, s2)})
	t.Logf("inside: seg 101 faults=%v events=%v; seg 102 faults=%v events=%v", types(r1.Faults), evTypes(r1.Events), types(r2.Faults), evTypes(r2.Events))
	// At the boundary: the same shift starts with segment 2.
	b := NewChain(DefaultThresholds())
	b0, b1, b2 := six.Segment(0), six.Segment(1), six.Segment(2)
	shiftVideo(&b2, 0, -half)
	b.Add(Input{Seq: 100, ExtInf: 6.039, Segment: analyzeSeg(t, b0)})
	b.Add(Input{Seq: 101, ExtInf: 6.039, Segment: analyzeSeg(t, b1)})
	rb := b.Add(Input{Seq: 102, ExtInf: 6.039, Segment: analyzeSeg(t, b2)})
	t.Logf("boundary: seg 102 faults=%v", types(rb.Faults))
	if len(r1.Faults)+len(r1.Events) == 0 && len(rb.Faults) > 0 {
		t.Error("the same -16.7 ms video step is a fault at a boundary but not even an event inside a segment")
	}
}

// A 3.2 s hole in the audio inside a segment (80 PES missing, timeline
// intact) is only an audio_retimed event; the same hole at a boundary is an
// audio_pts_gap fault. Real: channel6 61785424 (3.24 s internal hole, no
// incident) vs channel2 61783578 (+3194.6 ms at a boundary, incident).
func TestAuditDetInternalAudioHoleIsOnlyAnEvent(t *testing.T) {
	c := NewChain(DefaultThresholds())
	s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
	s1.Audio = append(s1.Audio[:40:40], s1.Audio[120:]...) // 80 PES x 40 ms = 3.2 s missing
	c.Add(Input{Seq: 100, Segment: analyzeSeg(t, s0)})
	r1 := c.Add(Input{Seq: 101, Segment: analyzeSeg(t, s1)})
	r2 := c.Add(Input{Seq: 102, Segment: analyzeSeg(t, s2)})
	for _, e := range r1.Events {
		t.Logf("seg 101 event %s gap_ms=%.3f %s", e.Type, e.GapMs, e.Detail)
	}
	t.Logf("inside: seg 101 faults=%v; seg 102 faults=%v", types(r1.Faults), types(r2.Faults))

	b := NewChain(DefaultThresholds())
	b0, b1 := six.Segment(0), six.Segment(1)
	b1.Audio = b1.Audio[80:] // the first 3.2 s of segment 1's audio missing
	b.Add(Input{Seq: 100, Segment: analyzeSeg(t, b0)})
	rb := b.Add(Input{Seq: 101, Segment: analyzeSeg(t, b1)})
	t.Logf("boundary: seg 101 faults=%v", types(rb.Faults))
	if len(r1.Faults) == 0 && len(rb.Faults) > 0 {
		t.Error("a 3.2 s audio hole is a fault at a boundary but only an event inside a segment")
	}
}

// Extra audio stacked on the timeline (8 PES that overlap the one before,
// like the 287-296-frame segments in events.csv): net -307 ms, audio
// boundaries continuous. Only an event.
func TestAuditDetOverlappingAudioIsOnlyAnEvent(t *testing.T) {
	c := NewChain(DefaultThresholds())
	s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
	var au []tstest.AudioPES
	for i, p := range s1.Audio {
		au = append(au, p)
		if i >= 60 && i < 68 { // a stacked copy 540 ticks later
			au = append(au, tstest.AudioPES{PTS: ts.Add(p.PTS, 540), Frames: p.Frames})
		}
	}
	s1.Audio = au
	c.Add(Input{Seq: 100, Segment: analyzeSeg(t, s0)})
	r1 := c.Add(Input{Seq: 101, Segment: analyzeSeg(t, s1)})
	r2 := c.Add(Input{Seq: 102, Segment: analyzeSeg(t, s2)})
	for _, e := range r1.Events {
		t.Logf("seg 101 event %s gap_ms=%.3f %s", e.Type, e.GapMs, e.Detail)
	}
	t.Logf("seg 101 faults=%v; seg 102 faults=%v", types(r1.Faults), types(r2.Faults))
	if len(r1.Faults) == 0 {
		t.Error("307 ms of overlapping audio inside a segment raises no fault")
	}
}

// A 2 s hole in the video inside a segment vs the same hole at a boundary.
func TestAuditDetInternalVideoHoleIsOnlyAnEvent(t *testing.T) {
	c := NewChain(DefaultThresholds())
	s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
	s1.Video = append(s1.Video[:61:61], s1.Video[121:]...) // 60 frames = 2.002 s missing, timeline intact
	c.Add(Input{Seq: 100, ExtInf: 6.039, Segment: analyzeSeg(t, s0)})
	r1 := c.Add(Input{Seq: 101, ExtInf: 6.039, Segment: analyzeSeg(t, s1)})
	r2 := c.Add(Input{Seq: 102, ExtInf: 6.039, Segment: analyzeSeg(t, s2)})
	t.Logf("inside: seg 101 faults=%v events=%v; seg 102 faults=%v", types(r1.Faults), evTypes(r1.Events), types(r2.Faults))
	if len(r1.Faults) == 0 {
		t.Error("a 2.0 s video hole inside a segment raises no fault")
	}
}

// The A/V baseline is the median of the first 5 segments. If 3 of them are
// outliers (the monitor starts inside a stretch with another offset), every
// normal segment afterwards is an av_offset fault until 10 agree.
func TestAuditDetAVBaselineFromOutliers(t *testing.T) {
	c := NewChain(DefaultThresholds())
	flagged := 0
	for k := range 20 {
		s := six.Segment(k)
		if k < 3 {
			for i := range s.Audio {
				s.Audio[i].PTS = ts.Add(s.Audio[i].PTS, -13500) // audio 150 ms earlier
			}
		}
		r := c.Add(Input{Seq: uint64(100 + k), Segment: analyzeSeg(t, s)})
		for _, f := range r.Faults {
			if f.Type == FaultAVOffset {
				flagged++
			}
		}
	}
	b, _ := c.Baseline()
	t.Logf("av_offset faults on normal segments after a 3-of-5 outlier start: %d; final baseline %.3f ms", flagged, ts.Millis(b))
}

// A segment whose frames are all 5 slots apart (a static/black slate coded
// at 6 fps) has FrameTicks = 166.8 ms. The next, normal segment starts one
// normal frame (33.4 ms) after its last frame: the boundary check expects
// 166.8 ms and flags -133 ms.
func TestAuditDetSparseSegmentMedianFrame(t *testing.T) {
	c := NewChain(DefaultThresholds())
	s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
	var sparse []tstest.AU
	for i, au := range s1.Video {
		if i%5 == 0 {
			sparse = append(sparse, au)
		}
	}
	s1.Video = sparse
	c.Add(Input{Seq: 100, ExtInf: 6.039, Segment: analyzeSeg(t, s0)})
	a1 := analyzeSeg(t, s1)
	r1 := c.Add(Input{Seq: 101, ExtInf: 6.039, Segment: a1})
	// segment 2 starts one normal frame after segment 1's last (sparse) frame
	a2s := s2
	shiftVideo(&a2s, 0, ts.Diff(ts.Add(a1.Video.LastDTS(), tstest.FrameTicks), s2.Video[0].DTS))
	r2 := c.Add(Input{Seq: 102, ExtInf: 6.039, Segment: analyzeSeg(t, a2s)})
	t.Logf("sparse seg 101: %d frames, FrameTicks %d; faults=%v events=%v", len(a1.Video.AUs), a1.Video.FrameTicks, types(r1.Faults), evTypes(r1.Events))
	for _, f := range r2.Faults {
		t.Logf("seg 102 fault %s: %s", f.Type, f.Message)
	}
}
