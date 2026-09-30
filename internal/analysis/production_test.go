package analysis

import (
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// productionShaped is a timeline shaped like the origin's streams: 6 s
// segments of 181 frames, every video frame spanning many packets, one AAC
// frame of 512 bytes per audio PES (so each spans three packets), PCR 800
// ticks before DTS, audio starting 16 ms after the first picture.
var productionShaped = tstest.Timeline{
	VideoStart:        900000,
	AudioStart:        900000 + 2*tstest.FrameTicks + 1440,
	FramesPerSegment:  181,
	PCRLead:           800,
	AudioFramesPerPES: 1,
	AudioFrameBytes:   512,
	VideoFiller:       2000,
}

// A production-shaped segment gives the values worked out by hand from its
// timeline, and two in a row are continuous.
func TestProductionShapedSegments(t *testing.T) {
	s0, s1 := analyzeSeg(t, productionShaped.Segment(0)), analyzeSeg(t, productionShaped.Segment(1))
	sum := s0.Summary()
	v, a := sum.Video, sum.Audio
	// Video: 181 frames of 3003 ticks. DTS-PCR is the 800-tick lead
	// everywhere; PTS-DTS is 1 (B), 2 (I) or 4 (P) frames, so PTS-PCR runs
	// from 3803 to 12812 ticks.
	if v.Frames != 181 || v.FrameMs != 33.367 || sum.DurationS != 6.039 {
		t.Errorf("video %d frames of %v ms, %v s; want 181 of 33.367 ms, 6.039 s", v.Frames, v.FrameMs, sum.DurationS)
	}
	if *v.DTSPCRMinMs != 8.889 || *v.DTSPCRMaxMs != 8.889 || *v.PTSPCRMinMs != 42.256 || *v.PTSPCRMaxMs != 142.356 {
		t.Errorf("DTS-PCR %v..%v, PTS-PCR %v..%v; want 8.889..8.889 and 42.256..142.356",
			*v.DTSPCRMinMs, *v.DTSPCRMaxMs, *v.PTSPCRMinMs, *v.PTSPCRMaxMs)
	}
	// Audio: PES j (PTS 1440 + 1920j ticks after the first picture) is in
	// segment 0 while that is under 181*3003 = 543543: j = 0..282.
	if a.PES != 283 || a.Frames != 283 || a.SampleRate != 48000 {
		t.Errorf("audio %d PES, %d frames at %d Hz; want 283, 283, 48000", a.PES, a.Frames, a.SampleRate)
	}
	for i, p := range s0.Audio.PES {
		if p.Frames != 1 || p.Ticks != 1920 {
			t.Fatalf("audio PES %d: %d frames, %d ticks; want 1 frame of 1920", i, p.Frames, p.Ticks)
		}
	}
	if sum.Packets < 181*12 {
		t.Errorf("%d packets: video frames should span many", sum.Packets)
	}
	c := NewChain(DefaultThresholds())
	c.Add(Input{Seq: 1, ExtInf: 6.039, Segment: s0, FrameTicks: tstest.FrameTicks})
	r := c.Add(Input{Seq: 2, ExtInf: 6.039, Segment: s1, FrameTicks: tstest.FrameTicks})
	if len(r.Faults)+len(r.Events) > 0 {
		t.Errorf("two continuous segments: faults %v events %v", faultTypes(r.Faults), eventTypes(r.Events))
	}
}
