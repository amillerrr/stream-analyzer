package ts

import (
	"encoding/binary"
	"testing"
)

// buildPAT constructs a PAT section with the given programs.
// section_length is computed; CRC is appended.
func buildPAT(tsid uint16, version uint8, progs []programEntry) []byte {
	// Fixed header (8 bytes) + 4 bytes per program + 4 bytes CRC.
	body := make([]byte, 0, 8+4*len(progs)+4)
	body = append(body, TableIDPAT)
	// section_syntax_indicator=1, '0'=0, reserved=11, section_length (12 bits) — fill later.
	body = append(body, 0xB0, 0x00) // placeholder for length
	body = append(body, byte(tsid>>8), byte(tsid))
	body = append(body, 0xC0|((version&0x1F)<<1)|0x01) // reserved=11, version, current_next=1
	body = append(body, 0x00, 0x00)                    // section_number, last_section_number
	for _, p := range progs {
		body = append(body, byte(p.programNumber>>8), byte(p.programNumber))
		body = append(body, byte((p.pid>>8)&0x1F)|0xE0, byte(p.pid))
	}
	// Patch section_length: bytes after the length field, +4 for CRC. Header
	// before length field is 3 bytes (table_id + length field is 2 bytes that
	// follows). section_length counts bytes from after the length field to end
	// of section (including CRC).
	secLen := len(body) - 3 + 4
	body[1] = 0xB0 | byte((secLen>>8)&0x0F)
	body[2] = byte(secLen)
	// Append CRC.
	crc := CRC32(body)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	return append(body, crcBytes...)
}

// buildPMT constructs a PMT section for one program.
func buildPMT(programNumber, pcrPID uint16, version uint8, streams []elementaryStream) []byte {
	body := make([]byte, 0, 32)
	body = append(body, TableIDPMT)
	body = append(body, 0xB0, 0x00) // placeholder for length
	body = append(body, byte(programNumber>>8), byte(programNumber))
	body = append(body, 0xC0|((version&0x1F)<<1)|0x01) // reserved, version, current_next
	body = append(body, 0x00, 0x00)                    // section_number, last_section_number
	// reserved (3) | PCR_PID (13)
	body = append(body, 0xE0|byte((pcrPID>>8)&0x1F), byte(pcrPID))
	// reserved (4) | program_info_length (12) = 0
	body = append(body, 0xF0, 0x00)
	for _, es := range streams {
		body = append(body, es.streamType)
		body = append(body, 0xE0|byte((es.pid>>8)&0x1F), byte(es.pid))
		// ES_info: if a language is set, emit an ISO_639 descriptor.
		var descriptors []byte
		if es.language != "" && len(es.language) >= 3 {
			descriptors = append(descriptors, 0x0A, 0x04) // tag=ISO_639, len=4
			descriptors = append(descriptors, es.language[0], es.language[1], es.language[2])
			descriptors = append(descriptors, 0x00) // audio_type
		}
		body = append(body, 0xF0|byte((len(descriptors)>>8)&0x0F), byte(len(descriptors)))
		body = append(body, descriptors...)
	}
	// Patch length and append CRC.
	secLen := len(body) - 3 + 4
	body[1] = 0xB0 | byte((secLen>>8)&0x0F)
	body[2] = byte(secLen)
	crc := CRC32(body)
	crcBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(crcBytes, crc)
	return append(body, crcBytes...)
}

func TestCRC32MPEG_KnownVector(t *testing.T) {
	// The string "123456789" has a known CRC-32/MPEG-2 of 0x0376E6E7.
	got := CRC32([]byte("123456789"))
	if got != 0x0376E6E7 {
		t.Errorf("CRC32(123456789) = 0x%08x, want 0x0376E6E7", got)
	}
}

func TestParsePAT_SimpleProgram(t *testing.T) {
	section := buildPAT(0x0001, 5, []programEntry{
		{programNumber: 1, pid: 0x0FFF},
	})
	if !verifyCRC32(section) {
		t.Fatal("self-built PAT failed CRC validation")
	}
	progs, ver, err := parsePAT(section)
	if err != nil {
		t.Fatalf("parsePAT: %v", err)
	}
	if ver != 5 {
		t.Errorf("version = %d, want 5", ver)
	}
	if len(progs) != 1 || progs[0].programNumber != 1 || progs[0].pid != 0x0FFF {
		t.Errorf("programs = %+v, want [{1, 0x0FFF}]", progs)
	}
}

func TestParsePMT_CanonicalHLS(t *testing.T) {
	// Matches the user's stream: PCR PID 0x0100, video 0x0100, audio 0x0101.
	section := buildPMT(1, 0x0100, 3, []elementaryStream{
		{pid: 0x0100, streamType: 0x1B},                  // H.264 AVC
		{pid: 0x0101, streamType: 0x0F, language: "eng"}, // AAC ADTS w/ language
	})
	if !verifyCRC32(section) {
		t.Fatal("self-built PMT failed CRC validation")
	}
	info, ver, err := parsePMT(section)
	if err != nil {
		t.Fatalf("parsePMT: %v", err)
	}
	if ver != 3 {
		t.Errorf("version = %d, want 3", ver)
	}
	if info.pcrPID != 0x0100 {
		t.Errorf("PCR PID = 0x%04x, want 0x0100", info.pcrPID)
	}
	if len(info.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(info.streams))
	}
	if info.streams[0].pid != 0x0100 || info.streams[0].streamType != 0x1B {
		t.Errorf("stream 0 = %+v", info.streams[0])
	}
	if info.streams[1].pid != 0x0101 || info.streams[1].streamType != 0x0F || info.streams[1].language != "eng" {
		t.Errorf("stream 1 = %+v", info.streams[1])
	}
}

func TestVerifyCRC32_CorruptionDetected(t *testing.T) {
	section := buildPAT(0x0001, 5, []programEntry{{programNumber: 1, pid: 0x0FFF}})
	if !verifyCRC32(section) {
		t.Fatal("clean PAT should pass CRC")
	}
	// Flip a bit in the middle of the section.
	section[4] ^= 0x01
	if verifyCRC32(section) {
		t.Error("corrupted PAT should fail CRC")
	}
}

func TestPSIBuilder_PAT_then_PMT(t *testing.T) {
	patSection := buildPAT(0x0001, 0, []programEntry{{programNumber: 1, pid: 0x0FFF}})
	pmtSection := buildPMT(1, 0x0100, 0, []elementaryStream{
		{pid: 0x0100, streamType: 0x1B},
		{pid: 0x0101, streamType: 0x0F, language: "eng"},
	})

	b := NewPSIBuilder()

	// Feed PAT in a single packet.
	patPkt := psiPacket(PIDPAT, patSection)
	if obs := b.OnPacket(patPkt); !obs.ProgramChanged {
		t.Fatal("expected PAT to trigger program change")
	} else if !obs.PATArrived {
		t.Error("PATArrived flag should be set when a complete PAT section is assembled")
	}
	pmtPID, ok := b.PMTPID()
	if !ok || pmtPID != 0x0FFF {
		t.Fatalf("PMT PID after PAT = (0x%04x, %v), want (0x0FFF, true)", pmtPID, ok)
	}

	// Feed PMT.
	pmtPkt := psiPacket(0x0FFF, pmtSection)
	if obs := b.OnPacket(pmtPkt); !obs.ProgramChanged {
		t.Fatal("expected PMT to trigger program change")
	} else if !obs.PMTArrived {
		t.Error("PMTArrived flag should be set when a complete PMT section is assembled")
	}
	prog := b.Program()
	if prog.PCRPID != 0x0100 {
		t.Errorf("PCR PID = 0x%04x, want 0x0100", prog.PCRPID)
	}
	if len(prog.Streams) != 2 {
		t.Fatalf("stream count = %d, want 2", len(prog.Streams))
	}
	if !prog.IsES(0x0100) || !prog.IsES(0x0101) {
		t.Error("PMT PIDs should be reported as ES")
	}
	if prog.IsES(0x0FFF) || prog.IsES(0x0000) {
		t.Error("PMT and PAT PIDs should NOT be reported as ES")
	}
	if prog.StreamTypeOf(0x0100) != 0x1B {
		t.Errorf("video stream_type = 0x%02x, want 0x1B", prog.StreamTypeOf(0x0100))
	}
	if prog.StreamTypeOf(0x0101) != 0x0F {
		t.Errorf("audio stream_type = 0x%02x, want 0x0F", prog.StreamTypeOf(0x0101))
	}
}

func TestPSIBuilder_VersionUpdateIgnoresSameVersion(t *testing.T) {
	section := buildPAT(0x0001, 7, []programEntry{{programNumber: 1, pid: 0x0FFF}})
	b := NewPSIBuilder()
	if obs := b.OnPacket(psiPacket(PIDPAT, section)); !obs.ProgramChanged {
		t.Fatal("first PAT should trigger program change")
	}
	// Replay same PAT — should still be reported as arrival (the section was
	// successfully assembled) but should not register as a program change.
	obs := b.OnPacket(psiPacket(PIDPAT, section))
	if obs.ProgramChanged {
		t.Error("identical PAT replay should not be reported as a program change")
	}
	if !obs.PATArrived {
		t.Error("identical PAT replay should still report PATArrived (the section was reassembled)")
	}
}

func TestSectionAssembler_SplitAcrossPackets(t *testing.T) {
	// Build a long PMT that won't fit in one 184-byte payload.
	streams := make([]elementaryStream, 0, 40)
	for i := 0; i < 40; i++ {
		streams = append(streams, elementaryStream{pid: uint16(0x0200 + i), streamType: 0x0F, language: "eng"})
	}
	section := buildPMT(1, 0x0100, 0, streams)
	if len(section) <= 184 {
		t.Fatalf("test setup: expected section > 184 bytes, got %d", len(section))
	}

	asm := sectionAssembler{}
	// First packet: PUSI=1, pointer_field=0, then first chunk.
	first := append([]byte{0x00}, section[:184]...)
	out, crcFailed := asm.AddPayload(first, true)
	if out != nil {
		t.Errorf("got section before second packet: %d bytes", len(out))
	}
	if crcFailed {
		t.Error("first partial packet should not report CRC failure")
	}
	// Second packet: PUSI=0, remaining bytes.
	rest := section[184:]
	out, crcFailed = asm.AddPayload(rest, false)
	if out == nil {
		t.Fatal("second packet should complete the section")
	}
	if crcFailed {
		t.Error("valid section should not report CRC failure")
	}
	if len(out) != len(section) {
		t.Errorf("reassembled length = %d, want %d", len(out), len(section))
	}
}

func TestSectionAssembler_CRCFailureSurfaced(t *testing.T) {
	// Build a valid section, then flip a byte to invalidate the CRC.
	section := buildPAT(0x0001, 5, []programEntry{{programNumber: 1, pid: 0x0FFF}})
	if len(section) > 184 {
		t.Skip("test PAT unexpectedly long")
	}
	corrupted := make([]byte, len(section))
	copy(corrupted, section)
	corrupted[len(corrupted)-5] ^= 0xFF // flip a payload byte; CRC will fail

	asm := sectionAssembler{}
	// pointer_field (0) + corrupted section in one packet.
	payload := append([]byte{0x00}, corrupted...)
	out, crcFailed := asm.AddPayload(payload, true)
	if out != nil {
		t.Error("corrupted section should not be returned to caller")
	}
	if !crcFailed {
		t.Error("expected CRC failure to be surfaced")
	}
}

// psiPacket builds a synthetic TS packet with PUSI=1, payload = pointer_field
// (0) followed by the section. Suitable for sections that fit in one packet.
func psiPacket(pid uint16, section []byte) Packet {
	if len(section) > 183 {
		panic("psiPacket helper only supports single-packet sections")
	}
	payload := make([]byte, 0, 1+len(section))
	payload = append(payload, 0x00) // pointer_field
	payload = append(payload, section...)
	return Packet{
		PID:                       pid,
		PayloadUnitStartIndicator: true,
		HasPayload:                true,
		Payload:                   payload,
	}
}
