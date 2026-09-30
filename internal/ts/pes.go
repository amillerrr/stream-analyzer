package ts

import (
	"errors"
)

// PESHeader is the minimal subset of PES header fields we care about: PTS and
// optionally DTS. Parsed from the first bytes of a PES packet (the payload of
// a TS packet that has PUSI=1 on an ES PID).
type PESHeader struct {
	StreamID  uint8
	HasPTS    bool
	HasDTS    bool
	PTS       uint64 // 33-bit
	DTS       uint64 // 33-bit
	HeaderLen int    // total PES header length in bytes, including PES_header_data_length field
}

var (
	// ErrShortPESHeader means there weren't enough bytes to parse the header.
	ErrShortPESHeader = errors.New("PES header truncated")
	// ErrInvalidPESStartCode means the start code wasn't 00 00 01.
	ErrInvalidPESStartCode = errors.New("invalid PES start code")
	// ErrPESMarkerMismatch means a marker bit in PTS/DTS wasn't set to 1.
	ErrPESMarkerMismatch = errors.New("PES PTS/DTS marker bit mismatch")
)

// ParsePESHeader parses the start of a PES packet (the bytes at the beginning
// of a TS packet's payload when PUSI=1). Returns ErrShortPESHeader if the
// buffer is too short. Most marker bit violations are tolerated (return the
// parsed value but flag it) because real-world streams are imperfect; we only
// hard-fail on a missing start code.
//
// Reference: ISO/IEC 13818-1 § 2.4.3.7 (PES_packet structure).
func ParsePESHeader(b []byte) (PESHeader, error) {
	if len(b) < 9 {
		return PESHeader{}, ErrShortPESHeader
	}
	// Start code: 00 00 01
	if b[0] != 0x00 || b[1] != 0x00 || b[2] != 0x01 {
		return PESHeader{}, ErrInvalidPESStartCode
	}
	streamID := b[3]
	// b[4..6] = PES_packet_length (we don't need it for PTS/DTS extraction)

	// For most stream_ids (audio: 0xC0-0xDF, video: 0xE0-0xEF, private_1: 0xBD, etc.)
	// the optional PES header follows. For stream_ids like padding_stream (0xBE),
	// private_2 (0xBF), program_stream_map/etc, there is no optional header.
	if !hasOptionalHeader(streamID) {
		return PESHeader{StreamID: streamID, HeaderLen: 6}, nil
	}

	// b[6] = flags1: '10' marker, PES_scrambling_control(2), PES_priority(1), data_alignment(1), copyright(1), original_or_copy(1)
	// b[7] = flags2: PTS_DTS_flags(2), ESCR_flag(1), ES_rate_flag(1), DSM_trick_mode(1), additional_copy_info_flag(1), PES_CRC_flag(1), PES_extension_flag(1)
	// b[8] = PES_header_data_length
	flags2 := b[7]
	ptsDTSFlags := (flags2 >> 6) & 0x03
	headerDataLen := int(b[8])
	totalHeaderLen := 9 + headerDataLen

	h := PESHeader{StreamID: streamID, HeaderLen: totalHeaderLen}

	switch ptsDTSFlags {
	case 0b00:
		// No PTS or DTS.
		return h, nil
	case 0b10:
		// PTS only.
		if len(b) < 14 {
			return h, ErrShortPESHeader
		}
		pts, err := parseTimestamp(b[9:14])
		if err != nil {
			return h, err
		}
		h.HasPTS = true
		h.PTS = pts
		return h, nil
	case 0b11:
		// PTS + DTS.
		if len(b) < 19 {
			return h, ErrShortPESHeader
		}
		pts, err := parseTimestamp(b[9:14])
		if err != nil {
			return h, err
		}
		dts, err := parseTimestamp(b[14:19])
		if err != nil {
			return h, err
		}
		h.HasPTS = true
		h.PTS = pts
		h.HasDTS = true
		h.DTS = dts
		return h, nil
	case 0b01:
		// Forbidden by spec, but tolerated as "no timestamps".
		return h, nil
	}
	return h, nil
}

// parseTimestamp decodes a 33-bit PTS/DTS encoded across 5 bytes as defined in
// ISO/IEC 13818-1. The high 4 bits of the first byte are the type tag (we don't
// check it — caller already verified ptsDTSFlags). Marker bits at positions 0
// of bytes 0, 2, 4 should be 1; we accept the value either way and return an
// error only if all three are zero.
func parseTimestamp(b []byte) (uint64, error) {
	if len(b) < 5 {
		return 0, ErrShortPESHeader
	}
	// 33-bit value laid out:
	//   byte0: tttt PPP 1   (3 high bits)
	//   byte1: PPPPPPPP     (8 bits)
	//   byte2: PPPPPPP M    (7 bits, marker)
	//   byte3: PPPPPPPP     (8 bits)
	//   byte4: PPPPPPP M    (7 bits, marker)
	ts := uint64(b[0]&0x0E) << 29 // bits 32..30
	ts |= uint64(b[1]) << 22      // bits 29..22
	ts |= uint64(b[2]&0xFE) << 14 // bits 21..15
	ts |= uint64(b[3]) << 7       // bits 14..7
	ts |= uint64(b[4]&0xFE) >> 1  // bits 6..0

	// Sanity check: at least one marker bit should be set. All-zero markers
	// is a clear sign of garbage or misalignment.
	if (b[0]&0x01) == 0 && (b[2]&0x01) == 0 && (b[4]&0x01) == 0 {
		return ts, ErrPESMarkerMismatch
	}
	return ts, nil
}

func hasOptionalHeader(streamID uint8) bool {
	switch streamID {
	case 0xBC, // program_stream_map
		0xBE, // padding_stream
		0xBF, // private_stream_2
		0xF0, // ECM
		0xF1, // EMM
		0xF2, // DSMCC
		0xF8, // ITU-T Rec. H.222.1 type E
		0xFF: // program_stream_directory
		return false
	}
	return true
}
