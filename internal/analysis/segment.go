// Package analysis extracts timing from MPEG-TS segments and checks it:
// A/V continuity between consecutive segments, DTS order, PTS against PCR,
// A/V start offset against a per-channel baseline and real duration against
// EXTINF. All timestamp arithmetic is modulo 2^33.
package analysis

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

var (
	// ErrNotTS means the bytes are not an MPEG transport stream.
	ErrNotTS = errors.New("not an MPEG transport stream")
	// ErrNoTimestamps means the segment parsed but has no PES timestamps.
	ErrNoTimestamps = errors.New("no audio or video timestamps")
)

// Segment is the timing extracted from one TS segment.
type Segment struct {
	Packets int
	PCRPID  uint16
	PCRs    int
	Video   *Video // nil when the segment has no video PES with timestamps
	Audio   *Audio // nil when the segment has no audio PES with timestamps
	// SkippedBytes were not TS packets (sync was lost and found again), and
	// TEIPackets carried transport_error_indicator: both were left out.
	SkippedBytes, TEIPackets int
}

// Video is the segment's video track in decode order.
type Video struct {
	PID        uint16
	StreamType uint8
	AUs        []AU
	FrameTicks int64 // the smallest common DTS step (see frameStep); 0 with fewer than two AUs
}

// AU is one video PES (one access unit).
type AU struct {
	PTS, DTS uint64 // DTS equals PTS when the PES carries no DTS
	PCR      uint64 // last PCR base at or before the packet that starts this PES
	HasPCR   bool
	// NoPTS: the PES carried no timestamps (legal; one is required only
	// every 700 ms). Its DTS is interpolated between its neighbours' and its
	// PTS is unknown, so PTS checks skip it.
	NoPTS bool
}

// Audio is the segment's audio track.
type Audio struct {
	PID        uint16
	StreamType uint8
	PES        []AudioPES
	SampleRate int // from the ADTS headers; 0 if unknown
}

// AudioPES is one audio PES.
type AudioPES struct {
	PTS    uint64
	Frames int   // ADTS frames in the PES; 0 if not ADTS
	Ticks  int64 // duration from the ADTS headers; 0 if unknown
}

// Analyze extracts the timing of one TS segment. It returns ErrNotTS when the
// bytes don't look like TS at all and ErrNoTimestamps when no PES carries a
// PTS.
func Analyze(data []byte) (*Segment, error) {
	// Pass 1: PAT and PMT, so the elementary streams are known before any PES
	// is classified, whatever order the packets come in.
	psi := ts.NewPSIBuilder()
	n, skipped, tei, err := eachPacket(data, func(_ int, p ts.Packet) {
		pmt, ok := psi.PMTPID()
		if p.HasPayload && (p.PID == ts.PIDPAT || ok && p.PID == pmt) {
			psi.OnPacket(p)
		}
	})
	if err != nil {
		return nil, err
	}

	seg := &Segment{Packets: n, SkippedBytes: skipped, TEIPackets: tei}
	v := &Video{}
	a := &Audio{}
	pcrKnown := false
	if prog := psi.Program(); prog.Version >= 0 {
		seg.PCRPID, pcrKnown = prog.PCRPID, true
		for _, pid := range slices.Sorted(maps.Keys(prog.Streams)) {
			st := prog.Streams[pid].StreamType
			switch {
			case v.PID == 0 && isVideoType(st):
				v.PID, v.StreamType = pid, st
			case a.PID == 0 && isAudioType(st):
				a.PID, a.StreamType = pid, st
			}
		}
	}

	// Pass 2: PCRs and PES timestamps in packet order.
	var lastPCR uint64
	var havePCR bool
	var acc audioAccumulator
	var vhead []byte // a video PES header that may continue in the next packet
	var vheadPCR uint64
	var vheadHasPCR, vpending bool
	_, _, _, _ = eachPacket(data, func(_ int, p ts.Packet) {
		if p.HasPCR {
			if !pcrKnown {
				// No PMT: take the first PID that carries a PCR.
				seg.PCRPID, pcrKnown = p.PID, true
			}
			if p.PID == seg.PCRPID {
				lastPCR, havePCR = p.PCRBase, true
				seg.PCRs++
			}
		}
		if !p.HasPayload || p.Scrambled {
			return
		}
		if p.PayloadUnitStartIndicator && (v.PID == 0 || a.PID == 0) {
			classifyByStreamID(p, v, a)
		}
		switch p.PID {
		case v.PID:
			switch {
			case p.PayloadUnitStartIndicator:
				vhead = append(vhead[:0], p.Payload...)
				vheadPCR, vheadHasPCR, vpending = lastPCR, havePCR, true
			case vpending:
				vhead = append(vhead, p.Payload...) // the header continues here
			default:
				return
			}
			h, err := ts.ParsePESHeader(vhead)
			if errors.Is(err, ts.ErrShortPESHeader) && len(vhead) < 64 {
				return // wait for the rest of the header
			}
			vpending = false
			if err != nil {
				return
			}
			au := AU{PTS: h.PTS, DTS: h.PTS, PCR: vheadPCR, HasPCR: vheadHasPCR, NoPTS: !h.HasPTS}
			if h.HasDTS {
				au.DTS = h.DTS
			}
			v.AUs = append(v.AUs, au)
		case a.PID:
			acc.add(p, a)
		}
	})
	acc.flush(a)
	v.AUs = interpolateDTS(v.AUs)

	if len(v.AUs) > 0 {
		v.FrameTicks = frameStep(len(v.AUs), func(i int) uint64 { return v.AUs[i].DTS })
		seg.Video = v
	}
	if len(a.PES) > 0 {
		seg.Audio = a
	}
	if seg.Video == nil && seg.Audio == nil {
		return nil, fmt.Errorf("%w in %d packets", ErrNoTimestamps, n)
	}
	return seg, nil
}

// eachPacket calls fn with the byte offset of every decodable packet. After
// lost sync it scans the rest of the data for the next packet, counting the
// bytes skipped; packets flagged transport_error_indicator are skipped and
// counted too.
func eachPacket(data []byte, fn func(off int, p ts.Packet)) (n, skipped, tei int, err error) {
	for off := 0; off+ts.PacketSize <= len(data); off += ts.PacketSize {
		if data[off] != ts.SyncByte {
			adj, ok := ts.Resync(data[off:])
			if !ok {
				skipped += len(data) - off
				break
			}
			skipped += adj
			off += adj
		}
		p, err := ts.Decode(data[off : off+ts.PacketSize])
		if err != nil {
			continue
		}
		if p.TransportError {
			tei++
			continue
		}
		n++
		fn(off, p)
	}
	if n == 0 {
		return 0, skipped, tei, fmt.Errorf("%w (%d bytes)", ErrNotTS, len(data))
	}
	return n, skipped, tei, nil
}

// interpolateDTS gives each AU without timestamps a DTS evenly between its
// neighbours', and those after the last timestamp a frame on each, at the
// step of the frames before. AUs that can't be placed (before the first
// timestamp, or after a lone one) are dropped.
func interpolateDTS(aus []AU) []AU {
	first := slices.IndexFunc(aus, func(au AU) bool { return !au.NoPTS })
	if first < 0 {
		return nil
	}
	aus = aus[first:]
	last := 0
	for i := 1; i < len(aus); i++ {
		if aus[i].NoPTS {
			continue
		}
		for j := last + 1; j < i; j++ {
			d := ts.Diff(aus[i].DTS, aus[last].DTS) * int64(j-last) / int64(i-last)
			aus[j].DTS = ts.Add(aus[last].DTS, d)
			aus[j].PTS = aus[j].DTS
		}
		last = i
	}
	if last == len(aus)-1 {
		return aus
	}
	step := frameStep(last+1, func(i int) uint64 { return aus[i].DTS })
	if step <= 0 {
		return aus[:last+1]
	}
	for j := last + 1; j < len(aus); j++ {
		aus[j].DTS = ts.Add(aus[last].DTS, step*int64(j-last))
		aus[j].PTS = aus[j].DTS
	}
	return aus
}

func isVideoType(st uint8) bool {
	switch st {
	case 0x01, 0x02, 0x10, 0x1B, 0x24, 0x33:
		return true
	}
	return false
}

func isAudioType(st uint8) bool {
	switch st {
	case 0x03, 0x04, 0x0F, 0x11, 0x1C, 0x81, 0x87:
		return true
	}
	return false
}

// classifyByStreamID assigns PIDs from PES stream_ids when the segment has
// no PMT.
func classifyByStreamID(p ts.Packet, v *Video, a *Audio) {
	b := p.Payload
	if len(b) < 4 || b[0] != 0 || b[1] != 0 || b[2] != 1 || p.PID == v.PID || p.PID == a.PID {
		return
	}
	switch id := b[3]; {
	case v.PID == 0 && id >= 0xE0 && id <= 0xEF:
		v.PID = p.PID
	case a.PID == 0 && id >= 0xC0 && id <= 0xDF:
		a.PID = p.PID
	}
}

// audioAccumulator collects one audio PES at a time so its ADTS frames can be
// counted when the next PES starts.
type audioAccumulator struct {
	open bool
	pts  uint64
	buf  []byte
}

func (acc *audioAccumulator) add(p ts.Packet, a *Audio) {
	if p.PayloadUnitStartIndicator {
		h, err := ts.ParsePESHeader(p.Payload)
		if err == nil && !h.HasPTS && acc.open {
			// A PES without a PTS (legal): its frames follow on from the open
			// one's, so they are counted with it.
			acc.buf = append(acc.buf, p.Payload[min(h.HeaderLen, len(p.Payload)):]...)
			return
		}
		acc.flush(a)
		if err != nil || !h.HasPTS {
			return
		}
		acc.open, acc.pts = true, h.PTS
		acc.buf = append(acc.buf[:0], p.Payload[min(h.HeaderLen, len(p.Payload)):]...)
		return
	}
	if acc.open {
		acc.buf = append(acc.buf, p.Payload...)
	}
}

func (acc *audioAccumulator) flush(a *Audio) {
	if !acc.open {
		return
	}
	acc.open = false
	pes := AudioPES{PTS: acc.pts}
	if frames, rate := parseADTS(acc.buf); frames > 0 {
		pes.Frames = frames
		pes.Ticks = int64(frames) * 1024 * ts.Hz / int64(rate)
		a.SampleRate = rate
	}
	a.PES = append(a.PES, pes)
}

var adtsRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// parseADTS counts the AAC frames in a run of ADTS frames. It returns zero
// frames if the bytes are not a clean run of ADTS headers, so callers fall
// back to PTS spacing instead of trusting a partial count.
func parseADTS(b []byte) (frames, rate int) {
	for len(b) >= 7 {
		if b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
			return 0, 0
		}
		sfi := int(b[2]>>2) & 0x0F
		if sfi >= len(adtsRates) {
			return 0, 0
		}
		flen := int(b[3]&0x03)<<11 | int(b[4])<<3 | int(b[5]>>5)
		if flen < 7 {
			return 0, 0
		}
		frames += int(b[6]&0x03) + 1
		rate = adtsRates[sfi]
		if flen >= len(b) {
			break
		}
		b = b[flen:]
	}
	return frames, rate
}

// medianStep returns the median difference between consecutive timestamps,
// or 0 when there are fewer than two or the median is not positive.
// frameStep is a video track's frame duration: the smallest DTS step that
// is common, meaning the median of a run of steps within 2 ticks of each
// other that holds at least a tenth of the steps (and at least two). An
// encoder that drops frames makes longer steps, never shorter ones, so this
// is the rate the track is coded at, where the median doubles once half the
// frames are dropped. With no such run it is the median; 0 with fewer than
// two AUs.
func frameStep(n int, at func(int) uint64) int64 {
	var steps []int64
	for i := 1; i < n; i++ {
		if d := ts.Diff(at(i), at(i-1)); d > 0 {
			steps = append(steps, d)
		}
	}
	if len(steps) == 0 {
		return 0
	}
	slices.Sort(steps)
	need := max(2, len(steps)/10)
	for i := 0; i < len(steps); {
		j := i + 1
		for j < len(steps) && steps[j]-steps[j-1] <= 2 {
			j++
		}
		if j-i >= need {
			return steps[i+(j-i)/2]
		}
		i = j
	}
	return steps[len(steps)/2]
}

func medianStep(n int, at func(int) uint64) int64 {
	if n < 2 {
		return 0
	}
	steps := make([]int64, 0, n-1)
	for i := 1; i < n; i++ {
		steps = append(steps, ts.Diff(at(i), at(i-1)))
	}
	return max(median(steps), 0)
}

func median(xs []int64) int64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[len(s)/2]
}

// FirstDTS is the DTS of the first AU in decode order.
func (v *Video) FirstDTS() uint64 { return v.AUs[0].DTS }

// LastDTS is the DTS of the last AU in decode order.
func (v *Video) LastDTS() uint64 { return v.AUs[len(v.AUs)-1].DTS }

// MinPTS is the earliest presentation time in the segment. PTS values are
// compared relative to the first DTS, so a wrap inside the segment is fine.
func (v *Video) MinPTS() uint64 {
	ref := v.AUs[0].DTS
	lo := ts.Diff(v.AUs[0].PTS, ref)
	for _, au := range v.AUs[1:] {
		if !au.NoPTS {
			lo = min(lo, ts.Diff(au.PTS, ref))
		}
	}
	return ts.Add(ref, lo)
}

// MaxPTS is the latest presentation time in the segment.
func (v *Video) MaxPTS() uint64 {
	ref := v.AUs[0].DTS
	hi := ts.Diff(v.AUs[0].PTS, ref)
	for _, au := range v.AUs[1:] {
		if !au.NoPTS {
			hi = max(hi, ts.Diff(au.PTS, ref))
		}
	}
	return ts.Add(ref, hi)
}

// FirstPTS is the PTS of the first audio PES.
func (a *Audio) FirstPTS() uint64 { return a.PES[0].PTS }

// LastPTS is the PTS of the last audio PES.
func (a *Audio) LastPTS() uint64 { return a.PES[len(a.PES)-1].PTS }

// MinPTS is the earliest audio PTS in the segment.
func (a *Audio) MinPTS() uint64 {
	ref := a.PES[0].PTS
	var lo int64
	for _, p := range a.PES[1:] {
		lo = min(lo, ts.Diff(p.PTS, ref))
	}
	return ts.Add(ref, lo)
}

// LastTicks is the duration of the last PES: from its ADTS headers, or the
// median PES spacing when those are unavailable.
func (a *Audio) LastTicks() int64 {
	if t := a.PES[len(a.PES)-1].Ticks; t > 0 {
		return t
	}
	return medianStep(len(a.PES), func(i int) uint64 { return a.PES[i].PTS })
}

// frameTicks is the duration of one audio frame: 1024 samples at the ADTS
// sample rate, or the last PES's duration when the rate is unknown.
func (a *Audio) frameTicks() int64 {
	if a.SampleRate > 0 {
		return 1024 * ts.Hz / int64(a.SampleRate)
	}
	return a.LastTicks()
}

// Timed reports whether every PES's duration is known from its frames
// (ADTS). Without it, where the audio ends is a guess, so continuity is not
// checked.
func (a *Audio) Timed() bool {
	return !slices.ContainsFunc(a.PES, func(p AudioPES) bool { return p.Ticks <= 0 })
}

// End is the PTS just after the last audio PES: where the next segment's
// audio should start.
func (a *Audio) End() uint64 { return ts.Add(a.LastPTS(), a.LastTicks()) }

// Duration is the real media duration in seconds: the video DTS span plus
// one frame, or the audio span when there is no usable video.
func (s *Segment) Duration() (float64, bool) {
	if v := s.Video; v != nil && len(v.AUs) >= 2 && v.FrameTicks > 0 {
		return float64(ts.Diff(v.LastDTS(), v.FirstDTS())+v.FrameTicks) / ts.Hz, true
	}
	if a := s.Audio; a != nil {
		if d := ts.Diff(a.End(), a.MinPTS()); d > 0 {
			return float64(d) / ts.Hz, true
		}
	}
	return 0, false
}

// AVOffset is min video PTS minus min audio PTS, in ticks: negative when the
// audio starts after the video.
func (s *Segment) AVOffset() (int64, bool) {
	if s.Video == nil || s.Audio == nil {
		return 0, false
	}
	return ts.Diff(s.Video.MinPTS(), s.Audio.MinPTS()), true
}

// Summary is the JSON form of a segment's timing, for reports.
type Summary struct {
	Packets    int           `json:"packets"`
	PCRPID     uint16        `json:"pcr_pid"`
	PCRs       int           `json:"pcrs"`
	DurationS  float64       `json:"duration_s"`
	AVOffsetMs *float64      `json:"av_offset_ms,omitempty"`
	Video      *VideoSummary `json:"video,omitempty"`
	Audio      *AudioSummary `json:"audio,omitempty"`
}

// VideoSummary summarizes the video track. Timestamps are raw 33-bit ticks.
type VideoSummary struct {
	PID         uint16   `json:"pid"`
	StreamType  uint8    `json:"stream_type"`
	Frames      int      `json:"frames"`
	FirstDTS    uint64   `json:"first_dts"`
	LastDTS     uint64   `json:"last_dts"`
	MinPTS      uint64   `json:"min_pts"`
	MaxPTS      uint64   `json:"max_pts"`
	FrameMs     float64  `json:"frame_ms"`
	FirstPCR    *uint64  `json:"first_pcr,omitempty"`
	LastPCR     *uint64  `json:"last_pcr,omitempty"`
	PTSPCRMinMs *float64 `json:"pts_pcr_min_ms,omitempty"`
	PTSPCRMaxMs *float64 `json:"pts_pcr_max_ms,omitempty"`
	DTSPCRMinMs *float64 `json:"dts_pcr_min_ms,omitempty"`
	DTSPCRMaxMs *float64 `json:"dts_pcr_max_ms,omitempty"`
}

// AudioSummary summarizes the audio track. Timestamps are raw 33-bit ticks.
type AudioSummary struct {
	PID        uint16  `json:"pid"`
	StreamType uint8   `json:"stream_type"`
	PES        int     `json:"pes"`
	Frames     int     `json:"frames"` // AAC frames; a PES of unknown content counts as one
	SampleRate int     `json:"sample_rate,omitzero"`
	FirstPTS   uint64  `json:"first_pts"`
	LastPTS    uint64  `json:"last_pts"`
	EndPTS     uint64  `json:"end_pts"`
	PESMs      float64 `json:"pes_ms"`
}

// Summary returns the segment's timing in report form.
func (s *Segment) Summary() Summary {
	out := Summary{Packets: s.Packets, PCRPID: s.PCRPID, PCRs: s.PCRs}
	if d, ok := s.Duration(); ok {
		out.DurationS = round3(d)
	}
	if off, ok := s.AVOffset(); ok {
		out.AVOffsetMs = new(ms(off))
	}
	if v := s.Video; v != nil {
		vs := &VideoSummary{
			PID:        v.PID,
			StreamType: v.StreamType,
			Frames:     len(v.AUs),
			FirstDTS:   v.FirstDTS(),
			LastDTS:    v.LastDTS(),
			MinPTS:     v.MinPTS(),
			MaxPTS:     v.MaxPTS(),
			FrameMs:    ms(v.FrameTicks),
		}
		// PTS-PCR and DTS-PCR: how far ahead of arrival each frame is due for
		// presentation and for decoding. DTS equals PTS on frames without one.
		var ptsLo, ptsHi, dtsLo, dtsHi int64
		for _, au := range v.AUs {
			if !au.HasPCR || au.NoPTS {
				continue
			}
			p, d := ts.Diff(au.PTS, au.PCR), ts.Diff(au.DTS, au.PCR)
			if vs.FirstPCR == nil {
				vs.FirstPCR, ptsLo, ptsHi, dtsLo, dtsHi = new(au.PCR), p, p, d, d
			}
			vs.LastPCR = new(au.PCR)
			ptsLo, ptsHi = min(ptsLo, p), max(ptsHi, p)
			dtsLo, dtsHi = min(dtsLo, d), max(dtsHi, d)
		}
		if vs.FirstPCR != nil {
			vs.PTSPCRMinMs, vs.PTSPCRMaxMs = new(ms(ptsLo)), new(ms(ptsHi))
			vs.DTSPCRMinMs, vs.DTSPCRMaxMs = new(ms(dtsLo)), new(ms(dtsHi))
		}
		out.Video = vs
	}
	if a := s.Audio; a != nil {
		frames := 0
		for _, p := range a.PES {
			frames += max(p.Frames, 1)
		}
		out.Audio = &AudioSummary{
			PID:        a.PID,
			StreamType: a.StreamType,
			PES:        len(a.PES),
			Frames:     frames,
			SampleRate: a.SampleRate,
			FirstPTS:   a.FirstPTS(),
			LastPTS:    a.LastPTS(),
			EndPTS:     a.End(),
			PESMs:      ms(a.LastTicks()),
		}
	}
	return out
}
