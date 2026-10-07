package hls

import (
	"fmt"
	"slices"
	"strings"
)

// Reasons a playlist update breaks the rules for a live playlist (RFC 8216
// section 6.2.1 and 6.2.2), or the origin's own numbering.
const (
	// ViolationRenumbered: a listed segment moved to another position.
	ViolationRenumbered = "renumbered"
	// ViolationSkippedNumber: a new entry's URI number is not one more
	// than the entry before it.
	ViolationSkippedNumber = "skipped_number"
	// ViolationTargetDuration: EXT-X-TARGETDURATION changed.
	ViolationTargetDuration = "target_duration_changed"
	// ViolationRewrittenEntry: an entry already listed changed its URI,
	// EXTINF or tags (the first entry's cue tags aside).
	ViolationRewrittenEntry = "rewritten_entry"
	// ViolationFirstEntryCues: the entry that became first gained cue tags,
	// perhaps losing others: the origin re-tags the head of a break, CUE-OUT
	// becoming CUE-OUT-CONT.
	ViolationFirstEntryCues = "first_entry_cue_rewritten"
	// ViolationDiscontinuitySequence: EXT-X-DISCONTINUITY-SEQUENCE changed
	// by something other than the discontinuities that left the playlist.
	ViolationDiscontinuitySequence = "discontinuity_sequence"
	// ViolationDiscontinuityTagDropped: EXT-X-DISCONTINUITY was removed
	// from a segment still listed, without incrementing
	// EXT-X-DISCONTINUITY-SEQUENCE.
	ViolationDiscontinuityTagDropped = "discontinuity_tag_dropped"
)

// Violation is one way a playlist update breaks the rules.
type Violation struct {
	Reason string
	// Seq and URI are the entry concerned, the first when there are several.
	// Seq is its Number.
	Seq    uint64
	URI    string
	Detail string
	Values map[string]any
}

// maxListed bounds the entries a violation lists.
const maxListed = 10

// CheckUpdate compares a media playlist with the previous snapshot of it
// (nil for the first one seen) and returns what the update does that a
// live playlist may not. Segments are matched by URI. The origin's practice
// of dropping cue tags from the entry that becomes first is not reported;
// cue tags that entry gains are, with a reason of their own.
// A MEDIA-SEQUENCE that went backward is the caller's to report: the
// snapshots are then not compared.
func CheckUpdate(prev, cur *Media) []Violation {
	var out []Violation
	if prev == nil {
		if v, ok := skippedNumbers(cur, nil); ok {
			out = append(out, v)
		}
		return out
	}
	if cur.MediaSequence < prev.MediaSequence {
		return nil
	}
	old := make(map[string]int, len(prev.Segments))
	for i, s := range prev.Segments {
		old[s.URI] = i
	}
	moved, isRenumbered := renumbered(prev, cur, old)
	if isRenumbered {
		out = append(out, moved)
	}
	if v, ok := skippedNumbers(cur, old); ok {
		out = append(out, v)
	}
	if prev.TargetDuration != cur.TargetDuration && len(cur.Segments) > 0 {
		out = append(out, targetChange(prev, cur))
	}
	if v, ok := rewritten(prev, cur, old, isRenumbered); ok {
		out = append(out, v)
	}
	if v, ok := firstEntryCues(prev, cur, old); ok {
		out = append(out, v)
	}
	dropped, m, bad := discontinuityChanges(prev, cur, old)
	if bad {
		out = append(out, Violation{
			Reason: ViolationDiscontinuitySequence, Seq: m.Seq, URI: m.URI,
			Detail: fmt.Sprintf("EXT-X-DISCONTINUITY-SEQUENCE went %d -> %d, which the discontinuities that left the playlist don't explain: segment %d's discontinuity number changed %d -> %d",
				m.PrevDSN, m.DSN, m.Seq, m.Was, m.Now),
			Values: map[string]any{
				"was": m.Was, "now": m.Now, "prev_dsn": m.PrevDSN, "dsn": m.DSN,
				"prev_msn": prev.MediaSequence, "msn": cur.MediaSequence,
			},
		})
	}
	if len(dropped) > 0 {
		out = append(out, tagDropped(prev, cur, dropped))
	}
	return out
}

// renumbered reports listed segments that moved to another position.
func renumbered(prev, cur *Media, old map[string]int) (Violation, bool) {
	var first *Segment
	var was uint64
	moved := 0
	listed := make(map[string]bool, len(cur.Segments))
	for i := range cur.Segments {
		s := &cur.Segments[i]
		listed[s.URI] = true
		j, ok := old[s.URI]
		if !ok || prev.Segments[j].Seq == s.Seq {
			continue
		}
		moved++
		if first == nil {
			first, was = s, prev.Segments[j].Seq
		}
	}
	if first == nil {
		return Violation{}, false
	}
	left := 0
	for _, s := range prev.Segments {
		if !listed[s.URI] {
			left++
		}
	}
	shift := int64(first.Seq) - int64(was)
	return Violation{
		Reason: ViolationRenumbered, Seq: first.Number(), URI: first.URI,
		Detail: fmt.Sprintf("the origin renumbered its playlist: EXT-X-MEDIA-SEQUENCE went %d -> %d while %d segment(s) left, so %d listed segment(s) moved %+d position(s) (%s was at %d, now at %d)",
			prev.MediaSequence, cur.MediaSequence, left, moved, shift, first.URI, was, first.Seq),
		Values: map[string]any{
			"prev_msn": prev.MediaSequence, "msn": cur.MediaSequence, "left": left, "moved": moved,
			"shift": shift, "prev_position": was, "position": first.Seq,
		},
	}, true
}

// skippedNumbers reports new entries whose URI number is more than one
// past the entry listed before them, in the same directory: numbers that
// packager never used. Numbers are each packager's own convention, and
// HLS doesn't require them to be unique, so a number that repeats or goes
// back, or one from another directory (an inserted ad), is no violation.
// Entries in old were checked when they were new.
func skippedNumbers(cur *Media, old map[string]int) (Violation, bool) {
	var v Violation
	var skips []map[string]any
	var parts []string
	for i := 1; i < len(cur.Segments); i++ {
		a, b := &cur.Segments[i-1], &cur.Segments[i]
		if _, known := old[b.URI]; known || !a.HasURISeq || !b.HasURISeq || b.URISeq <= a.URISeq+1 || Dir(a.URI) != Dir(b.URI) {
			continue
		}
		if len(skips) == 0 {
			v.Seq, v.URI = b.URISeq, b.URI
		}
		if len(skips) < maxListed {
			skips = append(skips, map[string]any{"after": a.URISeq, "seq": b.URISeq, "uri": b.URI})
		}
		switch {
		case b.URISeq == a.URISeq+2:
			parts = append(parts, fmt.Sprintf("number %d was never used: seq=%d follows seq=%d", a.URISeq+1, b.URISeq, a.URISeq))
		default:
			parts = append(parts, fmt.Sprintf("numbers %d-%d were never used: seq=%d follows seq=%d", a.URISeq+1, b.URISeq-1, b.URISeq, a.URISeq))
		}
	}
	if len(skips) == 0 {
		return Violation{}, false
	}
	v.Reason = ViolationSkippedNumber
	v.Detail = "the origin skipped segment numbers: " + strings.Join(parts, "; ")
	v.Values = map[string]any{"skips": skips}
	return v, true
}

// targetChange reports a changed EXT-X-TARGETDURATION, with the longest
// EXTINF listed, which is usually why.
func targetChange(prev, cur *Media) Violation {
	longest := &cur.Segments[0]
	for i := range cur.Segments {
		if cur.Segments[i].Duration > longest.Duration {
			longest = &cur.Segments[i]
		}
	}
	return Violation{
		Reason: ViolationTargetDuration, Seq: longest.Number(), URI: longest.URI,
		Detail: fmt.Sprintf("EXT-X-TARGETDURATION changed %g -> %g, which a live playlist may not do (the longest listed EXTINF is %.3f s, %s)",
			prev.TargetDuration, cur.TargetDuration, longest.Duration, longest.URI),
		Values: map[string]any{
			"prev_target_s": prev.TargetDuration, "target_s": cur.TargetDuration,
			"longest_extinf_s": longest.Duration, "prev_msn": prev.MediaSequence, "msn": cur.MediaSequence,
		},
	}
}

// rewritten reports entries already listed whose EXTINF or tags changed,
// and new URIs listed at a position (media sequence number) an old URI
// had. A dropped EXT-X-DISCONTINUITY is left to discontinuityChanges (a
// gained one is a rewrite), and the cue tags of the entry that became
// first to firstEntryCues. Positions are only compared when the playlist
// was not renumbered; numbers in URIs never are, since they need not be
// unique.
func rewritten(prev, cur *Media, old map[string]int, renumbered bool) (Violation, bool) {
	listed := make(map[string]bool, len(cur.Segments))
	for _, s := range cur.Segments {
		listed[s.URI] = true
	}
	byPos := make(map[uint64]*Segment, len(prev.Segments))
	for i := range prev.Segments {
		if !renumbered {
			byPos[prev.Segments[i].Seq] = &prev.Segments[i]
		}
	}
	var entries []map[string]any
	var parts []string
	var first *Segment
	for i := range cur.Segments {
		s := &cur.Segments[i]
		var what []string
		if j, ok := old[s.URI]; ok {
			p := &prev.Segments[j]
			if p.Duration != s.Duration {
				what = append(what, fmt.Sprintf("EXTINF %g -> %g", p.Duration, s.Duration))
			}
			if !p.Discontinuity && s.Discontinuity {
				what = append(what, "gained #EXT-X-DISCONTINUITY")
			}
			gone, added := tagChanges(p.Tags, s.Tags)
			if i == 0 {
				gone = slices.DeleteFunc(gone, IsSCTE35Tag)
				added = slices.DeleteFunc(added, IsSCTE35Tag)
			}
			for _, t := range gone {
				what = append(what, "lost "+t)
			}
			for _, t := range added {
				what = append(what, "gained "+t)
			}
		} else if p, ok := byPos[s.Seq]; ok && !listed[p.URI] {
			what = append(what, fmt.Sprintf("URI %s -> %s", p.URI, s.URI))
		}
		if len(what) == 0 {
			continue
		}
		if first == nil {
			first = s
		}
		if len(entries) < maxListed {
			entries = append(entries, map[string]any{"seq": s.Number(), "uri": s.URI, "changes": what})
			parts = append(parts, fmt.Sprintf("%d: %s", s.Number(), strings.Join(what, ", ")))
		}
	}
	if first == nil {
		return Violation{}, false
	}
	return Violation{
		Reason: ViolationRewrittenEntry, Seq: first.Number(), URI: first.URI,
		Detail: "the origin changed entries it had already listed: " + strings.Join(parts, "; "),
		Values: map[string]any{"entries": entries, "prev_msn": prev.MediaSequence, "msn": cur.MediaSequence},
	}, true
}

// firstEntryCues reports cue tags gained by the entry that became first:
// the origin re-tags the head of a break, CUE-OUT becoming CUE-OUT-CONT.
// Cue tags it only lost are the origin's usual practice, not reported.
func firstEntryCues(prev, cur *Media, old map[string]int) (Violation, bool) {
	if len(cur.Segments) == 0 {
		return Violation{}, false
	}
	s := &cur.Segments[0]
	j, ok := old[s.URI]
	if !ok {
		return Violation{}, false
	}
	gone, added := tagChanges(prev.Segments[j].Tags, s.Tags)
	notCue := func(t string) bool { return !IsSCTE35Tag(t) }
	gone, added = slices.DeleteFunc(gone, notCue), slices.DeleteFunc(added, notCue)
	if len(added) == 0 {
		return Violation{}, false
	}
	var what []string
	for _, t := range gone {
		what = append(what, "lost "+t)
	}
	for _, t := range added {
		what = append(what, "gained "+t)
	}
	values := map[string]any{"gained": added, "prev_msn": prev.MediaSequence, "msn": cur.MediaSequence}
	if len(gone) > 0 {
		values["lost"] = gone
	}
	return Violation{
		Reason: ViolationFirstEntryCues, Seq: s.Number(), URI: s.URI,
		Detail: fmt.Sprintf("the origin rewrote the cue tags of the entry that became first, %d: %s", s.Number(), strings.Join(what, ", ")),
		Values: values,
	}, true
}

// tagChanges returns the tags only a has and only b has, ignoring
// EXT-X-DISCONTINUITY, which discontinuityChanges looks at.
func tagChanges(a, b []string) (gone, added []string) {
	count := map[string]int{}
	for _, t := range a {
		count[t]++
	}
	for _, t := range b {
		count[t]--
	}
	for _, t := range a {
		if count[t] > 0 && t != "#EXT-X-DISCONTINUITY" {
			gone = append(gone, t)
			count[t]--
		}
	}
	for _, t := range b {
		if count[t] < 0 && t != "#EXT-X-DISCONTINUITY" {
			added = append(added, t)
			count[t]++
		}
	}
	return gone, added
}

// discontinuityChanges compares the discontinuity numbers of the segments
// listed in both snapshots. A segment keeps its number when
// EXT-X-DISCONTINUITY-SEQUENCE counts every tag that left, with its segment
// or on its own. dropped lists the segments that lost their tag while the
// sequence did not count it; mismatch is the first segment whose number
// changed for another reason.
func discontinuityChanges(prev, cur *Media, old map[string]int) (dropped []*Segment, mismatch DSNMismatch, bad bool) {
	var drops, adds int64
	for i := range cur.Segments {
		s := &cur.Segments[i]
		j, ok := old[s.URI]
		if !ok {
			continue
		}
		p := &prev.Segments[j]
		lost := p.Discontinuity && !s.Discontinuity
		if lost {
			drops++
		}
		if !p.Discontinuity && s.Discontinuity {
			adds++
		}
		d := int64(s.DiscSeq) - int64(p.DiscSeq)
		switch {
		case d == 0:
			// Stable: the sequence counted every tag that left.
		case d == adds-drops:
			// The tags that changed on listed segments explain it.
			if lost {
				dropped = append(dropped, s)
			}
		case !bad:
			mismatch, bad = DSNMismatch{
				Seq: s.Number(), URI: s.URI, Was: p.DiscSeq, Now: s.DiscSeq,
				PrevDSN: prev.DiscontinuitySequence, DSN: cur.DiscontinuitySequence,
			}, true
		}
	}
	return dropped, mismatch, bad
}

// tagDropped reports EXT-X-DISCONTINUITY tags removed from segments still
// listed without incrementing EXT-X-DISCONTINUITY-SEQUENCE.
func tagDropped(prev, cur *Media, dropped []*Segment) Violation {
	s := dropped[0]
	where := ""
	if s.URI == cur.Segments[0].URI {
		where = " (now the first entry)"
	}
	dsn := fmt.Sprintf("was not incremented (%d -> %d)", prev.DiscontinuitySequence, cur.DiscontinuitySequence)
	if !cur.HasDiscontinuitySequence {
		dsn = "is not sent at all"
	}
	var seqs []uint64
	for _, d := range dropped {
		seqs = append(seqs, d.Number())
	}
	return Violation{
		Reason: ViolationDiscontinuityTagDropped, Seq: s.Number(), URI: s.URI,
		Detail: fmt.Sprintf("EXT-X-DISCONTINUITY was removed from segment %d%s while it is still listed, and EXT-X-DISCONTINUITY-SEQUENCE %s, so its discontinuity number changed",
			s.Number(), where, dsn),
		Values: map[string]any{
			"segments": seqs, "first_entry": where != "", "dsn_declared": cur.HasDiscontinuitySequence,
			"prev_dsn": prev.DiscontinuitySequence, "dsn": cur.DiscontinuitySequence,
			"prev_msn": prev.MediaSequence, "msn": cur.MediaSequence,
		},
	}
}
