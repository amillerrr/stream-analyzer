package analysis

import (
	"math"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func mustAnalyze(t *testing.T, data []byte) *Segment {
	t.Helper()
	seg, err := Analyze(data)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return seg
}

// cont_0: 16 frames from DTS 900000 (3003 ticks each, IBBP so the first
// PTS is DTS+6006), 13 audio PES of 2 AAC frames from PTS 907446, PCR 63000
// behind DTS.
func TestAnalyzeExtractsSegmentTiming(t *testing.T) {
	seg := mustAnalyze(t, tstest.Load(t, "cont_0.ts"))

	if seg.Video == nil || seg.Audio == nil {
		t.Fatalf("tracks: video=%v audio=%v", seg.Video, seg.Audio)
	}
	v, a := seg.Video, seg.Audio
	if v.PID != 0x100 || v.StreamType != 0x1B || len(v.AUs) != 16 {
		t.Errorf("video pid=%#x type=%#x aus=%d", v.PID, v.StreamType, len(v.AUs))
	}
	if v.FirstDTS() != 900000 || v.LastDTS() != 945045 || v.MinPTS() != 906006 {
		t.Errorf("video first dts=%d last dts=%d min pts=%d", v.FirstDTS(), v.LastDTS(), v.MinPTS())
	}
	if v.FrameTicks != 3003 {
		t.Errorf("frame ticks = %d, want 3003", v.FrameTicks)
	}
	if first := v.AUs[0]; !first.HasPCR || first.PCR != 837000 {
		t.Errorf("first AU PCR = %d (has=%v), want 837000", first.PCR, first.HasPCR)
	}
	if a.PID != 0x101 || len(a.PES) != 13 || a.SampleRate != 48000 {
		t.Errorf("audio pid=%#x pes=%d rate=%d", a.PID, len(a.PES), a.SampleRate)
	}
	if a.FirstPTS() != 907446 || a.End() != 957366 {
		t.Errorf("audio first=%d end=%d, want 907446 957366", a.FirstPTS(), a.End())
	}
	if a.PES[0].Frames != 2 || a.PES[0].Ticks != 3840 {
		t.Errorf("audio PES frames=%d ticks=%d, want 2 3840", a.PES[0].Frames, a.PES[0].Ticks)
	}
	if seg.PCRPID != 0x100 || seg.PCRs != 16 {
		t.Errorf("pcr pid=%#x count=%d", seg.PCRPID, seg.PCRs)
	}
	if d, ok := seg.Duration(); !ok || !near(d, 48048.0/90000) {
		t.Errorf("duration = %v (%v), want %v", d, ok, 48048.0/90000)
	}
	if off, ok := seg.AVOffset(); !ok || off != -1440 {
		t.Errorf("A/V offset = %d (%v), want -1440", off, ok)
	}
}

// wrap_0 starts 5 frames before 2^33, so DTS, PTS and audio PTS all wrap
// inside the segment.
func TestAnalyzeAcrossPTSWrap(t *testing.T) {
	seg := mustAnalyze(t, tstest.Load(t, "wrap_0.ts"))
	v, a := seg.Video, seg.Audio
	if v.FirstDTS() != 8589919577 || v.LastDTS() != 30030 {
		t.Errorf("video first=%d last=%d, want 8589919577 30030", v.FirstDTS(), v.LastDTS())
	}
	if v.MinPTS() != 8589925583 {
		t.Errorf("min PTS = %d, want 8589925583", v.MinPTS())
	}
	if a.FirstPTS() != 8589927023 || a.End() != 42351 {
		t.Errorf("audio first=%d end=%d, want 8589927023 42351", a.FirstPTS(), a.End())
	}
	if d, ok := seg.Duration(); !ok || !near(d, 48048.0/90000) {
		t.Errorf("duration = %v, want %v", d, 48048.0/90000)
	}
	if off, ok := seg.AVOffset(); !ok || off != -1440 {
		t.Errorf("A/V offset = %d, want -1440", off)
	}
}

func TestAnalyzeRejectsNonTransportStream(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty": nil,
		"html":  []byte("<html><body>502 Bad Gateway</body></html>" + string(make([]byte, 400))),
	} {
		if _, err := Analyze(data); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSummaryReportsDerivedTiming(t *testing.T) {
	s := mustAnalyze(t, tstest.Load(t, "cont_0.ts")).Summary()

	if s.Video == nil || s.Audio == nil || s.AVOffsetMs == nil {
		t.Fatalf("summary missing parts: %+v", s)
	}
	v, a := s.Video, s.Audio
	if v.Frames != 16 || v.FirstDTS != 900000 || v.LastDTS != 945045 || v.MinPTS != 906006 || v.MaxPTS != 951051 {
		t.Errorf("video = %+v", v)
	}
	if !near(v.FrameMs, 33.367) {
		t.Errorf("frame_ms = %v", v.FrameMs)
	}
	// PTS-PCR: B frames sit 3003+63000 ahead (733.367 ms), P frames 12012+63000 (833.467 ms).
	if v.PTSPCRMinMs == nil || !near(*v.PTSPCRMinMs, 733.367) || !near(*v.PTSPCRMaxMs, 833.467) {
		t.Errorf("pts-pcr range = %v..%v", v.PTSPCRMinMs, v.PTSPCRMaxMs)
	}
	if v.FirstPCR == nil || *v.FirstPCR != 837000 || *v.LastPCR != 882045 {
		t.Errorf("pcr range = %v..%v", v.FirstPCR, v.LastPCR)
	}
	// DTS-PCR: the PCR is exactly 63000 ticks behind every DTS.
	if v.DTSPCRMinMs == nil || !near(*v.DTSPCRMinMs, 700) || !near(*v.DTSPCRMaxMs, 700) {
		t.Errorf("dts-pcr range = %v..%v, want 700..700", v.DTSPCRMinMs, v.DTSPCRMaxMs)
	}
	if a.PES != 13 || a.FirstPTS != 907446 || a.LastPTS != 953526 || a.EndPTS != 957366 || !near(a.PESMs, 42.667) {
		t.Errorf("audio = %+v", a)
	}
	if !near(*s.AVOffsetMs, -16) || !near(s.DurationS, 0.534) {
		t.Errorf("av offset %v duration %v", *s.AVOffsetMs, s.DurationS)
	}
}

// pcr_behind runs the PCR 45000 ticks ahead of every DTS: DTS-PCR is -500 ms
// on every frame, while PTS-PCR ranges from B frames (3003-45000) to P
// frames (12012-45000).
func TestSummaryDTSPCRWhenPCRIsAhead(t *testing.T) {
	v := mustAnalyze(t, tstest.Load(t, "pcr_behind.ts")).Summary().Video
	if v.DTSPCRMinMs == nil || !near(*v.DTSPCRMinMs, -500) || !near(*v.DTSPCRMaxMs, -500) {
		t.Errorf("dts-pcr range = %v..%v, want -500..-500", v.DTSPCRMinMs, v.DTSPCRMaxMs)
	}
	if !near(*v.PTSPCRMinMs, -466.633) || !near(*v.PTSPCRMaxMs, -366.533) {
		t.Errorf("pts-pcr range = %v..%v", *v.PTSPCRMinMs, *v.PTSPCRMaxMs)
	}
}
