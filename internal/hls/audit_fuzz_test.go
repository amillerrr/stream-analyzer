package hls

// Audit (2026-09-30): fuzz targets for the playlist parsers.

import (
	"math"
	"testing"
	"time"
)

// A made-up media playlist in the shape the origin serves, with the cue
// tags it uses.
const seedMedia = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-TARGETDURATION:7
#EXT-X-MEDIA-SEQUENCE:20000003
#EXTINF:6.006, no desc
feed02-avc1_1300000=2-mp4a_128000_eng=1-begin=1201200180180000-dur=60060000-seq=20000003.ts
#EXT-X-CUE-OUT:120.000
#EXTINF:6.006, no desc
feed02-avc1_1300000=2-mp4a_128000_eng=1-begin=1201200240240000-dur=60060000-seq=20000004.ts
#EXT-X-CUE-OUT-CONT:ElapsedTime=0.667,Duration=120.000
#EXTINF:6.006, no desc
feed02-avc1_1300000=2-mp4a_128000_eng=1-begin=1201200300300000-dur=60060000-seq=20000005.ts
#EXT-X-DISCONTINUITY
#EXT-X-CUE-IN
#EXTINF:6.006, no desc
feed02-avc1_1300000=2-mp4a_128000_eng=1-begin=1201200360360000-dur=60060000-seq=20000007.ts
#EXT-X-DATERANGE:ID="1",START-DATE="2026-01-01T12:00:00Z",PLANNED-DURATION=120,SCTE35-OUT=0xFC30
`

const seedMaster = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-STREAM-INF:BANDWIDTH=900000,AVERAGE-BANDWIDTH=820000,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=512x288,FRAME-RATE=29.970
feed02-avc1_700000=0-mp4a_128000_eng=1.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3800000,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1280x720
feed02-avc1_3200000=3-mp4a_128000_eng=1.m3u8
`

func FuzzParseMedia(f *testing.F) {
	f.Add(seedMedia)
	f.Add("#EXTM3U\n#EXT-X-TARGETDURATION:7\n")
	f.Add("<html>404</html>")
	f.Fuzz(func(t *testing.T, s string) {
		start := time.Now()
		pl, err := ParseMedia([]byte(s))
		if err != nil {
			return
		}
		if math.IsNaN(pl.TargetDuration) || math.IsInf(pl.TargetDuration, 0) || pl.TargetDuration < 0 {
			t.Fatalf("target duration %v", pl.TargetDuration)
		}
		prevDisc := pl.DiscontinuitySequence
		for i, seg := range pl.Segments {
			if seg.Seq != pl.MediaSequence+uint64(i) {
				t.Fatalf("segment %d has seq %d with media sequence %d", i, seg.Seq, pl.MediaSequence)
			}
			if math.IsNaN(seg.Duration) || math.IsInf(seg.Duration, 0) || seg.Duration < 0 {
				t.Fatalf("segment %d duration %v", i, seg.Duration)
			}
			if seg.DiscSeq < prevDisc {
				t.Fatalf("discontinuity number went down at segment %d", i)
			}
			prevDisc = seg.DiscSeq
			if seg.URI == "" {
				t.Fatalf("segment %d has no URI", i)
			}
			if d, ok := CueDuration(seg.SCTE35); ok && (math.IsNaN(d) || math.IsInf(d, 0) || d < 0) {
				t.Fatalf("cue duration %v", d)
			}
			_ = CueKind(seg.SCTE35)
		}
		if other, err := ParseMedia([]byte(s)); err != nil || len(other.Segments) != len(pl.Segments) {
			t.Fatal("parsing is not deterministic")
		}
		CheckDiscontinuitySequence(pl, pl)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v for %d bytes", d, len(s))
		}
	})
}

func FuzzParseMaster(f *testing.F) {
	f.Add(seedMaster)
	f.Add("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nx.m3u8\n")
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseMaster([]byte(s))
		if err != nil {
			return
		}
		for i, v := range m.Variants {
			if v.Index != i || v.URI == "" {
				t.Fatalf("variant %d: %+v", i, v)
			}
		}
		for _, sel := range []string{"", "highest", "lowest", "0", "1280x720", "avc1", "-1", "99"} {
			SelectVariant(m.Variants, sel)
		}
	})
}
