// Package tstest builds small synthetic MPEG-TS segments with exact
// timestamps, for tests. The committed fixtures in testdata/ are generated
// from Fixtures(); regenerate them with
//
//	go test ./internal/tstest -run TestFixturesUpToDate -update
package tstest

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// PIDs used by every synthetic segment. The PCR is carried on the video PID,
// as in the origin's streams.
const (
	PMTPID   = 0x1000
	VideoPID = 0x0100
	AudioPID = 0x0101
)

// Timing constants of the synthetic streams, in 90 kHz ticks.
const (
	FrameTicks        = 3003 // one 29.97 fps video frame
	AudioFrameTicks   = 1920 // one 1024-sample AAC frame at 48 kHz
	AudioFramesPerPES = 2
	AudioPESTicks     = AudioFrameTicks * AudioFramesPerPES
)

// AU is one video access unit, carried as one PES.
type AU struct {
	DTS, PTS uint64
	// PCR, when set, goes in the adaptation field of the PES's first packet.
	PCR *uint64
	// Filler adds payload bytes, to make the frame span more packets.
	Filler int
}

// AudioPES is one audio PES carrying Frames ADTS frames (AAC LC, 48 kHz),
// each FrameBytes long (15 when 0).
type AudioPES struct {
	PTS        uint64
	Frames     int
	FrameBytes int
}

// Segment is a synthetic segment: PAT and PMT, then the video and audio PES
// interleaved in timestamp order.
type Segment struct {
	Video []AU
	Audio []AudioPES
}

// Bytes muxes the segment into 188-byte TS packets.
func (s Segment) Bytes() []byte {
	w := &writer{cc: map[uint16]uint8{}}
	w.psi(0x0000, patSection())
	w.psi(PMTPID, pmtSection())

	type item struct {
		at    int64 // DTS (video) or PTS (audio) relative to the first timestamp
		video int   // index into s.Video, or -1
		audio int   // index into s.Audio, or -1
	}
	var ref uint64
	switch {
	case len(s.Video) > 0:
		ref = s.Video[0].DTS
	case len(s.Audio) > 0:
		ref = s.Audio[0].PTS
	}
	var items []item
	for i, au := range s.Video {
		items = append(items, item{at: ts.Diff(au.DTS, ref), video: i, audio: -1})
	}
	for i, a := range s.Audio {
		items = append(items, item{at: ts.Diff(a.PTS, ref), video: -1, audio: i})
	}
	slices.SortStableFunc(items, func(a, b item) int {
		switch {
		case a.at < b.at:
			return -1
		case a.at > b.at:
			return 1
		}
		return 0
	})
	for _, it := range items {
		if it.video >= 0 {
			au := s.Video[it.video]
			payload := append(videoPayload(it.video == 0), bytes.Repeat([]byte{0xA5}, au.Filler)...)
			w.pes(VideoPID, 0xE0, au.PTS, au.DTS, au.PCR, payload)
		} else {
			a := s.Audio[it.audio]
			w.pes(AudioPID, 0xC0, a.PTS, a.PTS, nil, adtsFrames(a.Frames, a.FrameBytes))
		}
	}
	return w.out
}

// Timeline generates continuous segments of FramesPerSegment video frames in
// a closed IBBP GOP (PTS = DTS + reorder delay) plus AAC audio in PES of
// AudioFramesPerPES frames. Audio PES are assigned to the segment whose video
// presentation range contains their PTS, the way a packager cuts them.
type Timeline struct {
	VideoStart       uint64 // DTS of the first frame of segment 0
	AudioStart       uint64 // PTS of the first audio PES (at or after segment 0's first video PTS)
	FramesPerSegment int    // must be 3n+1 so the IBBP GOP closes inside the segment
	PCRLead          int64  // DTS-PCR in ticks
	// Production-shaped options: AAC frames per audio PES (2 when 0), bytes
	// per AAC frame (15 when 0), and filler bytes on each video frame (a
	// real frame spans many packets).
	AudioFramesPerPES int
	AudioFrameBytes   int
	VideoFiller       int
}

func (tl Timeline) audioFrames() int { return cmp.Or(tl.AudioFramesPerPES, AudioFramesPerPES) }
func (tl Timeline) pesTicks() int64  { return int64(tl.audioFrames()) * AudioFrameTicks }

// SegmentTicks is the media duration of one segment.
func (tl Timeline) SegmentTicks() int64 { return int64(tl.FramesPerSegment) * FrameTicks }

// Segment returns segment k (k >= 0) of the timeline.
func (tl Timeline) Segment(k int) Segment {
	var s Segment
	n := tl.FramesPerSegment
	first := ts.Add(tl.VideoStart, int64(k*n)*FrameTicks)
	for i := range n {
		dts := ts.Add(first, int64(i)*FrameTicks)
		pts := ts.Add(first, int64(displayIndex(i)+2)*FrameTicks)
		pcr := ts.Add(dts, -tl.PCRLead)
		s.Video = append(s.Video, AU{DTS: dts, PTS: pts, PCR: new(pcr), Filler: tl.VideoFiller})
	}

	// Audio PES j has PTS AudioStart + j*AudioPESTicks and belongs to segment
	// floor((PTS - start0) / segTicks), where start0 is segment 0's first
	// video PTS.
	start0 := ts.Add(tl.VideoStart, 2*FrameTicks)
	a0 := ts.Diff(tl.AudioStart, start0)
	seg := tl.SegmentTicks()
	pes := tl.pesTicks()
	lo := ceilDiv(int64(k)*seg-a0, pes)
	hi := ceilDiv(int64(k+1)*seg-a0, pes)
	for j := max(lo, 0); j < hi; j++ {
		s.Audio = append(s.Audio, AudioPES{PTS: ts.Add(tl.AudioStart, j*pes), Frames: tl.audioFrames(), FrameBytes: tl.AudioFrameBytes})
	}
	return s
}

// displayIndex maps decode order to display order for an I P B B P B B ...
// GOP: the I frame shows first, each P frame shows after the two B frames
// that follow it in decode order.
func displayIndex(i int) int {
	switch {
	case i == 0:
		return 0
	case (i-1)%3 == 0:
		return i + 2 // P
	default:
		return i - 1 // B
	}
}

func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a > 0) == (b > 0) {
		q++
	}
	return q
}

// Load returns the named fixture from this package's testdata directory.
func Load(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(Path(name))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return b
}

// Path returns the path of the named fixture.
func Path(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata", name)
}

// --- muxing -----------------------------------------------------------------

type writer struct {
	out []byte
	cc  map[uint16]uint8
}

// packet writes one TS packet carrying as much of payload as fits and returns
// the number of payload bytes consumed. Short payloads are padded with
// adaptation-field stuffing.
func (w *writer) packet(pid uint16, pusi bool, pcr *uint64, payload []byte) int {
	var af []byte // adaptation field after its length byte
	if pcr != nil {
		af = append(af, 0x10) // PCR_flag
		af = append(af, encodePCR(*pcr)...)
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
			af = []byte{} // a zero-length adaptation field is just its length byte
		default:
			af = append([]byte{0x00}, bytes.Repeat([]byte{0xFF}, stuff-2)...)
		}
	}

	pkt := make([]byte, 0, ts.PacketSize)
	b1 := byte(pid>>8) & 0x1F
	if pusi {
		b1 |= 0x40
	}
	afc := byte(0x10)
	if af != nil {
		afc = 0x30
	}
	cc := w.cc[pid]
	w.cc[pid] = (cc + 1) & 0x0F
	pkt = append(pkt, ts.SyncByte, b1, byte(pid), afc|cc)
	if af != nil {
		pkt = append(pkt, byte(len(af)))
		pkt = append(pkt, af...)
	}
	pkt = append(pkt, payload[:n]...)
	if len(pkt) != ts.PacketSize {
		panic("tstest: bad packet size")
	}
	w.out = append(w.out, pkt...)
	return n
}

func (w *writer) psi(pid uint16, section []byte) {
	payload := append([]byte{0x00}, section...) // pointer_field
	payload = append(payload, bytes.Repeat([]byte{0xFF}, 184-len(payload))...)
	w.packet(pid, true, nil, payload)
}

func (w *writer) pes(pid uint16, streamID byte, pts, dts uint64, pcr *uint64, es []byte) {
	var hdr []byte
	if pts == dts {
		hdr = []byte{0x80, 0x80, 5}
		hdr = append(hdr, encodeTimestamp(0x2, pts)...)
	} else {
		hdr = []byte{0x80, 0xC0, 10}
		hdr = append(hdr, encodeTimestamp(0x3, pts)...)
		hdr = append(hdr, encodeTimestamp(0x1, dts)...)
	}
	length := 0 // unbounded, allowed for video
	if streamID != 0xE0 {
		length = len(hdr) + len(es)
	}
	data := []byte{0x00, 0x00, 0x01, streamID, byte(length >> 8), byte(length)}
	data = append(data, hdr...)
	data = append(data, es...)

	first := true
	for len(data) > 0 {
		var p *uint64
		if first {
			p = pcr
		}
		n := w.packet(pid, first, p, data)
		data = data[n:]
		first = false
	}
}

func encodeTimestamp(tag uint8, v uint64) []byte {
	return []byte{
		tag<<4 | byte((v>>29)&0x0E) | 0x01,
		byte(v >> 22),
		byte((v>>14)&0xFE) | 0x01,
		byte(v >> 7),
		byte((v<<1)&0xFE) | 0x01,
	}
}

func encodePCR(base uint64) []byte {
	// 33-bit base, 6 reserved bits (1s), 9-bit extension (0).
	return []byte{
		byte(base >> 25),
		byte(base >> 17),
		byte(base >> 9),
		byte(base >> 1),
		byte(base<<7) | 0x7E,
		0x00,
	}
}

func patSection() []byte {
	body := []byte{
		0x00,       // table_id
		0xB0, 0x00, // section_syntax_indicator, section_length (patched)
		0x00, 0x01, // transport_stream_id
		0xC1,       // version 0, current_next 1
		0x00, 0x00, // section_number, last_section_number
		0x00, 0x01, // program_number 1
		0xE0 | byte(PMTPID>>8), byte(PMTPID & 0xFF),
	}
	return withCRC(body)
}

func pmtSection() []byte {
	body := []byte{
		0x02,       // table_id
		0xB0, 0x00, // section_length (patched)
		0x00, 0x01, // program_number
		0xC1,       // version 0, current_next 1
		0x00, 0x00, // section_number, last_section_number
		0xE0 | byte(VideoPID>>8), byte(VideoPID & 0xFF), // PCR_PID
		0xF0, 0x00, // program_info_length
		0x1B, 0xE0 | byte(VideoPID>>8), byte(VideoPID & 0xFF), 0xF0, 0x00, // H.264
		0x0F, 0xE0 | byte(AudioPID>>8), byte(AudioPID & 0xFF), 0xF0, 0x00, // AAC ADTS
	}
	return withCRC(body)
}

func withCRC(body []byte) []byte {
	secLen := len(body) - 3 + 4
	body[1] = 0xB0 | byte(secLen>>8)&0x0F
	body[2] = byte(secLen)
	return binary.BigEndian.AppendUint32(body, ts.CRC32(body))
}

// videoPayload is an access unit delimiter plus a stub slice NAL. Nothing in
// stream-analyzer decodes synthetic video, so the slice data is filler.
func videoPayload(idr bool) []byte {
	nal := byte(0x41)
	if idr {
		nal = 0x65
	}
	b := []byte{0, 0, 0, 1, 0x09, 0xF0, 0, 0, 0, 1, nal}
	return append(b, bytes.Repeat([]byte{0xA5}, 120)...)
}

// adtsFrames returns n ADTS frames (AAC LC, 48 kHz, stereo) of size bytes
// each (15 when 0) with filler payloads.
func adtsFrames(n, size int) []byte {
	frameLen := cmp.Or(size, 7+8)
	var b []byte
	for range n {
		b = append(b,
			0xFF, 0xF1, // syncword, MPEG-4, layer 0, no CRC
			0x4C,                         // profile LC, sampling index 3 (48 kHz), channel config high bit 0
			0x80|byte(frameLen>>11)&0x03, // channel config 2, frame length bits 12-11
			byte(frameLen>>3),            // frame length bits 10-3
			byte(frameLen&0x07)<<5|0x1F,  // frame length bits 2-0, buffer fullness high
			0xFC,                         // buffer fullness low, 1 raw data block
		)
		b = append(b, bytes.Repeat([]byte{0x00}, frameLen-7)...)
	}
	return b
}
