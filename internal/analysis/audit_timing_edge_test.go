package analysis

// Scratch audit tests (timing reviewer): PES, ADTS, PCR and packet edge
// cases built packet by packet.

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// ---- low-level muxer --------------------------------------------------------

type mux struct {
	out []byte
	cc  map[uint16]uint8
}

func newMux() *mux { return &mux{cc: map[uint16]uint8{}} }

type pktOpt struct {
	pcr     *uint64
	pcrExt  uint16
	disc    bool
	afExtra int // extra adaptation-field stuffing bytes (forces a short payload)
	tei     bool
}

// packet writes one packet and returns the payload bytes consumed.
func (m *mux) packet(pid uint16, pusi bool, o pktOpt, payload []byte) int {
	var af []byte
	haveAF := o.pcr != nil || o.afExtra > 0 || o.disc
	if haveAF {
		fl := byte(0)
		if o.disc {
			fl |= 0x80
		}
		if o.pcr != nil {
			fl |= 0x10
		}
		af = append(af, fl)
		if o.pcr != nil {
			b := *o.pcr
			af = append(af, byte(b>>25), byte(b>>17), byte(b>>9), byte(b>>1), byte(b<<7)|0x7E|byte(o.pcrExt>>8)&1, byte(o.pcrExt))
		}
		af = append(af, bytes.Repeat([]byte{0xFF}, o.afExtra)...)
	}
	space := 184
	if af != nil {
		space -= 1 + len(af)
	}
	n := min(len(payload), space)
	if stuff := space - n; stuff > 0 {
		switch {
		case af != nil:
			af = append(af, bytes.Repeat([]byte{0xFF}, stuff)...)
		case stuff == 1:
			af = []byte{}
		default:
			af = append([]byte{0x00}, bytes.Repeat([]byte{0xFF}, stuff-2)...)
		}
	}
	b1 := byte(pid>>8) & 0x1F
	if pusi {
		b1 |= 0x40
	}
	if o.tei {
		b1 |= 0x80
	}
	afc := byte(0x10)
	if af != nil {
		afc = 0x30
		if n == 0 {
			afc = 0x20
		}
	}
	cc := m.cc[pid]
	m.cc[pid] = (cc + 1) & 0xF
	pkt := []byte{0x47, b1, byte(pid), afc | cc}
	if af != nil {
		pkt = append(pkt, byte(len(af)))
		pkt = append(pkt, af...)
	}
	pkt = append(pkt, payload[:n]...)
	if len(pkt) != 188 {
		panic(len(pkt))
	}
	m.out = append(m.out, pkt...)
	return n
}

func (m *mux) pes(pid uint16, first pktOpt, data []byte) {
	o := first
	pusi := true
	for len(data) > 0 {
		n := m.packet(pid, pusi, o, data)
		data = data[n:]
		pusi = false
		o = pktOpt{}
	}
}

func tsBytes(tag byte, v uint64) []byte {
	return []byte{tag<<4 | byte(v>>29)&0x0E | 1, byte(v >> 22), byte(v>>14)&0xFE | 1, byte(v >> 7), byte(v<<1)&0xFE | 1}
}

// pesData builds a PES. flags: 0 none, 1 forbidden '01', 2 PTS, 3 PTS+DTS.
func pesData(sid byte, flags byte, pts, dts uint64, stuffing int, es []byte) []byte {
	var opt []byte
	switch flags {
	case 2:
		opt = tsBytes(2, pts)
	case 3:
		opt = append(tsBytes(3, pts), tsBytes(1, dts)...)
	case 1:
		opt = tsBytes(1, dts)
	}
	opt = append(opt, bytes.Repeat([]byte{0xFF}, stuffing)...)
	h := []byte{0x80, flags << 6, byte(len(opt))}
	h = append(h, opt...)
	l := 0
	if sid != 0xE0 {
		l = len(h) + len(es)
	}
	d := []byte{0, 0, 1, sid, byte(l >> 8), byte(l)}
	d = append(d, h...)
	return append(d, es...)
}

func section(body []byte) []byte {
	secLen := len(body) - 3 + 4
	body[1] = 0xB0 | byte(secLen>>8)&0x0F
	body[2] = byte(secLen)
	return binary.BigEndian.AppendUint32(body, ts.CRC32(body))
}

type es struct {
	pid uint16
	st  byte
}

func (m *mux) psi(pcrPID uint16, streams ...es) {
	pat := section([]byte{0, 0xB0, 0, 0, 1, 0xC1, 0, 0, 0, 1, 0xE0 | 0x10, 0x00})
	m.packet(0, true, pktOpt{}, append(append([]byte{0}, pat...), bytes.Repeat([]byte{0xFF}, 184-1-len(pat))...))
	body := []byte{2, 0xB0, 0, 0, 1, 0xC1, 0, 0, 0xE0 | byte(pcrPID>>8), byte(pcrPID), 0xF0, 0}
	for _, s := range streams {
		body = append(body, s.st, 0xE0|byte(s.pid>>8), byte(s.pid), 0xF0, 0)
	}
	pmt := section(body)
	m.packet(0x1000, true, pktOpt{}, append(append([]byte{0}, pmt...), bytes.Repeat([]byte{0xFF}, 184-1-len(pmt))...))
}

// adts builds n ADTS frames at sampling index sfi; crc adds the 2-byte CRC
// (protection_absent=0); blocks is raw data blocks per frame.
func adts(n, sfi int, crc bool, blocks int) []byte {
	var b []byte
	for range n {
		hl := 7
		pa := byte(1)
		if crc {
			hl, pa = 9, 0
		}
		fl := hl + 20
		b = append(b, 0xFF, 0xF0|pa, 0x40|byte(sfi)<<2, 0x80|byte(fl>>11)&3, byte(fl>>3), byte(fl&7)<<5|0x1F, 0xFC|byte(blocks-1))
		if crc {
			b = append(b, 0x12, 0x34)
		}
		b = append(b, make([]byte, 20)...)
	}
	return b
}

const (
	vpid = 0x100
	apid = 0x101
)

// simple segment: n video frames (IPBB-less: PTS = DTS + 2 frames), PCR on
// every video PES at DTS-lead, audio PES of framesPerPES AAC frames.
type segSpec struct {
	start       uint64
	n           int
	lead        uint64
	audioStart  uint64
	audioPES    int
	framesPer   int
	dropVPTS    map[int]bool // video PES without PTS
	dropAPTS    map[int]bool // audio PES without PTS
	videoFill   int
	sfi         int
	audioDurTks int64 // override audio PES spacing
}

func buildSeg(s segSpec) []byte {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B}, es{apid, 0x0F})
	if s.sfi == 0 {
		s.sfi = 3
	}
	if s.framesPer == 0 {
		s.framesPer = 1
	}
	fill := s.videoFill
	if fill == 0 {
		fill = 2000
	}
	aStep := s.audioDurTks
	if aStep == 0 {
		aStep = int64(1920 * s.framesPer)
	}
	ai := 0
	for i := range s.n {
		dts := ts.Add(s.start, int64(i)*3003)
		pcr := ts.Add(dts, -int64(s.lead))
		flags := byte(3)
		if s.dropVPTS[i] {
			flags = 0
		}
		m.pes(vpid, pktOpt{pcr: &pcr}, pesData(0xE0, flags, ts.Add(dts, 6006), dts, 0, bytes.Repeat([]byte{0xA5}, fill)))
		// audio PES whose PTS falls before the next frame's DTS
		for ai < s.audioPES && ts.Diff(ts.Add(s.audioStart, int64(ai)*aStep), ts.Add(dts, 3003)) < 0 {
			fl := byte(2)
			if s.dropAPTS[ai] {
				fl = 0
			}
			m.pes(apid, pktOpt{}, pesData(0xC0, fl, ts.Add(s.audioStart, int64(ai)*aStep), 0, 0, adts(s.framesPer, s.sfi, false, 1)))
			ai++
		}
	}
	for ; ai < s.audioPES; ai++ {
		fl := byte(2)
		if s.dropAPTS[ai] {
			fl = 0
		}
		m.pes(apid, pktOpt{}, pesData(0xC0, fl, ts.Add(s.audioStart, int64(ai)*aStep), 0, 0, adts(s.framesPer, s.sfi, false, 1)))
	}
	return m.out
}

func auditAnalyze(t *testing.T, b []byte) *Segment {
	t.Helper()
	s, err := Analyze(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// two consecutive 60-frame segments with 2-frame audio PES; the last audio
// PES of the first one carries no PTS (legal: 13818-1 only requires a PTS
// every 700 ms).
func TestAuditAudioPESWithoutPTS(t *testing.T) {
	const n = 60
	frames := 2
	pesTicks := int64(1920 * frames)
	segTicks := int64(n * 3003)
	aPES := int(segTicks/pesTicks) + 1 // 47 PES: the audio covers the video
	s0 := segSpec{start: 900000, n: n, lead: 800, audioStart: 900000 + 6006, audioPES: aPES, framesPer: frames,
		dropAPTS: map[int]bool{aPES - 1: true}}
	s1 := segSpec{start: 900000 + uint64(segTicks), n: n, lead: 800, audioStart: 900000 + 6006 + uint64(int64(aPES)*pesTicks),
		audioPES: aPES, framesPer: frames}
	a := auditAnalyze(t, buildSeg(s0))
	b := auditAnalyze(t, buildSeg(s1))
	t.Logf("seg0: audio PES parsed %d of %d; End=%d; true end=%d", len(a.Audio.PES), aPES, a.Audio.End(), s1.audioStart)
	c := NewChain(DefaultThresholds())
	r1 := c.Add(Input{Seq: 1, Segment: a})
	r := c.Add(Input{Seq: 2, Segment: b})
	r.Faults = append(r1.Faults, r.Faults...) // a lost PES shows on its own segment
	for _, f := range r.Faults {
		t.Logf("FAULT %s: %s", f.Type, f.Message)
	}
	for _, e := range r.Events {
		t.Logf("EVENT %s: %s", e.Type, e.Detail)
	}
	// And in the middle of a segment:
	s2 := s1
	s2.dropAPTS = map[int]bool{10: true}
	mid := auditAnalyze(t, buildSeg(s2))
	if e, ok := firstEvent(audioSteps(3, mid.Audio, DefaultThresholds())); ok {
		t.Logf("mid-segment PES without PTS -> EVENT %s: %s", e.Type, e.Detail)
	}
	if len(r.Faults) > 0 {
		t.Errorf("a stream with a legal PTS-less audio PES raised %d fault(s)", len(r.Faults))
	}
}

func TestAuditVideoPESWithoutPTS(t *testing.T) {
	// PTS on every other video PES (legal), 60 frames.
	drop := map[int]bool{}
	for i := 1; i < 60; i += 2 {
		drop[i] = true
	}
	s := auditAnalyze(t, buildSeg(segSpec{start: 900000, n: 60, lead: 800, audioStart: 906006, audioPES: 90, dropVPTS: drop}))
	d, _ := s.Duration()
	t.Logf("frames=%d (true 60) FrameTicks=%d duration=%.3f (true %.3f)", len(s.Video.AUs), s.Video.FrameTicks, d, 60*3003/90000.0)
	// Only one PES without PTS: frame count 59 and a frame_gap event.
	one := auditAnalyze(t, buildSeg(segSpec{start: 900000, n: 60, lead: 800, audioStart: 906006, audioPES: 90, dropVPTS: map[int]bool{30: true}}))
	e, ok := firstEvent(videoSteps(1, one.Video, 0, DefaultThresholds()))
	sum1 := one.Summary()
	t.Logf("one PTS-less PES: frames=%d gap event=%v %+v; frame_ms=%v", len(one.Video.AUs), ok, e, sum1.Video.FrameMs)
	if len(one.Video.AUs) != 60 {
		t.Errorf("video PES without PTS is dropped: %d frames of 60", len(one.Video.AUs))
	}
}

// A video PES whose first packet carries so much adaptation field that the
// PES header (19 bytes with PTS+DTS) continues in the next packet.
func TestAuditVideoPESHeaderSplit(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B}, es{apid, 0x0F})
	for i := range 10 {
		dts := uint64(900000 + i*3003)
		pcr := dts - 800
		o := pktOpt{pcr: &pcr}
		if i == 5 {
			o.afExtra = 184 - 1 - 7 - 12 // leaves 12 payload bytes in the first packet
		}
		m.pes(vpid, o, pesData(0xE0, 3, dts+6006, dts, 0, bytes.Repeat([]byte{0xA5}, 3000)))
	}
	s := auditAnalyze(t, m.out)
	e, ok := firstEvent(videoSteps(1, s.Video, 0, DefaultThresholds()))
	t.Logf("frames=%d (true 10); frame_gap=%v %+v", len(s.Video.AUs), ok, e)
	if len(s.Video.AUs) != 10 {
		t.Errorf("PES header split across packets drops the AU: %d of 10 frames", len(s.Video.AUs))
	}
}

func TestAuditPTSDTSFlags01(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B})
	for i := range 5 {
		dts := uint64(900000 + i*3003)
		fl := byte(3)
		if i == 2 {
			fl = 1
		}
		m.pes(vpid, pktOpt{}, pesData(0xE0, fl, dts+6006, dts, 0, bytes.Repeat([]byte{0xA5}, 500)))
	}
	s := auditAnalyze(t, m.out)
	t.Logf("PTS_DTS_flags=01 on one PES: frames=%d of 5", len(s.Video.AUs))
}

// PES_header_data_length larger than the timestamps (stuffing in the PES
// header) must not shift the audio payload.
func TestAuditPESHeaderStuffing(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B}, es{apid, 0x0F})
	pcr := uint64(900000 - 800)
	m.pes(vpid, pktOpt{pcr: &pcr}, pesData(0xE0, 3, 906006, 900000, 7, bytes.Repeat([]byte{0xA5}, 300)))
	for i := range 3 {
		m.pes(apid, pktOpt{}, pesData(0xC0, 2, uint64(906006+i*1920), 0, 9, adts(1, 3, false, 1)))
	}
	s := auditAnalyze(t, m.out)
	for i, p := range s.Audio.PES {
		if p.Frames != 1 || p.Ticks != 1920 {
			t.Errorf("PES %d: frames=%d ticks=%d", i, p.Frames, p.Ticks)
		}
	}
}

func TestAuditADTSVariants(t *testing.T) {
	cases := []struct {
		name   string
		b      []byte
		frames int
		rate   int
	}{
		{"48k", adts(3, 3, false, 1), 3, 48000},
		{"48k CRC", adts(3, 3, true, 1), 3, 48000},
		{"44.1k", adts(1, 4, false, 1), 1, 44100},
		{"22.05k (HE-AAC core)", adts(1, 7, false, 1), 1, 22050},
		{"4 raw blocks", adts(2, 3, false, 4), 8, 48000},
		{"4 raw blocks + CRC", adts(2, 3, true, 4), 8, 48000},
	}
	for _, c := range cases {
		f, r := parseADTS(c.b)
		if f != c.frames || r != c.rate {
			t.Errorf("%s: got %d frames @%d, want %d @%d", c.name, f, r, c.frames, c.rate)
		}
	}
	// frame split across two PES: the first counts it, the second is unknown
	two := adts(2, 3, false, 1)
	f1, _ := parseADTS(two[:len(two)-10])
	f2, _ := parseADTS(two[len(two)-10:])
	t.Logf("ADTS frame split across PES: first PES counts %d, second %d (0 = unknown)", f1, f2)
	// integer truncation of per-PES ticks
	for _, rate := range []int{44100, 22050, 11025, 7350} {
		exact := 1024.0 * 90000 / float64(rate)
		t.Logf("rate %d: frameTicks=%d exact=%.4f error=%.4f ticks", rate, 1024*90000/rate, exact, exact-float64(1024*90000/rate))
	}
}

// AC-3 (0x81) or MPEG audio: parseADTS finds nothing, so End() uses the
// median PES spacing. A segment whose audio PES durations vary gets a wrong
// End() and the next segment raises a false audio_pts_gap.
func TestAuditNonAACAudioEnd(t *testing.T) {
	build := func(start uint64, ptss []uint64) []byte {
		m := newMux()
		m.psi(vpid, es{vpid, 0x1B}, es{apid, 0x81})
		pcr := start - 800
		m.pes(vpid, pktOpt{pcr: &pcr}, pesData(0xE0, 3, start+6006, start, 0, bytes.Repeat([]byte{0xA5}, 300)))
		for _, p := range ptss {
			m.pes(apid, pktOpt{}, pesData(0xBD, 2, p, 0, 0, bytes.Repeat([]byte{0x0B}, 200)))
		}
		return m.out
	}
	// AC-3 frames are 2880 ticks. PES 1..3 carry 1 frame, the last carries 3.
	p := []uint64{906006, 908886, 911766, 914646}
	a := auditAnalyze(t, build(900000, p))
	next := []uint64{914646 + 3*2880, 914646 + 4*2880}
	b := auditAnalyze(t, build(903003, next))
	if a.Audio == nil {
		t.Fatalf("no audio found: stream_type 0x81 with stream_id 0xBD")
	}
	t.Logf("AC-3: End=%d, true end=%d, LastTicks=%d frameTicks=%d", a.Audio.End(), next[0], a.Audio.LastTicks(), a.Audio.frameTicks())
	faults, events := compare(a, 1, Input{Seq: 2, Segment: b}, DefaultThresholds(), 0, 0)
	for _, f := range faults {
		t.Logf("FAULT %s: %s", f.Type, f.Message)
	}
	for _, e := range events {
		t.Logf("EVENT %s: %s", e.Type, e.Detail)
	}
	if len(faults) > 0 {
		t.Errorf("continuous AC-3 audio raised %d fault(s): End() uses the median PES spacing, not the last PES's frames", len(faults))
	}
	// single-PES segment: LastTicks is 0, End() == LastPTS
	one := auditAnalyze(t, build(900000, []uint64{906006}))
	b2 := auditAnalyze(t, build(903003, []uint64{906006 + 2880}))
	f2, e2 := compare(one, 1, Input{Seq: 2, Segment: b2}, DefaultThresholds(), 0, 0)
	t.Logf("single AC-3 PES: LastTicks=%d End=%d -> faults %d events %d", one.Audio.LastTicks(), one.Audio.End(), len(f2), len(e2))
	for _, f := range f2 {
		t.Logf("  FAULT %s: %s", f.Type, f.Message)
	}
	if len(f2) > 0 {
		t.Errorf("continuous single-PES AC-3 audio raised %d fault(s)", len(f2))
	}
}

// Two AAC PIDs: the lowest PID is monitored whatever the PMT order.
func TestAuditTwoAudioPIDs(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B}, es{0x105, 0x0F}, es{0x102, 0x0F})
	pcr := uint64(900000 - 800)
	m.pes(vpid, pktOpt{pcr: &pcr}, pesData(0xE0, 3, 906006, 900000, 0, bytes.Repeat([]byte{0xA5}, 300)))
	m.pes(0x105, pktOpt{}, pesData(0xC0, 2, 906006, 0, 0, adts(1, 3, false, 1)))
	m.pes(0x102, pktOpt{}, pesData(0xC1, 2, 999999, 0, 0, adts(1, 3, false, 1)))
	s := auditAnalyze(t, m.out)
	t.Logf("PMT order 0x105, 0x102 -> monitored audio PID 0x%x", s.Audio.PID)
}

// Resync: garbage longer than 16 packets drops the rest of the segment; a
// sync loss right before the final packet drops it.
func TestAuditResync(t *testing.T) {
	good := buildSeg(segSpec{start: 900000, n: 30, lead: 800, audioStart: 906006, audioPES: 40})
	ref := auditAnalyze(t, good)
	half := (len(good) / 188 / 2) * 188
	for _, junk := range []int{1, 100, 187, 188 * 15, 188*16 + 1, 188 * 20} {
		b := append(append(append([]byte(nil), good[:half]...), bytes.Repeat([]byte{0x00}, junk)...), good[half:]...)
		s, err := Analyze(b)
		if err != nil {
			t.Logf("junk %d: %v", junk, err)
			continue
		}
		t.Logf("junk %5d bytes mid-segment: packets %d/%d, frames %d/%d, audio PES %d/%d", junk, s.Packets, ref.Packets, len(s.Video.AUs), len(ref.Video.AUs), len(s.Audio.PES), len(ref.Audio.PES))
		if s.Packets < ref.Packets-1 {
			t.Errorf("junk %d bytes mid-segment: %d of %d packets analyzed, with no error", junk, s.Packets, ref.Packets)
		}
	}
	// junk before the final packet
	last := len(good) - 188
	b := append(append(append([]byte(nil), good[:last]...), 0x00, 0x00, 0x00), good[last:]...)
	s := auditAnalyze(t, b)
	t.Logf("3 junk bytes before the last packet: packets %d of %d", s.Packets, ref.Packets)
	// trailing partial packet
	s = auditAnalyze(t, append(append([]byte(nil), good...), good[:100]...))
	t.Logf("trailing 100-byte partial packet: packets %d (ref %d)", s.Packets, ref.Packets)
}

// A packet with transport_error_indicator set is used as if it were good.
func TestAuditTEI(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B})
	for i := range 6 {
		dts := uint64(900000 + i*3003)
		pcr := dts - 800
		o := pktOpt{pcr: &pcr}
		d := dts
		if i == 3 {
			o.tei = true
			d = dts ^ (1 << 20) // corrupted bits in a TEI packet
		}
		m.pes(vpid, o, pesData(0xE0, 3, d+6006, d, 0, bytes.Repeat([]byte{0xA5}, 400)))
	}
	s := auditAnalyze(t, m.out)
	faults := checkSegment(Input{Seq: 1, Segment: s}, DefaultThresholds())
	for _, f := range faults {
		t.Logf("TEI-corrupted PES -> FAULT %s: %s", f.Type, f.Message)
	}
	if e, ok := firstEvent(videoSteps(1, s.Video, 0, DefaultThresholds())); ok {
		t.Logf("TEI-corrupted PES -> EVENT %s: %s", e.Type, e.Detail)
	}
	if len(faults) > 0 {
		t.Errorf("a packet flagged transport_error_indicator was trusted: %d fault(s)", len(faults))
	}
}

// PCR on its own PID (PCR-only packets, no payload) and PCR extension.
func TestAuditPCRSeparatePID(t *testing.T) {
	m := newMux()
	m.psi(0x1FF, es{vpid, 0x1B})
	for i := range 6 {
		dts := uint64(900000 + i*3003)
		pcr := dts - 800
		m.packet(0x1FF, false, pktOpt{pcr: &pcr, pcrExt: 299}, nil)
		m.pes(vpid, pktOpt{}, pesData(0xE0, 3, dts+6006, dts, 0, bytes.Repeat([]byte{0xA5}, 400)))
	}
	s := auditAnalyze(t, m.out)
	sum := s.Summary()
	t.Logf("PCR PID 0x%x, pcrs %d, dts_pcr_min %.3f max %.3f, first_pcr %d", s.PCRPID, s.PCRs, *sum.Video.DTSPCRMinMs, *sum.Video.DTSPCRMaxMs, *sum.Video.FirstPCR)
	if s.PCRs != 6 || *sum.Video.DTSPCRMinMs != 8.889 {
		t.Errorf("unexpected")
	}
}

// last_pcr is the PCR before the last video PES start, not the segment's
// last PCR, when PCRs also appear inside a frame.
func TestAuditLastPCRSemantics(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B})
	var lastPCR uint64
	for i := range 4 {
		dts := uint64(900000 + i*3003)
		pcr := dts - 800
		data := pesData(0xE0, 3, dts+6006, dts, 0, bytes.Repeat([]byte{0xA5}, 2000))
		n := m.packet(vpid, true, pktOpt{pcr: &pcr}, data)
		data = data[n:]
		k := 0
		for len(data) > 0 {
			o := pktOpt{}
			if k == 5 {
				p2 := pcr + 1500
				o.pcr = &p2
				lastPCR = p2
			}
			data = data[m.packet(vpid, false, o, data):]
			k++
		}
	}
	s := auditAnalyze(t, m.out)
	sum := s.Summary()
	t.Logf("summary last_pcr=%d, actual last PCR in segment=%d (diff %d ticks)", *sum.Video.LastPCR, lastPCR, int64(lastPCR)-int64(*sum.Video.LastPCR))
}
