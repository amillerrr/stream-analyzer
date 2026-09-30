package analysis

import (
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

func shiftAudio(s *tstest.Segment, from int, d int64) {
	for i := from; i < len(s.Audio); i++ {
		s.Audio[i].PTS = ts.Add(s.Audio[i].PTS, d)
	}
}

// One size rule for boundaries and insides alike: a video gap or overlap of
// video_gap_fault_ms (0.5 s) or more, or an audio hole or overlap of more
// than audio_gap_fault_frames AAC frames (1.5: 32 ms), is a fault; anything
// smaller is an event.
func TestSizeRule(t *testing.T) {
	type want struct {
		fault, event string // "" for none
		not          string // a fault type that must not be raised
	}
	for _, tc := range []struct {
		name   string
		th     func(*Thresholds)
		change func(s1, s2 *tstest.Segment)
		want   want
	}{
		{"boundary: video one frame late", nil,
			func(_, s2 *tstest.Segment) { shiftVideo(s2, 0, tstest.FrameTicks) },
			want{event: EventVideoGap, not: FaultVideoDTSGap}},
		{"boundary: video half a frame early", nil,
			func(_, s2 *tstest.Segment) { shiftVideo(s2, 0, -tstest.FrameTicks/2) },
			want{event: EventVideoGap, not: FaultVideoDTSGap}},
		{"boundary: video 0.6 s late", nil,
			func(_, s2 *tstest.Segment) { shiftVideo(s2, 0, 54000) },
			want{fault: FaultVideoDTSGap}},
		{"boundary: 0.2 s is a fault when the size is 100 ms", func(th *Thresholds) { th.VideoGapFaultMs = 100 },
			func(_, s2 *tstest.Segment) { shiftVideo(s2, 0, 18000) },
			want{fault: FaultVideoDTSGap}},
		{"inside: 0.6 s of video missing", nil,
			func(s1, _ *tstest.Segment) { s1.Video = append(s1.Video[:61:61], s1.Video[79:]...) },
			want{fault: FaultVideoDTSGap}},
		{"inside: 0.2 s of video missing", nil,
			func(s1, _ *tstest.Segment) { s1.Video = append(s1.Video[:61:61], s1.Video[67:]...) },
			want{event: EventFrameGap, not: FaultVideoDTSGap}},
		{"boundary: audio 36 ms late", nil,
			func(_, s2 *tstest.Segment) { shiftAudio(s2, 0, 3240) },
			want{fault: FaultAudioPTSGap}},
		{"boundary: audio 30 ms late", nil,
			func(_, s2 *tstest.Segment) { shiftAudio(s2, 0, 2700) },
			want{event: EventAudioGap, not: FaultAudioPTSGap}},
		{"boundary: audio 15 ms late", nil,
			func(_, s2 *tstest.Segment) { shiftAudio(s2, 0, 1350) },
			want{event: EventAudioGap, not: FaultAudioPTSGap}},
		{"boundary: 36 ms is an event when the size is 3 AAC frames", func(th *Thresholds) { th.AudioGapFaultFrames = 3 },
			func(_, s2 *tstest.Segment) { shiftAudio(s2, 0, 3240) },
			want{event: EventAudioGap, not: FaultAudioPTSGap}},
		{"boundary: 25 ms is a fault when the size is 1 AAC frame", func(th *Thresholds) { th.AudioGapFaultFrames = 1 },
			func(_, s2 *tstest.Segment) { shiftAudio(s2, 0, 2250) },
			want{fault: FaultAudioPTSGap}},
		{"inside: one audio PES (42.7 ms) missing", nil,
			func(s1, _ *tstest.Segment) { s1.Audio = slices.Delete(s1.Audio, 30, 31) },
			want{fault: FaultAudioPTSGap}},
		{"inside: audio overlapping by 36 ms", nil,
			func(s1, s2 *tstest.Segment) { shiftAudio(s1, 30, -3240); shiftAudio(s2, 0, -3240) },
			want{fault: FaultAudioPTSGap}},
		{"inside: audio re-timed by 5.6 ms", nil,
			func(s1, s2 *tstest.Segment) { shiftAudio(s1, 30, 500); shiftAudio(s2, 0, 500) },
			want{event: EventAudioRetimed, not: FaultAudioPTSGap}},
	} {
		th := DefaultThresholds()
		if tc.th != nil {
			tc.th(&th)
		}
		c := NewChain(th)
		s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
		tc.change(&s1, &s2)
		c.Add(Input{Seq: 100, Segment: analyzeSeg(t, s0)})
		r1 := c.Add(Input{Seq: 101, Segment: analyzeSeg(t, s1)})
		r2 := c.Add(Input{Seq: 102, Segment: analyzeSeg(t, s2)})
		faults := append(faultTypes(r1.Faults), faultTypes(r2.Faults)...)
		events := append(eventTypes(r1.Events), eventTypes(r2.Events)...)
		if tc.want.fault != "" && !slices.Contains(faults, tc.want.fault) {
			t.Errorf("%s: faults %v, want %s", tc.name, faults, tc.want.fault)
		}
		if tc.want.event != "" && !slices.Contains(events, tc.want.event) {
			t.Errorf("%s: events %v, want %s", tc.name, events, tc.want.event)
		}
		if tc.want.not != "" && slices.Contains(faults, tc.want.not) {
			t.Errorf("%s: faults %v, want no %s", tc.name, faults, tc.want.not)
		}
	}
}

// Audio that stops 3 s before its segment's video ends is a fault on that
// segment. The next segment's audio starting 3 s after it ended is the same
// hole, not a second fault.
func TestAudioMustCoverTheVideo(t *testing.T) {
	c := NewChain(DefaultThresholds())
	s0, s1, s2 := six.Segment(0), six.Segment(1), six.Segment(2)
	s1.Audio = s1.Audio[:len(s1.Audio)-71] // 71 PES x 42.7 ms = 3.03 s
	c.Add(Input{Seq: 100, Segment: analyzeSeg(t, s0)})
	r1 := c.Add(Input{Seq: 101, Segment: analyzeSeg(t, s1)})
	r2 := c.Add(Input{Seq: 102, Segment: analyzeSeg(t, s2)})
	if !slices.Contains(faultTypes(r1.Faults), FaultAudioCoverage) {
		t.Errorf("101 faults %v, want %s", faultTypes(r1.Faults), FaultAudioCoverage)
	}
	for _, f := range r1.Faults {
		if f.Type == FaultAudioCoverage {
			if short, _ := f.Values["short_ms"].(float64); short < 2900 || short > 3100 {
				t.Errorf("audio_coverage values %v: want about 3030 ms short", f.Values)
			}
		}
	}
	if slices.Contains(faultTypes(r2.Faults), FaultAudioPTSGap) {
		t.Errorf("102 faults %v: the hole reported on 101 is reported again", faultTypes(r2.Faults))
	}
}

// A frame_gap event counts the frame slots skipped as well as the gaps.
func TestFrameGapCountsSlots(t *testing.T) {
	s := six.Segment(1)
	// 3 frames missing after frame 30, 4 after frame 90: 2 gaps, 7 slots.
	s.Video = append(append(s.Video[:31:31], s.Video[34:91]...), s.Video[95:]...)
	r := NewChain(DefaultThresholds()).Add(Input{Seq: 101, Segment: analyzeSeg(t, s), FrameTicks: tstest.FrameTicks})
	i := slices.IndexFunc(r.Events, func(e Event) bool { return e.Type == EventFrameGap })
	if i < 0 || r.Events[i].Count != 2 || r.Events[i].Slots != 7 {
		t.Errorf("events %+v, want a frame_gap with 2 gaps and 7 slots", r.Events)
	}
}
