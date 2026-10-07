package hls

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// e is a playlist entry for originMedia: an origin number, optional tags and
// an EXTINF (6.006 s when 0).
type e struct {
	n      uint64
	tags   []string
	extinf float64
}

// originMedia builds a playlist the way the origin writes them: URIs
// numbered ...-seq=N.ts, no EXT-X-DISCONTINUITY-SEQUENCE unless dsn >= 0.
func originMedia(t *testing.T, target int, msn uint64, dsn int, es ...e) *Media {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", target, msn)
	if dsn >= 0 {
		fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", dsn)
	}
	for _, x := range es {
		for _, tag := range x.tags {
			b.WriteString(tag + "\n")
		}
		d := x.extinf
		if d == 0 {
			d = 6.006
		}
		fmt.Fprintf(&b, "#EXTINF:%g,\nA-begin=0-dur=60060000-seq=%d.ts\n", d, x.n)
	}
	p, err := ParseMedia([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func nums(ns ...uint64) []e {
	var out []e
	for _, n := range ns {
		out = append(out, e{n: n})
	}
	return out
}

const disc = "#EXT-X-DISCONTINUITY"

func reasons(vs []Violation) []string {
	var out []string
	for _, v := range vs {
		out = append(out, fmt.Sprintf("%s@%d", v.Reason, v.Seq))
	}
	return out
}

// Each way a playlist update can break RFC 8216 section 6.2.1 and 6.2.2
// (or the origin's own numbering) is one violation with its reason.
func TestCheckUpdate(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prev, cur *Media
		want      []string
	}{
		{"window slides", originMedia(t, 7, 100, -1, nums(100, 101, 102)...), originMedia(t, 7, 101, -1, nums(101, 102, 103)...), nil},
		{"first playlist, no skips", nil, originMedia(t, 7, 100, -1, nums(100, 101, 102)...), nil},
		{"first playlist skips a number", nil, originMedia(t, 7, 100, -1, nums(100, 101, 103)...),
			[]string{"skipped_number@103"}},
		{"new entry skips a number", originMedia(t, 7, 100, -1, nums(100, 101)...), originMedia(t, 7, 100, -1, nums(100, 101, 103)...),
			[]string{"skipped_number@103"}},
		{"a skip already reported is not reported again", originMedia(t, 7, 100, -1, nums(100, 101, 103)...),
			originMedia(t, 7, 100, -1, nums(100, 101, 103, 104)...), nil},
		// As the origin does: MEDIA-SEQUENCE goes up 2 while one entry leaves,
		// so every listed entry moves up one position.
		{"renumbered", originMedia(t, 7, 100, -1, nums(100, 101, 103, 104)...), originMedia(t, 7, 102, -1, nums(101, 103, 104, 105)...),
			[]string{"renumbered@101"}},
		{"target duration changes", originMedia(t, 7, 100, -1, nums(100, 101)...),
			originMedia(t, 10, 100, -1, e{n: 100}, e{n: 101}, e{n: 102, extinf: 8.775}), []string{"target_duration_changed@102"}},
		{"EXTINF of a listed entry changes", originMedia(t, 7, 100, -1, nums(100, 101, 102)...),
			originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, extinf: 4.004}, e{n: 102}), []string{"rewritten_entry@101"}},
		{"a listed entry gains a cue tag", originMedia(t, 7, 100, -1, nums(100, 101, 102)...),
			originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, tags: []string{"#EXT-X-CUE-IN"}}, e{n: 102}), []string{"rewritten_entry@101"}},
		{"a listed entry loses a cue tag", originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, tags: []string{"#EXT-X-CUE-IN"}}, e{n: 102}),
			originMedia(t, 7, 100, -1, nums(100, 101, 102)...), []string{"rewritten_entry@101"}},
		// The origin drops cue tags from the entry that becomes first, on
		// every break: its practice, not reported.
		{"cue tag dropped as its entry becomes first", originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, tags: []string{"#EXT-X-CUE-IN"}}, e{n: 102}),
			originMedia(t, 7, 101, -1, nums(101, 102, 103)...), nil},
		// It sometimes re-tags that entry too, CUE-OUT becoming CUE-OUT-CONT:
		// a reason of its own, apart from rewritten entries.
		{"the first entry's cue tags are rewritten", originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, tags: []string{"#EXT-X-CUE-OUT:120.000"}}, e{n: 102}),
			originMedia(t, 7, 101, -1, e{n: 101, tags: []string{cont}}, e{n: 102}, e{n: 103}), []string{"first_entry_cue_rewritten@101"}},
		{"the first entry gains a cue tag", originMedia(t, 7, 100, -1, nums(100, 101, 102)...),
			originMedia(t, 7, 101, -1, e{n: 101, tags: []string{cont}}, e{n: 102}, e{n: 103}), []string{"first_entry_cue_rewritten@101"}},
		{"the first entry gains a cue tag and changes EXTINF", originMedia(t, 7, 100, -1, nums(100, 101, 102)...),
			originMedia(t, 7, 101, -1, e{n: 101, extinf: 4.004, tags: []string{cont}}, e{n: 102}, e{n: 103}),
			[]string{"rewritten_entry@101", "first_entry_cue_rewritten@101"}},
		{"a listed entry gains a discontinuity", originMedia(t, 7, 100, 5, nums(100, 101, 102)...),
			originMedia(t, 7, 100, 5, e{n: 100}, e{n: 101, tags: []string{disc}}, e{n: 102}), []string{"rewritten_entry@101"}},
		// The origin sends no EXT-X-DISCONTINUITY-SEQUENCE and drops
		// the tag as its entry becomes first.
		{"discontinuity dropped at the head, no DSN tag", originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, tags: []string{disc}}, e{n: 102}),
			originMedia(t, 7, 101, -1, nums(101, 102, 103)...), []string{"discontinuity_tag_dropped@101"}},
		{"discontinuity dropped at the head, DSN not incremented", originMedia(t, 7, 100, 5, e{n: 100}, e{n: 101, tags: []string{disc}}, e{n: 102}),
			originMedia(t, 7, 101, 5, nums(101, 102, 103)...), []string{"discontinuity_tag_dropped@101"}},
		{"discontinuity dropped at the head, DSN incremented", originMedia(t, 7, 100, 5, e{n: 100}, e{n: 101, tags: []string{disc}}, e{n: 102}),
			originMedia(t, 7, 101, 6, nums(101, 102, 103)...), nil},
		{"tagged entry leaves, DSN incremented", originMedia(t, 7, 100, 5, e{n: 100, tags: []string{disc}}, e{n: 101}),
			originMedia(t, 7, 101, 6, nums(101, 102)...), nil},
		{"tagged entry leaves, DSN not incremented", originMedia(t, 7, 100, 5, e{n: 100, tags: []string{disc}}, e{n: 101}),
			originMedia(t, 7, 101, 5, nums(101, 102)...), []string{"discontinuity_sequence@101"}},
		{"DSN bumped for nothing", originMedia(t, 7, 100, 5, nums(100, 101)...), originMedia(t, 7, 101, 6, nums(101, 102)...),
			[]string{"discontinuity_sequence@101"}},
	} {
		got := reasons(CheckUpdate(tc.prev, tc.cur))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Without numbers in the URIs, a URI listed at another position is a
// renumbering and a new URI at an old position is a rewrite.
func TestCheckUpdatePositional(t *testing.T) {
	p := func(msn uint64, uris ...string) *Media {
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:%d\n", msn)
		for _, u := range uris {
			fmt.Fprintf(&b, "#EXTINF:6,\n%s\n", u)
		}
		m, err := ParseMedia([]byte(b.String()))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, tc := range []struct {
		name      string
		prev, cur *Media
		want      []string
	}{
		{"window slides", p(100, "a.ts", "b.ts"), p(101, "b.ts", "c.ts"), nil},
		{"renumbered", p(100, "a.ts", "b.ts", "c.ts"), p(102, "b.ts", "c.ts", "d.ts"), []string{"renumbered@102"}},
		{"URI replaced", p(100, "a.ts", "b.ts", "c.ts"), p(100, "a.ts", "x.ts", "c.ts", "d.ts"), []string{"rewritten_entry@101"}},
	} {
		got := reasons(CheckUpdate(tc.prev, tc.cur))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The violations name what happened in plain words, with the evidence.
// cont is the tag an origin puts on an entry that became first at the head
// of a break.
const cont = "#EXT-X-CUE-OUT-CONT:ElapsedTime=0.034,Duration=120.000"

// A rewrite of the first entry's cue tags names the tags it lost and
// gained.
func TestFirstEntryCueRewriteDetail(t *testing.T) {
	prev := originMedia(t, 7, 100, -1, e{n: 100}, e{n: 101, tags: []string{"#EXT-X-CUE-OUT:120.000"}}, e{n: 102})
	cur := originMedia(t, 7, 101, -1, e{n: 101, tags: []string{cont}}, e{n: 102}, e{n: 103})
	vs := CheckUpdate(prev, cur)
	if len(vs) != 1 {
		t.Fatalf("violations %v", reasons(vs))
	}
	v := vs[0]
	if v.URI != "A-begin=0-dur=60060000-seq=101.ts" || !strings.Contains(v.Detail, "lost #EXT-X-CUE-OUT:120.000") ||
		!strings.Contains(v.Detail, "gained "+cont) {
		t.Errorf("URI %q, detail %q: want the entry and its lost and gained cue tags", v.URI, v.Detail)
	}
}

func TestCheckUpdateDetail(t *testing.T) {
	prev := originMedia(t, 7, 100, -1, nums(100, 101, 103, 104)...)
	cur := originMedia(t, 7, 102, -1, nums(101, 103, 104, 105)...)
	vs := CheckUpdate(prev, cur)
	if len(vs) != 1 {
		t.Fatalf("violations %v", reasons(vs))
	}
	v := vs[0]
	if want := "A-begin=0-dur=60060000-seq=101.ts"; v.URI != want {
		t.Errorf("URI %q, want %q", v.URI, want)
	}
	if !strings.Contains(v.Detail, "100 -> 102") || !strings.Contains(v.Detail, "+1") {
		t.Errorf("detail %q does not give the MEDIA-SEQUENCE change and the shift", v.Detail)
	}
	if v.Values["moved"] != 3 || v.Values["shift"] != int64(1) {
		t.Errorf("values %v", v.Values)
	}
}

// Tags the monitor can't honor make the playlist unusable, with a reason:
// it reads whole, clear MPEG-TS segments. EXT-X-GAP marks its segment, and
// EXT-X-PART is ignored, as RFC 8216bis lets a client that doesn't play
// low-latency do.
func TestParseMediaTagsTheMonitorCannotHonor(t *testing.T) {
	const head = "#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:5\n"
	for tag, want := range map[string]string{
		`#EXT-X-KEY:METHOD=AES-128,URI="k"`:    "encrypted",
		`#EXT-X-KEY:METHOD=SAMPLE-AES,URI="k"`: "encrypted",
		`#EXT-X-MAP:URI="init.mp4"`:            "EXT-X-MAP",
		`#EXT-X-BYTERANGE:1000@0`:              "EXT-X-BYTERANGE",
	} {
		_, err := ParseMedia([]byte(head + tag + "\n#EXTINF:6.006,\ns5.ts\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want one naming %s", tag, err, want)
		}
	}
	pl, err := ParseMedia([]byte(head + "#EXT-X-KEY:METHOD=NONE\n#EXTINF:6.006,\ns5.ts\n#EXT-X-GAP\n#EXTINF:6.006,\ns6.ts\n" +
		"#EXT-X-PART:DURATION=1,URI=\"p.ts\"\n#EXTINF:6.006,\ns7.ts\n"))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Segments[0].Gap || !pl.Segments[1].Gap || pl.Segments[2].Gap {
		t.Errorf("gap flags %v %v %v, want only s6.ts", pl.Segments[0].Gap, pl.Segments[1].Gap, pl.Segments[2].Gap)
	}
	if slices.ContainsFunc(pl.Segments[2].Tags, func(t string) bool { return strings.HasPrefix(t, "#EXT-X-PART") }) {
		t.Errorf("EXT-X-PART attached to s7.ts: %q", pl.Segments[2].Tags)
	}
}
