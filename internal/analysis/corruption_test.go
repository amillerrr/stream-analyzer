package analysis

import (
	"bytes"
	"slices"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// A video PES without timestamps is placed evenly between its neighbours,
// and one after the last timestamp a frame on, at the segment's own frame
// step (not the step between the frames that carry timestamps).
func TestPESWithoutTimestampsIsPlaced(t *testing.T) {
	drop := map[int]bool{}
	for i := 1; i < 60; i += 2 { // PTS on every other PES; frame 59 has none
		drop[i] = true
	}
	s := auditAnalyze(t, buildSeg(segSpec{start: 900000, n: 60, lead: 800, audioStart: 906006, audioPES: 90, dropVPTS: drop}))
	if len(s.Video.AUs) != 60 {
		t.Fatalf("%d frames, want 60", len(s.Video.AUs))
	}
	for i, au := range s.Video.AUs {
		if want := ts.Add(900000, int64(i)*3003); au.DTS != want {
			t.Errorf("frame %d: DTS %d, want %d", i, au.DTS, want)
		}
	}
}

// Packets flagged transport_error_indicator and bytes that are not TS
// packets are left out of the analysis and reported as a ts_corruption
// fault on the segment.
func TestCorruptionIsAFault(t *testing.T) {
	m := newMux()
	m.psi(vpid, es{vpid, 0x1B})
	for i := range 6 {
		dts := uint64(900000 + i*3003)
		pcr := dts - 800
		m.pes(vpid, pktOpt{pcr: &pcr, tei: i == 3}, pesData(0xE0, 3, dts+6006, dts, 0, bytes.Repeat([]byte{0xA5}, 400)))
	}
	// 100 bytes of garbage between two packets.
	cut := 4 * ts.PacketSize
	data := slices.Concat(m.out[:cut], bytes.Repeat([]byte{0xFF}, 100), m.out[cut:])
	r := NewChain(DefaultThresholds()).Add(Input{Seq: 1, Segment: auditAnalyze(t, data)})
	i := slices.IndexFunc(r.Faults, func(f Fault) bool { return f.Type == FaultTSCorruption })
	if i < 0 {
		t.Fatalf("faults %v, want %s", faultTypes(r.Faults), FaultTSCorruption)
	}
	v := r.Faults[i].Values
	if v["skipped_bytes"] != 100 || v["tei_packets"] == 0 {
		t.Errorf("values %v, want 100 skipped bytes and the TEI packets", v)
	}
}
