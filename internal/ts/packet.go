// Package ts parses the parts of an MPEG transport stream that stream-analyzer
// needs: packet headers and adaptation fields (PCR), PES headers (PTS/DTS)
// and PAT/PMT sections.
//
// The packet, PES, PSI and CRC code is lifted from ts-validator
// (internal/parser: parser.go Decode/resync, pes.go, psi.go, crc.go) with its
// analyzer coupling removed.
package ts

import "errors"

// PacketSize is the standard MPEG-TS packet size in bytes.
const PacketSize = 188

// SyncByte is the MPEG-TS sync byte at the start of every packet.
const SyncByte = 0x47

var (
	// ErrShortBuffer means the provided buffer ended mid-packet.
	ErrShortBuffer = errors.New("ts: short buffer")
	// ErrSyncLoss means the expected sync byte was not present.
	ErrSyncLoss = errors.New("ts: sync byte not found")
)

// Packet is a parsed TS packet header + slice into the underlying bytes.
// The Payload field points into the source buffer; do not retain it after
// the source has been overwritten.
type Packet struct {
	PID                       uint16
	PayloadUnitStartIndicator bool
	TransportError            bool
	Scrambled                 bool
	HasAdaptationField        bool
	HasPayload                bool
	ContinuityCounter         uint8

	// Adaptation field fields. Valid only when HasAdaptationField is true.
	AFLength               uint8
	DiscontinuityIndicator bool
	RandomAccessIndicator  bool
	HasPCR                 bool
	PCRBase                uint64 // 33-bit, 90 kHz
	PCRExtension           uint16 // 9-bit, 27 MHz fraction

	// Payload slice. Empty if HasPayload is false.
	Payload []byte
}

// Decode parses one 188-byte TS packet at the start of buf.
func Decode(buf []byte) (Packet, error) {
	if len(buf) < PacketSize {
		return Packet{}, ErrShortBuffer
	}
	if buf[0] != SyncByte {
		return Packet{}, ErrSyncLoss
	}
	p := Packet{
		TransportError:            buf[1]&0x80 != 0,
		PayloadUnitStartIndicator: buf[1]&0x40 != 0,
		PID:                       (uint16(buf[1]&0x1F) << 8) | uint16(buf[2]),
		Scrambled:                 buf[3]&0xC0 != 0,
	}
	afc := (buf[3] >> 4) & 0x03
	p.ContinuityCounter = buf[3] & 0x0F
	p.HasAdaptationField = afc == 0b10 || afc == 0b11
	p.HasPayload = afc == 0b01 || afc == 0b11

	payloadStart := 4
	if p.HasAdaptationField {
		afLen := buf[4]
		p.AFLength = afLen
		payloadStart = 5 + int(afLen)
		if payloadStart > PacketSize {
			// Malformed: AF length exceeds packet. Treat as no payload.
			payloadStart = PacketSize
			p.HasPayload = false
		}
		if afLen >= 1 {
			flags := buf[5]
			p.DiscontinuityIndicator = flags&0x80 != 0
			p.RandomAccessIndicator = flags&0x40 != 0
			pcrFlag := flags&0x10 != 0
			if pcrFlag && afLen >= 7 {
				// PCR is 6 bytes starting at buf[6].
				b := buf[6:12]
				p.HasPCR = true
				p.PCRBase = (uint64(b[0]) << 25) |
					(uint64(b[1]) << 17) |
					(uint64(b[2]) << 9) |
					(uint64(b[3]) << 1) |
					(uint64(b[4]>>7) & 0x01)
				p.PCRExtension = (uint16(b[4]&0x01) << 8) | uint16(b[5])
			}
		}
	}
	if p.HasPayload {
		p.Payload = buf[payloadStart:PacketSize]
	}
	return p, nil
}

// Resync scans forward, through the whole buffer, for the next packet: a
// sync byte followed by another one a packet later, or by the end of the
// buffer. Returns the offset adjustment and whether it found one.
func Resync(buf []byte) (int, bool) {
	for i := 0; i+PacketSize <= len(buf); i++ {
		if buf[i] == SyncByte && (i+PacketSize == len(buf) || buf[i+PacketSize] == SyncByte) {
			return i, true
		}
	}
	return 0, false
}
