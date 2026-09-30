package analysis

import (
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

func types(fs []Fault) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Type)
	}
	return out
}

func find(t *testing.T, fs []Fault, typ string) Fault {
	t.Helper()
	for _, f := range fs {
		if f.Type == typ {
			return f
		}
	}
	t.Fatalf("no %s fault in %v", typ, types(fs))
	return Fault{}
}

func value(t *testing.T, f Fault, key string) float64 {
	t.Helper()
	switch v := f.Values[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	}
	t.Fatalf("%s value %q missing or not numeric: %#v", f.Type, key, f.Values[key])
	return 0
}

// add runs a fixture through the chain. extinf 0 skips the duration check.
func add(t *testing.T, c *Chain, seq uint64, fixture string, disc bool) Result {
	t.Helper()
	return c.Add(Input{Seq: seq, Discontinuity: disc, Segment: mustAnalyze(t, tstest.Load(t, fixture))})
}

func TestContinuousSegmentsHaveNoFaults(t *testing.T) {
	c := NewChain(DefaultThresholds())
	for i, name := range []string{"cont_0.ts", "cont_1.ts", "cont_2.ts"} {
		r := add(t, c, uint64(100+i), name, false)
		if len(r.Faults) != 0 || r.Gap != nil {
			t.Errorf("%s: faults %v gap %v", name, types(r.Faults), r.Gap)
		}
	}
}

func TestPTSWrapIsNotAFault(t *testing.T) {
	c := NewChain(DefaultThresholds())
	for i, name := range []string{"wrap_0.ts", "wrap_1.ts"} {
		// wrap_1 also carries the PCR across 2^33 while PTS has already wrapped.
		r := add(t, c, uint64(300+i), name, false)
		if len(r.Faults) != 0 {
			t.Errorf("%s: unexpected faults %v", name, types(r.Faults))
		}
	}
}

// wrapb_0 ends on DTS 2^33-3003 and wrapb_1 starts on DTS 0: the wrap
// falls exactly between the segments, so only the modular gap is one frame.
func TestPTSWrapOnSegmentBoundaryIsNotAFault(t *testing.T) {
	first := mustAnalyze(t, tstest.Load(t, "wrapb_0.ts"))
	second := mustAnalyze(t, tstest.Load(t, "wrapb_1.ts"))
	if first.Video.LastDTS() != 8589931589 || second.Video.FirstDTS() != 0 {
		t.Fatalf("fixture boundary: last=%d first=%d", first.Video.LastDTS(), second.Video.FirstDTS())
	}
	c := NewChain(DefaultThresholds())
	c.Add(Input{Seq: 400, Segment: first})
	if r := c.Add(Input{Seq: 401, Segment: second}); len(r.Faults) != 0 {
		t.Errorf("faults across a wrap on the boundary: %v", types(r.Faults))
	}
}

// jump_1 is cont_1 moved 10 s later: video, audio and PCR all jump.
func TestTimestampJumpWithoutDiscontinuityIsFlagged(t *testing.T) {
	c := NewChain(DefaultThresholds())
	add(t, c, 200, "cont_0.ts", false)
	r := add(t, c, 201, "jump_1.ts", false)

	v := find(t, r.Faults, FaultVideoDTSGap)
	if v.Seq != 201 || v.PrevSeq == nil || *v.PrevSeq != 200 {
		t.Errorf("video fault seq=%d prev=%v", v.Seq, v.PrevSeq)
	}
	if got := value(t, v, "prev_last_dts"); got != 945045 {
		t.Errorf("prev_last_dts = %v", got)
	}
	if got := value(t, v, "first_dts"); got != 1848048 {
		t.Errorf("first_dts = %v", got)
	}
	if got := value(t, v, "deviation_ms"); !near(got, 10000) {
		t.Errorf("video deviation_ms = %v, want 10000", got)
	}

	a := find(t, r.Faults, FaultAudioPTSGap)
	if got := value(t, a, "prev_end_pts"); got != 957366 {
		t.Errorf("prev_end_pts = %v", got)
	}
	if got := value(t, a, "first_pts"); got != 1857366 {
		t.Errorf("first_pts = %v", got)
	}
	if got := value(t, a, "deviation_ms"); !near(got, 10000) {
		t.Errorf("audio deviation_ms = %v, want 10000", got)
	}

	// The PCR moved with the PTS, so the PTS-PCR offset did not jump.
	if slices.Contains(types(r.Faults), FaultPTSPCRJump) {
		t.Errorf("unexpected %s", FaultPTSPCRJump)
	}
}

func TestDiscontinuityTagSuppressesContinuityChecks(t *testing.T) {
	c := NewChain(DefaultThresholds())
	add(t, c, 200, "cont_0.ts", false)
	r := add(t, c, 201, "jump_1.ts", true)
	if len(r.Faults) != 0 {
		t.Errorf("faults across a tagged discontinuity: %v", types(r.Faults))
	}
}

// cont_0 and cont_2 are two segments apart, so their timestamps don't
// continue. With 101 missing, that must be a monitor gap, not a fault.
func TestSkippedSegmentIsMonitorGapNotFault(t *testing.T) {
	c := NewChain(DefaultThresholds())
	add(t, c, 100, "cont_0.ts", false)
	r := add(t, c, 102, "cont_2.ts", false)
	if len(r.Faults) != 0 {
		t.Errorf("faults across a skipped segment: %v", types(r.Faults))
	}
	if r.Gap == nil || r.Gap.From != 101 || r.Gap.To != 101 {
		t.Errorf("gap = %+v, want 101..101", r.Gap)
	}
}

func TestUnparseableSegmentBreaksChainWithoutGap(t *testing.T) {
	c := NewChain(DefaultThresholds())
	add(t, c, 100, "cont_0.ts", false)
	c.Break(101)
	r := add(t, c, 102, "cont_2.ts", false)
	if len(r.Faults) != 0 || r.Gap != nil {
		t.Errorf("faults %v gap %+v, want none", types(r.Faults), r.Gap)
	}
}

func TestSequenceGoingBackwardsIsNotCompared(t *testing.T) {
	c := NewChain(DefaultThresholds())
	add(t, c, 500, "cont_1.ts", false)
	r := add(t, c, 7, "cont_2.ts", false)
	if len(r.Faults) != 0 || r.Gap != nil {
		t.Errorf("faults %v gap %+v, want none", types(r.Faults), r.Gap)
	}
}

// Frame 7 repeats frame 6's DTS (918018).
func TestNonIncreasingDTSIsFlagged(t *testing.T) {
	r := add(t, NewChain(DefaultThresholds()), 1, "dts_repeat.ts", false)
	f := find(t, r.Faults, FaultVideoDTSNotIncreasing)
	if value(t, f, "index") != 7 || value(t, f, "dts") != 918018 || value(t, f, "prev_dts") != 918018 {
		t.Errorf("values = %v", f.Values)
	}
}

// The PCR runs 45000 ticks ahead of DTS, so all 16 PTS are behind it; the
// worst is a B frame at PTS-DTS = 3003, i.e. 3003-45000 = -41997 ticks.
func TestPTSBehindPCRIsFlagged(t *testing.T) {
	r := add(t, NewChain(DefaultThresholds()), 1, "pcr_behind.ts", false)
	f := find(t, r.Faults, FaultPTSBehindPCR)
	if value(t, f, "count") != 16 || value(t, f, "index") != 0 {
		t.Errorf("values = %v", f.Values)
	}
	if got := value(t, f, "worst_offset_ms"); !near(got, -466.633) {
		t.Errorf("worst_offset_ms = %v, want -466.633", got)
	}
}

// From frame 8 the PCR lead drops from 63000 to 9000 ticks: frame 7 (P,
// PTS-DTS 12012) sits 75012 ahead of its PCR, frame 8 (B, 3003) only 12003.
func TestPTSPCROffsetJumpIsFlagged(t *testing.T) {
	r := add(t, NewChain(DefaultThresholds()), 1, "pcr_jump.ts", false)
	f := find(t, r.Faults, FaultPTSPCRJump)
	if value(t, f, "index") != 8 || value(t, f, "count") != 1 {
		t.Errorf("values = %v", f.Values)
	}
	if got := value(t, f, "jump_ms"); !near(got, -700.1) {
		t.Errorf("jump_ms = %v, want -700.1", got)
	}
	if slices.Contains(types(r.Faults), FaultPTSBehindPCR) {
		t.Error("PTS is still ahead of PCR; no pts_behind_pcr expected")
	}
}

// The next segment's PCR lead is 9000 instead of 63000: cont_0 ends on a B
// frame 66003 ticks ahead of its PCR, the next starts on an I frame
// 6006+9000 = 15006 ahead.
func TestPTSPCROffsetJumpAcrossSegmentBoundary(t *testing.T) {
	c := NewChain(DefaultThresholds())
	add(t, c, 1, "cont_0.ts", false)
	next := tstest.Base
	next.PCRLead = 9000
	r := c.Add(Input{Seq: 2, Segment: mustAnalyze(t, next.Segment(1).Bytes())})
	f := find(t, r.Faults, FaultPTSPCRJump)
	if f.PrevSeq == nil || *f.PrevSeq != 1 {
		t.Errorf("prev seq = %v, want 1", f.PrevSeq)
	}
	if got := value(t, f, "jump_ms"); !near(got, -566.633) {
		t.Errorf("jump_ms = %v, want -566.633", got)
	}
}

func TestDurationMismatchIsFlagged(t *testing.T) {
	seg := mustAnalyze(t, tstest.Load(t, "cont_0.ts")) // 0.534 s of media

	r := NewChain(DefaultThresholds()).Add(Input{Seq: 1, ExtInf: 1.0, Segment: seg})
	f := find(t, r.Faults, FaultDurationMismatch)
	if got := value(t, f, "actual_s"); !near(got, 0.534) {
		t.Errorf("actual_s = %v", got)
	}

	r = NewChain(DefaultThresholds()).Add(Input{Seq: 1, ExtInf: 0.534, Segment: seg})
	if len(r.Faults) != 0 {
		t.Errorf("faults with matching EXTINF: %v", types(r.Faults))
	}
}

// Baseline from cont_0..2 is the median of -1440, -3312, -1344 = -1440
// ticks (-16 ms). avshift is segment 3 with audio 200 ms late: its offset is
// 1050150-1071366 = -21216 ticks (-235.733 ms), 219.733 ms off the baseline.
func TestAVOffsetDriftIsFlagged(t *testing.T) {
	th := DefaultThresholds()
	th.BaselineSegments = 3
	c := NewChain(th)
	for i, name := range []string{"cont_0.ts", "cont_1.ts", "cont_2.ts"} {
		if r := add(t, c, uint64(i), name, false); len(r.Faults) != 0 {
			t.Fatalf("%s: faults %v", name, types(r.Faults))
		}
	}
	r := add(t, c, 3, "avshift.ts", false)
	f := find(t, r.Faults, FaultAVOffset)
	if got := value(t, f, "baseline_ms"); !near(got, -16) {
		t.Errorf("baseline_ms = %v, want -16", got)
	}
	if got := value(t, f, "deviation_ms"); !near(got, -219.733) {
		t.Errorf("deviation_ms = %v, want -219.733", got)
	}
}

// From segment 3 on, every segment's audio starts 200 ms after its video
// (offsets -19152..-21216 ticks against a -1440 baseline). After
// RebaselineAfter such segments the baseline moves there and the flagging
// stops.
func TestAVBaselineMovesAfterSustainedShift(t *testing.T) {
	th := DefaultThresholds()
	th.BaselineSegments = 3
	th.RebaselineAfter = 4
	c := NewChain(th)

	var avFaults []uint64
	for k := range 12 {
		seg := tstest.Base.Segment(k)
		if k >= 3 {
			for i := range seg.Audio {
				seg.Audio[i].PTS += 18000
			}
		}
		r := c.Add(Input{Seq: uint64(k), Segment: mustAnalyze(t, seg.Bytes())})
		if slices.Contains(types(r.Faults), FaultAVOffset) {
			avFaults = append(avFaults, uint64(k))
		}
	}
	if want := []uint64{3, 4, 5, 6}; !slices.Equal(avFaults, want) {
		t.Errorf("av_offset faults on segments %v, want %v", avFaults, want)
	}
	if base, ok := c.Baseline(); !ok || base > -19000 || base < -21300 {
		t.Errorf("baseline = %d (%v), want it moved into -21216..-19152", base, ok)
	}
}

func eventTypes(es []Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

func findEvent(t *testing.T, es []Event, typ string) Event {
	t.Helper()
	for _, e := range es {
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("no %s event in %v", typ, eventTypes(es))
	return Event{}
}

// Removing frame 9 leaves a 6006-tick DTS step: one missing frame slot,
// 33.367 ms. ±1-tick jitter (3002/3004 steps) is not a gap.
func TestFrameGapIsAnEventNotAFault(t *testing.T) {
	s := tstest.Base.Segment(0)
	s.Video = append(s.Video[:9:9], s.Video[10:]...)
	r := NewChain(DefaultThresholds()).Add(Input{Seq: 1, Segment: mustAnalyze(t, s.Bytes())})
	if len(r.Faults) != 0 {
		t.Errorf("faults: %v", types(r.Faults))
	}
	e := findEvent(t, r.Events, EventFrameGap)
	if e.Count != 1 || !near(e.GapMs, 33.367) {
		t.Errorf("frame gap = %+v, want 1 gap of 33.367 ms", e)
	}

	jitter := tstest.Base.Segment(0)
	for i := 1; i < len(jitter.Video); i += 2 {
		jitter.Video[i].DTS--
	}
	r = NewChain(DefaultThresholds()).Add(Input{Seq: 1, Segment: mustAnalyze(t, jitter.Bytes())})
	if len(r.Events) != 0 {
		t.Errorf("events on 1-tick jitter: %v", eventTypes(r.Events))
	}
}

// From PES 5 on, audio is 80 ticks early: one step of 3760 where the
// 2-frame PES lasts 3840. Net re-timing -80 ticks = -0.889 ms.
func TestAudioRetimingIsAnEvent(t *testing.T) {
	s := tstest.Base.Segment(0)
	for i := 5; i < len(s.Audio); i++ {
		s.Audio[i].PTS -= 80
	}
	r := NewChain(DefaultThresholds()).Add(Input{Seq: 1, Segment: mustAnalyze(t, s.Bytes())})
	e := findEvent(t, r.Events, EventAudioRetimed)
	if e.Count != 1 || !near(e.GapMs, -0.889) {
		t.Errorf("audio re-timing = %+v, want 1 step, -0.889 ms net", e)
	}
	if len(r.Faults) != 0 {
		t.Errorf("faults: %v", types(r.Faults))
	}
}

// An audio gap between segments of at most audio_gap_fault_frames AAC
// frames (1.5: 2880 ticks, 32 ms at 48 kHz) is an event; anything larger is
// still an audio_pts_gap fault.
func TestAudioGapUpToOneAndAHalfAACFramesIsAnEvent(t *testing.T) {
	for _, tc := range []struct {
		shift     uint64
		wantEvent bool
		gapMs     float64
	}{
		{1906, true, 21.178},  // like channel10: just under one frame
		{1429, true, 15.878},  // like channel7
		{2500, true, 27.778},  // one missing frame plus jitter
		{3000, false, 33.333}, // more than 1.5 frames
	} {
		c := NewChain(DefaultThresholds())
		add(t, c, 1, "cont_0.ts", false)
		next := tstest.Base.Segment(1)
		for i := range next.Audio {
			next.Audio[i].PTS += tc.shift
		}
		r := c.Add(Input{Seq: 2, Segment: mustAnalyze(t, next.Bytes())})
		hasFault := slices.Contains(types(r.Faults), FaultAudioPTSGap)
		if tc.wantEvent {
			if hasFault {
				t.Errorf("shift %d: audio_pts_gap fault, want only an event", tc.shift)
			}
			if e := findEvent(t, r.Events, EventAudioGap); !near(e.GapMs, tc.gapMs) {
				t.Errorf("shift %d: gap %v ms, want %v", tc.shift, e.GapMs, tc.gapMs)
			}
		} else if !hasFault || slices.Contains(eventTypes(r.Events), EventAudioGap) {
			t.Errorf("shift %d: faults %v events %v, want the fault only", tc.shift, types(r.Faults), eventTypes(r.Events))
		}
	}
}

// After five 16-frame segments, a 13-frame one is odd: 3 frames short,
// -100.1 ms. There is no nominal length before five segments.
func TestOddLengthSegmentIsAnEvent(t *testing.T) {
	c := NewChain(DefaultThresholds())
	short := func(k int) *Segment {
		s := tstest.Base.Segment(k)
		s.Video = s.Video[:13]
		return mustAnalyze(t, s.Bytes())
	}
	if r := c.Add(Input{Seq: 0, Segment: short(0)}); slices.Contains(eventTypes(r.Events), EventOddLength) {
		t.Error("odd_length with no history")
	}
	for k := 1; k <= 5; k++ {
		c.Add(Input{Seq: uint64(k), Segment: mustAnalyze(t, tstest.Base.Segment(k).Bytes())})
	}
	r := c.Add(Input{Seq: 6, Segment: short(6)})
	e := findEvent(t, r.Events, EventOddLength)
	if !near(e.GapMs, -100.1) {
		t.Errorf("odd length = %+v, want -100.1 ms", e)
	}
}
