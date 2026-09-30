// Package hls parses HLS master and media playlists: variants, sequence
// numbers, EXTINF, discontinuities and SCTE-35 signaling tags.
package hls

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Variant is one EXT-X-STREAM-INF entry.
type Variant struct {
	Index            int // position in the master playlist
	URI              string
	Bandwidth        int
	AverageBandwidth int
	Resolution       string
	Codecs           string
	FrameRate        string
}

// FrameTicks is the variant's frame duration in 90 kHz ticks from its
// FRAME-RATE attribute (3003 for 29.970), or 0 when it has none.
func (v Variant) FrameTicks() int64 {
	fps, err := strconv.ParseFloat(strings.TrimSpace(v.FrameRate), 64)
	if err != nil || !(fps > 0) || math.IsInf(fps, 0) {
		return 0
	}
	return int64(math.Round(90000 / fps))
}

// Label identifies the variant in file names: index_resolution_bandwidth.
// RESOLUTION comes from the origin, so only WxH digits are used as they
// are; anything else is "unknown", and no label can hold a path.
func (v Variant) Label() string {
	res := "audio"
	switch {
	case resolution.MatchString(v.Resolution):
		res = v.Resolution
	case v.Resolution != "":
		res = "unknown"
	}
	return fmt.Sprintf("%d_%s_%d", v.Index, res, v.Bandwidth)
}

var resolution = regexp.MustCompile(`^[0-9]{1,5}x[0-9]{1,5}$`)

// Master is a master playlist.
type Master struct {
	Variants []Variant
}

// Media is a media playlist.
type Media struct {
	TargetDuration        float64
	MediaSequence         uint64
	DiscontinuitySequence uint64
	// HasDiscontinuitySequence: the playlist sends
	// EXT-X-DISCONTINUITY-SEQUENCE (the origin never does).
	HasDiscontinuitySequence bool
	Endlist                  bool
	Segments                 []Segment
	// Trailing holds tags after the last segment URI, such as an
	// EXT-X-CUE-IN that will apply to the next segment once it is listed.
	Trailing []string
}

// Segment is one media segment entry.
type Segment struct {
	// Seq is the segment's position: EXT-X-MEDIA-SEQUENCE plus its index.
	Seq uint64
	// URISeq is the origin's own number for the segment, from a "seq=N" in
	// its URI (the origin names segments ...-seq=N.ts). It stays
	// with the segment when the origin renumbers its playlist, which the
	// position does not.
	URISeq          uint64
	HasURISeq       bool
	URI             string
	Duration        float64
	Discontinuity   bool
	DiscSeq         uint64 // discontinuity sequence number
	ProgramDateTime string
	// Gap: EXT-X-GAP marks the segment as having no media.
	Gap    bool
	Tags   []string // every segment tag except EXTINF, raw
	SCTE35 []string // the tags that carry ad or SCTE-35 signaling
}

// Playlist-level tags, which never attach to a segment.
var playlistTags = []string{
	"#EXTM3U", "#EXT-X-VERSION", "#EXT-X-TARGETDURATION", "#EXT-X-MEDIA-SEQUENCE",
	"#EXT-X-DISCONTINUITY-SEQUENCE", "#EXT-X-PLAYLIST-TYPE", "#EXT-X-ENDLIST",
	"#EXT-X-INDEPENDENT-SEGMENTS", "#EXT-X-START", "#EXT-X-ALLOW-CACHE",
	"#EXT-X-I-FRAMES-ONLY", "#EXT-X-SERVER-CONTROL", "#EXT-X-PART-INF",
}

// IsSCTE35Tag reports whether a tag line carries ad-break or SCTE-35
// signaling (the cue, OATCLS, SCTE35 and DATERANGE SCTE35 styles).
func IsSCTE35Tag(line string) bool {
	for _, p := range []string{
		"#EXT-X-CUE", "#EXT-OATCLS-SCTE35", "#EXT-X-SCTE35", "#EXT-X-SPLICEPOINT-SCTE35",
		"#EXT-X-ASSET", "#EXT-X-PLACEMENT-OPPORTUNITY",
	} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return strings.HasPrefix(line, "#EXT-X-DATERANGE") && strings.Contains(line, "SCTE35")
}

// Ad-break positions a segment's SCTE-35 tags can signal (see CueKind).
const (
	CueOut   = "OUT"   // the first segment of a break
	CueCont  = "CONT"  // a segment inside a break
	CueIn    = "IN"    // the first segment after a break
	CueOther = "OTHER" // SCTE-35 signaling that names no position
)

// CueKind names the ad-break position a segment's tags signal: OUT for
// #EXT-X-CUE-OUT or an EXT-X-DATERANGE with SCTE35-OUT, CONT for
// #EXT-X-CUE-OUT-CONT, IN for #EXT-X-CUE-IN or SCTE35-IN, OTHER for other
// SCTE-35 tags, and "" for none. When one segment signals several
// positions, such as a break ending as the next begins, they are joined
// with "+" in playlist order.
func CueKind(tags []string) string {
	var kinds []string
	add := func(k string) {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	for _, tag := range tags {
		name, rest, _ := strings.Cut(tag, ":")
		switch name {
		case "#EXT-X-CUE-OUT":
			add(CueOut)
		case "#EXT-X-CUE-OUT-CONT":
			add(CueCont)
		case "#EXT-X-CUE-IN":
			add(CueIn)
		case "#EXT-X-DATERANGE":
			a := parseAttrs(rest)
			if _, ok := a["SCTE35-OUT"]; ok {
				add(CueOut)
			}
			if _, ok := a["SCTE35-IN"]; ok {
				add(CueIn)
			}
		}
	}
	switch {
	case len(kinds) > 0:
		return strings.Join(kinds, "+")
	case slices.ContainsFunc(tags, IsSCTE35Tag):
		return CueOther
	}
	return ""
}

// CueDuration returns a break's planned length, in seconds, from the tag
// that starts it: #EXT-X-CUE-OUT:120.000, #EXT-X-CUE-OUT:DURATION=120, or
// an EXT-X-DATERANGE with SCTE35-OUT and PLANNED-DURATION or DURATION.
func CueDuration(tags []string) (float64, bool) {
	for _, tag := range tags {
		name, rest, _ := strings.Cut(tag, ":")
		var value string
		switch name {
		case "#EXT-X-CUE-OUT":
			value = rest
			if strings.Contains(rest, "=") {
				value = parseAttrs(rest)["DURATION"]
			}
		case "#EXT-X-DATERANGE":
			a := parseAttrs(rest)
			if _, ok := a["SCTE35-OUT"]; !ok {
				continue
			}
			value = cmp.Or(a["PLANNED-DURATION"], a["DURATION"])
		default:
			continue
		}
		if d, err := parseSeconds(value); err == nil {
			return d, true
		}
	}
	return 0, false
}

// lines returns the playlist's non-empty lines, trimmed, after checking the
// #EXTM3U header.
func lines(data []byte) ([]string, error) {
	var out []string
	for line := range strings.Lines(strings.TrimPrefix(string(data), "\ufeff")) {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 || out[0] != "#EXTM3U" {
		return nil, errors.New("not an HLS playlist: missing #EXTM3U")
	}
	return out, nil
}

// IsMaster reports whether a playlist is a master playlist.
func IsMaster(data []byte) bool {
	ls, err := lines(data)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ls, func(l string) bool { return strings.HasPrefix(l, "#EXT-X-STREAM-INF:") })
}

// ParseMaster parses a master playlist.
func ParseMaster(data []byte) (*Master, error) {
	ls, err := lines(data)
	if err != nil {
		return nil, err
	}
	var m Master
	var pending *Variant
	for _, line := range ls {
		if rest, ok := strings.CutPrefix(line, "#EXT-X-STREAM-INF:"); ok {
			a := parseAttrs(rest)
			bw, _ := strconv.Atoi(a["BANDWIDTH"])
			avg, _ := strconv.Atoi(a["AVERAGE-BANDWIDTH"])
			pending = &Variant{
				Index:            len(m.Variants),
				Bandwidth:        bw,
				AverageBandwidth: avg,
				Resolution:       a["RESOLUTION"],
				Codecs:           a["CODECS"],
				FrameRate:        a["FRAME-RATE"],
			}
			continue
		}
		if strings.HasPrefix(line, "#") || pending == nil {
			continue
		}
		pending.URI = line
		m.Variants = append(m.Variants, *pending)
		pending = nil
	}
	if len(m.Variants) == 0 {
		return nil, errors.New("master playlist has no variants")
	}
	return &m, nil
}

// parseAttrs parses an HLS attribute list; quoted values may contain commas.
func parseAttrs(s string) map[string]string {
	m := map[string]string{}
	for s != "" {
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var val string
		if q, ok := strings.CutPrefix(rest, `"`); ok {
			val, rest, _ = strings.Cut(q, `"`)
			rest = strings.TrimPrefix(rest, ",")
		} else {
			val, rest, _ = strings.Cut(rest, ",")
		}
		m[strings.TrimSpace(key)] = val
		s = rest
	}
	return m
}

// ParseMedia parses a media playlist. Tags between two segment URIs attach
// to the second segment, as HLS defines.
//
// A body the origin didn't finish is not a playlist: every line of one ends
// with a newline (RFC 8216 section 4.1), EXT-X-TARGETDURATION is required,
// and a URI line holds no whitespace, quotes or angle brackets, which an
// error page after #EXTM3U would.
func ParseMedia(data []byte) (*Media, error) {
	ls, err := lines(data)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(string(data), "\n") {
		return nil, errors.New("truncated playlist: the last line has no newline")
	}
	var p Media
	var cur Segment
	sawTarget := false
	for _, line := range ls {
		name, rest, _ := strings.Cut(line, ":")
		switch {
		case name == "#EXT-X-STREAM-INF":
			return nil, errors.New("got a master playlist, want a media playlist")
		case name == "#EXT-X-TARGETDURATION":
			if p.TargetDuration, err = parseSeconds(rest); err != nil {
				return nil, fmt.Errorf("bad EXT-X-TARGETDURATION %q", rest)
			}
			sawTarget = true
		case name == "#EXT-X-MEDIA-SEQUENCE":
			if p.MediaSequence, err = strconv.ParseUint(rest, 10, 64); err != nil {
				return nil, fmt.Errorf("bad EXT-X-MEDIA-SEQUENCE %q", rest)
			}
		case name == "#EXT-X-DISCONTINUITY-SEQUENCE":
			if p.DiscontinuitySequence, err = strconv.ParseUint(rest, 10, 64); err != nil {
				return nil, fmt.Errorf("bad EXT-X-DISCONTINUITY-SEQUENCE %q", rest)
			}
			p.HasDiscontinuitySequence = true
		case name == "#EXT-X-ENDLIST":
			p.Endlist = true
		case slices.Contains(playlistTags, name):
		case name == "#EXTINF":
			dur, _, _ := strings.Cut(rest, ",")
			if cur.Duration, err = parseSeconds(dur); err != nil {
				return nil, fmt.Errorf("bad EXTINF %q", rest)
			}
		case name == "#EXT-X-PART" || name == "#EXT-X-PRELOAD-HINT" || name == "#EXT-X-RENDITION-REPORT":
			// Low-latency HLS. The full segments are listed too, and a
			// client that doesn't play low-latency ignores these.
		case name == "#EXT-X-KEY" && parseAttrs(rest)["METHOD"] != "NONE":
			return nil, fmt.Errorf("encrypted segments are not supported (%s): the monitor reads clear MPEG-TS", line)
		case name == "#EXT-X-MAP":
			return nil, errors.New("EXT-X-MAP is not supported: the monitor reads MPEG-TS segments, which have no initialization section")
		case name == "#EXT-X-BYTERANGE":
			return nil, errors.New("EXT-X-BYTERANGE is not supported: the monitor fetches and checks whole URIs")
		case strings.HasPrefix(line, "#EXT"):
			switch name {
			case "#EXT-X-DISCONTINUITY":
				cur.Discontinuity = true
			case "#EXT-X-PROGRAM-DATE-TIME":
				cur.ProgramDateTime = rest
			case "#EXT-X-GAP":
				cur.Gap = true
			}
			cur.Tags = append(cur.Tags, line)
			if IsSCTE35Tag(line) {
				cur.SCTE35 = append(cur.SCTE35, line)
			}
		case strings.HasPrefix(line, "#"):
			// Comment.
		default:
			if strings.ContainsAny(line, " \t\"<>") {
				return nil, fmt.Errorf("not a segment URI: %q", line)
			}
			cur.URI = line
			p.Segments = append(p.Segments, cur)
			cur = Segment{}
		}
	}
	if !sawTarget {
		return nil, errors.New("not a media playlist: no EXT-X-TARGETDURATION")
	}
	// A segment's discontinuity number is the playlist's
	// EXT-X-DISCONTINUITY-SEQUENCE plus the EXT-X-DISCONTINUITY tags up to
	// and including it. A server that removes a tag, with its segment or on
	// its own, must count it in EXT-X-DISCONTINUITY-SEQUENCE so these numbers
	// stay stable, which is what CheckDiscontinuitySequence checks.
	disc := p.DiscontinuitySequence
	for i := range p.Segments {
		p.Segments[i].Seq = p.MediaSequence + uint64(i)
		p.Segments[i].URISeq, p.Segments[i].HasURISeq = URINumber(p.Segments[i].URI)
		if p.Segments[i].Discontinuity {
			disc++
		}
		p.Segments[i].DiscSeq = disc
	}
	p.Trailing = cur.Tags
	return &p, nil
}

// Number identifies a segment: the origin's number from its URI when it
// has one, otherwise its playlist position.
func (s Segment) Number() uint64 {
	if s.HasURISeq {
		return s.URISeq
	}
	return s.Seq
}

var uriSeq = regexp.MustCompile(`(?:^|[-_/?&])seq=(\d+)`)

// URINumber returns the origin's number in a "seq=N" part of a segment URI.
func URINumber(uri string) (uint64, bool) {
	m := uriSeq.FindStringSubmatch(uri)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	return n, err == nil
}

// parseSeconds parses a duration in seconds: finite and not negative, so
// it can never poison arithmetic or the JSON reports.
func parseSeconds(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}

// SelectVariant picks the variant to monitor: "highest" (default) or
// "lowest" bandwidth, a 0-based index, a WxH resolution, or a substring of
// the variant URI. Several matches resolve to the highest bandwidth.
func SelectVariant(vs []Variant, sel string) (Variant, error) {
	if len(vs) == 0 {
		return Variant{}, errors.New("no variants")
	}
	byBandwidth := func(a, b Variant) int { return cmp.Compare(a.Bandwidth, b.Bandwidth) }
	sel = strings.TrimSpace(sel)
	switch strings.ToLower(sel) {
	case "", "highest":
		return slices.MaxFunc(vs, byBandwidth), nil
	case "lowest":
		return slices.MinFunc(vs, byBandwidth), nil
	}
	if n, err := strconv.Atoi(sel); err == nil {
		if n < 0 || n >= len(vs) {
			return Variant{}, fmt.Errorf("variant index %d out of range (0-%d)", n, len(vs)-1)
		}
		return vs[n], nil
	}
	var matches []Variant
	for _, v := range vs {
		if isResolution(sel) && strings.EqualFold(v.Resolution, sel) || !isResolution(sel) && strings.Contains(v.URI, sel) {
			matches = append(matches, v)
		}
	}
	if len(matches) == 0 {
		var have []string
		for _, v := range vs {
			have = append(have, v.Label())
		}
		return Variant{}, fmt.Errorf("no variant matches %q (have %s)", sel, strings.Join(have, ", "))
	}
	return slices.MaxFunc(matches, byBandwidth), nil
}

func isResolution(s string) bool {
	w, h, ok := strings.Cut(strings.ToLower(s), "x")
	_, errW := strconv.Atoi(w)
	_, errH := strconv.Atoi(h)
	return ok && errW == nil && errH == nil
}

// Resolve resolves a playlist URI against the playlist's URL.
func Resolve(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

// DSNMismatch is a segment whose discontinuity sequence number differs
// between two snapshots of the same playlist.
type DSNMismatch struct {
	Seq          uint64 // the segment's Number
	URI          string
	Was, Now     uint64 // the segment's number in the older and newer snapshot
	PrevDSN, DSN uint64 // EXT-X-DISCONTINUITY-SEQUENCE of each snapshot
}

// CheckDiscontinuitySequence compares two snapshots of a media playlist.
// When the window slides, EXT-X-DISCONTINUITY-SEQUENCE may only change by
// the discontinuities that left with it, so every segment listed in both
// keeps its discontinuity number. Segments are matched by URI, since the
// origin may renumber its playlist. It returns the first segment whose
// number changed for a reason other than a tag removed from, or added to, a
// segment still listed (CheckUpdate reports those); snapshots with no
// segment in common can't be compared.
func CheckDiscontinuitySequence(prev, cur *Media) (DSNMismatch, bool) {
	old := make(map[string]int, len(prev.Segments))
	for i, s := range prev.Segments {
		old[s.URI] = i
	}
	_, m, bad := discontinuityChanges(prev, cur, old)
	return m, bad
}
