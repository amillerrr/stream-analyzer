package analysis

import (
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// A picture due before it is decoded is a video_pts_error fault. Two
// pictures due at the same instant are one frame slot overlapped: under
// the size rule, a duplicate_pts event.
func TestPTSUniquenessAndOrder(t *testing.T) {
	s := six.Segment(1)
	s.Video[20].PTS = ts.Add(s.Video[20].DTS, -tstest.FrameTicks)
	r := NewChain(DefaultThresholds()).Add(Input{Seq: 101, Segment: analyzeSeg(t, s)})
	if i := slices.IndexFunc(r.Faults, func(f Fault) bool { return f.Type == FaultVideoPTSError }); i < 0 {
		t.Errorf("PTS before DTS: faults %v, want %s", faultTypes(r.Faults), FaultVideoPTSError)
	} else if n, _ := r.Faults[i].Values["pts_before_dts"].(int); n != 1 {
		t.Errorf("PTS before DTS: values %v", r.Faults[i].Values)
	}

	s = six.Segment(1)
	s.Video[11].PTS = s.Video[10].PTS
	r = NewChain(DefaultThresholds()).Add(Input{Seq: 101, Segment: analyzeSeg(t, s)})
	if i := slices.IndexFunc(r.Events, func(e Event) bool { return e.Type == EventDuplicatePTS }); i < 0 || r.Events[i].Count != 1 {
		t.Errorf("duplicate PTS: events %+v, want one %s", r.Events, EventDuplicatePTS)
	}
	if slices.Contains(faultTypes(r.Faults), FaultVideoPTSError) {
		t.Errorf("duplicate PTS: faults %v", faultTypes(r.Faults))
	}

	r = NewChain(DefaultThresholds()).Add(Input{Seq: 101, Segment: analyzeSeg(t, six.Segment(1))})
	if slices.Contains(faultTypes(r.Faults), FaultVideoPTSError) || slices.Contains(eventTypes(r.Events), EventDuplicatePTS) {
		t.Errorf("a clean segment: faults %v events %v", faultTypes(r.Faults), eventTypes(r.Events))
	}
}

// Presentation continues across a boundary when the next segment's first
// picture is due one frame after the previous segment's last. A change in
// presentation that DTS doesn't show (a changed reorder delay) follows the
// size rule: an event under video_gap_fault_ms, a video_pts_gap fault at or
// over it.
func TestPresentationContinuityAtBoundaries(t *testing.T) {
	shiftPTS := func(s *tstest.Segment, d int64) {
		for i := range s.Video {
			s.Video[i].PTS = ts.Add(s.Video[i].PTS, d)
		}
	}
	for _, tc := range []struct {
		name         string
		shift        int64
		fault, event bool
	}{
		{"continuous", 0, false, false},
		{"16.7 ms later (half a frame)", tstest.FrameTicks / 2, false, true},
		{"0.6 s later", 54000, true, false},
	} {
		c := NewChain(DefaultThresholds())
		s0, s1 := six.Segment(0), six.Segment(1)
		shiftPTS(&s1, tc.shift)
		c.Add(Input{Seq: 100, Segment: analyzeSeg(t, s0)})
		r := c.Add(Input{Seq: 101, Segment: analyzeSeg(t, s1)})
		if got := slices.Contains(faultTypes(r.Faults), FaultVideoPTSGap); got != tc.fault {
			t.Errorf("%s: faults %v, want video_pts_gap %v", tc.name, faultTypes(r.Faults), tc.fault)
		}
		if got := slices.Contains(eventTypes(r.Events), EventPresentationGap); got != tc.event {
			t.Errorf("%s: events %v, want presentation_gap %v", tc.name, eventTypes(r.Events), tc.event)
		}
		if slices.Contains(faultTypes(r.Faults), FaultVideoDTSGap) {
			t.Errorf("%s: DTS is continuous, but faults %v", tc.name, faultTypes(r.Faults))
		}
	}
}
