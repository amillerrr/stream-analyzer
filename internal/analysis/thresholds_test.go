package analysis

import (
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// kinds returns the fault and event types of the second of two segments,
// s0 and s1 of the six timeline after change.
func kinds(t *testing.T, th Thresholds, extinf float64, change func(s0, s1 *tstest.Segment)) (faults, events []string) {
	t.Helper()
	s0, s1 := six.Segment(0), six.Segment(1)
	change(&s0, &s1)
	c := NewChain(th)
	c.Add(Input{Seq: 100, ExtInf: extinf, Segment: analyzeSeg(t, s0), FrameTicks: tstest.FrameTicks})
	r := c.Add(Input{Seq: 101, ExtInf: extinf, Segment: analyzeSeg(t, s1), FrameTicks: tstest.FrameTicks})
	return faultTypes(r.Faults), eventTypes(r.Events)
}

// Every documented threshold, just inside and just outside: a regression
// that widens or narrows one fails here.
func TestThresholdEdges(t *testing.T) {
	th := DefaultThresholds()
	const segSeconds = 181 * 3003.0 / 90000 // 6.039033 s
	for _, tc := range []struct {
		name         string
		extinf       float64
		change       func(s0, s1 *tstest.Segment)
		fault, event string // what must be there; "" for nothing of that kind
		notFault     string // a fault that must not be
		notEvent     string
	}{
		{"video 9.9 ms off: within continuity_ms", 0, func(_, s1 *tstest.Segment) { shiftVideo(s1, 0, 891) },
			"", "", FaultVideoDTSGap, EventVideoGap},
		{"video 10.1 ms off: an event", 0, func(_, s1 *tstest.Segment) { shiftVideo(s1, 0, 909) },
			"", EventVideoGap, FaultVideoDTSGap, ""},
		{"video 499.5 ms off: still an event", 0, func(_, s1 *tstest.Segment) { shiftVideo(s1, 0, 44955) },
			"", EventVideoGap, FaultVideoDTSGap, ""},
		{"video 500.5 ms off: a fault", 0, func(_, s1 *tstest.Segment) { shiftVideo(s1, 0, 45045) },
			FaultVideoDTSGap, "", "", ""},
		{"audio 9.9 ms off: within continuity_ms", 0, func(_, s1 *tstest.Segment) { shiftAudio(s1, 0, 891) },
			"", "", FaultAudioPTSGap, EventAudioGap},
		{"audio one AAC frame off: an event", 0, func(_, s1 *tstest.Segment) { shiftAudio(s1, 0, 1922) },
			"", EventAudioGap, FaultAudioPTSGap, ""},
		{"audio one missing frame plus jitter (22 ms): an event", 0, func(_, s1 *tstest.Segment) { shiftAudio(s1, 0, 1980) },
			"", EventAudioGap, FaultAudioPTSGap, ""},
		{"audio 1.5 AAC frames (32 ms) off: still an event", 0, func(_, s1 *tstest.Segment) { shiftAudio(s1, 0, 2882) },
			"", EventAudioGap, FaultAudioPTSGap, ""},
		{"audio a tick more: a fault", 0, func(_, s1 *tstest.Segment) { shiftAudio(s1, 0, 2883) },
			FaultAudioPTSGap, "", "", ""},
		{"audio 22 ms hole inside a segment: an event", 0, func(_, s1 *tstest.Segment) { shiftAudio(s1, 30, 1980) },
			"", EventAudioRetimed, FaultAudioPTSGap, ""},
		{"duration 9.9 % off EXTINF", segSeconds / 1.099, func(_, _ *tstest.Segment) {},
			"", "", FaultDurationMismatch, ""},
		{"duration 10.1 % off EXTINF", segSeconds / 1.101, func(_, _ *tstest.Segment) {},
			FaultDurationMismatch, "", "", ""},
		{"PTS-PCR jumps 499 ms inside a segment", 0, func(_, s1 *tstest.Segment) { shiftPCR(s1, 90, -44910) },
			"", "", FaultPTSPCRJump, ""},
		{"PTS-PCR jumps 501 ms inside a segment", 0, func(_, s1 *tstest.Segment) { shiftPCR(s1, 90, -45090) },
			FaultPTSPCRJump, "", "", ""},
	} {
		faults, events := kinds(t, th, tc.extinf, tc.change)
		if tc.fault != "" && !slices.Contains(faults, tc.fault) {
			t.Errorf("%s: faults %v, want %s", tc.name, faults, tc.fault)
		}
		if tc.event != "" && !slices.Contains(events, tc.event) {
			t.Errorf("%s: events %v, want %s", tc.name, events, tc.event)
		}
		if tc.notFault != "" && slices.Contains(faults, tc.notFault) {
			t.Errorf("%s: faults %v, want no %s", tc.name, faults, tc.notFault)
		}
		if tc.notEvent != "" && slices.Contains(events, tc.notEvent) {
			t.Errorf("%s: events %v, want no %s", tc.name, events, tc.notEvent)
		}
	}
}

// av_offset_ms 100: once the baseline is set, an offset 8999 ticks (99.99
// ms) from it is fine and one 9001 ticks from it is a fault. (The synthetic
// segments' own offsets vary, so the shift is worked out from them.)
func TestAVOffsetThresholdEdges(t *testing.T) {
	var offs []int64
	for k := range 7 {
		off, _ := analyzeSeg(t, six.Segment(k)).AVOffset()
		offs = append(offs, off)
	}
	base := median(offs[:5])
	for _, tc := range []struct {
		from  int64 // how far the last segment's offset ends up from the baseline
		fault bool
	}{{8999, false}, {9001, true}} {
		c := NewChain(DefaultThresholds())
		var r Result
		for k := range 7 {
			s := six.Segment(k)
			if k == 6 {
				shiftAudio(&s, 0, offs[6]-base+tc.from) // audio later: the offset drops by the shift
			}
			r = c.Add(Input{Seq: uint64(100 + k), Segment: analyzeSeg(t, s)})
		}
		if got := slices.Contains(faultTypes(r.Faults), FaultAVOffset); got != tc.fault {
			t.Errorf("offset %d ticks from the baseline: av_offset %v, want %v (%v)", tc.from, got, tc.fault, faultTypes(r.Faults))
		}
	}
}

func shiftPCR(s *tstest.Segment, from int, d int64) {
	for i := from; i < len(s.Video); i++ {
		if s.Video[i].PCR != nil {
			s.Video[i].PCR = new(ts.Add(*s.Video[i].PCR, d))
		}
	}
}
