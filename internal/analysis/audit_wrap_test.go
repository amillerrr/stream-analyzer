package analysis

// Audit (2026-09-30): 33-bit wraparound on real segments. The monitor has
// never seen a real wrap (the one on 2026-09-30 came at about 13:10 UTC, three
// minutes after it stopped), and the committed fixtures are synthetic. This
// test takes consecutive real segments, adds a constant to every PTS, DTS and
// PCR base (modulo 2^33) so that the wrap falls inside a segment or exactly
// on a boundary, and checks that every check gives the same result as on
// the unshifted segments.
//
// The segments are too big to commit; name them in AUDIT_SEGMENTS, in
// sequence order, separated by commas:
//
//	AUDIT_SEGMENTS=/tmp/x/seg_1.ts,/tmp/x/seg_2.ts,/tmp/x/seg_3.ts \
//	  go test ./internal/analysis -run TestAuditWrapOnRealSegments -v

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// shiftTS returns a copy of a segment with every PTS, DTS and PCR base moved
// by d ticks, modulo 2^33. It reports how many of each it moved.
func shiftTS(t *testing.T, in []byte, d int64) ([]byte, [3]int) {
	b := append([]byte(nil), in...)
	var n [3]int // PCR, PTS, DTS
	for off := 0; off+ts.PacketSize <= len(b); off += ts.PacketSize {
		p := b[off : off+ts.PacketSize]
		if p[0] != ts.SyncByte {
			t.Fatalf("sync lost at %d", off)
		}
		afc := p[3] >> 4 & 3
		payload := 4
		if afc&2 != 0 {
			afLen := int(p[4])
			if afLen >= 7 && p[5]&0x10 != 0 {
				base := uint64(p[6])<<25 | uint64(p[7])<<17 | uint64(p[8])<<9 | uint64(p[9])<<1 | uint64(p[10]>>7)
				base = ts.Add(base, d)
				p[6], p[7], p[8], p[9] = byte(base>>25), byte(base>>17), byte(base>>9), byte(base>>1)
				p[10] = p[10]&0x7F | byte(base&1)<<7
				n[0]++
			}
			payload = 5 + afLen
		}
		if afc&1 == 0 || p[1]&0x40 == 0 || payload+9 > ts.PacketSize {
			continue
		}
		e := p[payload:]
		if e[0] != 0 || e[1] != 0 || e[2] != 1 || !hasTimestamps(e[3]) {
			continue
		}
		if need := map[byte]int{2: 14, 3: 19}[e[7]>>6]; len(e) < need {
			t.Fatalf("PES header split across packets at byte %d", off)
		}
		move := func(at int) {
			v := uint64(e[at]&0x0E)<<29 | uint64(e[at+1])<<22 | uint64(e[at+2]&0xFE)<<14 | uint64(e[at+3])<<7 | uint64(e[at+4])>>1
			v = ts.Add(v, d)
			e[at] = e[at]&0xF1 | byte(v>>29)&0x0E
			e[at+1] = byte(v >> 22)
			e[at+2] = byte(v>>14)&0xFE | e[at+2]&1
			e[at+3] = byte(v >> 7)
			e[at+4] = byte(v<<1) | e[at+4]&1
		}
		switch e[7] >> 6 {
		case 2:
			move(9)
			n[1]++
		case 3:
			move(9)
			move(14)
			n[1]++
			n[2]++
		}
	}
	return b, n
}

func hasTimestamps(streamID byte) bool {
	switch streamID {
	case 0xBC, 0xBE, 0xBF, 0xF0, 0xF1, 0xF2, 0xF8, 0xFF:
		return false
	}
	return true
}

// outcome is everything the checks produce for a chain of segments, with
// the absolute timestamps taken out so shifted and unshifted runs compare.
type outcome struct {
	Faults  []string
	Events  []string
	Summary []string
}

func runChain(t *testing.T, segs [][]byte, first uint64) outcome {
	var o outcome
	c := NewChain(DefaultThresholds())
	for i, b := range segs {
		seg, err := Analyze(b)
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		s := seg.Summary()
		o.Summary = append(o.Summary, fmt.Sprintf("frames=%d dur=%v av=%v frame=%v ptspcr=%v..%v dtspcr=%v..%v audio=%d/%d pes=%v",
			s.Video.Frames, s.DurationS, deref(s.AVOffsetMs), s.Video.FrameMs,
			deref(s.Video.PTSPCRMinMs), deref(s.Video.PTSPCRMaxMs), deref(s.Video.DTSPCRMinMs), deref(s.Video.DTSPCRMaxMs),
			s.Audio.PES, s.Audio.Frames, s.Audio.PESMs))
		r := c.Add(Input{Seq: first + uint64(i), ExtInf: 6.006, Segment: seg})
		for _, f := range r.Faults {
			o.Faults = append(o.Faults, fmt.Sprintf("%d %s %s", f.Seq, f.Type, stripTicks(f.Message)))
		}
		for _, e := range r.Events {
			o.Events = append(o.Events, fmt.Sprintf("%d %s %v %s", e.Seq, e.Type, e.GapMs, stripTicks(e.Detail)))
		}
	}
	return o
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// stripTicks removes raw timestamps from messages such as "(123 -> 456, ...)".
func stripTicks(s string) string {
	var out []string
	for f := range strings.FieldsSeq(s) {
		if len(f) >= 7 && strings.Trim(f, "0123456789(),") == "" {
			f = "<ticks>"
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

func TestAuditWrapOnRealSegments(t *testing.T) {
	env := os.Getenv("AUDIT_SEGMENTS")
	if env == "" {
		t.Skip("set AUDIT_SEGMENTS to consecutive real segments")
	}
	var segs [][]byte
	for p := range strings.SplitSeq(env, ",") {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		segs = append(segs, b)
	}
	base := runChain(t, segs, 1000)
	first, err := Analyze(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	second, err := Analyze(segs[1])
	if err != nil {
		t.Fatal(err)
	}
	// Shifts that put the wrap: in the middle of segment 0's video; exactly
	// on segment 1's first DTS; one tick after segment 0's last audio PTS;
	// between a frame's PCR and its DTS (the PCR is 800 ticks earlier).
	shifts := map[string]int64{
		"inside segment 0":        int64(ts.Wrap) - int64(first.Video.AUs[len(first.Video.AUs)/2].DTS),
		"on the 0/1 boundary":     int64(ts.Wrap) - int64(second.Video.FirstDTS()),
		"after segment 0's audio": int64(ts.Wrap) - int64(first.Audio.LastPTS()) - 1,
		"between PCR and DTS":     int64(ts.Wrap) - int64(second.Video.FirstDTS()) + 400,
	}
	for name, d := range shifts {
		var shifted [][]byte
		for i, b := range segs {
			s, n := shiftTS(t, b, d)
			if n[0] == 0 || n[1] == 0 {
				t.Fatalf("%s: segment %d: moved %v PCR/PTS/DTS", name, i, n)
			}
			shifted = append(shifted, s)
		}
		got := runChain(t, shifted, 1000)
		if !reflect.DeepEqual(got, base) {
			t.Errorf("%s: results differ after the wrap\n got: %+v\nwant: %+v", name, got, base)
		}
	}
}
