package monitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

const (
	statusPending    = "pending"
	statusFetched    = "fetched"
	statusFailed     = "failed"
	statusExpired    = "expired"
	statusNotListed  = "not_listed"
	statusMismatched = "mismatched"
)

// mediaMatchTolerance is how far apart two renditions' copies of a segment
// may start and still be the same media: far less than the shortest
// segment. The origin's renditions start on the same DTS.
const mediaMatchTolerance = ts.Hz / 2

// rendition is another variant of an incident's channel. Its segments with
// the faults' numbers are fetched while it still lists them and checked
// like the monitored rendition, which shows whether a fault is upstream of
// the packager (every rendition) or local to one. Segments are matched by
// the origin's number (hls.Segment.Number), then checked to start at the
// same media time as the monitored copy. Its playlist is fetched every poll
// while the incident is open, which is what a stall is judged by.
type rendition struct {
	variant hls.Variant
	url     string
	dir     string
	// blackMin is blackdetect's trigger_min: the shortest black run that is
	// a black_video fault.
	blackMin float64
	// colorRange is the channel's color_range.
	colorRange string

	// Guarded by incidents.mu.
	segs        map[uint64]*RenditionSegment
	data        map[uint64]renditionData
	faults      map[uint64][]analysis.Fault // the recheck's faults, by number
	runs        []blackRun                  // the recheck's black runs
	th          analysis.Thresholds         // the recheck's thresholds
	listings    []listing                   // successful playlist fetches, oldest first
	playlistErr string                      // why the last playlist fetch failed; "" when it didn't
}

// listing is one successful fetch of a rendition's playlist.
type listing struct {
	at     time.Time
	newest uint64 // the newest number it listed
}

// maxListings bounds a rendition's remembered playlist fetches: more than
// max_incident's worth at any poll interval the monitor uses.
const maxListings = 5000

// blackRun is a black run in PTS, joined across a rendition's adjacent
// segments.
type blackRun struct {
	start, end  uint64
	seconds     float64 // on screen
	frames      int     // black frames
	blackS      float64 // their time on screen
	undecoded   int     // frames inside it ffmpeg did not decode
	unconfirmed bool    // part of it is in a segment not fully checked
	segs        []uint64
}

type renditionData struct {
	seg    *analysis.Segment // nil when the bytes were not usable TS
	extinf float64
	disc   bool
	// prev is the number of the entry this rendition's playlist lists
	// right before this one, when it lists one.
	prev     *uint64
	black    []BlackInterval
	decode   BlackDecode
	blackErr error
	// notChecked says why the black check was incomplete or skipped.
	notChecked string
}

// expire marks a wanted segment that left the playlist before it could be
// fetched, keeping the last fetch error.
func expire(s *RenditionSegment) {
	msg := "left the playlist before it could be fetched"
	if s.Error != "" {
		msg += "; last error: " + s.Error
	}
	s.Status, s.Error = statusExpired, msg
}

// contentFaults are the fault types a segment's bytes are checked for.
var contentFaults = []string{
	analysis.FaultVideoDTSGap, analysis.FaultAudioPTSGap, analysis.FaultVideoDTSNotIncreasing,
	analysis.FaultPTSBehindPCR, analysis.FaultPTSPCRJump, analysis.FaultAVOffset,
	analysis.FaultDurationMismatch, analysis.FaultBlackVideo, analysis.FaultAudioCoverage,
	analysis.FaultVideoPTSError, analysis.FaultVideoPTSGap, analysis.FaultUndecodedFrames,
}

func (r *rendition) want(seqs []uint64) {
	for _, s := range seqs {
		if _, ok := r.segs[s]; !ok {
			r.segs[s] = &RenditionSegment{Seq: s, Status: statusPending}
		}
	}
}

// fetchRendition polls one other rendition until the incident closes.
func (in *incidents) fetchRendition(ctx context.Context, inc *incident, r *rendition) {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		inc.ch.log.Error("cannot create rendition directory", "error", err)
		return
	}
	for {
		if pending, open := in.pending(inc, r); open {
			inc.ch.safely("rendition "+r.variant.Label(), func() { in.fetchRenditionOnce(ctx, inc, r, pending) })
		}
		if !sleepCtx(ctx, inc.ch.pollInterval()) {
			return
		}
	}
}

// pending lists the wanted segments not fetched yet; open is false once the
// incident has closed.
func (in *incidents) pending(inc *incident, r *rendition) (out []uint64, open bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if inc.closed {
		return nil, false
	}
	for seq, s := range r.segs {
		if s.Status == statusPending || s.Status == statusFailed {
			out = append(out, seq)
		}
	}
	slices.Sort(out)
	return out, true
}

// update applies f to the incident's rendition state and rewrites the
// report, unless the incident has closed meanwhile.
func (in *incidents) update(inc *incident, f func()) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !inc.closed {
		f()
		in.write(inc)
	}
}

// fetchRenditionOnce fetches the rendition's playlist, remembers what it
// listed and fetches the pending segments it lists.
func (in *incidents) fetchRenditionOnce(ctx context.Context, inc *incident, r *rendition, pending []uint64) {
	target := inc.ch.targetDuration()
	body, meta, err := in.m.fetch(ctx, r.url, true, playlistTimeout(target))
	if ctx.Err() != nil {
		return
	}
	var pl *hls.Media
	if err == nil {
		if pl, err = hls.ParseMedia(body); err != nil {
			meta.Error = "parse: " + err.Error()
		}
	}
	name := "playlist_" + stamp(meta.RequestedAt)
	if pl != nil {
		name = playlistName(meta.RequestedAt, pl.MediaSequence)
	}
	if body != nil {
		if err := writeFileAtomic(filepath.Join(r.dir, name+playlistExt(meta)), body); err != nil {
			inc.ch.writeFailed("cannot save rendition playlist", name+playlistExt(meta), err)
		}
	}
	if err := writeJSON(filepath.Join(r.dir, name+".json"), meta); err != nil {
		inc.ch.writeFailed("cannot save rendition playlist metadata", name+".json", err)
	}
	now := in.m.now()
	func() {
		in.mu.Lock()
		defer in.mu.Unlock()
		stalled := inc.lastByType[analysis.FaultStall] != time.Time{}
		switch {
		case err != nil:
			r.playlistErr = err.Error()
			for _, seq := range pending {
				r.segs[seq].Error = "playlist: " + err.Error()
			}
		case len(pl.Segments) > 0:
			r.playlistErr = ""
			r.listings = append(r.listings, listing{at: now, newest: pl.Segments[len(pl.Segments)-1].Number()})
			if len(r.listings) > maxListings {
				r.listings = r.listings[1:]
			}
		}
		if !inc.closed && (err != nil && len(pending) > 0 || stalled) {
			in.write(inc)
		}
	}()
	if err != nil {
		return
	}
	segs := pl.Segments
	for _, seq := range pending {
		i := slices.IndexFunc(segs, func(s hls.Segment) bool { return s.Number() == seq })
		switch {
		case i >= 0:
			var prev *uint64
			if i > 0 {
				prev = new(segs[i-1].Number())
			}
			in.fetchRenditionSegment(ctx, inc, r, segs[i], prev, meta.base(), target)
		case len(segs) == 0 || seq > segs[len(segs)-1].Number():
			// Not listed yet; try again next poll.
		case seq < segs[0].Number():
			in.update(inc, func() { expire(r.segs[seq]) })
		default:
			in.update(inc, func() {
				s := r.segs[seq]
				s.Status, s.Error = statusNotListed, fmt.Sprintf("this rendition's playlist skips number %d", seq)
			})
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// fetchRenditionSegment downloads and checks one segment; prev is the
// entry listed before it and base the rendition playlist's URL after
// redirects.
func (in *incidents) fetchRenditionSegment(ctx context.Context, inc *incident, r *rendition, e hls.Segment, prev *uint64, base string, target time.Duration) {
	num := e.Number()
	url, err := hls.Resolve(base, e.URI)
	var body []byte
	var meta FetchMeta
	if err == nil {
		body, meta, err = in.m.fetch(ctx, url, false, segmentTimeout(target))
	}
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		in.update(inc, func() {
			s := r.segs[num]
			s.Status, s.Fetch, s.Error = statusFailed, &meta, err.Error()
		})
		return
	}
	file := segmentFile(num, e.URI) + ".ts"
	path := filepath.Join(r.dir, file)
	if err := writeFileAtomic(path, body); err != nil {
		inc.ch.writeFailed("cannot save rendition segment", path, err)
		file = ""
	}
	if err := writeJSON(filepath.Join(r.dir, segmentFile(num, e.URI)+".json"), meta); err != nil {
		inc.ch.writeFailed("cannot save rendition segment metadata", path, err)
	}
	f := in.m.checkRenditionSegment(ctx, r, e, prev, file, path, meta, body)
	if ctx.Err() != nil {
		return
	}
	in.update(inc, func() {
		inc.addRenditionSegment(r, f)
		r.recheck(in.m.cfg.Checks, inc.baseline)
	})
}

// renditionFetch is one segment fetched from another rendition, checked.
type renditionFetch struct {
	entry      hls.Segment // its entry in the rendition's playlist
	prev       *uint64     // the number of the entry listed before it
	file       string      // its file in the rendition directory; "" if not saved
	meta       FetchMeta
	seg        *analysis.Segment
	err        error // why the bytes are not usable TS
	black      []BlackInterval
	decode     BlackDecode
	blackErr   error
	notChecked string // why the black check was incomplete or skipped
}

// checkRenditionSegment analyzes a rendition's segment and runs blackdetect
// on its saved file at path.
func (m *Monitor) checkRenditionSegment(ctx context.Context, r *rendition, e hls.Segment, prev *uint64, file, path string, meta FetchMeta, body []byte) renditionFetch {
	f := renditionFetch{entry: e, prev: prev, file: file, meta: meta}
	f.seg, f.err = analysis.Analyze(body)
	switch {
	case f.err != nil || !m.cfg.Blackdetect.Enabled:
	case f.seg.Video == nil:
		f.notChecked = blackSkipped(f.seg, file)
	case file == "":
		f.blackErr = errors.New("the segment could not be saved for ffmpeg")
	default:
		res, err := m.blackRuns(ctx, path, f.seg, cmp.Or(r.variant.FrameTicks(), f.seg.Video.FrameTicks), r.colorRange)
		if nc, ok := errors.AsType[*notCheckedError](err); ok {
			f.notChecked, err = nc.why, nil
		}
		f.black, f.decode, f.blackErr = res.runs, res.decode, err
	}
	return f
}

// addRenditionSegment records a fetched segment of rendition r. A copy that
// does not start at the monitored copy's media time is marked mismatched
// and not checked. Called with incidents.mu held.
func (inc *incident) addRenditionSegment(r *rendition, f renditionFetch) {
	num := f.entry.Number()
	s := r.segs[num]
	if s == nil {
		s = &RenditionSegment{Seq: num}
		r.segs[num] = s
	}
	seg := f.seg
	s.Status, s.URI, s.File, s.Fetch, s.Black, s.Error = statusFetched, f.entry.URI, f.file, &f.meta, f.black, ""
	s.BlackDecode = nil
	switch {
	case f.blackErr == nil && f.seg != nil && f.seg.Video != nil && f.decode.Frames > 0:
		s.BlackDecode = new(f.decode)
	case f.notChecked != "":
		s.BlackDecode = &BlackDecode{NotChecked: f.notChecked}
	}
	switch {
	case f.err != nil:
		s.Error = f.err.Error()
		seg = nil
	case f.blackErr != nil:
		s.Error = "blackdetect: " + f.blackErr.Error()
	}
	if seg != nil {
		sum := seg.Summary()
		s.Analysis = &sum
		if off, ok := inc.mediaOffset(num, sum); ok {
			s.MediaOffsetMs = new(ts.Millis(off))
			if max(off, -off) > mediaMatchTolerance {
				// Same number, different media: comparing them would
				// say nothing about the fault.
				s.Status = statusMismatched
				s.Error = fmt.Sprintf("starts %+.3f s from the monitored rendition's segment %d: not the same media", float64(off)/ts.Hz, num)
				return
			}
		}
	}
	r.data[num] = renditionData{
		seg: seg, extinf: f.entry.Duration, disc: f.entry.Discontinuity, prev: f.prev,
		black: f.black, decode: f.decode, blackErr: f.blackErr, notChecked: f.notChecked,
	}
}

// mediaOffset is how far this rendition's copy of segment num starts from
// the monitored copy (first video DTS), when both are known. Called with
// in.mu held.
func (inc *incident) mediaOffset(num uint64, sum analysis.Summary) (int64, bool) {
	segs := inc.report.Segments
	i, found := slices.BinarySearchFunc(segs, num, func(s SegmentRecord, n uint64) int { return cmp.Compare(s.Seq, n) })
	if !found || sum.Video == nil {
		return 0, false
	}
	a := segs[i].Analysis
	if a == nil || a.Video == nil {
		return 0, false
	}
	return ts.Diff(sum.Video.FirstDTS, a.Video.FirstDTS), true
}

// checkVerdict says whether this rendition's copy of the fault's segments
// shows the same fault when checked the same way: reproduced (the same
// fault, at the same boundary, of about the same size), different (the same
// type of fault, but of another size or, for black, too short),
// not_reproduced, or why it can't say. values are the rendition's own
// measurements.
func (r *rendition) checkVerdict(f FaultRecord) (verdict string, values map[string]any) {
	s := r.segs[f.Seq]
	switch {
	case s == nil:
		return "not_requested", nil
	case s.Status != statusFetched:
		return s.Status, nil
	}
	d := r.data[f.Seq]
	if f.PrevSeq != nil {
		if d.prev != nil && *d.prev != *f.PrevSeq {
			return "inconclusive", map[string]any{
				"prev_seq": *d.prev, "reason": fmt.Sprintf("this rendition lists %d, not %d, before %d", *d.prev, *f.PrevSeq, f.Seq),
			}
		}
		switch prev, ok := r.data[*f.PrevSeq]; {
		case !ok:
			return "incomplete", nil
		case prev.seg == nil:
			return "inconclusive", nil // the previous segment was not usable TS
		}
	}
	switch {
	case slices.Contains(s.Unchecked, f.Type):
		return "inconclusive", nil
	case f.Type == analysis.FaultBlackVideo:
		return r.blackVerdict(f)
	case f.Type == analysis.FaultInvalidSegment:
		if d.seg == nil {
			return "reproduced", nil
		}
		return "not_reproduced", nil
	}
	i := slices.IndexFunc(r.faults[f.Seq], func(o analysis.Fault) bool { return o.Type == f.Type })
	if i < 0 {
		return "not_reproduced", r.measured(f)
	}
	own := r.faults[f.Seq][i]
	if key := sizeKey(f.Type); key != "" {
		want, ok1 := number(f.Values[key])
		got, ok2 := number(own.Values[key])
		if ok1 && ok2 && math.Abs(got-want) > sizeTolerance(key, want, r.th) {
			return "different", own.Values
		}
	}
	return "reproduced", own.Values
}

// sizeKey is the value that says how big a fault of this type is.
func sizeKey(typ string) string {
	switch typ {
	case analysis.FaultVideoDTSGap, analysis.FaultAudioPTSGap, analysis.FaultAVOffset, analysis.FaultVideoPTSGap:
		return "deviation_ms"
	case analysis.FaultAudioCoverage:
		return "short_ms"
	case analysis.FaultPTSPCRJump:
		return "jump_ms"
	case analysis.FaultDurationMismatch:
		return "diff_pct"
	}
	return ""
}

// sizeTolerance is how far a rendition's measurement may be from the
// monitored one's and still be the same fault: the continuity tolerance or
// a tenth of the size, whichever is more (one percentage point for
// durations).
func sizeTolerance(key string, want float64, th analysis.Thresholds) float64 {
	if key == "diff_pct" {
		return max(1, math.Abs(want)/10)
	}
	return max(th.ContinuityMs, math.Abs(want)/10)
}

// number reads a JSON-ish number.
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	}
	return 0, false
}

// measured is what this rendition shows at the fault's boundary when it
// has no such fault: the DTS step, or where its audio starts.
func (r *rendition) measured(f FaultRecord) map[string]any {
	if f.PrevSeq == nil {
		return nil
	}
	prev, cur := r.data[*f.PrevSeq].seg, r.data[f.Seq].seg
	if prev == nil || cur == nil {
		return nil
	}
	switch f.Type {
	case analysis.FaultVideoDTSGap:
		if prev.Video == nil || cur.Video == nil {
			return nil
		}
		frame := cmp.Or(r.variant.FrameTicks(), prev.Video.FrameTicks)
		gap := ts.Diff(cur.Video.FirstDTS(), prev.Video.LastDTS())
		return map[string]any{"gap_ms": round3(ts.Millis(gap)), "frame_ms": round3(ts.Millis(frame)), "deviation_ms": round3(ts.Millis(gap - frame))}
	case analysis.FaultAudioPTSGap:
		if prev.Audio == nil || cur.Audio == nil {
			return nil
		}
		return map[string]any{"deviation_ms": round3(ts.Millis(ts.Diff(cur.Audio.FirstPTS(), prev.Audio.End())))}
	}
	return nil
}

// blackVerdict: a black run in this rendition at least trigger_min long
// that overlaps the monitored run reproduces it; a shorter one is
// different. A fault recorded before runs were located falls back to
// whether this rendition has a black_video fault on the segment.
func (r *rendition) blackVerdict(f FaultRecord) (string, map[string]any) {
	start, ok1 := number(f.Values["run_start_pts"])
	end, ok2 := number(f.Values["run_end_pts"])
	if !ok1 || !ok2 {
		if slices.Contains(r.segs[f.Seq].Faults, analysis.FaultBlackVideo) {
			return "reproduced", nil
		}
		return "not_reproduced", nil
	}
	var best *blackRun
	for i := range r.runs {
		run := &r.runs[i]
		overlaps := ts.Diff(run.start, uint64(end)) < 0 && ts.Diff(uint64(start), run.end) < 0
		if overlaps && (best == nil || run.blackS > best.blackS) {
			best = run
		}
	}
	unchecked := r.data[f.Seq].notChecked != ""
	if best == nil {
		if unchecked {
			return "inconclusive", nil // not finding black there proves nothing
		}
		return "not_reproduced", nil
	}
	frame := cmp.Or(r.variant.FrameTicks(), fallbackFrameTicks)
	values := map[string]any{
		"longest_run_s": round3(best.seconds), "run_start_pts": best.start, "run_end_pts": best.end,
		"black_frames": best.frames, "black_frames_s": round3(best.blackS), "undecoded_frames": best.undecoded,
		"no_frame_s": round3(noFrame(best.seconds, best.blackS, best.undecoded, frame)),
		"segments":   best.segs,
	}
	if best.unconfirmed {
		values["unconfirmed"] = true
	}
	switch {
	case best.blackS >= r.blackMin:
		return "reproduced", values
	case unchecked:
		return "inconclusive", values
	}
	return "different", values
}

// stallVerdict judges a stall by this rendition's playlist while the
// monitored one was stalled (from the fault to ended, or to now while it
// lasts): listing a newer segment means it was not stalled; listing
// nothing newer means it was too. With no successful fetch in that time it
// is pending while the incident is open, and inconclusive once closed.
func (r *rendition) stallVerdict(f FaultRecord, ended time.Time, closed bool) (string, map[string]any) {
	fetches, newest := 0, uint64(0)
	for _, l := range r.listings {
		if l.at.Before(f.DetectedAt) || !ended.IsZero() && l.at.After(ended) {
			continue
		}
		if l.newest > f.Seq {
			return "not_reproduced", map[string]any{"newest_seq": l.newest, "listed_at": l.at.UTC()}
		}
		fetches++
		newest = max(newest, l.newest)
	}
	if fetches > 0 {
		return "reproduced", map[string]any{"newest_seq": newest, "fetches": fetches}
	}
	values := map[string]any{"fetches": 0}
	if r.playlistErr != "" {
		values["last_error"] = r.playlistErr
	}
	if closed {
		return "inconclusive", values
	}
	return "pending", values
}

// unavailableVerdict: does this rendition's origin refuse the same segment?
func (r *rendition) unavailableVerdict(seq uint64) string {
	s := r.segs[seq]
	switch {
	case s == nil:
		return "not_requested"
	case s.Status == statusFetched:
		return "not_reproduced"
	case s.Fetch != nil && refused(s.Fetch.Status):
		return "reproduced"
	}
	return s.Status
}

// tagVerdict: does this rendition's playlist tag the same segment with
// EXT-X-DISCONTINUITY?
func (r *rendition) tagVerdict(seq uint64) string {
	s := r.segs[seq]
	d, fetched := r.data[seq]
	switch {
	case s == nil:
		return "not_requested"
	case !fetched:
		return s.Status
	case d.disc:
		return "reproduced"
	}
	return "not_reproduced"
}

// recheck runs the monitored rendition's checks over everything fetched
// from this one. The A/V offset is compared with the monitored channel's
// baseline, and video against this rendition's own FRAME-RATE when its
// master playlist entry has one. Black runs are joined across adjacent
// segments; a segment is marked black_video when a run through it is at
// least trigger_min long.
//
// Two segments are compared when this rendition's playlist lists them one
// after the other; when that is unknown, when their numbers are consecutive.
func (r *rendition) recheck(th analysis.Thresholds, baseline *int64) {
	th.BaselineSegments = 0
	r.th = th
	chain := analysis.NewChain(th)
	tol := int64(math.Round(th.AVOffsetMs * ts.Hz / 1000))
	r.faults, r.runs = map[uint64][]analysis.Fault{}, nil
	open := -1 // the run that reached the end of the previous segment
	var last *uint64
	for _, seq := range slices.Sorted(maps.Keys(r.data)) {
		d := r.data[seq]
		var order *analysis.Order
		adjacent := last != nil && *last+1 == seq
		if d.prev != nil {
			adjacent = last != nil && *last == *d.prev
			order = &analysis.Order{Adjacent: adjacent}
		}
		last = new(seq)
		var unchecked []string
		if d.seg == nil {
			chain.Break(seq)
			r.faults[seq] = []analysis.Fault{{Type: analysis.FaultInvalidSegment, Seq: seq}}
			r.segs[seq].Unchecked = contentFaults
			open = -1
			continue
		}
		if baseline == nil {
			unchecked = append(unchecked, analysis.FaultAVOffset)
		}
		res := chain.Add(analysis.Input{
			Seq: seq, ExtInf: d.extinf, Discontinuity: d.disc, Segment: d.seg, Order: order,
			FrameTicks: r.variant.FrameTicks(),
		})
		faults := res.Faults
		if off, ok := d.seg.AVOffset(); ok && baseline != nil && max(off-*baseline, *baseline-off) > tol {
			faults = append(faults, analysis.Fault{Type: analysis.FaultAVOffset, Seq: seq, Values: map[string]any{
				"offset_ms": round3(ts.Millis(off)), "baseline_ms": round3(ts.Millis(*baseline)), "deviation_ms": round3(ts.Millis(off - *baseline)),
			}})
		}
		if d.blackErr == nil && d.decode.Undecoded > 0 {
			faults = append(faults, undecodedFault(seq, d.decode))
		}
		r.faults[seq] = faults
		if d.blackErr != nil {
			unchecked = append(unchecked, analysis.FaultBlackVideo)
			open = -1
		} else {
			open = r.addBlack(seq, d.black, adjacent && !d.disc, open)
		}
		r.segs[seq].Unchecked = unchecked
	}
	for seq := range r.data {
		var types []string
		for _, f := range r.faults[seq] {
			types = append(types, f.Type)
		}
		if slices.ContainsFunc(r.runs, func(b blackRun) bool { return b.blackS >= r.blackMin && slices.Contains(b.segs, seq) }) {
			types = append(types, analysis.FaultBlackVideo)
		}
		r.segs[seq].Faults = types
	}
}

// addBlack adds a segment's black runs to r.runs. The first joins the run
// open at the end of the previous segment when the segments are adjacent
// and joinsBlack says it goes on. It returns the run that lasts to this
// segment's end, or -1.
func (r *rendition) addBlack(seq uint64, black []BlackInterval, adjacent bool, open int) int {
	frame := cmp.Or(r.variant.FrameTicks(), fallbackFrameTicks)
	ends := -1
	for i, b := range black {
		var at int
		if i == 0 && open >= 0 && adjacent && b.FromStart && joinsBlack(r.runs[open].end, b.StartPTS, frame) {
			at = open
			run := &r.runs[at]
			run.end, run.frames, run.segs = b.EndPTS, run.frames+b.Frames, append(run.segs, seq)
			run.blackS, run.undecoded = run.blackS+b.BlackS, run.undecoded+b.UndecodedFrames
			run.unconfirmed = run.unconfirmed || b.Unconfirmed
			run.seconds = float64(ts.Diff(run.end, run.start)) / ts.Hz
		} else {
			r.runs = append(r.runs, blackRun{
				start: b.StartPTS, end: b.EndPTS, seconds: b.Duration, frames: b.Frames,
				blackS: b.BlackS, undecoded: b.UndecodedFrames, unconfirmed: b.Unconfirmed, segs: []uint64{seq},
			})
			at = len(r.runs) - 1
		}
		if b.ToEnd {
			ends = at
		}
	}
	return ends
}
