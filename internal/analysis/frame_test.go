package analysis

import (
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// dropFrames keeps the AUs of s whose index keep accepts.
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

func faultTypes(fs []Fault) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Type)
	}
	return out
}

// A segment whose encoder dropped every other frame for most of it (the
// 512x288 rendition during black) still has frames one 29.97 fps slot long:
// its frame is the smallest DTS step that is common, not the median. A
// single stray short step does not count.
func TestSegmentFrameIsTheSmallestCommonStep(t *testing.T) {
	mostlyHalf := dropFrames(six.Segment(0), func(i int) bool { return i < 45 || i%2 == 0 })
	if got := analyzeSeg(t, mostlyHalf).Video.FrameTicks; got != tstest.FrameTicks {
		t.Errorf("frame %d ticks, want %d", got, tstest.FrameTicks)
	}
	stray := dropFrames(six.Segment(0), func(i int) bool { return i%2 == 0 })
	stray.Video = slices.Insert(stray.Video, 1, stray.Video[0])
	stray.Video[1].DTS = ts.Add(stray.Video[0].DTS, 1500)
	stray.Video[1].PTS = ts.Add(stray.Video[0].PTS, 1500)
	if got := analyzeSeg(t, stray).Video.FrameTicks; got != 2*tstest.FrameTicks {
		t.Errorf("frame %d ticks with one stray 1500-tick step, want %d", got, 2*tstest.FrameTicks)
	}
}

// halfRate is six.Segment(k) with every other frame dropped: it keeps the
// even-numbered frames, or the odd-numbered ones.
func halfRate(t *testing.T, k int, odd bool) *Segment {
	return analyzeSeg(t, dropFrames(six.Segment(k), func(i int) bool { return i%2 == 1 == odd }))
}

// When the playlist gives the frame rate, boundaries are checked against
// it: after segments at half rate, a boundary two frame slots wide is one
// frame off (a video_gap event under the fault size), as it is in a
// rendition at the full rate.
func TestBoundaryIsCheckedAgainstTheNominalFrame(t *testing.T) {
	c := NewChain(DefaultThresholds())
	c.Add(Input{Seq: 100, Segment: halfRate(t, 0, false), FrameTicks: tstest.FrameTicks})
	// Segment 0 at half rate ends on its frame 180 (of 181); segment 1
	// starts on its frame 1, two slots later.
	r := c.Add(Input{Seq: 101, Segment: halfRate(t, 1, true), FrameTicks: tstest.FrameTicks})
	i := slices.IndexFunc(r.Events, func(e Event) bool { return e.Type == EventVideoGap })
	if i < 0 {
		t.Fatalf("events %v, want a video_gap", eventTypes(r.Events))
	}
	if e := r.Events[i]; e.GapMs != ms(tstest.FrameTicks) {
		t.Errorf("video_gap %+v: want %.3f ms off", e, ms(tstest.FrameTicks))
	}
}

// Without a frame rate from the playlist, the chain learns the channel's
// frame from its recent segments, so a few segments at half rate don't
// change what one frame is.
func TestBoundaryIsCheckedAgainstTheLearnedFrame(t *testing.T) {
	c := NewChain(DefaultThresholds())
	for k := range 5 {
		c.Add(Input{Seq: uint64(100 + k), Segment: analyzeSeg(t, six.Segment(k))})
	}
	c.Add(Input{Seq: 105, Segment: halfRate(t, 5, false)})
	r := c.Add(Input{Seq: 106, Segment: halfRate(t, 6, true)})
	if !slices.Contains(eventTypes(r.Events), EventVideoGap) {
		t.Errorf("events %v, want a video_gap", eventTypes(r.Events))
	}
}

// firstEvent returns the first event of a videoSteps or audioSteps result.
func firstEvent(_ []Fault, es []Event) (Event, bool) {
	if len(es) == 0 {
		return Event{}, false
	}
	return es[0], true
}
