package hls

// Scratch audit tests (parsing reviewer). Each TestAuditX fails when the
// behavior it describes is present.

import (
	"os"
	"strings"
	"testing"
)

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Two consecutive fetches of one media playlist, made up to match real
// ones: the origin never sends EXT-X-DISCONTINUITY-SEQUENCE and drops the
// EXT-X-DISCONTINUITY tag once its segment (10000001) is first in the
// window.
func TestAuditRealDiscontinuityTagDropIsAFault(t *testing.T) {
	prev, err := ParseMedia(mustRead(t, "testdata/audit/playlist_20260101T120000.000Z_msn10000000.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	cur, err := ParseMedia(mustRead(t, "testdata/audit/playlist_20260101T120003.501Z_msn10000001.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prev: msn=%d dsn=%d first=%d disc=%v", prev.MediaSequence, prev.DiscontinuitySequence, prev.Segments[1].Seq, prev.Segments[1].Discontinuity)
	t.Logf("cur:  msn=%d dsn=%d first=%d disc=%v", cur.MediaSequence, cur.DiscontinuitySequence, cur.Segments[0].Seq, cur.Segments[0].Discontinuity)
	if m, ok := CheckDiscontinuitySequence(prev, cur); ok {
		t.Errorf("real playlists with no DSN tag: tag dropped from the head of the window is reported as an unexplained DSN change: %+v", m)
	}
}

// A 200 response cut off in the middle of the last URI (for example a
// close-delimited body) parses as a playlist whose last segment has a
// partial URI and the right positional sequence number.
func TestAuditTruncatedMidURI(t *testing.T) {
	full := string(mustRead(t, "testdata/audit/playlist_20260101T120000.000Z_msn10000000.m3u8"))
	cut := full[:len(full)-20] // inside the last URI
	pl, err := ParseMedia([]byte(cut))
	if err != nil {
		t.Logf("rejected: %v", err)
		return
	}
	last := pl.Segments[len(pl.Segments)-1]
	t.Logf("last segment seq=%d uri=%q", last.Seq, last.URI)
	if !strings.HasSuffix(last.URI, ".ts") {
		t.Errorf("truncated playlist accepted; its last segment %d has the partial URI %q", last.Seq, last.URI)
	}
}

// Cut right after the first digits of EXT-X-MEDIA-SEQUENCE: accepted with a
// wrong (small) media sequence and no segments.
func TestAuditTruncatedInMSN(t *testing.T) {
	full := string(mustRead(t, "testdata/audit/playlist_20260101T120000.000Z_msn10000000.m3u8"))
	i := strings.Index(full, "#EXT-X-MEDIA-SEQUENCE:") + len("#EXT-X-MEDIA-SEQUENCE:") + 3
	pl, err := ParseMedia([]byte(full[:i]))
	if err != nil {
		t.Logf("rejected: %v", err)
		return
	}
	t.Errorf("playlist cut inside EXT-X-MEDIA-SEQUENCE accepted: msn=%d segments=%d target=%v", pl.MediaSequence, len(pl.Segments), pl.TargetDuration)
}

// "#EXTM3U" followed by anything: every non-# line is a segment.
func TestAuditGarbageAfterHeader(t *testing.T) {
	pl, err := ParseMedia([]byte("#EXTM3U\n<html>\n<body>Service Unavailable</body>\n</html>\n"))
	if err != nil {
		t.Logf("rejected: %v", err)
		return
	}
	var uris []string
	for _, s := range pl.Segments {
		uris = append(uris, s.URI)
	}
	t.Errorf("accepted as a media playlist: msn=%d target=%v segments=%q", pl.MediaSequence, pl.TargetDuration, uris)
}

// Tags the monitor can't honor are accepted silently.
func TestAuditUnsupportedTagsAccepted(t *testing.T) {
	for _, tag := range []string{
		`#EXT-X-KEY:METHOD=AES-128,URI="k"`,
		`#EXT-X-MAP:URI="init.mp4"`,
		`#EXT-X-BYTERANGE:1000@0`,
		`#EXT-X-GAP`,
		`#EXT-X-PART:DURATION=1,URI="p.ts"`,
	} {
		pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:5\n" + tag + "\n#EXTINF:6.006,\ns5.ts\n"))
		if err != nil {
			t.Logf("%s: rejected: %v", tag, err)
			continue
		}
		// EXT-X-GAP is honored (the segment is marked, and not fetched)
		// and EXT-X-PART ignored, as RFC 8216bis allows a client that doesn't
		// play low-latency; neither is silent.
		switch {
		case tag == "#EXT-X-GAP" && pl.Segments[0].Gap:
			continue
		case strings.HasPrefix(tag, "#EXT-X-PART:") && len(pl.Segments[0].Tags) == 0:
			continue
		}
		t.Errorf("%s accepted silently: segment tags %q", tag, pl.Segments[0].Tags)
	}
}

func TestAuditMisc(t *testing.T) {
	// CRLF + BOM + spaces.
	pl, err := ParseMedia([]byte("\ufeff#EXTM3U\r\n#EXT-X-TARGETDURATION:7\r\n#EXT-X-MEDIA-SEQUENCE:10\r\n#EXTINF:6.006, title, with, commas\r\n  s10.ts  \r\n#EXTINF:6.006,\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CRLF/BOM: segs=%d uri=%q dur=%v trailing=%q", len(pl.Segments), pl.Segments[0].URI, pl.Segments[0].Duration, pl.Trailing)
	// A URI with no EXTINF.
	pl, err = ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:10\ns10.ts\n"))
	t.Logf("URI without EXTINF: err=%v dur=%v", err, pl.Segments[0].Duration)
	// Header not on the first line.
	_, err = ParseMedia([]byte("\n\n  #EXTM3U\n#EXT-X-TARGETDURATION:7\ns.ts\n"))
	t.Logf("leading blank lines: err=%v", err)
	// ENDLIST.
	pl, _ = ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n"))
	t.Logf("ENDLIST: endlist=%v trailing=%q", pl.Endlist, pl.Trailing)
	// Tag names are matched exactly; lowercase/whitespace variants.
	_, err = ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION: 7\n#EXT-X-MEDIA-SEQUENCE: 10\n#EXTINF: 6.006 ,\na.ts\n"))
	t.Logf("spaces after colon: err=%v", err)
	// Negative/NaN/huge target durations.
	for _, td := range []string{"-1", "NaN", "1e400", "0", "0x10", "7abc", "1e-9", "1e9"} {
		pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:" + td + "\n#EXTINF:6,\na.ts\n"))
		if err == nil {
			t.Logf("TARGETDURATION %s accepted as %v", td, pl.TargetDuration)
		} else {
			t.Logf("TARGETDURATION %s rejected: %v", td, err)
		}
	}
	for _, ei := range []string{"-1,", "NaN,", "6", "6.006 no comma", "", ",", "6;x", "0x1p3,"} {
		pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:" + ei + "\na.ts\n"))
		if err == nil {
			t.Logf("EXTINF %q accepted as %v", ei, pl.Segments[0].Duration)
		} else {
			t.Logf("EXTINF %q rejected: %v", ei, err)
		}
	}
	// MSN formats.
	for _, msn := range []string{"+5", "05", " 5", "5 ", "-5", "18446744073709551615"} {
		pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:" + msn + "\n#EXTINF:6,\na.ts\n#EXTINF:6,\nb.ts\n"))
		if err == nil {
			t.Logf("MSN %q accepted as %d; seqs %d,%d", msn, pl.MediaSequence, pl.Segments[0].Seq, pl.Segments[1].Seq)
		} else {
			t.Logf("MSN %q rejected: %v", msn, err)
		}
	}
}

func TestAuditCueForms(t *testing.T) {
	cases := [][]string{
		{"#EXT-X-CUE-OUT:120.000"},
		{"#EXT-X-CUE-OUT"},
		{"#EXT-X-CUE-OUT:"},
		{"#EXT-X-CUE-OUT:DURATION=120"},
		{"#EXT-X-CUE-OUT:Duration=120"},
		{"#EXT-X-CUE-OUT:DURATION=\"120\""},
		{"#EXT-X-CUE-OUT:120.000,BreakID=7"},
		{"#EXT-X-CUE-OUT: 120"},
		{"#EXT-X-CUE-OUT:-5"},
		{"#EXT-X-CUE-OUT-CONT:ElapsedTime=0.667,Duration=120.000"},
		{"#EXT-X-CUE-OUT-CONT:Duration=120,ElapsedTime=0.667"},
		{"#EXT-X-CUE-OUT-CONT:0.667/120"},
		{"#EXT-X-CUE-IN"},
		{"#EXT-X-CUE-IN:"},
		{"#EXT-X-CUE-IN:ID=1"},
		{"#EXT-X-CUE:TYPE=out"},
		{"#EXT-X-CUE-SPAN:TIMEFROMSIGNAL=PT10S"},
		{"#EXT-OATCLS-SCTE35:/DAlAAAAAAAAAP/wFAUAAAABf+/+AAAAAH4AUmXAAAAAAAA"},
		{"#EXT-X-SCTE35:CUE=\"/DA...\",CUE-OUT=YES"},
		{"#EXT-X-SCTE35:CUE=\"/DA...\",CUE-IN=YES"},
		{"#EXT-X-SCTE35:CUE=\"/DA...\",CUE-OUT=CONT"},
		{`#EXT-X-DATERANGE:ID="1",START-DATE="2026-01-01T12:00:00Z",PLANNED-DURATION=120,SCTE35-OUT=0xFC30`},
		{`#EXT-X-DATERANGE:ID="1",START-DATE="2026-01-01T12:00:00Z",DURATION=118.5,SCTE35-OUT=0xFC30,SCTE35-IN=0xFC31`},
		{`#EXT-X-DATERANGE:ID="1",START-DATE="2026-01-01T12:00:00Z",SCTE35-IN=0xFC31`},
		{`#EXT-X-DATERANGE:ID="1",START-DATE="2026-01-01T12:00:00Z",SCTE35-CMD=0xFC31`},
		{`#EXT-X-DATERANGE:ID="SCTE35-1",START-DATE="2026-01-01T12:00:00Z",CLASS="com.example.program"`},
		{`#EXT-X-DATERANGE:ID="x",CLASS="a,SCTE35-OUT=1",START-DATE="2026-01-01T12:00:00Z"`},
		{`#EXT-X-DATERANGE:ID="1",START-DATE="2026-01-01T12:00:00Z",scte35-out=0xFC30`},
		{"#EXT-X-CUE-IN", "#EXT-X-CUE-OUT:120"},
		{"#EXT-X-SPLICEPOINT-SCTE35:/DA..."},
		{"#EXT-X-ASSET:CAID=0x0"},
		{"#EXT-X-PLACEMENT-OPPORTUNITY"},
		{"#EXT-X-CUEPOINT:foo"},
	}
	for _, tags := range cases {
		d, ok := CueDuration(tags)
		t.Logf("%-100q scte35=%v kind=%-8q dur=%v,%v", tags, IsSCTE35Tag(tags[0]), CueKind(tags), d, ok)
	}
}

func TestAuditAttrs(t *testing.T) {
	for _, s := range []string{
		`BANDWIDTH=1,CODECS="a,b",RESOLUTION=1x2`,
		`bandwidth=1,resolution=1x2`,
		`BANDWIDTH=1,BANDWIDTH=2`,
		`CODECS="a,b,RESOLUTION=9x9`,
		`CODECS=a,b,RESOLUTION=1x2`,
		` BANDWIDTH = 5 , RESOLUTION = 1x2 `,
		`BANDWIDTH=1,,RESOLUTION=1x2`,
		`RESOLUTION="../../x"`,
		`A="x"B=1`,
		`NOVALUE,BANDWIDTH=3`,
	} {
		t.Logf("%-45q -> %q", s, parseAttrs(s))
	}
}

func TestAuditMaster(t *testing.T) {
	m, err := ParseMaster([]byte("#EXTM3U\nstray.m3u8\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",NAME=\"en\",URI=\"audio.m3u8\"\n#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=9,URI=\"if.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=5\n#EXT-X-STREAM-INF:BANDWIDTH=7,AUDIO=\"a\"\nv7.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=7\nv7b.m3u8\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range m.Variants {
		t.Logf("variant %+v", v)
	}
	for _, sel := range []string{"highest", "lowest", "0", "00", "+1", "-0", " 1", "1 ", "HIGHEST", "1X2", "v7", "7", "x", "0x0", "１"} {
		v, err := SelectVariant(m.Variants, sel)
		t.Logf("SelectVariant(%q) = %d %q err=%v", sel, v.Index, v.URI, err)
	}
	t.Logf("IsMaster(media with STREAM-INF inside) = %v", IsMaster([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXTINF:6,\n#EXT-X-STREAM-INF:BANDWIDTH=1\na.ts\n")))
}

func TestAuditResolve(t *testing.T) {
	base := "https://origin.example.com/live/cluster1/feed01/hls/feed01-avc1.m3u8?token=abc"
	for _, ref := range []string{
		"seg-1.ts", "seg-1.ts?x=1", "/abs/seg.ts", "//evil.example/seg.ts", "https://evil.example/seg.ts",
		"../../../../etc/passwd", "file:///etc/passwd", "http://127.0.0.1:8765/capture?channel=channel1",
		"seg 1.ts", "seg%zz.ts", "javascript:alert(1)", "data:text/plain,hi", "#frag",
	} {
		u, err := Resolve(base, ref)
		t.Logf("%-55q -> %q err=%v", ref, u, err)
	}
}
