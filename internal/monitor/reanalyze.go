package monitor

import (
	"cmp"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/hls"
)

// reanalyzeFile is the report reanalyze writes next to an incident's
// report.json.
const reanalyzeFile = "report.v2.json"

// statusNotSaved is a rendition segment a fault needs that the original run
// never fetched from that rendition, so it cannot be checked now.
const statusNotSaved = "not_saved"

// Reanalyze runs the monitor's checks again on an incident it saved, and
// writes what they find to report.v2.json in the incident directory. Nothing
// else there changes: the playlists, segments and report.json stay as they
// are, and report.v2.json lists them all with their SHA-256.
//
// The saved playlists are replayed in the order they were fetched, through
// the same code the poller runs, and each segment they queue is checked
// from its saved file, found by its URI, as the worker checks it. Faults are
// timed when their playlist or segment arrived. The other renditions'
// verdicts come from their saved playlists and segments; a segment a fault
// needs that the original run never fetched from a rendition is not_saved.
// Unlike the live monitor, reanalyze reports every fault it finds: nothing
// is suppressed, and no second incident is split off.
//
// o.Config supplies the checks' thresholds and blackdetect's settings; its
// data directory and channels are not used.
func Reanalyze(ctx context.Context, dir string, o Options) (Report, error) {
	old, err := loadReport(dir)
	if err != nil {
		return Report{}, err
	}
	// The incident's channel, with its color_range when the config has it.
	ch := config.Channel{Name: old.Channel, URL: old.Stream.URL}
	if i := slices.IndexFunc(o.Config.Channels, func(c config.Channel) bool { return c.Name == old.Channel }); i >= 0 {
		ch.ColorRange = o.Config.Channels[i].ColorRange
	}
	o.Config.Channels = []config.Channel{ch}
	o.Config.DataDir = dir
	m, err := New(o)
	if err != nil {
		return Report{}, err
	}
	c := m.channels[0]
	c.dir = filepath.Join(dir, kindSegments) // where check() finds the files for blackdetect
	fetches, err := loadFetches(filepath.Join(dir, kindPlaylists))
	if err != nil {
		return Report{}, err
	}
	rp := &replay{ctx: ctx, dir: dir, c: c, fetches: fetches}
	rp.inc = &incident{
		id: old.ID, dir: dir, ch: c, opened: old.OpenedAt, closed: true,
		lastByType: map[string]time.Time{}, prevByType: map[string]time.Time{}, pending: map[string]time.Time{},
		report: Report{
			ID: old.ID, Channel: old.Channel, Status: old.Status, OpenedAt: old.OpenedAt.UTC(),
			ClosedAt: old.ClosedAt.UTC(), CloseReason: old.CloseReason, Stream: old.Stream,
			Blackdetect: m.blackdetectInfo(ch.ColorRange),
		},
	}
	var fetchedAt map[uint64]time.Time
	if rp.segs, fetchedAt, rp.notes, err = loadSegments(filepath.Join(dir, kindSegments), fetches); err != nil {
		return Report{}, err
	}
	if err := rp.run(); err != nil {
		return Report{}, err
	}
	// A manual capture is not in the media; it stays as it was.
	for _, f := range old.Faults {
		if f.Type == analysis.FaultManual {
			rp.fault(analysis.Fault{Type: f.Type, Seq: f.Seq, URI: f.URI, Message: f.Message, Values: f.Values}, f.DetectedAt)
		}
	}
	inc := rp.inc
	if c.baseline != nil {
		inc.baseline = new(*c.baseline)
		inc.report.Stream.AVBaselineMs = new(round3(float64(*c.baseline) / 90))
	}
	if c.target > 0 {
		inc.report.Stream.TargetDurationS = c.target.Seconds()
	}
	if err := rp.renditions(old); err != nil {
		return Report{}, err
	}
	if n := c.st.blackErrors; n > 0 {
		rp.notes = append(rp.notes, fmt.Sprintf("blackdetect failed on %d segments: black runs through them are not checked", n))
	}
	if n := c.st.blackNotChecked; n > 0 {
		rp.notes = append(rp.notes, fmt.Sprintf("black was not fully checked on %d segment(s): their records' black_decode says why, and black found in them is unconfirmed", n))
	}
	inc.notes = rp.notes
	inc.refresh()
	opened := openedBy(old, fetchedAt)
	var pre []FaultRecord
	for i := range inc.report.Faults {
		if f := &inc.report.Faults[i]; f.DetectedAt.Before(opened) {
			f.PreRoll = true
			pre = append(pre, *f)
		}
	}
	summary := "original report.json: " + FaultSummary(old.Faults) + "; reanalysis: " + FaultSummary(inc.report.Faults)
	if len(pre) > 0 {
		summary += fmt.Sprintf(", of which %s in the pre-roll, found before the original incident opened (marked pre_roll)", FaultSummary(pre))
	}
	for i := range inc.report.OriginNotes {
		if n := &inc.report.OriginNotes[i]; n.DetectedAt.Before(opened) {
			n.PreRoll = true
		}
	}
	if n := inc.report.OriginNotes; len(n) > 0 {
		summary += "; and " + noteSummary(n)
	}
	inc.report.Notes = append([]string{
		"reanalyzed at " + time.Now().UTC().Format(time.RFC3339) + " by stream-analyzer reanalyze, from this directory's saved playlists and segments; report.json is the original report",
		summary,
	}, inc.report.Notes...)
	for _, f := range inc.report.Faults {
		if f.DetectedAt.After(inc.report.LastFaultAt) {
			inc.report.LastFaultAt = f.DetectedAt
		}
	}
	if inc.report.Files, err = manifest(dir, reanalyzeFile); err != nil {
		return Report{}, fmt.Errorf("listing the evidence files: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, reanalyzeFile), &inc.report); err != nil {
		return Report{}, err
	}
	return inc.report, nil
}

// openedBy is when the original incident's first fault was found, on the
// replay's clock: the fetch of the segment it is on (the replay times a
// segment's faults by its fetch), or a second before opened_at for a fault
// found in a playlist or a manual capture. fetchedAt maps the numbers the
// saved records use to their fetch times.
func openedBy(old Report, fetchedAt map[uint64]time.Time) time.Time {
	if len(old.Faults) == 0 {
		return old.OpenedAt
	}
	first := slices.MinFunc(old.Faults, func(a, b FaultRecord) int { return a.DetectedAt.Compare(b.DetectedAt) })
	segment := first.Type != analysis.FaultStall && first.Type != analysis.FaultManual && !playlistLevel(first.Type, first.Values)
	if at, ok := fetchedAt[first.Seq]; segment && ok {
		return at
	}
	return old.OpenedAt.Add(-time.Second)
}

// loadReport reads an incident's report.json, in any version the monitor
// has written.
func loadReport(dir string) (Report, error) {
	b, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return Report{}, fmt.Errorf("not an incident directory: %w", err)
	}
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return Report{}, fmt.Errorf("report.json: %w", err)
	}
	if r.ID == "" || r.Channel == "" {
		return Report{}, errors.New("report.json names no incident or channel")
	}
	return r, nil
}

// noteSummary counts origin notes by reason: "2 origin notes (...)".
func noteSummary(notes []OriginNote) string {
	n := map[string]int{}
	for _, o := range notes {
		n[o.Reason]++
	}
	var parts []string
	for _, r := range slices.Sorted(maps.Keys(n)) {
		parts = append(parts, fmt.Sprintf("%s %d", r, n[r]))
	}
	return fmt.Sprintf("%d origin notes (%s)", len(notes), strings.Join(parts, ", "))
}

// FaultSummary counts faults by type: "3 faults (audio_pts_gap 1, ...)".
func FaultSummary(faults []FaultRecord) string {
	n := map[string]int{}
	for _, f := range faults {
		n[f.Type]++
	}
	var parts []string
	for _, typ := range slices.Sorted(maps.Keys(n)) {
		parts = append(parts, fmt.Sprintf("%s %d", typ, n[typ]))
	}
	s := fmt.Sprintf("%d faults", len(faults))
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

// savedFetch is a playlist fetch saved in an incident: its metadata and,
// when the fetch returned one, its body.
type savedFetch struct {
	name string // file name without extension
	meta FetchMeta
	at   time.Time  // when it arrived
	pl   *hls.Media // the playlist, when the origin sent one that parses
}

// loadFetches reads the playlist fetches saved in dir, oldest first.
func loadFetches(dir string) ([]savedFetch, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []savedFetch
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !strings.HasPrefix(name, "playlist_") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		f := savedFetch{name: name}
		if err := json.Unmarshal(b, &f.meta); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		f.at = f.meta.CompletedAt
		if f.at.IsZero() {
			f.at = f.meta.RequestedAt
		}
		// Only what the origin answered with 2xx was ever a playlist; one
		// the parser once rejected is parsed again with today's.
		fetched := f.meta.Error == "" || strings.HasPrefix(f.meta.Error, "parse: ")
		if f.meta.Status >= 200 && f.meta.Status <= 299 && fetched {
			if body, err := os.ReadFile(filepath.Join(dir, name+".m3u8")); err == nil {
				f.pl, _ = hls.ParseMedia(body)
			}
		}
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b savedFetch) int {
		return cmp.Or(a.meta.RequestedAt.Compare(b.meta.RequestedAt), strings.Compare(a.name, b.name))
	})
	return out, nil
}

// savedSegment is a monitored segment saved in an incident.
type savedSegment struct {
	file string         // its .ts file; "" when only its record was saved
	rec  *SegmentRecord // its record; nil when none was saved
}

// segFile matches a segment's file: seg_<number>_<hash of its URI>, or
// seg_<number> as earlier versions named them. Group 1 is the number,
// group 3 the extension.
var segFile = regexp.MustCompile(`^seg_(\d+)(_[0-9a-f]{8})?\.(ts|json)$`)

// loadSegments finds each saved segment by its URI. Its record (the .json
// sidecar) names the URI. Before the origin's numbers were used, files were
// named by playlist position, and a renumbered playlist could make the
// monitor download a segment twice: the first copy is used, and the second
// is noted. A file without a record takes its URI from the first saved
// playlist that lists its number (a position, in an incident saved before
// records had an msn). fetchedAt maps every record's number, as it was
// saved, to when its fetch completed.
func loadSegments(dir string, fetches []savedFetch) (segs map[string]*savedSegment, fetchedAt map[uint64]time.Time, notes []string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]*savedSegment{}, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	type sidecar struct {
		base string
		rec  SegmentRecord
	}
	var recs []sidecar
	files := map[string]bool{}
	byPosition := true
	for _, e := range entries {
		m := segFile.FindStringSubmatch(e.Name())
		switch {
		case m == nil:
			continue
		case m[3] == "ts":
			files[e.Name()] = true
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, nil, nil, err
		}
		var probe struct {
			MSN *uint64 `json:"msn"`
		}
		s := sidecar{base: strings.TrimSuffix(e.Name(), ".json")}
		if err := json.Unmarshal(b, &s.rec); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if json.Unmarshal(b, &probe) == nil && probe.MSN != nil {
			byPosition = false
		}
		recs = append(recs, s)
	}
	slices.SortStableFunc(recs, func(a, b sidecar) int {
		return cmp.Or(a.rec.Fetch.RequestedAt.Compare(b.rec.Fetch.RequestedAt), strings.Compare(a.base, b.base))
	})
	out := map[string]*savedSegment{}
	fetchedAt = map[uint64]time.Time{}
	used := map[string]bool{}
	for _, s := range recs {
		if at := s.rec.Fetch.CompletedAt; !at.IsZero() {
			fetchedAt[s.rec.Seq] = at
		}
		file := s.base + ".ts"
		if !files[file] {
			file = ""
		}
		used[s.base+".ts"] = true
		switch first, dup := out[s.rec.URI]; {
		case s.rec.URI == "":
			notes = append(notes, s.base+".json names no URI; the segment is not reanalyzed")
		case dup && file != "":
			notes = append(notes, fmt.Sprintf("%s is a second download of %s; %s is the first", file, s.rec.URI, cmp.Or(first.file, first.rec.File)))
		case dup:
		default:
			out[s.rec.URI] = &savedSegment{file: file, rec: &s.rec}
		}
	}
	for _, file := range slices.Sorted(maps.Keys(files)) {
		if used[file] {
			continue
		}
		m := segFile.FindStringSubmatch(file)
		n, _ := strconv.ParseUint(m[1], 10, 64)
		uri, from := listedAs(fetches, n, strings.TrimPrefix(m[2], "_"), byPosition)
		switch _, dup := out[uri]; {
		case uri == "":
			notes = append(notes, file+" has no record and no saved playlist lists it; it is not reanalyzed")
		case dup:
			notes = append(notes, fmt.Sprintf("%s has no record; playlist %s lists it as %s, which another file holds", file, from, uri))
		default:
			out[uri] = &savedSegment{file: file}
			notes = append(notes, fmt.Sprintf("%s has no record; its URI %s is from playlist %s", file, uri, from))
		}
	}
	return out, fetchedAt, notes, nil
}

// listedAs finds the URI of number n in the first saved playlist that lists
// it: the entry at position n, or with origin number n, and when hash
// isn't "" (a file named by number and URI hash) the URI with that hash.
func listedAs(fetches []savedFetch, n uint64, hash string, byPosition bool) (uri, playlist string) {
	for _, f := range fetches {
		if f.pl == nil {
			continue
		}
		for _, s := range f.pl.Segments {
			if hash != "" && hls.URIHash(s.URI) != hash {
				continue
			}
			if byPosition && s.Seq == n || !byPosition && s.Number() == n {
				return s.URI, f.name + ".m3u8"
			}
		}
	}
	return "", ""
}

// replay drives a Channel's poller and worker logic over saved files.
type replay struct {
	ctx     context.Context
	dir     string
	c       *Channel
	inc     *incident
	fetches []savedFetch
	segs    map[string]*savedSegment
	done    map[string]bool // saved segments handled
	notes   []string
}

// run replays the monitored rendition's playlists and checks the segments
// they queue.
func (rp *replay) run() error {
	c, cfg := rp.c, rp.c.m.cfg
	rp.done = map[string]bool{}
	var p pollState
	for _, f := range rp.fetches {
		if err := rp.ctx.Err(); err != nil {
			return err
		}
		if f.pl != nil {
			c.adoptTarget(f.pl.TargetDuration)
			u := p.update(f.pl, f.meta.base(), f.name, f.meta, f.at, math.MaxInt)
			for _, fl := range u.faults {
				rp.fault(fl, f.at)
			}
			rp.inc.report.OriginNotes = append(rp.inc.report.OriginNotes, u.notes...)
			if u.restart {
				c.resetWorker()
			}
			if u.ended != nil {
				rp.fault(*u.ended, f.at)
				rp.inc.stallEnd = f.at
				rp.notes = append(rp.notes, stallEndNote(*u.ended, f.at))
			}
			for _, j := range u.queue {
				if err := rp.process(j, f.at, nil); err != nil {
					return err
				}
			}
		}
		// Only an answer from the origin says anything about a stall.
		if f.meta.Status != 0 {
			if fl, ok := p.stall(f.at, c.targetDuration(), cfg.StallTargetDurations, f.meta); ok {
				rp.fault(fl, f.at)
			}
		}
	}
	// Segments no saved playlist queued are checked on their own.
	var rest []string
	for uri := range rp.segs {
		if !rp.done[uri] {
			rest = append(rest, uri)
		}
	}
	if len(rest) == 0 {
		return nil
	}
	slices.SortFunc(rest, func(a, b string) int { return cmp.Compare(savedNumber(rp.segs[a], a), savedNumber(rp.segs[b], b)) })
	rp.notes = append(rp.notes, fmt.Sprintf("%d saved segments were not queued by any saved playlist; each was checked on its own, not against the one before it", len(rest)))
	for _, uri := range rest {
		n := savedNumber(rp.segs[uri], uri)
		c.chain.Break(n)
		c.dropBlack()
		j := segJob{seg: hls.Segment{URI: uri, Seq: n}, num: n}
		if err := rp.process(j, time.Time{}, &analysis.Order{}); err != nil {
			return err
		}
	}
	return nil
}

// savedNumber is a saved segment's number: the origin's, from its URI, or
// its record's.
func savedNumber(s *savedSegment, uri string) uint64 {
	if n, ok := hls.URINumber(uri); ok {
		return n
	}
	if s.rec != nil {
		return s.rec.Seq
	}
	return 0
}

// fault records a fault found at time at.
func (rp *replay) fault(f analysis.Fault, at time.Time) {
	rp.inc.addFault(f, at)
}

// process checks one queued segment from its saved file, as the worker's
// process does after the download. Segments before the first saved one and
// after the last were never the incident's: they are skipped. order, when
// set, replaces the worker's placement of the segment.
func (rp *replay) process(j segJob, at time.Time, order *analysis.Order) error {
	c := rp.c
	s, saved := rp.segs[j.seg.URI]
	if !saved && (len(rp.done) == 0 || len(rp.done) == len(rp.segs)) {
		return nil
	}
	rec := j.record()
	if order == nil {
		order = new(c.order(j))
	}
	faults := j.discontinuity()
	if j.seg.Gap {
		c.gapTagged(j, &rec, *order)
		rec.addFaults(faults)
		for _, f := range faults {
			rp.fault(f, at)
		}
		rp.inc.addSegment(rec)
		return nil
	}
	state := handledAnalyzed
	switch {
	case saved && s.file != "":
		body, err := os.ReadFile(filepath.Join(rp.dir, kindSegments, s.file))
		if err != nil {
			return err
		}
		rec.File = s.file
		if s.rec != nil {
			rec.Fetch = s.rec.Fetch
		}
		f, ok := c.check(rp.ctx, &rec, body, *order)
		faults = append(faults, f...)
		if !ok {
			state = handledUnavailable
		}
	case saved && refused(s.rec.Fetch.Status) && rp.listed(j.seg.URI, s.rec.Fetch.CompletedAt):
		rec.Fetch, rec.Error = s.rec.Fetch, s.rec.Error
		state = handledUnavailable
		c.chain.Break(j.num)
		c.dropBlack()
		faults = append(faults, unavailableFault(rec))
	case saved:
		rec.Fetch, rec.Error = s.rec.Fetch, s.rec.Error
		state = handledFailed
	default:
		rec.Error = "not in the incident: the monitor did not save this segment"
		state = handledFailed
	}
	if saved {
		rp.done[j.seg.URI] = true
	}
	if order.Gap != nil && state != handledFailed {
		rec.Gap = order.Gap
	}
	c.advance(j, state, *order)
	rec.addFaults(faults)
	if t := rec.Fetch.CompletedAt; !t.IsZero() {
		at = t
	}
	for _, f := range faults {
		rp.fault(f, at)
	}
	rp.inc.addSegment(rec)
	return nil
}

// listed reports whether the first playlist saved after at (or the last one
// saved) lists uri: whether a refused segment was still the stream's.
func (rp *replay) listed(uri string, at time.Time) bool {
	var pl *hls.Media
	for _, f := range rp.fetches {
		if f.pl == nil {
			continue
		}
		pl = f.pl
		if !f.at.Before(at) {
			break
		}
	}
	return pl != nil && slices.ContainsFunc(pl.Segments, func(s hls.Segment) bool { return s.URI == uri })
}

var labelParts = regexp.MustCompile(`^(\d+)_([0-9]{1,5}x[0-9]{1,5}|audio|unknown)_(\d+)$`)

// variantFromLabel reads a rendition directory's name (hls.Variant.Label).
func variantFromLabel(label string) (hls.Variant, bool) {
	m := labelParts.FindStringSubmatch(label)
	if m == nil {
		return hls.Variant{}, false
	}
	index, err1 := strconv.Atoi(m[1])
	bandwidth, err2 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil {
		return hls.Variant{}, false
	}
	v := hls.Variant{Index: index, Bandwidth: bandwidth}
	if m[2] != "audio" {
		v.Resolution = m[2]
	}
	return v, true
}

// renditionEntry is a segment as a rendition's playlist lists it, with the
// number of the entry before it.
type renditionEntry struct {
	seg  hls.Segment
	prev *uint64
}

// renditions rebuilds each other rendition from its saved playlists and
// segments, as the fetchers left them, and checks it.
func (rp *replay) renditions(old Report) error {
	root := filepath.Join(rp.dir, "renditions")
	dirs, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	cfg := rp.c.m.cfg
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		v, ok := variantFromLabel(d.Name())
		if !ok {
			rp.notes = append(rp.notes, "renditions/"+d.Name()+" is not a rendition label; not reanalyzed")
			continue
		}
		r := &rendition{
			variant: v, dir: filepath.Join(root, d.Name()), blackMin: cfg.Blackdetect.TriggerMin,
			colorRange: rp.c.cfg.ColorRange,
			segs:       map[uint64]*RenditionSegment{}, data: map[uint64]renditionData{},
		}
		if i := slices.IndexFunc(old.Renditions, func(o RenditionReport) bool { return o.Label == d.Name() }); i >= 0 {
			r.url = old.Renditions[i].PlaylistURL
		}
		if err := rp.rendition(r); err != nil {
			return fmt.Errorf("renditions/%s: %w", d.Name(), err)
		}
		rp.inc.rend = append(rp.inc.rend, r)
	}
	return nil
}

// rendition loads one rendition's saved playlists and segments.
func (rp *replay) rendition(r *rendition) error {
	fetches, err := loadFetches(r.dir)
	if err != nil {
		return err
	}
	entries := map[string]renditionEntry{}
	for _, f := range fetches {
		if f.pl == nil {
			r.playlistErr = cmp.Or(f.meta.Error, fmt.Sprintf("HTTP %d", f.meta.Status))
			continue
		}
		r.playlistErr = ""
		segs := f.pl.Segments
		if len(segs) > 0 {
			r.listings = append(r.listings, listing{at: f.at, newest: segs[len(segs)-1].Number()})
		}
		for i, s := range segs {
			if _, ok := entries[s.URI]; !ok {
				e := renditionEntry{seg: s}
				if i > 0 {
					e.prev = new(segs[i-1].Number())
				}
				entries[s.URI] = e
			}
		}
		if r.url == "" {
			r.url = f.meta.URL
		}
	}
	files, err := os.ReadDir(r.dir)
	if err != nil {
		return err
	}
	type fetched struct {
		base string
		meta FetchMeta
	}
	var saved []fetched
	for _, e := range files {
		m := segFile.FindStringSubmatch(e.Name())
		if m == nil || m[3] != "json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(r.dir, e.Name()))
		if err != nil {
			return err
		}
		f := fetched{base: strings.TrimSuffix(e.Name(), ".json")}
		if err := json.Unmarshal(b, &f.meta); err != nil {
			return fmt.Errorf("%s: %w", e.Name(), err)
		}
		saved = append(saved, f)
	}
	slices.SortStableFunc(saved, func(a, b fetched) int {
		return cmp.Or(a.meta.RequestedAt.Compare(b.meta.RequestedAt), strings.Compare(a.base, b.base))
	})
	label := r.variant.Label()
	for _, f := range saved {
		file := f.base + ".ts"
		body, err := os.ReadFile(filepath.Join(r.dir, file))
		if err != nil {
			rp.notes = append(rp.notes, fmt.Sprintf("renditions/%s/%s.json has no segment file; not reanalyzed", label, f.base))
			continue
		}
		uri := uriOf(f.meta.URL)
		e, ok := entries[uri]
		if !ok {
			n, has := hls.URINumber(uri)
			if !has {
				rp.notes = append(rp.notes, fmt.Sprintf("renditions/%s/%s: no saved playlist lists %s; not reanalyzed", label, file, uri))
				continue
			}
			e = renditionEntry{seg: hls.Segment{URI: uri, URISeq: n, HasURISeq: true}}
		}
		if s := r.segs[e.seg.Number()]; s != nil {
			rp.notes = append(rp.notes, fmt.Sprintf("renditions/%s/%s is a second download of %s; %s is the first", label, file, uri, s.File))
			continue
		}
		checked := rp.c.m.checkRenditionSegment(rp.ctx, r, e.seg, e.prev, file, filepath.Join(r.dir, file), f.meta, body)
		rp.inc.addRenditionSegment(r, checked)
	}
	// The segments the faults need, as the live incident asks for them.
	for _, f := range rp.inc.report.Faults {
		var prev *uint64
		if i := slices.IndexFunc(rp.inc.report.Segments, func(s SegmentRecord) bool { return s.Seq == f.Seq }); i >= 0 {
			prev = rp.inc.report.Segments[i].PrevSeq
		}
		r.want(faultSeqs([]analysis.Fault{{Type: f.Type, Seq: f.Seq, PrevSeq: f.PrevSeq, Values: f.Values}}, prev))
	}
	for _, s := range r.segs {
		if s.Status == statusPending {
			s.Status, s.Error = statusNotSaved, "the original run did not fetch this segment from this rendition"
		}
	}
	r.recheck(rp.c.m.cfg.Checks, rp.inc.baseline)
	return nil
}

// uriOf is the last element of a URL's path: a segment URI as the playlist
// lists it.
func uriOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return path.Base(u.Path)
	}
	return path.Base(raw)
}
