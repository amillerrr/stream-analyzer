package monitor

import (
	"cmp"
	"context"
	"encoding/csv"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// maxListedFaults bounds report.json; further faults are counted, not listed.
const maxListedFaults = 1000

// incidents owns every channel's open incident, report.json, the CSV
// index, the storage cap and fault-type suppression after max_incident.
//
// Lock order: incidents.mu before Channel.mu, never the other way round.
type incidents struct {
	m        *Monitor
	mu       sync.Mutex
	open     map[string]*incident            // by channel name
	suppress map[string]map[string]time.Time // channel -> fault type -> last seen
	fetchers sync.WaitGroup                  // cross-rendition fetchers
	stopped  bool                            // after shutdown no incident may open
	csvMu    sync.Mutex
}

// errStopped means the monitor is shutting down.
var errStopped = errors.New("stream-analyzer is shutting down")

type incident struct {
	id, dir  string
	ch       *Channel
	opened   time.Time
	deadline time.Time
	capped   bool // a fault wanted post-roll beyond max_incident
	held     bool // a stall is going on: stay open until it ends (or max_incident)
	// pending holds the segment files copied in whose sidecars haven't
	// followed yet (the segment is still being checked), with when.
	pending    map[string]time.Time
	draining   bool      // past its deadline, waiting for pending segments: takes no new ones
	stallEnd   time.Time // when the last stall ended; zero while it lasts
	closed     bool
	cancel     context.CancelFunc // stops the rendition fetchers
	fetchers   sync.WaitGroup     // this incident's rendition fetchers
	report     Report
	lastByType map[string]time.Time
	prevByType map[string]time.Time // the time before lastByType's, per type
	rend       []*rendition
	baseline   *int64   // channel A/V baseline, for checking other renditions
	notes      []string // events worth keeping in report.json, such as a stall ending
}

func newIncidents(m *Monitor) *incidents {
	return &incidents{m: m, open: map[string]*incident{}, suppress: map[string]map[string]time.Time{}}
}

// run closes incidents whose post-roll has elapsed and applies the storage
// cap once a minute.
func (in *incidents) run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	capTick := time.NewTicker(time.Minute)
	defer capTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			in.m.safely("incidents", func() { in.tick(in.m.now()) })
		case <-capTick.C:
			in.m.safely("storage cap", in.enforceCap)
		}
	}
}

// recording returns the open incident directory for a channel, if any.
// A draining incident takes no new files.
func (in *incidents) recording(channel string) (string, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if inc := in.open[channel]; inc != nil && !inc.draining {
		return inc.dir, true
	}
	return "", false
}

func (in *incidents) openID(channel string) string {
	in.mu.Lock()
	defer in.mu.Unlock()
	if inc := in.open[channel]; inc != nil {
		return inc.id
	}
	return "none"
}

// fault opens an incident for the channel or merges into the open one.
// rec is the segment the faults were found in.
func (in *incidents) fault(c *Channel, faults []analysis.Fault, rec SegmentRecord) {
	in.faultFrom(c, faults, rec, nil)
}

// faultFrom is fault for a segment whose files went into target (see hold
// and claim), or nil. A fault reopens a draining incident; the files the
// buffer got meanwhile join it then.
func (in *incidents) faultFrom(c *Channel, faults []analysis.Fault, rec SegmentRecord, target *incident) {
	now := in.m.now()
	in.mu.Lock()
	defer in.mu.Unlock()
	faults, held := in.unsuppressed(c, faults, now)
	inc := in.open[c.name]
	if len(faults) == 0 {
		if inc != nil && (!inc.draining || inc == target) {
			// Suppressed types open nothing and extend nothing, but an
			// incident already open still lists them.
			inc.addSegment(rec)
			for _, f := range held {
				inc.addFault(f, now)
			}
			in.write(inc)
		}
		return
	}
	if inc == nil {
		var err error
		if inc, err = in.start(c, now); err != nil {
			if !errors.Is(err, errStopped) {
				c.log.Error("cannot open incident", "error", err)
			}
			return
		}
		c.log.Warn("incident opened", "incident", inc.id, "fault", faults[0].Type, "seq", faults[0].Seq)
	} else {
		if inc.draining {
			inc.draining = false
			in.catchUp(c, inc)
		}
		c.log.Warn("fault merged into open incident", "incident", inc.id, "fault", faults[0].Type, "seq", faults[0].Seq)
	}
	inc.addSegment(rec)
	in.addFaults(inc, faults, rec.PrevSeq, now, true)
	for _, f := range held {
		inc.addFault(f, now)
	}
	in.write(inc)
}

// stall records a stall fault and holds the incident open while it lasts.
func (in *incidents) stall(c *Channel, f analysis.Fault) {
	in.fault(c, []analysis.Fault{f}, SegmentRecord{})
	in.mu.Lock()
	defer in.mu.Unlock()
	if inc := in.open[c.name]; inc != nil {
		inc.held = true
	}
}

// stallEnded releases a held incident: it closes after the usual post-roll
// from now, so the segments that end the stall are recorded too. A stall
// that outlasted its incident (max_incident) gets a short incident of its
// own, opened by ended, so its end and length are on file.
func (in *incidents) stallEnded(c *Channel, now time.Time, note string, ended analysis.Fault) {
	in.mu.Lock()
	defer in.mu.Unlock()
	inc := in.open[c.name]
	if inc == nil {
		var err error
		if inc, err = in.start(c, now); err != nil {
			if !errors.Is(err, errStopped) {
				c.log.Error("cannot open an incident for the end of a stall", "error", err)
			}
			return
		}
		c.log.Warn("incident opened for the end of a stall that outlasted its incident", "incident", inc.id)
		in.addFaults(inc, []analysis.Fault{ended}, nil, now, false)
	}
	inc.held, inc.stallEnd = false, now
	inc.notes = append(inc.notes, note)
	cfg := in.m.cfg
	until := now.Add(max(cfg.PostRoll, cfg.MergeWindow))
	if limit := inc.opened.Add(cfg.MaxIncident); until.After(limit) {
		until = limit
	}
	if until.After(inc.deadline) {
		inc.deadline = until
	}
	in.write(inc)
}

// manual handles POST /capture: a manual fault on the latest segment.
func (in *incidents) manual(c *Channel) (id string, created bool, err error) {
	seq, uri, known := c.latestSeq()
	f := analysis.Fault{Type: analysis.FaultManual, Seq: seq, URI: uri, Message: "manual capture requested via POST /capture"}
	now := in.m.now()
	in.mu.Lock()
	defer in.mu.Unlock()
	inc := in.open[c.name]
	if created = inc == nil; created {
		if inc, err = in.start(c, now); err != nil {
			return "", false, err
		}
		c.log.Warn("incident opened by manual capture", "incident", inc.id)
	} else {
		c.log.Warn("manual capture merged into open incident", "incident", inc.id)
	}
	in.addFaults(inc, []analysis.Fault{f}, nil, now, known)
	in.write(inc)
	return inc.id, created, nil
}

// noteAll adds a note to every open incident.
func (in *incidents) noteAll(text string) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, inc := range in.open {
		inc.notes = append(inc.notes, text)
		in.write(inc)
	}
}

// pendingLimit is how long an incident waits for a segment's sidecar: far
// longer than any fetch and check.
const pendingLimit = 2 * time.Minute

// hold returns the channel's open incident, if it takes new files, and
// keeps it from closing until the segment file about to go into it has
// its sidecar and record there too (see claim and release).
func (in *incidents) hold(channel, file string) *incident {
	in.mu.Lock()
	defer in.mu.Unlock()
	inc := in.open[channel]
	if inc == nil || inc.draining {
		return nil
	}
	inc.pending[file] = in.m.now()
	return inc
}

// claim returns the incident a segment's sidecar and record go into: the
// open one, if it takes new files or holds the segment's file.
func (in *incidents) claim(channel, file string) *incident {
	in.mu.Lock()
	defer in.mu.Unlock()
	inc := in.open[channel]
	if inc == nil {
		return nil
	}
	if _, held := inc.pending[file]; held || !inc.draining {
		return inc
	}
	return nil
}

// release ends the wait for a segment's sidecar.
func (in *incidents) release(inc *incident, file string) {
	if inc == nil {
		return
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	delete(inc.pending, file)
}

// waiting reports whether the incident still waits for a segment's sidecar.
func (inc *incident) waiting(now time.Time) bool {
	for file, since := range inc.pending {
		if now.Sub(since) < pendingLimit {
			return true
		}
		delete(inc.pending, file) // its segment will never finish
	}
	return false
}

// sidecars reads the segment records (seg_*.json) in dir.
func sidecars(dir string) []SegmentRecord {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SegmentRecord
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "seg_") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r SegmentRecord
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// note adds a note to the channel's open incident, if any.
// originNote lists an origin note in the channel's open incident, and keeps
// it for the pre-roll of one that opens later. Both happen under in.mu, so
// a note is listed once whether the incident opens before or after it.
func (in *incidents) originNote(c *Channel, n OriginNote) {
	in.mu.Lock()
	defer in.mu.Unlock()
	c.rememberNote(n)
	if inc := in.open[c.name]; inc != nil && !inc.draining {
		inc.report.OriginNotes = append(inc.report.OriginNotes, n)
		in.write(inc)
	}
}

func (in *incidents) segment(c *Channel, rec SegmentRecord) {
	in.segmentFrom(c, rec, nil)
}

// segmentFrom adds a segment's record to the open incident, unless that is
// draining and doesn't hold the segment's files (target).
func (in *incidents) segmentFrom(c *Channel, rec SegmentRecord, target *incident) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if inc := in.open[c.name]; inc != nil && (!inc.draining || inc == target) {
		inc.addSegment(rec)
		in.write(inc)
	}
}

// catchUp copies into an incident that stopped draining what the buffer
// got meanwhile, and lists those segments. Called with in.mu held.
func (in *incidents) catchUp(c *Channel, inc *incident) {
	if err := copyBuffer(c.dir, inc.dir); err != nil {
		c.log.Warn("some buffered files could not be added to the incident", "incident", inc.id, "error", err)
	}
	for _, r := range sidecars(filepath.Join(inc.dir, kindSegments)) {
		if !slices.ContainsFunc(inc.report.Segments, func(s SegmentRecord) bool { return s.Seq == r.Seq && s.URI == r.URI }) {
			inc.addSegment(r)
		}
	}
}

// unsuppressed splits off (held) the faults of types that outlived a
// max_incident cap and have not yet been quiet for merge_window: they open
// and extend no incident.
func (in *incidents) unsuppressed(c *Channel, faults []analysis.Fault, now time.Time) (out, held []analysis.Fault) {
	sup := in.suppress[c.name]
	if len(sup) == 0 {
		return faults, nil
	}
	for _, f := range faults {
		if last, ok := sup[f.Type]; ok {
			if now.Sub(last) < in.m.cfg.MergeWindow {
				sup[f.Type] = now
				held = append(held, f)
				continue
			}
			delete(sup, f.Type)
		}
		out = append(out, f)
	}
	if len(held) > 0 {
		c.count(func(s *stats) { s.suppressed += len(held) })
	}
	return out, held
}

// start creates an incident: its directory, the buffered evidence and the
// fetchers for the other renditions. Called with in.mu held.
func (in *incidents) start(c *Channel, now time.Time) (*incident, error) {
	if in.stopped {
		return nil, errStopped
	}
	id, err := claimIncidentDir(in.m.incidentDir, now.UTC().Format("20060102T150405Z")+"_"+c.name)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(in.m.incidentDir, id)
	for _, sub := range []string{kindSegments, kindPlaylists} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if free, err := in.m.freeSpace(); err == nil && in.m.cfg.MinFreeBytes > 0 && free < uint64(in.m.cfg.MinFreeBytes) {
		c.log.Error("opening an incident with little free space left; evidence may not fit",
			"incident", id, "free_gb", math.Round(float64(free)/1e8)/10, "min_free_gb", float64(in.m.cfg.MinFreeBytes)/1e9)
	}

	c.mu.Lock()
	info := StreamInfo{URL: c.cfg.URL, MediaPlaylistURL: c.mediaURL, TargetDurationS: c.target.Seconds()}
	variants, monitored, masterBase := c.variants, c.variant, c.masterBase
	if monitored != nil {
		info.Rendition, info.Codecs = monitored.Label(), monitored.Codecs
	}
	var baseline *int64
	if c.baseline != nil {
		baseline = new(*c.baseline)
		info.AVBaselineMs = new(ts.Millis(*c.baseline))
	}
	c.mu.Unlock()

	cfg := in.m.cfg
	inc := &incident{
		id: id, dir: dir, ch: c, opened: now,
		deadline:   now.Add(max(cfg.PostRoll, cfg.MergeWindow)),
		baseline:   baseline,
		lastByType: map[string]time.Time{},
		prevByType: map[string]time.Time{},
		pending:    map[string]time.Time{},
		report: Report{
			ID: id, Channel: c.name, Status: "open", OpenedAt: now.UTC(), Stream: info,
			OriginNotes: c.preRollNotes(now),
		},
	}
	// Register first: from here on the channel copies every new file itself,
	// so nothing written during the snapshot below is missed.
	in.open[c.name] = inc
	if err := copyBuffer(c.dir, dir); err != nil {
		c.log.Warn("some buffered files could not be added to the incident", "incident", id, "error", err)
	}
	// The opening report lists the segments whose files were copied: their
	// sidecars. A segment copied while still being checked has none yet: the
	// incident waits for it to add its sidecar and record.
	segDir := filepath.Join(dir, kindSegments)
	for _, r := range sidecars(segDir) {
		inc.addSegment(r)
	}
	if entries, err := os.ReadDir(segDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if base, ok := strings.CutSuffix(name, ".ts"); ok && strings.HasPrefix(name, "seg_") {
				if _, err := os.Stat(filepath.Join(segDir, base+".json")); errors.Is(err, fs.ErrNotExist) {
					inc.pending[name] = now
				}
			}
		}
	}

	if monitored != nil && len(variants) > 1 {
		ctx, cancel := context.WithCancel(in.m.runContext())
		inc.cancel = cancel
		for _, v := range variants {
			if v.Index == monitored.Index {
				continue
			}
			url, err := hls.Resolve(masterBase, v.URI)
			if err != nil {
				c.log.Warn("cannot resolve a rendition's URI; not fetching it", "rendition", v.Label(), "uri", v.URI, "error", err)
				continue
			}
			rdir := filepath.Join(dir, "renditions", v.Label())
			if rel, err := filepath.Rel(dir, rdir); err != nil || !filepath.IsLocal(rel) {
				c.log.Error("rendition directory would leave the incident; not fetching it", "rendition", v.Label())
				continue
			}
			r := &rendition{
				variant: v, url: url,
				dir:      rdir,
				blackMin: cfg.Blackdetect.TriggerMin,
				segs:     map[uint64]*RenditionSegment{},
				data:     map[uint64]renditionData{},
			}
			inc.rend = append(inc.rend, r)
			in.fetchers.Add(1)
			inc.fetchers.Go(func() {
				defer in.fetchers.Done()
				in.fetchRendition(ctx, inc, r)
			})
		}
	}
	return inc, nil
}

// addFaults records faults, extends the post-roll and asks the other
// renditions for the segments involved. prev is the number of the entry the
// playlist lists before the faults' segment, when known.
//
// The post-roll never goes past max_incident for stream faults. A manual
// capture gets its full post-roll, but never past max_incident plus one
// post-roll: repeated captures can't hold an incident open forever.
func (in *incidents) addFaults(inc *incident, faults []analysis.Fault, prev *uint64, now time.Time, wantRenditions bool) {
	cfg := in.m.cfg
	manualOnly := true
	for _, f := range faults {
		inc.addFault(f, now)
		manualOnly = manualOnly && f.Type == analysis.FaultManual
	}
	inc.report.LastFaultAt = now.UTC()
	until := now.Add(max(cfg.PostRoll, cfg.MergeWindow))
	limit := inc.opened.Add(cfg.MaxIncident)
	if manualOnly {
		limit = limit.Add(cfg.PostRoll)
	}
	if until.After(limit) {
		until, inc.capped = limit, !manualOnly
	}
	if until.After(inc.deadline) {
		inc.deadline = until
	}
	if wantRenditions {
		seqs := faultSeqs(faults, prev)
		for _, r := range inc.rend {
			r.want(seqs)
		}
	}
}

func (inc *incident) addFault(f analysis.Fault, now time.Time) {
	inc.report.FaultCount++
	if last, ok := inc.lastByType[f.Type]; ok {
		inc.prevByType[f.Type] = last
	}
	inc.lastByType[f.Type] = now
	if !slices.Contains(inc.report.FaultTypes, f.Type) {
		inc.report.FaultTypes = append(inc.report.FaultTypes, f.Type)
		slices.Sort(inc.report.FaultTypes)
	}
	if len(inc.report.Faults) >= maxListedFaults {
		return
	}
	inc.report.Faults = append(inc.report.Faults, FaultRecord{
		Type: f.Type, DetectedAt: now.UTC(), Seq: f.Seq, PrevSeq: f.PrevSeq, URI: f.URI, Message: f.Message, Values: f.Values,
	})
}

// addSegment adds or updates a segment in the report, kept in sequence order.
func (inc *incident) addSegment(rec SegmentRecord) {
	if rec.URI == "" && rec.Seq == 0 {
		return
	}
	segs := inc.report.Segments
	i, found := slices.BinarySearchFunc(segs, rec.Seq, func(s SegmentRecord, seq uint64) int { return cmp.Compare(s.Seq, seq) })
	if found {
		segs[i] = rec
	} else {
		segs = slices.Insert(segs, i, rec)
	}
	inc.report.Segments = segs
}

// faultSeqs are the segments to fetch from other renditions: each fault's
// segment and the one before it, so boundary checks can be repeated. prev
// is the entry the playlist lists before the faults' segment, when known;
// the origin may skip numbers, so it need not be one less.
func faultSeqs(faults []analysis.Fault, prev *uint64) []uint64 {
	var seqs []uint64
	for _, f := range faults {
		if f.Type == analysis.FaultStall || playlistLevel(f.Type, f.Values) {
			continue // judged by the renditions' playlists, fetched every poll
		}
		seqs = append(seqs, f.Seq)
		switch {
		case f.PrevSeq != nil:
			seqs = append(seqs, *f.PrevSeq)
		case prev != nil:
			seqs = append(seqs, *prev)
		case f.Seq > 0:
			seqs = append(seqs, f.Seq-1)
		}
	}
	return seqs
}

// tick closes incidents whose deadline has passed.
func (in *incidents) tick(now time.Time) {
	cfg := in.m.cfg
	in.mu.Lock()
	var done []*incident
	for name, inc := range in.open {
		switch limit := inc.opened.Add(cfg.MaxIncident); {
		case inc.held && now.Before(limit):
			continue // a stall is going on
		case inc.held:
			inc.capped = true // the stall outlasted max_incident
		case now.Before(inc.deadline):
			continue
		}
		if inc.waiting(now) {
			// Segments whose files went into it are still being checked:
			// it takes no new ones, and closes once they are done.
			inc.draining = true
			continue
		}
		reason := "post_roll_elapsed"
		if inc.capped {
			reason = "max_duration"
			// Faults still firing at the cap are held back until they stop,
			// so one endless fault can't fill the storage cap on its own.
			sup := in.suppress[name]
			if sup == nil {
				sup = map[string]time.Time{}
				in.suppress[name] = sup
			}
			// Only types still firing are held: seen at least twice in the
			// last merge window, not a single late fault.
			var held []string
			for typ, at := range inc.lastByType {
				prev, twice := inc.prevByType[typ]
				if typ != analysis.FaultManual && now.Sub(at) < cfg.MergeWindow && twice && now.Sub(prev) < cfg.MergeWindow {
					sup[typ] = at
					held = append(held, typ)
				}
			}
			slices.Sort(held)
			inc.ch.log.Warn("incident reached max_incident; suppressing its active fault types until they stop",
				"incident", inc.id, "types", strings.Join(held, ","))
		}
		in.closeLocked(inc, now, reason)
		delete(in.open, name)
		done = append(done, inc)
	}
	in.mu.Unlock()
	in.finalize(done)
}

func (in *incidents) closeLocked(inc *incident, now time.Time, reason string) {
	inc.closed = true
	if inc.cancel != nil {
		inc.cancel()
	}
	inc.report.Status, inc.report.ClosedAt, inc.report.CloseReason = "closed", now.UTC(), reason
	in.write(inc)
}

// finalize lists each closed incident's files with their SHA-256 in its
// report (once its rendition fetchers have stopped writing), logs it,
// indexes it in the CSV and applies the storage cap.
func (in *incidents) finalize(done []*incident) {
	for _, inc := range done {
		inc.fetchers.Wait()
		files, err := manifest(inc.dir, "report.json")
		if err != nil {
			inc.ch.log.Error("cannot list the incident's files and their SHA-256", "incident", inc.id, "error", err)
		}
		in.mu.Lock()
		inc.report.Files = files
		in.write(inc)
		r := inc.report
		in.mu.Unlock()
		in.appendCSV(r)
		inc.ch.log.Info("incident closed", "incident", r.ID, "faults", r.FaultCount,
			"types", strings.Join(r.FaultTypes, ","), "duration", r.ClosedAt.Sub(r.OpenedAt).Round(time.Second),
			"reason", r.CloseReason, "dir", inc.dir)
	}
	if len(done) > 0 {
		in.enforceCap()
	}
}

// shutdown closes every open incident and waits for the rendition fetchers.
func (in *incidents) shutdown() {
	in.mu.Lock()
	in.stopped = true
	now := in.m.now()
	var done []*incident
	for name, inc := range in.open {
		in.closeLocked(inc, now, "shutdown")
		delete(in.open, name)
		done = append(done, inc)
	}
	in.mu.Unlock()
	in.fetchers.Wait()
	in.finalize(done)
}

// write refreshes the derived report fields and rewrites report.json.
// Called with in.mu held.
func (in *incidents) write(inc *incident) {
	inc.refresh()
	if err := writeJSON(filepath.Join(inc.dir, "report.json"), &inc.report); err != nil {
		inc.ch.writeFailed("cannot write report", filepath.Join(inc.id, "report.json"), err)
	}
}

// refresh recomputes the report's derived fields, the renditions' verdicts
// and what they fetched, and the notes.
func (inc *incident) refresh() {
	r := &inc.report
	r.derive()
	for i := range r.Faults {
		r.Faults[i].Renditions, r.Faults[i].RenditionValues = inc.reproduction(r.Faults[i])
	}
	r.Renditions = r.Renditions[:0]
	for _, rd := range inc.rend {
		rr := RenditionReport{Label: rd.variant.Label(), PlaylistURL: rd.url}
		for _, seq := range slices.Sorted(maps.Keys(rd.segs)) {
			rr.Segments = append(rr.Segments, *rd.segs[seq])
		}
		r.Renditions = append(r.Renditions, rr)
	}
	r.Notes = slices.Clone(inc.notes)
	if n := r.FaultCount - len(r.Faults); n > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d further faults are counted in fault_count but not listed (limit %d)", n, maxListedFaults))
	}
}

// derive recomputes the fields that follow from Faults and Segments.
func (r *Report) derive() {
	seqs := map[uint64]bool{}
	for _, f := range r.Faults {
		seqs[f.Seq] = true
		if f.PrevSeq != nil {
			seqs[*f.PrevSeq] = true
		}
		if !slices.Contains(r.FaultTypes, f.Type) {
			r.FaultTypes = append(r.FaultTypes, f.Type)
		}
	}
	slices.Sort(r.FaultTypes)
	r.FaultCount = max(r.FaultCount, len(r.Faults))
	r.SequenceNumbers = slices.Sorted(maps.Keys(seqs))

	r.Discontinuities, r.SCTE35, r.MonitorGaps, r.BlackRuns = nil, nil, nil, nil
	bySeq := map[uint64]*SegmentRecord{}
	for i := range r.Segments {
		s := &r.Segments[i]
		bySeq[s.Seq] = s
		for _, b := range s.Black {
			r.BlackRuns = append(r.BlackRuns, BlackRun{
				Seq: s.Seq, Start: b.Start, End: b.End, Duration: b.Duration, StartPTS: b.StartPTS, EndPTS: b.EndPTS,
			})
		}
		if s.Gap != nil {
			r.MonitorGaps = append(r.MonitorGaps, *s.Gap)
		}
		if s.Discontinuity {
			r.Discontinuities = append(r.Discontinuities, s.Seq)
		}
		if len(s.SCTE35) > 0 {
			r.SCTE35 = append(r.SCTE35, TagRecord{Seq: s.Seq, Tags: s.SCTE35})
		}
	}
	for i := range r.Faults {
		f := &r.Faults[i]
		if s := bySeq[f.Seq]; s != nil {
			f.Discontinuity = s.Discontinuity
		}
		f.SCTE35Nearby = nil
		for _, t := range r.SCTE35 {
			if t.Seq+2 >= f.Seq && t.Seq <= f.Seq+2 {
				f.SCTE35Nearby = append(f.SCTE35Nearby, t)
			}
		}
	}
}

// playlistLevel reports a fault found in the playlist rather than in a
// segment's media.
func playlistLevel(typ string, values map[string]any) bool {
	switch typ {
	case analysis.FaultMediaSequenceBackward, analysis.FaultPlaylistViolation, analysis.FaultStallEnded, analysis.FaultStreamEnded:
		return true
	case analysis.FaultDiscontinuity:
		return values["reason"] != "tag"
	}
	return false
}

// reproduction says, per other rendition, whether a fault shows there too,
// and what that rendition measured.
func (inc *incident) reproduction(f FaultRecord) (map[string]string, map[string]map[string]any) {
	switch {
	case len(inc.rend) == 0, f.Type == analysis.FaultManual:
		return nil, nil
	case playlistLevel(f.Type, f.Values):
		return nil, nil // the renditions' playlists are saved alongside
	}
	out, values := map[string]string{}, map[string]map[string]any{}
	for _, r := range inc.rend {
		label := r.variant.Label()
		var v map[string]any
		switch f.Type {
		case analysis.FaultStall:
			ended := inc.stallEnd
			if ended.Before(f.DetectedAt) {
				ended = time.Time{} // an earlier stall's end
			}
			out[label], v = r.stallVerdict(f, ended, inc.closed)
		case analysis.FaultUnavailable:
			out[label] = r.unavailableVerdict(f.Seq)
		case analysis.FaultDiscontinuity:
			out[label] = r.tagVerdict(f.Seq)
		default:
			out[label], v = r.checkVerdict(f)
		}
		if v != nil {
			values[label] = v
		}
	}
	if len(values) == 0 {
		values = nil
	}
	return out, values
}

// claimIncidentDir creates the directory for a new incident, base or
// base-2, base-3 and so on when that exists, and returns its name. Creating
// the directory is the claim, so any error other than "exists" ends the
// search instead of retrying forever.
func claimIncidentDir(root, base string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	for n := 1; n <= 1000; n++ {
		id := base
		if n > 1 {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		err := os.Mkdir(filepath.Join(root, id), 0o755)
		switch {
		case err == nil:
			return id, nil
		case !errors.Is(err, fs.ErrExist):
			return "", err
		}
	}
	return "", fmt.Errorf("no free incident directory name for %s", base)
}

// incidentName matches an incident directory's name: its UTC open time,
// then the channel.
var incidentName = regexp.MustCompile(`^\d{8}T\d{6}Z_`)

// enforceCap deletes the oldest closed incidents until everything the
// monitor keeps fits the storage cap: the buffer, open incidents and
// closed ones. Only directories named like incidents are counted as such
// or deleted; directory names start with the UTC open time, so name order
// is age order.
func (in *incidents) enforceCap() {
	entries, err := os.ReadDir(in.m.incidentDir)
	if err != nil {
		in.m.log.Error("cannot read the incidents directory: the storage cap is not applied", "dir", in.m.incidentDir, "error", err)
		return
	}
	in.mu.Lock()
	active := map[string]bool{}
	for _, inc := range in.open {
		active[inc.dir] = true
	}
	in.mu.Unlock()

	type entry struct {
		path string
		size int64
	}
	var closed []entry
	size := func(dir string) int64 {
		n, err := dirSize(dir)
		if err != nil {
			in.m.log.Warn("cannot size all of a directory for the storage cap; counting what could be read", "dir", dir, "error", err)
		}
		return n
	}
	total := size(in.m.bufferDir)
	for _, e := range entries {
		if !e.IsDir() || !incidentName.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(in.m.incidentDir, e.Name())
		s := size(p)
		total += s
		if !active[p] {
			closed = append(closed, entry{p, s})
		}
	}
	limit := in.m.cfg.IncidentStorageBytes
	for _, d := range closed {
		if total <= limit {
			break
		}
		if err := os.RemoveAll(d.path); err != nil {
			in.m.log.Error("cannot delete old incident", "dir", d.path, "error", err)
			continue
		}
		total -= d.size
		in.m.log.Warn("deleted oldest incident to stay under the storage cap", "incident", filepath.Base(d.path),
			"freed_mb", d.size/1e6, "cap_gb", float64(limit)/1e9)
	}
	if total > limit {
		in.m.log.Warn("over the storage cap with no closed incident left to delete: the buffer and open incidents hold the rest",
			"used_gb", math.Round(float64(total)/1e8)/10, "cap_gb", float64(limit)/1e9)
	}
}

var csvHeader = []string{
	"id", "channel", "rendition", "status", "opened_utc", "closed_utc", "duration_s", "fault_count",
	"fault_types", "first_fault", "seq_first", "seq_last", "close_reason", "dir",
}

// appendCSV adds a row for a finished incident to data/incidents/incidents.csv.
func (in *incidents) appendCSV(r Report) {
	in.csvMu.Lock()
	defer in.csvMu.Unlock()
	err := os.MkdirAll(in.m.incidentDir, 0o755)
	if err == nil {
		err = appendRecord(filepath.Join(in.m.incidentDir, "incidents.csv"), csvHeader, csvRow(r))
	}
	if err != nil {
		in.m.log.Error("cannot append to incidents.csv", "incident", r.ID, "error", err)
	}
}

func csvRow(r Report) []string {
	var first, closed, dur, seqFirst, seqLast string
	if len(r.Faults) > 0 {
		first = r.Faults[0].Type
	}
	if !r.ClosedAt.IsZero() {
		closed = r.ClosedAt.UTC().Format(time.RFC3339)
		dur = strconv.FormatFloat(r.ClosedAt.Sub(r.OpenedAt).Seconds(), 'f', 0, 64)
	}
	if n := len(r.SequenceNumbers); n > 0 {
		seqFirst, seqLast = strconv.FormatUint(r.SequenceNumbers[0], 10), strconv.FormatUint(r.SequenceNumbers[n-1], 10)
	}
	return []string{
		r.ID, r.Channel, r.Stream.Rendition, r.Status, r.OpenedAt.UTC().Format(time.RFC3339), closed, dur,
		strconv.Itoa(r.FaultCount), strings.Join(r.FaultTypes, ";"), first, seqFirst, seqLast, r.CloseReason,
		filepath.Join("incidents", r.ID),
	}
}

// indexed lists the incidents incidents.csv has a row for.
func (in *incidents) indexed() map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(filepath.Join(in.m.incidentDir, "incidents.csv"))
	if err != nil {
		return out
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	for {
		rec, err := r.Read()
		var bad *csv.ParseError
		switch {
		case err == nil && len(rec) > 0:
			out[rec[0]] = true
		case err != nil && !errors.As(err, &bad): // io.EOF, or the file can't be read
			return out
		}
	}
}

// recover marks incidents left open by a crash or kill as interrupted and
// indexes them, once.
func (in *incidents) recover() {
	entries, err := os.ReadDir(in.m.incidentDir)
	if err != nil {
		return
	}
	var indexed map[string]bool
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(in.m.incidentDir, e.Name(), "report.json")
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r Report
		if err := json.Unmarshal(b, &r); err != nil || r.Status != "open" {
			continue
		}
		r.Status, r.CloseReason = "interrupted", "process_exit"
		r.Notes = append(r.Notes, "stream-analyzer stopped before this incident closed; its post-roll may be incomplete")
		r.derive()
		if r.Files, err = manifest(filepath.Join(in.m.incidentDir, e.Name()), "report.json"); err != nil {
			in.m.log.Error("cannot list the interrupted incident's files", "incident", r.ID, "error", err)
		}
		if err := writeJSON(path, r); err != nil {
			// Still "open" on disk, so every start finds it again: it is
			// indexed only the first time.
			in.m.log.Error("cannot update interrupted incident", "incident", r.ID, "error", err)
		}
		if indexed == nil {
			indexed = in.indexed()
		}
		if !indexed[r.ID] {
			in.appendCSV(r)
			indexed[r.ID] = true
		}
		in.m.log.Warn("marked incident interrupted", "incident", r.ID)
	}
}
