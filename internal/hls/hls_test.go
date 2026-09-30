package hls

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A made-up master playlist in the shape the origin serves: four
// renditions, with EXT-X-INDEPENDENT-SEGMENTS after them.
const originMaster = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=900000,AVERAGE-BANDWIDTH=820000,CODECS="avc1.428016,mp4a.40.2",RESOLUTION=512x288,FRAME-RATE=29.970
feed01-avc1_600000=6-mp4a_128000_eng=1.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1300000,AVERAGE-BANDWIDTH=1180000,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=640x360,FRAME-RATE=29.970
feed01-avc1_1000000=5-mp4a_128000_eng=1.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1700000,AVERAGE-BANDWIDTH=1550000,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=768x432,FRAME-RATE=29.970
feed01-avc1_1300000=4-mp4a_128000_eng=1.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3600000,AVERAGE-BANDWIDTH=3300000,CODECS="avc1.64001f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=29.970
feed01-avc1_3000000=3-mp4a_128000_eng=1.m3u8
#EXT-X-INDEPENDENT-SEGMENTS
`

func TestParseMasterReadsVariants(t *testing.T) {
	m, err := ParseMaster([]byte(originMaster))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Variants) != 4 {
		t.Fatalf("variants = %d, want 4", len(m.Variants))
	}
	v := m.Variants[3]
	if v.Index != 3 || v.Bandwidth != 3600000 || v.Resolution != "1280x720" ||
		v.Codecs != "avc1.64001f,mp4a.40.2" || v.URI != "feed01-avc1_3000000=3-mp4a_128000_eng=1.m3u8" {
		t.Errorf("variant 3 = %+v", v)
	}
	if got := v.Label(); got != "3_1280x720_3600000" {
		t.Errorf("label = %q", got)
	}
}

func TestSelectVariant(t *testing.T) {
	m, err := ParseMaster([]byte(originMaster))
	if err != nil {
		t.Fatal(err)
	}
	for sel, want := range map[string]int{
		"":             3,
		"highest":      3,
		"lowest":       0,
		"1":            1,
		"640x360":      1,
		"avc1_1300000": 2,
	} {
		v, err := SelectVariant(m.Variants, sel)
		if err != nil || v.Index != want {
			t.Errorf("SelectVariant(%q) = %d, %v; want %d", sel, v.Index, err, want)
		}
	}
	for _, sel := range []string{"4", "-1", "1920x1080", "nomatch"} {
		if _, err := SelectVariant(m.Variants, sel); err == nil {
			t.Errorf("SelectVariant(%q): expected an error", sel)
		}
	}
}

const mediaWithSignals = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:7
#EXT-X-MEDIA-SEQUENCE:100
#EXT-X-DISCONTINUITY-SEQUENCE:4
#EXTINF:6.006,
seg-100.ts
#EXT-X-CUE-OUT:30.030
#EXT-X-PROGRAM-DATE-TIME:2026-09-28T15:56:42.000Z
#EXTINF:6.006,
seg-101.ts
#EXT-X-DISCONTINUITY
#EXT-X-CUE-OUT-CONT:ElapsedTime=6.006,Duration=30.030
#EXTINF:5.9,
seg-102.ts
#EXT-X-DATERANGE:ID="1",START-DATE="2026-09-28T15:57:00Z",SCTE35-IN=0xFC30
#EXTINF:6.02269,
seg-103.ts
#EXT-X-CUE-IN
`

func TestParseMediaAttachesTagsToSegments(t *testing.T) {
	p, err := ParseMedia([]byte(mediaWithSignals))
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetDuration != 7 || p.MediaSequence != 100 || p.DiscontinuitySequence != 4 || len(p.Segments) != 4 {
		t.Fatalf("playlist = %+v", p)
	}
	var seqs []uint64
	for _, s := range p.Segments {
		seqs = append(seqs, s.Seq)
	}
	if !slices.Equal(seqs, []uint64{100, 101, 102, 103}) {
		t.Errorf("seqs = %v", seqs)
	}
	s100, s101, s102, s103 := p.Segments[0], p.Segments[1], p.Segments[2], p.Segments[3]
	if s100.URI != "seg-100.ts" || s100.Duration != 6.006 || len(s100.SCTE35) != 0 {
		t.Errorf("100 = %+v", s100)
	}
	if !slices.Equal(s101.SCTE35, []string{"#EXT-X-CUE-OUT:30.030"}) || s101.ProgramDateTime != "2026-09-28T15:56:42.000Z" {
		t.Errorf("101 = %+v", s101)
	}
	if !s102.Discontinuity || s101.Discontinuity || s102.Duration != 5.9 ||
		!slices.Equal(s102.SCTE35, []string{"#EXT-X-CUE-OUT-CONT:ElapsedTime=6.006,Duration=30.030"}) {
		t.Errorf("102 = %+v", s102)
	}
	if len(s103.SCTE35) != 1 || s103.Duration != 6.02269 {
		t.Errorf("103 = %+v", s103)
	}
	if !slices.Equal(p.Trailing, []string{"#EXT-X-CUE-IN"}) {
		t.Errorf("trailing = %v", p.Trailing)
	}
}

func TestParseRejectsNonPlaylists(t *testing.T) {
	for _, body := range []string{"", "<html>502</html>", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\n"} {
		if _, err := ParseMedia([]byte(body)); err == nil {
			t.Errorf("ParseMedia(%q): expected an error", body)
		}
	}
	for _, body := range []string{
		"#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:NaN,\na.ts\n",
		"#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:+Inf,\na.ts\n",
		"#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:-6.006,\na.ts\n",
		"#EXTM3U\n#EXT-X-TARGETDURATION:inf\n#EXTINF:6,\na.ts\n",
	} {
		// A NaN EXTINF would make every report containing the segment unwritable.
		if _, err := ParseMedia([]byte(body)); err == nil {
			t.Errorf("ParseMedia(%q): expected an error", body)
		}
	}
	if _, err := ParseMaster([]byte("#EXTM3U\n#EXTINF:6,\na.ts\n")); err == nil {
		t.Error("ParseMaster of a media playlist: expected an error")
	}
}

func TestIsMaster(t *testing.T) {
	if !IsMaster([]byte(originMaster)) || IsMaster([]byte(mediaWithSignals)) {
		t.Error("IsMaster misclassified the sample playlists")
	}
}

func TestResolve(t *testing.T) {
	base := "https://origin.example.com/live/cluster1/feed01/hls/index.m3u8"
	got, err := Resolve(base, "feed01-avc1_3000000=3-mp4a_128000_eng=1.m3u8")
	want := "https://origin.example.com/live/cluster1/feed01/hls/feed01-avc1_3000000=3-mp4a_128000_eng=1.m3u8"
	if err != nil || got != want {
		t.Errorf("Resolve = %q, %v", got, err)
	}
	if got, _ := Resolve(base, "https://cdn.example/x.ts"); got != "https://cdn.example/x.ts" {
		t.Errorf("absolute URI rewritten: %q", got)
	}
}

func TestSegmentsCarryTheirDiscontinuitySequenceNumber(t *testing.T) {
	p, err := ParseMedia([]byte(mediaWithSignals)) // DSN 4, EXT-X-DISCONTINUITY on 102
	if err != nil {
		t.Fatal(err)
	}
	var got []uint64
	for _, s := range p.Segments {
		got = append(got, s.DiscSeq)
	}
	if !slices.Equal(got, []uint64{4, 4, 5, 5}) {
		t.Errorf("discontinuity numbers = %v, want [4 4 5 5]", got)
	}
}

// playlist builds a media playlist starting at msn; tagged[i] puts
// EXT-X-DISCONTINUITY before segment msn+i. dsn < 0 omits the
// EXT-X-DISCONTINUITY-SEQUENCE tag.
func playlist(t *testing.T, msn uint64, dsn int, tagged ...bool) *Media {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:%d\n", msn)
	if dsn >= 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", dsn)
	}
	for i, tag := range tagged {
		if tag {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		fmt.Fprintf(&b, "#EXTINF:6,\ns%d.ts\n", msn+uint64(i))
	}
	p, err := ParseMedia([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckDiscontinuitySequence(t *testing.T) {
	const T, F = true, false
	for _, tc := range []struct {
		name      string
		prev, cur *Media
		want      *DSNMismatch
	}{
		{"window slides, no tags", playlist(t, 100, 5, F, F, F), playlist(t, 101, 5, F, F, F), nil},
		{"tagged segment leaves, DSN counts it", playlist(t, 100, 5, T, F, F), playlist(t, 101, 6, F, F, F), nil},
		{"tag dropped as its segment becomes first, DSN counts it", playlist(t, 100, 5, F, T, F), playlist(t, 101, 6, F, F, F), nil},
		{"tagged segment still listed", playlist(t, 100, 5, F, T, F), playlist(t, 100, 5, F, T, F, F), nil},
		{"no overlap: cannot tell", playlist(t, 100, 5, F, F), playlist(t, 200, 9, F, F), nil},
		{"DSN bumps with no tag leaving", playlist(t, 100, 5, F, F, F), playlist(t, 101, 6, F, F, F),
			&DSNMismatch{Seq: 101, URI: "s101.ts", Was: 5, Now: 6, PrevDSN: 5, DSN: 6}},
		{"tag leaves but DSN stays", playlist(t, 100, 5, T, F, F), playlist(t, 101, 5, F, F, F),
			&DSNMismatch{Seq: 101, URI: "s101.ts", Was: 6, Now: 5, PrevDSN: 5, DSN: 5}},
		{"DSN goes backwards", playlist(t, 100, 5, F, F), playlist(t, 101, 4, F, F),
			&DSNMismatch{Seq: 101, URI: "s101.ts", Was: 5, Now: 4, PrevDSN: 5, DSN: 4}},
		{"tag leaves a playlist without DSN tag", playlist(t, 100, -1, T, F), playlist(t, 101, -1, F, F),
			&DSNMismatch{Seq: 101, URI: "s101.ts", Was: 1, Now: 0, PrevDSN: 0, DSN: 0}},
	} {
		got, ok := CheckDiscontinuitySequence(tc.prev, tc.cur)
		switch {
		case tc.want == nil && ok:
			t.Errorf("%s: unexpected mismatch %+v", tc.name, got)
		case tc.want != nil && (!ok || got != *tc.want):
			t.Errorf("%s: got %+v (%v), want %+v", tc.name, got, ok, *tc.want)
		}
	}
}

// A segment's SCTE-35 tags name its place in an ad break: OUT is the first
// segment of a break, CONT one inside it, IN the first one after it.
func TestCueKindNamesTheBreakPosition(t *testing.T) {
	for _, c := range []struct {
		tags []string
		want string
	}{
		{nil, ""},
		{[]string{"#EXT-X-CUE-OUT:120.000"}, "OUT"},
		{[]string{"#EXT-X-CUE-OUT"}, "OUT"},
		{[]string{"#EXT-X-CUE-OUT-CONT:ElapsedTime=6.006,Duration=120.000"}, "CONT"},
		{[]string{"#EXT-X-CUE-IN"}, "IN"},
		{[]string{`#EXT-X-DATERANGE:ID="7",START-DATE="2026-09-28T15:57:00Z",PLANNED-DURATION=120,SCTE35-OUT=0xFC30`}, "OUT"},
		{[]string{`#EXT-X-DATERANGE:ID="7",START-DATE="2026-09-28T15:59:00Z",SCTE35-IN=0xFC30`}, "IN"},
		{[]string{`#EXT-X-DATERANGE:ID="8",START-DATE="2026-09-28T15:59:00Z",SCTE35-CMD=0xFC30`}, "OTHER"},
		{[]string{"#EXT-OATCLS-SCTE35:/DAlAAAAAAAAAP/wFAUAAAABf+/+"}, "OTHER"},
		// A recognized tag says more than one that isn't.
		{[]string{"#EXT-X-CUE-OUT:120.000", "#EXT-OATCLS-SCTE35:/DAlAAAAAAAAAP/wFAUAAAABf+/+"}, "OUT"},
		// One break ends and the next begins on the same segment.
		{[]string{"#EXT-X-CUE-IN", "#EXT-X-CUE-OUT:30.000"}, "IN+OUT"},
		{[]string{"#EXT-X-CUE-OUT-CONT:ElapsedTime=6", "#EXT-X-CUE-OUT-CONT:ElapsedTime=6"}, "CONT"},
	} {
		if got := CueKind(c.tags); got != c.want {
			t.Errorf("CueKind(%q) = %q, want %q", c.tags, got, c.want)
		}
	}
}

// A break's planned length, from the tag that starts it.
func TestCueDurationReadsThePlannedLength(t *testing.T) {
	for _, c := range []struct {
		tags []string
		want float64
		ok   bool
	}{
		{[]string{"#EXT-X-CUE-OUT:120.000"}, 120, true},
		{[]string{"#EXT-X-CUE-OUT:DURATION=30"}, 30, true},
		{[]string{`#EXT-X-DATERANGE:ID="7",START-DATE="2026-09-28T15:57:00Z",PLANNED-DURATION=90.5,SCTE35-OUT=0xFC30`}, 90.5, true},
		{[]string{`#EXT-X-DATERANGE:ID="7",START-DATE="2026-09-28T15:57:00Z",DURATION=60,SCTE35-OUT=0xFC30`}, 60, true},
		{[]string{"#EXT-OATCLS-SCTE35:/DAl", "#EXT-X-CUE-OUT:60"}, 60, true},
		{[]string{"#EXT-X-CUE-OUT"}, 0, false},
		{[]string{"#EXT-X-CUE-OUT-CONT:ElapsedTime=6.006,Duration=120.000"}, 0, false},
		{[]string{"#EXT-X-CUE-IN"}, 0, false},
		{[]string{"#EXT-X-CUE-OUT:soon"}, 0, false},
	} {
		got, ok := CueDuration(c.tags)
		if got != c.want || ok != c.ok {
			t.Errorf("CueDuration(%q) = %v, %v; want %v, %v", c.tags, got, ok, c.want, c.ok)
		}
	}
}

// The origin names each segment ...-seq=N.ts. That number, not the
// playlist position, identifies the segment: the origin sometimes lists
// seq=N+1 at position N and renumbers later. A URI without a number falls
// back to its position.
func TestSegmentNumberComesFromTheURI(t *testing.T) {
	pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:20000006\n" +
		"#EXTINF:6.006,\nfeed02-avc1_3200000=3-mp4a_128000_eng=1-begin=1201200180180000-dur=60060000-seq=20000007.ts\n" +
		"#EXT-X-DISCONTINUITY\n#EXTINF:4.004,\nfeed02-begin=2-dur=40040000-seq=20000009.ts?token=x\n" +
		"#EXTINF:6.006,\nplain.ts\n"))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		pos, num uint64
		fromURI  bool
	}{
		{20000006, 20000007, true},
		{20000007, 20000009, true},
		{20000008, 20000008, false},
	} {
		s := pl.Segments[i]
		if s.Seq != want.pos || s.Number() != want.num || s.HasURISeq != want.fromURI {
			t.Errorf("segment %d: position %d number %d from URI %v; want %d %d %v",
				i, s.Seq, s.Number(), s.HasURISeq, want.pos, want.num, want.fromURI)
		}
	}
}

// A variant's FRAME-RATE gives its frame duration in 90 kHz ticks.
func TestVariantFrameTicks(t *testing.T) {
	for rate, want := range map[string]int64{"29.970": 3003, "25": 3600, "59.940": 1502, "23.976": 3754, "": 0, "abc": 0, "0": 0, "-30": 0, "Inf": 0} {
		if got := (Variant{FrameRate: rate}).FrameTicks(); got != want {
			t.Errorf("FRAME-RATE %q: %d ticks, want %d", rate, got, want)
		}
	}
}

// A variant's label names files and directories, so it is built from
// parts that can't hold a path: RESOLUTION as WxH digits only.
func TestVariantLabelIsSafe(t *testing.T) {
	for res, want := range map[string]string{
		"1280x720":                  "1_1280x720_500000",
		"":                          "1_audio_500000",
		"../../../../../../escaped": "1_unknown_500000",
		"1280x720/..":               "1_unknown_500000",
		"1280X720":                  "1_unknown_500000",
	} {
		if got := (Variant{Index: 1, Resolution: res, Bandwidth: 500000}).Label(); got != want {
			t.Errorf("RESOLUTION %q: label %q, want %q", res, got, want)
		}
	}
}

// A body the origin didn't finish, or an error page after #EXTM3U, is not a
// playlist: every line of one ends with a newline, EXT-X-TARGETDURATION is
// required, and a URI has no spaces or '<'.
func TestParseMediaRejectsTruncatedOrGarbage(t *testing.T) {
	good := "#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:6.006,\na-seq=10.ts\n"
	if _, err := ParseMedia([]byte(good)); err != nil {
		t.Fatalf("a whole playlist: %v", err)
	}
	if _, err := ParseMedia([]byte(strings.ReplaceAll(good, "\n", "\r\n"))); err != nil {
		t.Errorf("CRLF line ends: %v", err)
	}
	for name, body := range map[string]string{
		"cut inside the last URI":    good[:len(good)-4],
		"cut inside MEDIA-SEQUENCE":  "#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:1",
		"no EXT-X-TARGETDURATION":    "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:6.006,\na.ts\n",
		"an error page after EXTM3U": "#EXTM3U\n<html>\n<body>Service Unavailable</body>\n</html>\n",
		"a URI with a space":         "#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:6.006,\nService Unavailable\n",
	} {
		if _, err := ParseMedia([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
