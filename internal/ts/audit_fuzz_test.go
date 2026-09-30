package ts

// Audit (2026-09-30): fuzz targets and spec-derived round trips for the TS,
// PES and PCR parsing. The encoders here are written from ISO/IEC 13818-1
// (2.4.3.4 adaptation field, 2.4.3.7 PES header), not from this package, so
// the round trips check the bit layout independently.

import (
	"testing"
	"time"
)

// specPCRPacket builds a packet whose adaptation field carries a PCR, laid
// out as ISO/IEC 13818-1 2.4.3.4 gives it: 33-bit base, 6 reserved bits,
// 9-bit extension.
func specPCRPacket(base uint64, ext uint16) []byte {
	p := make([]byte, PacketSize)
	p[0], p[1], p[2], p[3] = SyncByte, 0x01, 0x00, 0x20 // PID 0x100, adaptation field only
	p[4] = 183                                          // adaptation_field_length
	p[5] = 0x10                                         // PCR_flag
	v := base<<15 | 0x3F<<9 | uint64(ext)               // 48 bits
	for i := range 6 {
		p[6+i] = byte(v >> (40 - 8*i))
	}
	for i := 12; i < PacketSize; i++ {
		p[i] = 0xFF
	}
	return p
}

// specTimestamp encodes a 33-bit PTS or DTS with its 4-bit prefix and
// marker bits (2.4.3.7).
func specTimestamp(prefix byte, v uint64) []byte {
	return []byte{
		prefix<<4 | byte(v>>30)<<1 | 1,
		byte(v >> 22),
		byte(v>>15)<<1 | 1,
		byte(v >> 7),
		byte(v)<<1 | 1,
	}
}

func FuzzPCRRoundTrip(f *testing.F) {
	f.Add(uint64(0), uint16(0))
	f.Add(Wrap-1, uint16(299))
	f.Add(uint64(1345529333), uint16(0))
	f.Fuzz(func(t *testing.T, base uint64, ext uint16) {
		base &= Wrap - 1
		ext %= 300 // program_clock_reference_extension counts 0..299
		p, err := Decode(specPCRPacket(base, ext))
		if err != nil {
			t.Fatal(err)
		}
		if !p.HasPCR || p.PCRBase != base || p.PCRExtension != ext {
			t.Fatalf("PCR %d/%d decoded as has=%v %d/%d", base, ext, p.HasPCR, p.PCRBase, p.PCRExtension)
		}
	})
}

func FuzzPTSDTSRoundTrip(f *testing.F) {
	f.Add(uint64(0), uint64(0))
	f.Add(Wrap-1, Wrap-3003)
	f.Add(uint64(1346135757), uint64(1346118721))
	f.Fuzz(func(t *testing.T, pts, dts uint64) {
		pts &= Wrap - 1
		dts &= Wrap - 1
		b := []byte{0, 0, 1, 0xE0, 0, 0, 0x80, 0xC0, 10}
		b = append(b, specTimestamp(3, pts)...)
		b = append(b, specTimestamp(1, dts)...)
		h, err := ParsePESHeader(b)
		if err != nil {
			t.Fatal(err)
		}
		if !h.HasPTS || !h.HasDTS || h.PTS != pts || h.DTS != dts || h.HeaderLen != 19 {
			t.Fatalf("PTS %d DTS %d decoded as %+v", pts, dts, h)
		}
	})
}

// FuzzDecode: no panic, and every field stays inside the packet and inside
// its width.
func FuzzDecode(f *testing.F) {
	f.Add(make([]byte, PacketSize))
	f.Add(specPCRPacket(1345529333, 12))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Decode(b)
		if err != nil {
			return
		}
		switch {
		case p.PCRBase >= Wrap:
			t.Fatalf("PCR base %d wider than 33 bits", p.PCRBase)
		case p.PCRExtension >= 512:
			t.Fatalf("PCR extension %d wider than 9 bits", p.PCRExtension)
		case len(p.Payload) > PacketSize-4:
			t.Fatalf("payload of %d bytes", len(p.Payload))
		case p.HasPCR && !p.HasAdaptationField:
			t.Fatal("PCR without an adaptation field")
		case p.HasPayload && len(p.Payload) != PacketSize-4-boolInt(p.HasAdaptationField)*(1+int(p.AFLength)):
			t.Fatalf("payload length %d does not match AF length %d", len(p.Payload), p.AFLength)
		}
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FuzzParsePESHeader: no panic, timestamps within 33 bits, a sane length.
func FuzzParsePESHeader(f *testing.F) {
	b := []byte{0, 0, 1, 0xE0, 0, 0, 0x80, 0xC0, 10}
	b = append(b, specTimestamp(3, 1346127730)...)
	b = append(b, specTimestamp(1, 1346118721)...)
	f.Add(b)
	f.Add([]byte{0, 0, 1, 0xBE, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := ParsePESHeader(b)
		if err != nil {
			return
		}
		if h.PTS >= Wrap || h.DTS >= Wrap {
			t.Fatalf("timestamp wider than 33 bits: %+v", h)
		}
		if h.HeaderLen < 6 || h.HeaderLen > 9+255 {
			t.Fatalf("header length %d", h.HeaderLen)
		}
	})
}

// FuzzPSIBuilder feeds arbitrary packets through PAT/PMT reassembly.
func FuzzPSIBuilder(f *testing.F) {
	f.Add(make([]byte, 3*PacketSize))
	f.Fuzz(func(t *testing.T, b []byte) {
		start := time.Now()
		psi := NewPSIBuilder()
		for off := 0; off+PacketSize <= len(b); off += PacketSize {
			p, err := Decode(b[off : off+PacketSize])
			if err != nil {
				continue
			}
			if pmt, ok := psi.PMTPID(); p.PID == PIDPAT || ok && p.PID == pmt {
				psi.OnPacket(p)
			}
		}
		prog := psi.Program()
		for pid, s := range prog.Streams {
			if pid != s.PID || pid > 0x1FFF {
				t.Fatalf("stream %d recorded as %+v", pid, s)
			}
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v for %d bytes", d, len(b))
		}
	})
}
