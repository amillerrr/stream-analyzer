package ts

import (
	"errors"
	"fmt"
)

// PSI tables we care about live on these PIDs.
const (
	// PIDPAT is the well-known PID for the Program Association Table.
	PIDPAT uint16 = 0x0000
	// TableIDPAT is the table_id value identifying a PAT section.
	TableIDPAT uint8 = 0x00
	// TableIDPMT is the table_id value identifying a PMT section.
	TableIDPMT uint8 = 0x02
)

// StreamInfo is the per-elementary-stream view extracted from a PMT entry.
type StreamInfo struct {
	PID        uint16
	StreamType uint8
	Language   string // ISO-639 3-letter code from ISO_639_language_descriptor, if present
}

// ProgramInfo is one program's parsed PMT, kept current as the PMT changes.
// A zero ProgramInfo means no PMT has been parsed yet.
type ProgramInfo struct {
	ProgramNumber uint16
	PMTPID        uint16
	PCRPID        uint16
	Streams       map[uint16]StreamInfo // ES PID -> info
	Version       int8                  // -1 if no PMT seen
}

// IsES reports whether pid is an elementary stream in the current program.
func (p ProgramInfo) IsES(pid uint16) bool {
	if p.Streams == nil {
		return false
	}
	_, ok := p.Streams[pid]
	return ok
}

// StreamTypeOf returns the stream_type for an ES PID, or 0 if unknown.
func (p ProgramInfo) StreamTypeOf(pid uint16) uint8 {
	if p.Streams == nil {
		return 0
	}
	return p.Streams[pid].StreamType
}

// PSIObservation summarizes what one OnPacket call produced. All booleans
// are false (and CRCFailures empty) if the packet was not a PSI packet,
// or if it was a PSI packet that didn't complete a section.
type PSIObservation struct {
	// ProgramChanged is true if either PAT or PMT changed the active program
	// view (new program, new PMT contents, etc.). Callers should re-read
	// Program() when this is set.
	ProgramChanged bool
	// PATArrived is true when a complete, CRC-valid PAT section was assembled
	// during this OnPacket call. Used by PAT_ERROR watchdog rule.
	PATArrived bool
	// PMTArrived is true when a complete, CRC-valid PMT section was assembled
	// during this OnPacket call. Used by PMT_ERROR watchdog rule.
	PMTArrived bool
	// CRCFailures lists any CRC32 failures detected during section
	// reassembly in this call. A single packet can only complete one section,
	// so this is at most one entry, but kept as a slice for API consistency.
	CRCFailures []CRCFailure
}

// CRCFailure identifies one CRC32-failed PSI section.
type CRCFailure struct {
	TableID uint8  // 0x00 PAT, 0x02 PMT, etc.
	PID     uint16 // the PSI PID the bad section arrived on
}

// PSIBuilder consumes TS packets and maintains the current PAT/PMT view.
// Not safe for concurrent use; one builder per stream.
type PSIBuilder struct {
	// PAT state.
	patAsm     sectionAssembler
	patVersion int8

	// First-program PMT state. We track only the first program in PAT — typical
	// HLS streams carry a single program. Multi-program support could be added
	// later by promoting these fields to a map.
	hasPMTPID  bool
	pmtPID     uint16
	pmtAsm     sectionAssembler
	pmtVersion int8

	// Current program view, snapshot-able.
	program ProgramInfo

	// changed is set whenever PSI tables are updated; readers should call
	// AcceptChange to reset.
	changed bool
}

// NewPSIBuilder returns a fresh builder.
func NewPSIBuilder() *PSIBuilder {
	return &PSIBuilder{
		patVersion: -1,
		pmtVersion: -1,
		program:    ProgramInfo{Version: -1},
	}
}

// OnPacket feeds one TS packet to the builder and returns what was observed.
// PAT and PMT packets drive section reassembly; non-PSI packets return a
// zero-valued PSIObservation.
func (b *PSIBuilder) OnPacket(pkt Packet) PSIObservation {
	var obs PSIObservation
	if pkt.PID == PIDPAT {
		section, crcFailed := b.patAsm.AddPayload(pkt.Payload, pkt.PayloadUnitStartIndicator)
		if crcFailed {
			obs.CRCFailures = append(obs.CRCFailures, CRCFailure{TableID: TableIDPAT, PID: PIDPAT})
		}
		if section != nil {
			b.consumePAT(section)
			obs.PATArrived = true
		}
		obs.ProgramChanged = b.takeChanged()
		return obs
	}
	if b.hasPMTPID && pkt.PID == b.pmtPID {
		section, crcFailed := b.pmtAsm.AddPayload(pkt.Payload, pkt.PayloadUnitStartIndicator)
		if crcFailed {
			obs.CRCFailures = append(obs.CRCFailures, CRCFailure{TableID: TableIDPMT, PID: b.pmtPID})
		}
		if section != nil {
			b.consumePMT(section)
			obs.PMTArrived = true
		}
		obs.ProgramChanged = b.takeChanged()
		return obs
	}
	return obs
}

func (b *PSIBuilder) takeChanged() bool {
	c := b.changed
	b.changed = false
	return c
}

// Program returns a snapshot of the current program view.
func (b *PSIBuilder) Program() ProgramInfo {
	// Deep-copy the streams map so callers don't see future mutations.
	out := b.program
	if b.program.Streams != nil {
		out.Streams = make(map[uint16]StreamInfo, len(b.program.Streams))
		for k, v := range b.program.Streams {
			out.Streams[k] = v
		}
	}
	return out
}

// PMTPID returns the PMT PID currently being tracked, or 0 if not yet known.
func (b *PSIBuilder) PMTPID() (uint16, bool) {
	return b.pmtPID, b.hasPMTPID
}

func (b *PSIBuilder) consumePAT(section []byte) {
	progs, version, err := parsePAT(section)
	if err != nil {
		return
	}
	if version == b.patVersion && b.hasPMTPID {
		// Same version; nothing new.
		return
	}
	// Take the first non-NIT program as our active program.
	for _, p := range progs {
		if p.programNumber == 0 {
			continue // NIT PID; not a program
		}
		if !b.hasPMTPID || p.pid != b.pmtPID {
			b.pmtPID = p.pid
			b.hasPMTPID = true
			// Reset PMT assembler since the target PID changed.
			b.pmtAsm.reset()
			b.pmtVersion = -1
		}
		b.program.ProgramNumber = p.programNumber
		b.program.PMTPID = p.pid
		break
	}
	b.patVersion = version
	b.changed = true
}

func (b *PSIBuilder) consumePMT(section []byte) {
	info, version, err := parsePMT(section)
	if err != nil {
		return
	}
	if version == b.pmtVersion {
		return // Same version; nothing new.
	}
	b.program.PCRPID = info.pcrPID
	b.program.Version = version
	b.program.Streams = make(map[uint16]StreamInfo, len(info.streams))
	for _, es := range info.streams {
		b.program.Streams[es.pid] = StreamInfo{
			PID:        es.pid,
			StreamType: es.streamType,
			Language:   es.language,
		}
	}
	b.pmtVersion = version
	b.changed = true
}

// --- section assembly --------------------------------------------------------

// sectionAssembler reassembles a PSI section possibly split across multiple
// TS packets. It assumes the caller has already verified the packet belongs
// to the PID this assembler is tracking.
type sectionAssembler struct {
	buf     []byte
	needLen int // total expected section length (3 + section_length); 0 if not yet known
}

// AddPayload feeds one TS packet's payload to the assembler.
// Returns (section, crcFailed). One of three outcomes:
//   - section != nil, crcFailed == false: a complete CRC-valid section is
//     available; caller may keep the returned slice.
//   - section == nil, crcFailed == true: a complete section was assembled
//     but its CRC32 did not validate; the section is discarded.
//   - section == nil, crcFailed == false: still assembling, or a malformed
//     header caused a reset.
func (a *sectionAssembler) AddPayload(payload []byte, pusi bool) ([]byte, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if pusi {
		// New section begins. payload[0] is pointer_field — bytes to skip
		// before the section actually starts. Anything before that is the
		// tail of a previous section we may not be tracking.
		ptr := int(payload[0])
		if 1+ptr > len(payload) {
			a.reset()
			return nil, false
		}
		// Discard any in-flight section — we have no way to know we missed bytes.
		a.buf = a.buf[:0]
		a.needLen = 0
		payload = payload[1+ptr:]
	}
	if len(payload) == 0 {
		return nil, false
	}
	a.buf = append(a.buf, payload...)

	if a.needLen == 0 && len(a.buf) >= 3 {
		// section_length is the low 12 bits of the second and third bytes.
		secLen := (int(a.buf[1]&0x0F) << 8) | int(a.buf[2])
		a.needLen = 3 + secLen
		// Sanity: PSI sections are bounded by 1021 (private) / 4093 bytes max.
		if a.needLen > 4096 {
			a.reset()
			return nil, false
		}
	}
	if a.needLen > 0 && len(a.buf) >= a.needLen {
		section := make([]byte, a.needLen)
		copy(section, a.buf[:a.needLen])
		// Validate CRC-32. On failure, surface the failure to the caller
		// rather than swallowing — v1.2 CRC_ERROR rule listens for these.
		if !verifyCRC32(section) {
			a.reset()
			return nil, true
		}
		a.reset()
		return section, false
	}
	return nil, false
}

func (a *sectionAssembler) reset() {
	a.buf = a.buf[:0]
	a.needLen = 0
}

// --- PAT / PMT parsing -------------------------------------------------------

type programEntry struct {
	programNumber uint16
	pid           uint16 // PMT PID (or network_PID if program_number == 0)
}

// parsePAT parses a PAT section. Returns the program list, the version_number,
// and any parse error. The caller should already have CRC-validated the section.
//
// Reference: ISO/IEC 13818-1 §2.4.4.3.
func parsePAT(section []byte) ([]programEntry, int8, error) {
	if len(section) < 12 {
		return nil, -1, errors.New("PAT section too short")
	}
	if section[0] != TableIDPAT {
		return nil, -1, fmt.Errorf("not a PAT (table_id=0x%02x)", section[0])
	}
	version := int8((section[5] >> 1) & 0x1F)
	loopStart := 8
	loopEnd := len(section) - 4 // exclude CRC32

	progs := make([]programEntry, 0, (loopEnd-loopStart)/4)
	for i := loopStart; i+4 <= loopEnd; i += 4 {
		progNum := (uint16(section[i]) << 8) | uint16(section[i+1])
		pid := (uint16(section[i+2]&0x1F) << 8) | uint16(section[i+3])
		progs = append(progs, programEntry{programNumber: progNum, pid: pid})
	}
	return progs, version, nil
}

type elementaryStream struct {
	pid        uint16
	streamType uint8
	language   string
}

type pmtInfo struct {
	pcrPID  uint16
	streams []elementaryStream
}

// parsePMT parses a PMT section. The caller should already have CRC-validated.
//
// Reference: ISO/IEC 13818-1 §2.4.4.8.
func parsePMT(section []byte) (pmtInfo, int8, error) {
	if len(section) < 16 {
		return pmtInfo{}, -1, errors.New("PMT section too short")
	}
	if section[0] != TableIDPMT {
		return pmtInfo{}, -1, fmt.Errorf("not a PMT (table_id=0x%02x)", section[0])
	}
	version := int8((section[5] >> 1) & 0x1F)
	pcrPID := (uint16(section[8]&0x1F) << 8) | uint16(section[9])
	progInfoLen := (int(section[10]&0x0F) << 8) | int(section[11])

	info := pmtInfo{pcrPID: pcrPID}
	loopStart := 12 + progInfoLen
	loopEnd := len(section) - 4 // exclude CRC32

	if loopStart > loopEnd {
		return pmtInfo{}, -1, errors.New("PMT program_info_length exceeds section")
	}

	info.streams = make([]elementaryStream, 0, 4)
	for i := loopStart; i+5 <= loopEnd; {
		streamType := section[i]
		esPID := (uint16(section[i+1]&0x1F) << 8) | uint16(section[i+2])
		esInfoLen := (int(section[i+3]&0x0F) << 8) | int(section[i+4])
		nextI := i + 5 + esInfoLen
		if nextI > loopEnd {
			break // truncated; bail
		}
		descriptors := section[i+5 : nextI]
		es := elementaryStream{pid: esPID, streamType: streamType}
		es.language = parseLanguageDescriptor(descriptors)
		info.streams = append(info.streams, es)
		i = nextI
	}
	return info, version, nil
}

// parseLanguageDescriptor finds an ISO_639_language_descriptor (tag 0x0A) and
// returns the first language code. Used to annotate audio streams.
func parseLanguageDescriptor(descriptors []byte) string {
	for i := 0; i+2 <= len(descriptors); {
		tag := descriptors[i]
		length := int(descriptors[i+1])
		if i+2+length > len(descriptors) {
			return ""
		}
		if tag == 0x0A && length >= 3 {
			// language_code is 3 ASCII bytes; clamp to printable range.
			c1, c2, c3 := descriptors[i+2], descriptors[i+3], descriptors[i+4]
			if isLangChar(c1) && isLangChar(c2) && isLangChar(c3) {
				return string([]byte{c1, c2, c3})
			}
		}
		i += 2 + length
	}
	return ""
}

func isLangChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
