package ts

import (
	"testing"
)

func TestParseTimestamp_KnownValue(t *testing.T) {
	// Construct a PES timestamp manually for a known value.
	// PTS = 0x100000005 (5 with bit 32 set) — exercises the high bits.
	want := uint64(0x100000005)
	tsBytes := encodeTimestamp(0b0010, want)
	got, err := parseTimestamp(tsBytes)
	if err != nil {
		t.Fatalf("parseTimestamp: %v", err)
	}
	if got != want {
		t.Errorf("got %#x, want %#x", got, want)
	}
}

func TestParseTimestamp_Zero(t *testing.T) {
	tsBytes := encodeTimestamp(0b0010, 0)
	got, err := parseTimestamp(tsBytes)
	if err != nil {
		t.Fatalf("parseTimestamp: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestParseTimestamp_NoMarkers_Errors(t *testing.T) {
	// All marker bits zero — should error.
	b := []byte{0x20, 0x00, 0x00, 0x00, 0x00}
	_, err := parseTimestamp(b)
	if err == nil {
		t.Error("expected error on missing markers")
	}
}

func TestParsePESHeader_PTSOnly(t *testing.T) {
	// Construct: start code + stream_id=0xE0 (video) + len(0) + flags1 + flags2(PTS_only=10b<<6) + hdr_len=5 + 5 bytes PTS.
	wantPTS := uint64(40500_662056 / 1000 * 90 / 1000) // arbitrary realistic value
	_ = wantPTS
	pts := 0xDEADBEEF & ((uint64(1) << 33) - 1)
	tsBytes := encodeTimestamp(0b0010, pts)

	buf := []byte{
		0x00, 0x00, 0x01, // start code
		0xE0,       // stream_id (video)
		0x00, 0x00, // PES_packet_length (don't care)
		0x80, // flags1: marker bits, all else zero
		0x80, // flags2: PTS_DTS_flags = 10b (PTS only)
		0x05, // PES_header_data_length = 5
	}
	buf = append(buf, tsBytes...)

	h, err := ParsePESHeader(buf)
	if err != nil {
		t.Fatalf("ParsePESHeader: %v", err)
	}
	if !h.HasPTS || h.HasDTS {
		t.Fatalf("flags wrong: HasPTS=%v HasDTS=%v", h.HasPTS, h.HasDTS)
	}
	if h.PTS != pts {
		t.Errorf("PTS = %#x, want %#x", h.PTS, pts)
	}
	if h.StreamID != 0xE0 {
		t.Errorf("StreamID = %#x, want 0xE0", h.StreamID)
	}
}

func TestParsePESHeader_PTSAndDTS(t *testing.T) {
	pts := 0x123456789 & ((uint64(1) << 33) - 1)
	dts := 0x123455000 & ((uint64(1) << 33) - 1)
	buf := []byte{
		0x00, 0x00, 0x01,
		0xE0,
		0x00, 0x00,
		0x80,
		0xC0, // PTS_DTS_flags = 11b
		0x0A, // header_data_len = 10
	}
	buf = append(buf, encodeTimestamp(0b0011, pts)...)
	buf = append(buf, encodeTimestamp(0b0001, dts)...)
	h, err := ParsePESHeader(buf)
	if err != nil {
		t.Fatalf("ParsePESHeader: %v", err)
	}
	if !h.HasPTS || !h.HasDTS {
		t.Errorf("expected both PTS and DTS")
	}
	if h.PTS != pts {
		t.Errorf("PTS = %#x, want %#x", h.PTS, pts)
	}
	if h.DTS != dts {
		t.Errorf("DTS = %#x, want %#x", h.DTS, dts)
	}
}

func TestParsePESHeader_BadStartCode(t *testing.T) {
	buf := []byte{0x00, 0x00, 0x00, 0xE0, 0x00, 0x00, 0x80, 0x00, 0x00}
	_, err := ParsePESHeader(buf)
	if err == nil {
		t.Error("expected error on bad start code")
	}
}

func TestParsePESHeader_TooShort(t *testing.T) {
	buf := []byte{0x00, 0x00, 0x01}
	_, err := ParsePESHeader(buf)
	if err == nil {
		t.Error("expected error on truncated header")
	}
}

// encodeTimestamp builds a 5-byte PES timestamp from a 33-bit value, with the
// provided 4-bit type tag in the high nibble of byte 0.
func encodeTimestamp(tag uint8, ts uint64) []byte {
	b := make([]byte, 5)
	b[0] = (tag&0x0F)<<4 | byte((ts>>29)&0x0E) | 0x01
	b[1] = byte((ts >> 22) & 0xFF)
	b[2] = byte((ts>>14)&0xFE) | 0x01
	b[3] = byte((ts >> 7) & 0xFF)
	b[4] = byte((ts<<1)&0xFE) | 0x01
	return b
}
