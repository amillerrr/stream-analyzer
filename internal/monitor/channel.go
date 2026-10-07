package monitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// SegmentRecord is everything known about one segment: its playlist entry,
// the fetch, the timing analysis and the faults. It is saved as the
// segment's .json sidecar and listed in incident reports.
type SegmentRecord struct {
	// Seq identifies the segment: the origin's number from its URI
	// (...-seq=N.ts) when it has one, otherwise its playlist position.
	Seq uint64 `json:"seq"`
	// MSN is the segment's playlist position (EXT-X-MEDIA-SEQUENCE plus its
	// index) when it was first listed. The origin sometimes renumbers, so
	// the same segment can later sit at another position.
	MSN uint64 `json:"msn"`
	URI string `json:"uri"`
	// PrevSeq and PrevURI are the entry the playlist listed right before
	// this one. Continuity is checked across that boundary only.
	PrevSeq *uint64 `json:"prev_seq,omitempty"`
	PrevURI string  `json:"prev_uri,omitempty"`
	File    string  `json:"file,omitempty"`
	// SaveError says why the segment's file could not be written; the
	// segment was still analyzed, from memory.
	SaveError     string  `json:"save_error,omitempty"`
	ExtInf        float64 `json:"extinf"`
	Discontinuity bool    `json:"discontinuity,omitzero"`
	// DiscSeq is the segment's discontinuity sequence number.
	DiscSeq  uint64            `json:"discontinuity_sequence"`
	Tags     []string          `json:"tags,omitempty"`
	SCTE35   []string          `json:"scte35,omitempty"`
	Fetch    FetchMeta         `json:"fetch"`
	Analysis *analysis.Summary `json:"analysis,omitempty"`
	Black    []BlackInterval   `json:"black_intervals,omitempty"`
	// BlackDecode says how much of the video ffmpeg decoded for the black
	// check.
	BlackDecode *BlackDecode `json:"black_decode,omitempty"`
	Faults      []string     `json:"faults,omitempty"`
	// Gap lists sequence numbers before this one that were never analyzed
	// (our fetch failed or they left the playlist first). No continuity
	// check was made across them.
	Gap    *analysis.Gap    `json:"monitor_gap_before,omitempty"`
	Events []analysis.Event `json:"events,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// Channel monitors one channel: a poller goroutine fetches the media
// playlist twice per target duration, checks it and queues new segments;
// a worker goroutine downloads and checks them in order.
type Channel struct {
	m    *Monitor
	cfg  config.Channel
	name string
	dir  string // buffer directory
	log  *slog.Logger
	jobs chan segJob

	// Worker goroutine only.
	chain *analysis.Chain
	black blackCarry
	cue   cueMark // whether the previous segment carried a SCTE-35 tag
	// The last segment the worker handled and how that went, and the
	// numbers not analyzed since then that no record has reported yet.
	lastURI   string
	lastNum   uint64
	lastState handled
	pending   *analysis.Gap
	// For black faults' thumbnails: the segment being checked (its file
	// and video PID), the last frame decoded before it, and the strip
	// its black fault wants.
	tileFile  string
	tilePID   uint16
	lastFrame *frameRef
	strip     *stripJob

	mu         sync.Mutex
	variants   []hls.Variant // every variant, when the URL is a master playlist
	variant    *hls.Variant  // the monitored variant
	masterBase string        // master playlist URL after redirects; "" for a media playlist URL
	mediaURL   string
	target     time.Duration
	listing    map[string]bool // URIs the latest playlist lists
	listingAt  time.Time       // when it was fetched
	lastSegAt  time.Time       // when the worker last finished a segment
	stalled    bool
	baseline   *int64       // A/V offset baseline, ticks
	notes      []OriginNote // origin notes of the last buffer window, for an incident's pre-roll
	st         stats
	lastSeq    uint64
	lastSegURI string
	anyRecord  bool
}

type segJob struct {
	seg         hls.Segment
	num         uint64 // seg.Number(): how the segment is known
	playlistURL string
	// prevURI and prevNum are the entry the playlist lists right before
	// this one; prevURI is "" when it is the first entry.
	prevURI string
	prevNum uint64
	// skipped, when set, are numbers right before this entry that the
	// origin never listed (a window jump): not our monitor gap.
	skipped *analysis.Gap
	reset   bool // media sequence restarted: forget the comparison state
}

// segmentFile names a segment's files, without extension:
// seg_<number>_<hash of its URI>. Numbers need not be unique, so two
// segments with one number never share or overwrite a file.
func segmentFile(num uint64, uri string) string { return "seg_" + hls.SegmentKey(num, uri) }

// handled is how the worker's last segment ended.
type handled int

const (
	handledAnalyzed    handled = iota // downloaded and checked
	handledUnavailable                // accounted for: refused, or not usable TS
	handledFailed                     // our own fetch failed: a monitor gap
)

// pollState is the poller goroutine's view of the playlist.
type pollState struct {
	started   bool
	last      uint64 // number of the newest segment queued
	lastURI   string // its URI
	queued    map[string]bool
	order     []string   // queued URIs, oldest first, to bound queued
	failures  int        // consecutive failed playlist fetches
	prev      *hls.Media // the previous playlist that parsed, unless a stale copy
	prevName  string     // its file name in the buffer
	prevAt    time.Time  // when it was fetched
	backward  *hls.Media // a playlist behind the newest segment queued, not yet confirmed as a restart
	lastNewAt time.Time  // when a new segment last appeared
	stalled   bool
	ended     bool // the playlist carries EXT-X-ENDLIST: no stall while it does
}

// maxQueued bounds how many URIs the poller remembers as queued: far more
// than a playlist lists.
const maxQueued = 2000

// remember records a URI as queued.
func (p *pollState) remember(uri string) {
	if p.queued == nil {
		p.queued = map[string]bool{}
	}
	p.queued[uri] = true
	p.order = append(p.order, uri)
	if len(p.order) > maxQueued {
		delete(p.queued, p.order[0])
		p.order = p.order[1:]
	}
}

// cueMark remembers whether a segment carried a SCTE-35 tag.
type cueMark struct {
	uri    string
	tagged bool
}

// blackCarry is a black run that reached the end of a segment and may
// continue into the next one.
type blackCarry struct {
	open        bool
	start, end  uint64 // where the run started, and ends on screen so far, in PTS
	seconds     float64
	frames      int     // black frames so far
	blackS      float64 // their time on screen
	undecoded   int     // frames inside it ffmpeg did not decode
	unconfirmed bool    // part of it is in a segment not fully checked
	tiles       *blackTiles
}

func newChannel(m *Monitor, cc config.Channel) *Channel {
	return &Channel{
		m:     m,
		cfg:   cc,
		name:  cc.Name,
		dir:   filepath.Join(m.bufferDir, cc.Name),
		log:   m.log.With("channel", cc.Name),
		jobs:  make(chan segJob, 64),
		chain: analysis.NewChain(m.cfg.Checks),
	}
}

func (c *Channel) run(ctx context.Context) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		c.log.Error("cannot create buffer directory", "error", err)
		return
	}
	// What a previous run left is no pre-roll of this one's, unless it
	// stopped moments ago: prune before anything can copy it.
	if err := pruneBuffer(c.dir, time.Now(), c.m.cfg.Buffer, time.Time{}); err != nil {
		c.log.Warn("buffer prune failed", "error", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { c.poll(ctx) })
	wg.Go(func() { c.work(ctx) })
	wg.Go(func() { c.pruneLoop(ctx) })
	wg.Wait()
}

// --- playlists ------------------------------------------------------------

func (c *Channel) poll(ctx context.Context) {
	defer close(c.jobs)
	for delay := time.Second; !c.resolve(ctx); delay = min(2*delay, 30*time.Second) {
		if !sleepCtx(ctx, delay) {
			return
		}
	}
	var p pollState
	for {
		start := time.Now()
		pl, base, meta, err := c.fetchPlaylist(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.failures++
			c.log.Warn("playlist fetch failed", "error", err, "consecutive", p.failures)
			if p.failures%5 == 0 {
				// The variant list or URLs may have changed. A failed
				// re-resolve keeps the current playlist URL.
				c.resolve(ctx)
			}
		} else {
			p.failures = 0
			c.safely("playlist", func() { c.handlePlaylist(&p, pl, base, meta) })
		}
		// Only an answer from the origin says anything about a stall; our
		// own network errors say nothing about the stream.
		if meta.Status != 0 {
			c.checkStall(&p, meta)
		}
		if !sleepCtx(ctx, time.Until(start.Add(c.pollInterval()))) {
			return
		}
	}
}

// resolve loads the channel URL: a master playlist selects the variant to
// monitor, a media playlist is monitored directly. Anything else (an error
// page, an empty 200) changes nothing and reports failure, so a CDN hiccup
// can't switch the channel to polling the wrong URL.
func (c *Channel) resolve(ctx context.Context) bool {
	body, meta, err := c.m.fetch(ctx, c.cfg.URL, true, playlistTimeout(0))
	if ctx.Err() != nil {
		return false
	}
	if err == nil && hls.IsMaster(body) {
		c.saveFetch("master_"+stamp(meta.RequestedAt), body, meta)
		return c.useMaster(body, meta.base())
	}
	var pl *hls.Media
	if err == nil {
		if pl, err = hls.ParseMedia(body); err != nil {
			meta.Error = "parse: " + err.Error()
		}
	}
	name := "playlist_" + stamp(meta.RequestedAt)
	if pl != nil {
		name += fmt.Sprintf("_msn%d", pl.MediaSequence)
	}
	c.saveFetch(name, body, meta)
	if err != nil {
		c.log.Warn("cannot load channel URL", "url", c.cfg.URL, "error", err)
		c.count(func(s *stats) { s.resolveErrors++ })
		return false
	}
	c.mu.Lock()
	changed := c.mediaURL != c.cfg.URL
	c.mediaURL, c.masterBase, c.variants, c.variant = c.cfg.URL, "", nil, nil
	c.mu.Unlock()
	if changed {
		c.log.Info("monitoring media playlist", "url", c.cfg.URL)
	}
	return true
}

// useMaster selects the variant to monitor from a master playlist fetched
// from base (its URL after redirects).
func (c *Channel) useMaster(body []byte, base string) bool {
	master, err := hls.ParseMaster(body)
	if err != nil {
		c.log.Warn("bad master playlist", "error", err)
		c.count(func(s *stats) { s.resolveErrors++ })
		return false
	}
	v, err := hls.SelectVariant(master.Variants, cmp.Or(c.cfg.Rendition, c.m.cfg.Rendition))
	if err != nil {
		c.log.Error("cannot select rendition", "error", err)
		c.count(func(s *stats) { s.resolveErrors++ })
		return false
	}
	url, err := hls.Resolve(base, v.URI)
	if err != nil {
		c.log.Error("bad variant URI", "uri", v.URI, "error", err)
		c.count(func(s *stats) { s.resolveErrors++ })
		return false
	}
	c.mu.Lock()
	changed := c.mediaURL != url
	c.variants, c.variant, c.mediaURL, c.masterBase = master.Variants, &v, url, base
	c.mu.Unlock()
	if changed {
		c.log.Info("monitoring rendition", "rendition", v.Label(), "codecs", v.Codecs,
			"renditions", len(master.Variants), "url", url)
	}
	return true
}

// fetchPlaylist fetches and saves the media playlist. It also returns the
// URL it was served from after redirects, which segment URIs resolve
// against, and the fetch metadata.
func (c *Channel) fetchPlaylist(ctx context.Context) (*hls.Media, string, FetchMeta, error) {
	c.mu.Lock()
	url, target := c.mediaURL, c.target
	c.mu.Unlock()
	body, meta, err := c.m.fetch(ctx, url, true, playlistTimeout(target))
	if ctx.Err() != nil {
		return nil, "", meta, ctx.Err()
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
	c.saveFetch(name, body, meta)
	c.count(func(s *stats) {
		s.playlists++
		if err != nil {
			s.playlistErrors++
		}
	})
	if err != nil {
		return nil, "", meta, err
	}
	c.adoptTarget(pl.TargetDuration)
	return pl, meta.base(), meta, nil
}

// handlePlaylist checks a playlist against the previous one and queues the
// segments newer than any queued so far. base is the playlist's URL after
// redirects.
func (c *Channel) handlePlaylist(p *pollState, pl *hls.Media, base string, meta FetchMeta) {
	name, now := playlistName(meta.RequestedAt, pl.MediaSequence), c.m.now()
	u := p.update(pl, base, name, meta, now, c.m.opts.InitialSegments)
	if !u.stale {
		c.setListing(pl, now)
	}
	for _, f := range u.faults {
		c.playlistFault(f)
	}
	for _, n := range u.notes {
		c.originNote(n)
	}
	switch {
	case u.stale:
		c.count(func(s *stats) { s.stale++ })
		return
	case u.restart:
		c.log.Warn("the playlist went back to a new timeline twice in a row; treating it as a stream restart",
			"was", u.was, "now", u.newest)
		c.enqueue(segJob{reset: true})
	}
	if u.fresh {
		c.count(func(s *stats) { s.lastNew = time.Now() })
	}
	if u.ended != nil {
		c.stallEnded(*u.ended, now)
	}
	for _, j := range u.queue {
		c.enqueue(j)
	}
}

// playlistUpdate is what the poller makes of one playlist: the faults and
// origin notes it shows, and the segments to queue.
type playlistUpdate struct {
	// stale: it ends before the newest segment queued (a stale copy);
	// restart: a second such playlist confirmed a packager restart.
	stale, restart bool
	was, newest    uint64 // the newest number queued before, and listed now
	faults         []analysis.Fault
	notes          []OriginNote
	queue          []segJob
	// fresh: it lists new segments (or is the first playlist); ended is
	// the stall_ended fault when that ends a stall.
	fresh bool
	ended *analysis.Fault
}

// update checks a playlist against the previous one and decides which of
// its segments to queue, keeping the poll state. name is the playlist's
// file name, now when it arrived, and initial how many segments of the
// first playlist, or of a restart's, to queue. It has no other effect, so
// reanalyze can replay saved playlists through it.
func (p *pollState) update(pl *hls.Media, base, name string, meta FetchMeta, now time.Time, initial int) playlistUpdate {
	segs := pl.Segments
	u := playlistUpdate{was: p.last}
	if len(segs) > 0 {
		u.newest = segs[len(segs)-1].Number()
	}
	// A playlist that ends before the newest segment queued is a stale copy
	// from another cache, or the packager restarted. Only a second playlist
	// on the same new timeline confirms a restart; until then it is stale
	// and says nothing new.
	// A playlist that still lists the newest segment queued is neither,
	// whatever its numbers: they are the packager's own and may repeat or
	// go back (an ad break numbered from 1).
	lists := slices.ContainsFunc(segs, func(s hls.Segment) bool { return s.URI == p.lastURI })
	backward := p.started && len(segs) > 0 && u.newest < p.last && !lists
	u.restart = backward && confirmsRestart(p.backward, pl, p.queued)
	u.stale = backward && !u.restart
	switch prev := p.prev; {
	case prev != nil && len(segs) > 0 && pl.MediaSequence < prev.MediaSequence:
		// An empty playlist's MEDIA-SEQUENCE says nothing about order.
		u.faults = append(u.faults, backwardFault(prev, pl, meta))
	case !u.stale && !u.restart:
		for _, v := range hls.CheckUpdate(prev, pl) {
			if originPractice(v, pl) {
				u.notes = append(u.notes, originNote(v, p.prevName, name, now))
				continue
			}
			u.faults = append(u.faults, violationFault(v, p.prevName, name))
		}
	}
	jump := p.windowJump(pl, now)
	if !u.stale {
		p.prev, p.prevName, p.prevAt = pl, name, now
		if pl.Endlist && !p.ended {
			u.faults = append(u.faults, streamEndedFault(pl, name))
		}
		p.ended = pl.Endlist
	}
	if len(segs) == 0 {
		return u
	}
	switch {
	case !p.started:
		for i := max(0, len(segs)-initial); i < len(segs); i++ {
			u.queue = append(u.queue, newJob(segs, i, base))
		}
	case u.stale:
		p.backward = pl
		return u
	case u.restart:
		p.backward = nil
		for i := max(0, len(segs)-initial); i < len(segs); i++ {
			u.queue = append(u.queue, newJob(segs, i, base))
		}
	default:
		p.backward = nil
		// A URI is queued once, whatever position a later playlist gives it:
		// the origin sometimes renumbers its playlist. New segments are the
		// unknown URIs after the newest one queued.
		k := slices.IndexFunc(segs, func(s hls.Segment) bool { return s.URI == p.lastURI })
		for i, s := range segs {
			switch {
			case p.queued[s.URI]:
			case k >= 0 && i < k:
			case k < 0 && s.Number() <= p.last:
			default:
				u.queue = append(u.queue, newJob(segs, i, base))
			}
		}
		if jump != nil && len(u.queue) > 0 {
			u.faults = append(u.faults, jumpFault(*jump, u.queue[0].seg, p.lastURI, name))
			u.queue[0].skipped = jump
		}
	}
	if u.fresh = len(u.queue) > 0 || !p.started; u.fresh {
		if p.stalled {
			u.ended = new(p.endStall(now))
		}
		p.lastNewAt = now
	}
	for _, j := range u.queue {
		p.remember(j.seg.URI)
	}
	if n := len(u.queue); n > 0 {
		p.last, p.lastURI = u.queue[n-1].num, u.queue[n-1].seg.URI
	}
	p.started = true
	return u
}

// streamEndedFault reports a playlist that ends with EXT-X-ENDLIST.
func streamEndedFault(pl *hls.Media, name string) analysis.Fault {
	f := analysis.Fault{
		Type:    analysis.FaultStreamEnded,
		Message: "the playlist carries EXT-X-ENDLIST: the origin has ended the stream, and no new segments will be listed",
		Values:  map[string]any{"msn": pl.MediaSequence, "playlist": name + ".m3u8"},
	}
	if n := len(pl.Segments); n > 0 {
		f.Seq, f.URI = pl.Segments[n-1].Number(), pl.Segments[n-1].URI
	}
	return f
}

// windowJump returns the numbers between the newest segment of the previous
// playlist and the first of this one when the two share no segment and
// more are missing than the origin could have produced and removed in the
// time between the fetches: the origin jumped its window. Fewer missing
// means our polls were late, which is our own monitor gap.
func (p *pollState) windowJump(pl *hls.Media, now time.Time) *analysis.Gap {
	prev := p.prev
	if prev == nil || len(prev.Segments) == 0 || len(pl.Segments) == 0 || pl.MediaSequence < prev.MediaSequence {
		return nil
	}
	for _, s := range pl.Segments {
		if slices.ContainsFunc(prev.Segments, func(o hls.Segment) bool { return o.URI == s.URI }) {
			return nil
		}
	}
	a, b := prev.Segments[len(prev.Segments)-1], pl.Segments[0]
	if a.HasURISeq != b.HasURISeq || a.HasURISeq && hls.Dir(a.URI) != hls.Dir(b.URI) {
		return nil // two packagers' numbers, or numbers against positions: nothing to count
	}
	from, to := a.Number()+1, b.Number()
	if to <= from {
		return nil
	}
	to--
	target := time.Duration(max(prev.TargetDuration, pl.TargetDuration) * float64(time.Second))
	if target <= 0 {
		return nil
	}
	if possible := uint64(now.Sub(p.prevAt)/target) + 2; to-from+1 <= possible {
		return nil
	}
	return &analysis.Gap{From: from, To: to}
}

// playlistName is a saved playlist's file name, without extension.
func playlistName(at time.Time, msn uint64) string {
	return fmt.Sprintf("playlist_%s_msn%d", stamp(at), msn)
}

// violationFault reports a playlist update that breaks the rules for a live
// playlist. prevName and name are the two playlists' files in the buffer.
func violationFault(v hls.Violation, prevName, name string) analysis.Fault {
	values := maps.Clone(v.Values)
	if values == nil {
		values = map[string]any{}
	}
	values["reason"] = v.Reason
	values["playlist"] = name + ".m3u8"
	if prevName != "" {
		values["prev_playlist"] = prevName + ".m3u8"
	}
	return analysis.Fault{Type: analysis.FaultPlaylistViolation, Seq: v.Seq, URI: v.URI, Message: v.Detail, Values: values}
}

// jumpFault reports numbers the origin never listed: its window jumped past
// them between two playlist fetches.
func jumpFault(g analysis.Gap, first hls.Segment, lastURI, name string) analysis.Fault {
	return analysis.Fault{
		Type: analysis.FaultPlaylistViolation,
		Seq:  first.Number(),
		URI:  first.URI,
		Message: fmt.Sprintf("the origin's playlist jumped: numbers %d-%d were never listed between two playlist fetches, too few seconds apart for them to have come and gone",
			g.From, g.To),
		Values: map[string]any{
			"reason": "window_jump", "from": g.From, "to": g.To, "count": g.To - g.From + 1,
			"last_listed_uri": lastURI, "playlist": name + ".m3u8",
		},
	}
}

// originPractice reports a playlist violation that is the origin's
// normal practice, noted rather than a fault: dropping
// EXT-X-DISCONTINUITY from the entry that becomes first when it sends no
// EXT-X-DISCONTINUITY-SEQUENCE (which would make every discontinuity
// reported twice), raising EXT-X-TARGETDURATION while a long black or
// filler segment is listed, and re-tagging the entry that becomes first at
// the head of a break.
func originPractice(v hls.Violation, pl *hls.Media) bool {
	switch v.Reason {
	case hls.ViolationDiscontinuityTagDropped:
		return !pl.HasDiscontinuitySequence
	case hls.ViolationTargetDuration, hls.ViolationFirstEntryCues:
		return true
	}
	return false
}

// originNote makes the note for a violation that is the origin's
// practice; prevName and name are the two playlists' files.
func originNote(v hls.Violation, prevName, name string, at time.Time) OriginNote {
	return OriginNote{
		DetectedAt: at.UTC(), Seq: v.Seq, URI: v.URI, Reason: v.Reason, Detail: v.Detail,
		Values: violationFault(v, prevName, name).Values,
	}
}

// originNoteColumns are data/origin_notes.csv's columns, in order.
var originNoteColumns = []string{"time_utc", "channel", "seq", "uri", "reason", "detail"}

// originNote records origin behaviour that breaks the rules for a live
// playlist but is the origin's normal practice: logged, one row in
// data/origin_notes.csv, counted in the health line when it is a target
// duration change, and listed in the incident whose evidence covers it.
// It opens no incident.
func (c *Channel) originNote(n OriginNote) {
	c.log.Info("origin note", "reason", n.Reason, "seq", n.Seq, "uri", n.URI, "detail", n.Detail)
	row := map[string]string{
		"time_utc": n.DetectedAt.UTC().Format("2006-01-02T15:04:05.000Z"), "channel": c.name,
		"seq": strconv.FormatUint(n.Seq, 10), "uri": n.URI, "reason": n.Reason, "detail": n.Detail,
	}
	if err := c.m.appendCSV("origin_notes.csv", originNoteColumns, row); err != nil {
		c.log.Error("cannot append to origin_notes.csv", "error", err)
	}
	if n.Reason == hls.ViolationTargetDuration {
		c.count(func(s *stats) { s.targetChanges++ })
	}
	c.m.incidents.originNote(c, n)
}

// rememberNote keeps an origin note for the pre-roll of an incident that
// opens within the buffer window. Called with incidents.mu held.
func (c *Channel) rememberNote(n OriginNote) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := n.DetectedAt.Add(-c.m.cfg.Buffer)
	c.notes = append(slices.DeleteFunc(c.notes, func(o OriginNote) bool { return o.DetectedAt.Before(from) }), n)
}

// preRollNotes returns the origin notes of the buffer window before now,
// marked pre_roll. Called with incidents.mu held.
func (c *Channel) preRollNotes(now time.Time) []OriginNote {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []OriginNote
	for _, n := range c.notes {
		if !n.DetectedAt.Before(now.Add(-c.m.cfg.Buffer)) {
			n.PreRoll = true
			out = append(out, n)
		}
	}
	return out
}

// confirmsRestart reports whether pl, a playlist behind the newest segment
// queued, confirms the earlier one (cand) as a restart: both are on the
// same new timeline (they share a segment) and pl's newest segment was
// never queued, as it would have been on a stale copy of the old one.
func confirmsRestart(cand, pl *hls.Media, queued map[string]bool) bool {
	if cand == nil || len(pl.Segments) == 0 || queued[pl.Segments[len(pl.Segments)-1].URI] {
		return false
	}
	for _, s := range pl.Segments {
		if slices.ContainsFunc(cand.Segments, func(o hls.Segment) bool { return o.URI == s.URI }) {
			return true
		}
	}
	return false
}

// setListing remembers which URIs the latest playlist lists, and when.
func (c *Channel) setListing(pl *hls.Media, at time.Time) {
	listing := make(map[string]bool, len(pl.Segments))
	for _, s := range pl.Segments {
		listing[s.URI] = true
	}
	c.mu.Lock()
	c.listing, c.listingAt = listing, at
	c.mu.Unlock()
}

// stillListed fetches the playlist again to see whether it still lists
// uri. The listing the poller last saw may be out of date (after a sleep
// or an outage), and a segment that has left the playlist is not the
// origin's fault. When the fetch fails, a listing from the last two target
// durations still counts.
func (c *Channel) stillListed(ctx context.Context, uri string) bool {
	pl, _, _, err := c.fetchPlaylist(ctx)
	if err != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.listing[uri] && c.m.now().Sub(c.listingAt) <= 2*max(c.target, time.Second)
	}
	return slices.ContainsFunc(pl.Segments, func(s hls.Segment) bool { return s.URI == uri })
}

// newJob is the job for segs[i], a new entry of a playlist fetched from
// base, with the entry listed right before it.
func newJob(segs []hls.Segment, i int, base string) segJob {
	j := segJob{seg: segs[i], num: segs[i].Number(), playlistURL: base}
	if i > 0 {
		j.prevURI, j.prevNum = segs[i-1].URI, segs[i-1].Number()
	}
	return j
}

// checkStall opens a stall incident once no new segment has appeared for
// stall_target_durations target durations.
func (c *Channel) checkStall(p *pollState, meta FetchMeta) {
	f, ok := p.stall(c.m.now(), c.targetDuration(), c.m.cfg.StallTargetDurations, meta)
	if !ok {
		return
	}
	c.mu.Lock()
	c.stalled = true
	c.mu.Unlock()
	c.log.Warn("fault", "type", f.Type, "seq", f.Seq, "uri", f.URI, "detail", f.Message)
	c.count(func(s *stats) { s.faults++ })
	c.m.incidents.stall(c, f)
}

// stall returns a stall fault once no new segment has appeared for
// targets target durations, as of now; meta is the latest playlist fetch.
func (p *pollState) stall(now time.Time, target time.Duration, targets float64, meta FetchMeta) (analysis.Fault, bool) {
	limit := time.Duration(targets * float64(target))
	if !p.started || p.stalled || p.ended || limit <= 0 {
		return analysis.Fault{}, false
	}
	stalledFor := now.Sub(p.lastNewAt)
	if stalledFor < limit {
		return analysis.Fault{}, false
	}
	p.stalled = true
	return analysis.Fault{
		Type: analysis.FaultStall,
		Seq:  p.last,
		URI:  p.lastURI,
		Message: fmt.Sprintf("no new segment for %.1f s (%g target durations of %v); the playlist still ends at %d",
			stalledFor.Seconds(), targets, target, p.last),
		Values: map[string]any{
			"last_seq":               p.last,
			"last_new_segment_at":    p.lastNewAt.UTC().Format(time.RFC3339Nano),
			"stalled_for_s":          math.Round(stalledFor.Seconds()*1000) / 1000,
			"threshold_s":            limit.Seconds(),
			"target_duration_s":      target.Seconds(),
			"playlist_status":        meta.Status,
			"playlist_age":           meta.Headers.Get("Age"),
			"playlist_last_modified": meta.Headers.Get("Last-Modified"),
			"playlist_error":         meta.Error,
		},
	}, true
}

// endStall ends a stall: new segments were listed again at now.
func (p *pollState) endStall(now time.Time) analysis.Fault {
	stalled := now.Sub(p.lastNewAt)
	p.stalled = false
	return analysis.Fault{
		Type: analysis.FaultStallEnded,
		Seq:  p.last,
		URI:  p.lastURI,
		Message: fmt.Sprintf("the stall that began at %s ended at %s after %.1f s: new segments listed again",
			p.lastNewAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339), stalled.Seconds()),
		Values: map[string]any{
			"stalled_for_s": math.Round(stalled.Seconds()*1000) / 1000,
			"began_at":      p.lastNewAt.UTC().Format(time.RFC3339Nano),
			"ended_at":      now.UTC().Format(time.RFC3339Nano),
			"last_seq":      p.last,
		},
	}
}

// stallEndNote is the incident note for a stall that ended.
func stallEndNote(ended analysis.Fault, now time.Time) string {
	return fmt.Sprintf("stall ended at %s after %.1f s: new segments listed again",
		now.UTC().Format(time.RFC3339), ended.Values["stalled_for_s"])
}

// stallEnded reports that new segments appeared again.
func (c *Channel) stallEnded(ended analysis.Fault, now time.Time) {
	c.mu.Lock()
	c.stalled = false
	c.mu.Unlock()
	c.log.Warn("stall ended: new segments listed again", "after_s", ended.Values["stalled_for_s"])
	c.m.incidents.stallEnded(c, now, stallEndNote(ended, now), ended)
}

// playlistFault reports a fault found in the playlist rather than in a
// segment.
func (c *Channel) playlistFault(f analysis.Fault) {
	c.log.Warn("fault", "type", f.Type, "seq", f.Seq, "uri", f.URI, "detail", f.Message)
	c.count(func(s *stats) { s.faults++ })
	c.m.incidents.fault(c, []analysis.Fault{f}, SegmentRecord{})
}

func backwardFault(prev, cur *hls.Media, meta FetchMeta) analysis.Fault {
	var prevNewest, newest uint64
	var uri string
	if n := len(prev.Segments); n > 0 {
		prevNewest = prev.Segments[n-1].Number()
	}
	if n := len(cur.Segments); n > 0 {
		newest, uri = cur.Segments[n-1].Number(), cur.Segments[n-1].URI
	}
	return analysis.Fault{
		Type: analysis.FaultMediaSequenceBackward,
		Seq:  newest,
		URI:  uri,
		Message: fmt.Sprintf("EXT-X-MEDIA-SEQUENCE went backward from %d to %d (an older playlist, or the packager restarted)",
			prev.MediaSequence, cur.MediaSequence),
		Values: map[string]any{
			"prev_msn":      prev.MediaSequence,
			"msn":           cur.MediaSequence,
			"prev_newest":   prevNewest,
			"newest":        newest,
			"age":           meta.Headers.Get("Age"),
			"last_modified": meta.Headers.Get("Last-Modified"),
			"etag":          meta.Headers.Get("ETag"),
		},
	}
}

func (c *Channel) enqueue(j segJob) {
	select {
	case c.jobs <- j:
	default:
		c.log.Warn("segment queue full; skipping segment (it will show as a monitor gap)", "seq", j.num, "uri", j.seg.URI)
		c.count(func(s *stats) { s.queueDrops++ })
	}
}

// Plausible EXT-X-TARGETDURATION values, in seconds.
const minTarget, maxTarget = 1, 30

// adoptTarget takes a playlist's EXT-X-TARGETDURATION as the channel's
// target, which paces polling and stall detection. A value outside 1-30 s
// (milliseconds mistaken for seconds, a garbled line) would stop polling
// for as long, so it is logged and ignored.
func (c *Channel) adoptTarget(seconds float64) {
	if seconds == 0 {
		return
	}
	if !(seconds >= minTarget && seconds <= maxTarget) {
		c.log.Warn("ignoring an implausible EXT-X-TARGETDURATION", "value", seconds, "keeping", c.targetDuration())
		return
	}
	c.mu.Lock()
	c.target = time.Duration(seconds * float64(time.Second))
	c.mu.Unlock()
}

// pollInterval is half the target duration, between 250 ms and 10 s.
func (c *Channel) pollInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.target <= 0 {
		return time.Second
	}
	return min(max(c.target/2, 250*time.Millisecond), 10*time.Second)
}

// frameTicks is the monitored variant's frame duration from the master
// playlist's FRAME-RATE, or 0 when unknown.
func (c *Channel) frameTicks() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.variant == nil {
		return 0
	}
	return c.variant.FrameTicks()
}

// frame is the channel's video frame duration for seg: the variant's
// FRAME-RATE, or what the chain learned from recent segments, or seg's own.
func (c *Channel) frame(seg *analysis.Segment) int64 {
	return cmp.Or(c.frameTicks(), c.chain.Frame(), seg.Video.FrameTicks, fallbackFrameTicks)
}

// saveFetch writes a fetched playlist and its metadata to the buffer.
func (c *Channel) saveFetch(name string, body []byte, meta FetchMeta) {
	if body != nil {
		c.persist(kindPlaylists, name+playlistExt(meta), body)
	}
	c.persistJSON(kindPlaylists, name+".json", meta)
}

// persist writes a file to the buffer and, while an incident is open, a
// copy of it into the incident too (each written through a temporary file
// and a rename). A buffer directory that was removed is made again. Every
// failure is logged and counted in the health line.
func (c *Channel) persist(kind, name string, data []byte) error {
	return c.persistTo(nil, kind, name, data)
}

// persistTo is persist for a segment whose files go into target (see
// incidents.hold and claim), even while it drains. With no target they go
// wherever persist would.
func (c *Channel) persistTo(target *incident, kind, name string, data []byte) error {
	path := filepath.Join(c.dir, name)
	err := writeFileAtomic(path, data)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.MkdirAll(c.dir, 0o755); err == nil {
			c.log.Warn("buffer directory was missing; made it again", "dir", c.dir)
			err = writeFileAtomic(path, data)
		}
	}
	if err != nil {
		c.writeFailed("cannot write buffer file", path, err)
		return err
	}
	dir, ok := "", false
	if target != nil {
		dir, ok = target.dir, true
	} else {
		dir, ok = c.m.incidents.recording(c.name)
	}
	if ok {
		if err := writeFileAtomic(filepath.Join(dir, kind, name), data); err != nil {
			c.writeFailed("cannot add file to incident", name, err)
		}
	}
	return nil
}

// writeFailed logs and counts an evidence file that could not be written.
func (c *Channel) writeFailed(msg, file string, err error) {
	c.log.Error(msg, "file", file, "error", err)
	c.count(func(s *stats) { s.writeErrors++ })
}

func (c *Channel) persistJSON(kind, name string, v any) error {
	return c.persistJSONTo(nil, kind, name, v)
}

func (c *Channel) persistJSONTo(target *incident, kind, name string, v any) error {
	b, err := marshalJSON(v)
	if err != nil {
		c.writeFailed("cannot encode JSON", name, err)
		return err
	}
	return c.persistTo(target, kind, name, append(b, '\n'))
}

func (c *Channel) pruneLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			last := c.lastSegAt
			c.mu.Unlock()
			if err := pruneBuffer(c.dir, time.Now(), c.m.cfg.Buffer, last); err != nil {
				c.log.Warn("buffer prune failed", "error", err)
			}
		}
	}
}

// --- segments -------------------------------------------------------------

func (c *Channel) work(ctx context.Context) {
	for j := range c.jobs {
		switch {
		case ctx.Err() != nil:
			// Drain until the poller closes the queue.
		case j.reset:
			c.resetWorker()
		default:
			c.safely("segment", func() { c.process(ctx, j) })
		}
	}
}

// resetWorker forgets the comparison state after a stream restart.
func (c *Channel) resetWorker() {
	c.chain = analysis.NewChain(c.m.cfg.Checks)
	c.dropBlack()
	c.lastURI, c.lastNum, c.lastState, c.pending = "", 0, handledAnalyzed, nil
}

// safely runs f, turning a panic into an error with the stack in the log
// and a count in the health line, so one bad segment or playlist can't end
// the process and leave incidents open.
func (c *Channel) safely(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("recovered from a panic; carrying on", "in", what, "panic", r, "stack", string(debug.Stack()))
			c.count(func(s *stats) { s.panics++ })
		}
	}()
	f()
}

func (c *Channel) process(ctx context.Context, j segJob) {
	s := j.seg
	rec := j.record()
	order := c.order(j)
	faults := j.discontinuity()
	if s.Gap {
		c.gapTagged(j, &rec, order)
		c.finish(rec, faults, nil)
		return
	}
	url, err := hls.Resolve(j.playlistURL, s.URI)
	var body []byte
	var bodies [][]byte
	var recovered bool
	if err == nil {
		body, rec.Fetch, bodies, recovered, err = c.fetchSegment(ctx, url)
	}
	if ctx.Err() != nil {
		return // shutting down
	}
	c.saveRefusals(j.num, s.URI, &rec.Fetch, bodies, body, err != nil)
	if recovered {
		c.count(func(st *stats) { st.originRecovered++ })
		c.log.Info("segment fetched on retry after an origin error", "seq", j.num, "failed", rec.Fetch.FailedStatuses)
	}
	state := handledAnalyzed
	var held *incident // the incident this segment's files go into, if any
	switch {
	case err != nil && refused(rec.Fetch.Status) && c.stillListed(ctx, s.URI):
		// The playlist lists it and the origin won't serve it: the stream
		// is missing a segment. It is accounted for, so the next segment is
		// not a monitor gap.
		rec.Error = err.Error()
		state = handledUnavailable
		c.chain.Break(j.num)
		c.dropBlack()
		faults = append(faults, unavailableFault(rec))
	case err != nil:
		rec.Error = err.Error()
		state = handledFailed
		c.log.Warn("segment fetch failed; it will show as a monitor gap", "seq", j.num, "error", err)
	default:
		rec.File = segmentFile(j.num, s.URI) + ".ts"
		// The incident this file goes into, if any, stays open until the
		// segment's sidecar and record have joined it.
		held = c.m.incidents.hold(c.name, rec.File)
		if err := c.persistTo(held, kindSegments, rec.File, body); err != nil {
			c.m.incidents.release(held, rec.File)
			held = nil
			rec.File, rec.SaveError = "", err.Error()
		}
		f, ok := c.check(ctx, &rec, body, order)
		faults = append(faults, f...)
		if !ok {
			state = handledUnavailable
		}
	}
	if order.Gap != nil && state != handledFailed {
		rec.Gap = order.Gap
		c.log.Warn("monitor gap: these segments were not analyzed, so continuity is not checked across them",
			"from", order.Gap.From, "to", order.Gap.To)
	}
	c.advance(j, state, order)
	c.finish(rec, faults, held)
}

// gapTagged handles a segment the playlist marks EXT-X-GAP: it has no media,
// so it is not fetched and not compared; it is accounted for, so the next
// segment is not a monitor gap.
func (c *Channel) gapTagged(j segJob, rec *SegmentRecord, order analysis.Order) {
	rec.Events = append(rec.Events, analysis.Event{
		Type: analysis.EventGapTagged, Seq: j.num, GapMs: round3(j.seg.Duration * 1000),
		Detail: fmt.Sprintf("the playlist marks segment %d EXT-X-GAP: %.3f s with no media; not fetched", j.num, j.seg.Duration),
	})
	if order.Gap != nil {
		rec.Gap = order.Gap
	}
	c.chain.Break(j.num)
	c.dropBlack()
	c.advance(j, handledUnavailable, order)
}

// record starts the segment's record from its playlist entry.
func (j segJob) record() SegmentRecord {
	s := j.seg
	rec := SegmentRecord{
		Seq: j.num, MSN: s.Seq, URI: s.URI, PrevURI: j.prevURI, ExtInf: s.Duration,
		Discontinuity: s.Discontinuity, DiscSeq: s.DiscSeq, Tags: s.Tags, SCTE35: s.SCTE35,
	}
	if j.prevURI != "" {
		rec.PrevSeq = new(j.prevNum)
	}
	return rec
}

// discontinuity is the discontinuity fault for a segment the playlist
// tags with EXT-X-DISCONTINUITY.
func (j segJob) discontinuity() []analysis.Fault {
	s := j.seg
	if !s.Discontinuity {
		return nil
	}
	return []analysis.Fault{{
		Type:    analysis.FaultDiscontinuity,
		Seq:     j.num,
		Message: fmt.Sprintf("EXT-X-DISCONTINUITY before segment %d (discontinuity sequence %d)", j.num, s.DiscSeq),
		Values:  map[string]any{"reason": "tag", "discontinuity_sequence": s.DiscSeq},
	}}
}

// order places a job relative to the last segment the worker handled:
// adjacent when the playlist lists it right after that segment and that one
// was analyzed; after a monitor gap when segments between them were never
// analyzed. The origin may skip numbers, so only the playlist says which
// segment comes next.
func (c *Channel) order(j segJob) analysis.Order {
	if c.lastURI == "" {
		return analysis.Order{} // the first segment
	}
	if j.prevURI == c.lastURI && c.pending == nil {
		return analysis.Order{Adjacent: c.lastState == handledAnalyzed}
	}
	g := analysis.Gap{From: c.lastNum + 1, To: j.num - 1}
	if c.pending != nil {
		g.From = c.pending.From
	}
	switch {
	case j.prevURI == c.lastURI:
		g.To = c.lastNum // only segments we failed to fetch are missing
	case j.skipped != nil:
		g.To = min(g.To, j.skipped.From-1) // the origin never listed the rest
	}
	if g.To < g.From || j.num <= g.To {
		if c.pending == nil {
			// Nothing of ours is missing: the origin rewrote an entry, or
			// never listed what came between.
			return analysis.Order{}
		}
		g = *c.pending
	}
	return analysis.Order{Gap: &g}
}

// advance records how the worker's latest segment ended.
func (c *Channel) advance(j segJob, st handled, order analysis.Order) {
	c.pending = nil
	if st == handledFailed {
		g := analysis.Gap{From: j.num, To: j.num}
		if order.Gap != nil {
			g.From = min(order.Gap.From, j.num)
		}
		c.pending = &g
	}
	c.lastURI, c.lastNum, c.lastState = j.seg.URI, j.num, st
}

// saveRefusals writes the body of each refused attempt at segment num
// (with URI uri) next to the segment's files, as seg_<N>_<hash>.attemptK.body,
// and names the file in that attempt's metadata. bodies are the earlier
// attempts', body the last one's, which was refused too when failed.
func (c *Channel) saveRefusals(num uint64, uri string, meta *FetchMeta, bodies [][]byte, body []byte, failed bool) {
	save := func(m *FetchMeta, b []byte) {
		if !refused(m.Status) || len(b) == 0 {
			return
		}
		name := fmt.Sprintf("%s.attempt%d.body", segmentFile(num, uri), m.Attempts)
		if c.persist(kindSegments, name, b) == nil {
			m.BodyFile = name
		}
	}
	for i := range meta.Failed {
		if i < len(bodies) {
			save(&meta.Failed[i], bodies[i])
		}
	}
	if failed {
		save(meta, body)
	}
}

// refused reports an HTTP answer that isn't a success: any 4xx or 5xx (or
// another non-2xx). Only a missing answer is a network error.
func refused(status int) bool { return status != 0 && (status < 200 || status > 299) }

// fetchSegment downloads raw segment bytes. A refusal (any non-2xx answer)
// is retried once after OriginRetryDelay; our own network errors, including
// a 2xx whose body didn't arrive, are retried with backoff. The attempts
// before the last are kept in meta.Failed, and their bodies in bodies.
// recovered reports that a refusal was cured by the retry.
func (c *Channel) fetchSegment(ctx context.Context, url string) (body []byte, meta FetchMeta, bodies [][]byte, recovered bool, err error) {
	target := c.targetDuration()
	delay := c.m.opts.RetryDelay
	var failed []int
	var earlier []FetchMeta
	netRetries, originRetried := 0, false
	for attempt := 1; ; attempt++ {
		body, meta, err = c.m.fetch(ctx, url, false, segmentTimeout(target))
		meta.Attempts = attempt
		this := meta
		if refused(meta.Status) {
			failed = append(failed, meta.Status)
		}
		meta.FailedStatuses = slices.Clone(failed)
		meta.Failed = slices.Clone(earlier)
		if err == nil {
			return body, meta, bodies, len(failed) > 0, nil
		}
		switch {
		case refused(meta.Status):
			if originRetried || !sleepCtx(ctx, c.m.opts.OriginRetryDelay) {
				return body, meta, bodies, false, err
			}
			originRetried = true
		default:
			if netRetries >= c.m.opts.SegmentRetries || !sleepCtx(ctx, delay) {
				return body, meta, bodies, false, err
			}
			netRetries++
			delay *= 2
		}
		earlier, bodies = append(earlier, this), append(bodies, body)
	}
}

func unavailableFault(rec SegmentRecord) analysis.Fault {
	return analysis.Fault{
		Type: analysis.FaultUnavailable,
		Seq:  rec.Seq,
		Message: fmt.Sprintf("segment %d is listed in the playlist but the origin answered %v (%d attempts)",
			rec.Seq, rec.Fetch.FailedStatuses, rec.Fetch.Attempts),
		Values: map[string]any{
			"status":   rec.Fetch.Status,
			"statuses": rec.Fetch.FailedStatuses,
			"attempts": rec.Fetch.Attempts,
			"url":      rec.Fetch.URL,
			"age":      rec.Fetch.Headers.Get("Age"),
			"server":   rec.Fetch.Headers.Get("Server"),
		},
	}
}

// check analyzes a downloaded segment and runs every check on it. ok is
// false when the bytes are not usable TS.
func (c *Channel) check(ctx context.Context, rec *SegmentRecord, body []byte, order analysis.Order) (faults []analysis.Fault, ok bool) {
	seg, err := analysis.Analyze(body)
	if err != nil {
		c.chain.Break(rec.Seq)
		c.dropBlack()
		return []analysis.Fault{{
			Type:    analysis.FaultInvalidSegment,
			Seq:     rec.Seq,
			Message: "segment is not usable MPEG-TS: " + err.Error(),
			Values: map[string]any{
				"bytes":            len(body),
				"content_type":     rec.Fetch.Headers.Get("Content-Type"),
				"content_encoding": rec.Fetch.Headers.Get("Content-Encoding"),
				"error":            err.Error(),
			},
		}}, false
	}
	sum := seg.Summary()
	rec.Analysis = &sum
	order.Gap = nil // process reports it
	res := c.chain.Add(analysis.Input{
		Seq: rec.Seq, ExtInf: rec.ExtInf, Discontinuity: rec.Discontinuity, Segment: seg, Order: &order,
		FrameTicks: c.frameTicks(),
	})
	faults = res.Faults
	rec.Events = res.Events
	if b, ok := c.chain.Baseline(); ok {
		c.mu.Lock()
		c.baseline = new(b)
		c.mu.Unlock()
	}
	if !c.m.cfg.Blackdetect.Enabled {
		c.dropBlack()
		return faults, true
	}
	if skipped := blackSkipped(seg, rec.File); skipped != "" {
		rec.BlackDecode = &BlackDecode{NotChecked: skipped}
		c.log.Info("black not checked", "seq", rec.Seq, "why", skipped)
		c.count(func(s *stats) { s.blackNotChecked++ })
		c.dropBlack()
		return faults, true
	}
	br, err := c.m.blackRuns(ctx, filepath.Join(c.dir, rec.File), seg, c.frame(seg), c.cfg.ColorRange)
	notChecked, incomplete := errors.AsType[*notCheckedError](err)
	switch {
	case err != nil && !incomplete && ctx.Err() == nil:
		c.log.Warn("blackdetect failed", "seq", rec.Seq, "error", err)
		c.count(func(s *stats) { s.blackErrors++ })
		c.dropBlack()
	case err == nil || incomplete:
		rec.Black, rec.BlackDecode = br.runs, &br.decode
		c.count(func(s *stats) {
			s.leadingDropped += br.decode.LeadingDropped
			if incomplete {
				s.blackNotChecked++
			}
		})
		if incomplete {
			c.log.Warn("black not fully checked; black found in it is unconfirmed", "seq", rec.Seq, "why", notChecked.why)
		}
		// Strips need the frames ffmpeg decoded, not a stand-in detector's.
		c.tileFile, c.tilePID, c.strip = "", seg.Video.PID, nil
		if c.m.opts.BlackDetector == nil {
			c.tileFile = rec.File
		}
		if f, ok := c.blackTrigger(rec.Seq, order.Adjacent && !rec.Discontinuity, seg, rec.Black); ok {
			if c.strip != nil {
				c.strip.uri, c.strip.name = rec.URI, stripName(rec.Seq, rec.URI)
				f.Values["thumbnails"] = c.strip.name
			}
			faults = append(faults, f)
		}
		c.lastFrame = nil
		if br.last != nil && c.tileFile != "" {
			c.lastFrame = &frameRef{file: c.tileFile, pid: c.tilePID, pts: *br.last}
		}
		if br.decode.Undecoded > 0 {
			faults = append(faults, undecodedFault(rec.Seq, br.decode))
		}
	}
	return faults, true
}

// blackSkipped says why a segment's black check can't run at all, or "".
func blackSkipped(seg *analysis.Segment, file string) string {
	switch {
	case seg.Video == nil:
		return "the segment has no video stream"
	case file == "":
		return "the segment's file could not be saved for ffmpeg"
	}
	return ""
}

// blackTrigger joins black runs across segment boundaries and returns a
// fault when the run with the most black frames has trigger_min of them:
// time with no frame, and frames ffmpeg did not decode, never count. A run that lasts to the
// end of the segment stays open until the next segment shows whether it
// goes on; every run is counted, once it ends, as short if it never reached
// trigger_min. Runs are joined in PTS (see joinsBlack).
//
// adjacent says the playlist lists this segment right after the previous
// one analyzed, with no discontinuity between them, so a run carried from
// that one can continue here.
func (c *Channel) blackTrigger(seq uint64, adjacent bool, seg *analysis.Segment, runs []BlackInterval) (analysis.Fault, bool) {
	carry := c.black
	c.black = blackCarry{}
	frame := c.frame(seg)
	joins := carry.open && adjacent && len(runs) > 0 && runs[0].FromStart && joinsBlack(carry.end, runs[0].StartPTS, frame)
	if carry.open && !joins {
		c.endBlackRun(carry.blackS) // it ended at the boundary
	}
	var best blackSpan
	for i, run := range runs {
		s := blackSpan{
			start: run.StartPTS, end: run.EndPTS, seconds: run.Duration,
			frames: run.Frames, blackS: run.BlackS, undecoded: run.UndecodedFrames, unconfirmed: run.Unconfirmed,
		}
		s.tiles = &blackTiles{}
		if i == 0 && joins {
			s.tiles = carry.tiles.clone()
		} else if run.before != nil {
			s.tiles.before = &frameRef{file: c.tileFile, pid: c.tilePID, pts: *run.before}
		} else if adjacent && run.FromStart {
			s.tiles.before = c.lastFrame
		}
		for _, p := range run.black {
			s.tiles.add(frameRef{file: c.tileFile, pid: c.tilePID, pts: p})
		}
		if run.after != nil {
			s.tiles.after = &frameRef{file: c.tileFile, pid: c.tilePID, pts: *run.after}
		}
		if i == 0 && joins {
			s.start, s.frames = carry.start, carry.frames+run.Frames
			s.blackS, s.undecoded = carry.blackS+run.BlackS, carry.undecoded+run.UndecodedFrames
			s.unconfirmed = s.unconfirmed || carry.unconfirmed
			s.seconds = float64(ts.Diff(run.EndPTS, s.start)) / ts.Hz
			s.joined = float64(ts.Diff(run.StartPTS, s.start)) / ts.Hz
		}
		if s.blackS > best.blackS {
			best = s
		}
		if i == len(runs)-1 && run.ToEnd {
			c.black = blackCarry{
				open: true, start: s.start, end: s.end, seconds: s.seconds,
				frames: s.frames, blackS: s.blackS, undecoded: s.undecoded, unconfirmed: s.unconfirmed,
				tiles: s.tiles,
			}
		} else {
			c.endBlackRun(s.blackS)
		}
	}
	bd := c.m.cfg.Blackdetect
	if len(runs) == 0 || best.blackS < bd.TriggerMin {
		return analysis.Fault{}, false
	}
	f := blackFault(seq, runs, best, frame, bd)
	if c.tileFile != "" && best.tiles != nil && best.tiles.seen > 0 {
		c.strip = &stripJob{seq: seq, tiles: best.tiles}
	}
	return f, true
}

// blackSpan is a black run, possibly joined across segments.
type blackSpan struct {
	start, end uint64  // PTS: the first black frame, and one frame past the last
	seconds    float64 // on screen
	frames     int     // black frames
	blackS     float64 // their time on screen
	undecoded  int     // frames inside it ffmpeg did not decode
	joined     float64 // seconds of it before this segment
	// unconfirmed: part of it is in a segment not fully checked.
	unconfirmed bool
	tiles       *blackTiles // where its frames are, for its thumbnails
}

// endBlackRun counts a black run that has ended as short if it had too few
// black frames (black seconds of them) to open an incident by itself.
func (c *Channel) endBlackRun(black float64) {
	if bd := c.m.cfg.Blackdetect; black >= bd.Duration && black < bd.TriggerMin {
		c.count(func(s *stats) { s.shortBlack++ })
	}
}

// dropBlack ends an open black run where the next segment can't continue
// it (a gap, an unusable segment, a failed check or a restart).
func (c *Channel) dropBlack() {
	if c.black.open {
		c.endBlackRun(c.black.blackS)
	}
	c.black = blackCarry{}
}

// finish records a processed segment: sidecar, buffer history, stats,
// fault logging and incident handling.
//
// held is the incident the segment's file went into, if known: its sidecar
// and record go there too, as they do into an incident that copied the
// file from the buffer while the segment was being checked.
func (c *Channel) finish(rec SegmentRecord, faults []analysis.Fault, held *incident) {
	target := held
	if target == nil && rec.File != "" {
		target = c.m.incidents.claim(c.name, rec.File)
	}
	defer c.m.incidents.release(target, rec.File)
	rec.addFaults(faults)
	c.persistJSONTo(target, kindSegments, segmentFile(rec.Seq, rec.URI)+".json", rec)
	c.remember(rec)
	c.logEvents(rec)
	for _, f := range faults {
		c.log.Warn("fault", "type", f.Type, "seq", f.Seq, "uri", f.URI, "detail", f.Message)
	}
	if len(faults) > 0 {
		c.m.incidents.faultFrom(c, faults, rec, target)
		if job := c.strip; job != nil && job.seq == rec.Seq {
			c.strip = nil
			if dir := c.m.incidents.openDir(c.name); dir != "" {
				if err := c.makeStrip(c.m.runContext(), dir, *job); err != nil {
					c.log.Warn("cannot make the black fault's thumbnails", "seq", rec.Seq, "error", err)
					c.m.incidents.note(c, fmt.Sprintf("the thumbnails for the black fault on segment %d could not be made: %v", rec.Seq, err))
				}
			}
		}
	} else {
		c.m.incidents.segmentFrom(c, rec, target)
	}
	if hook := c.m.opts.OnSegment; hook != nil {
		hook(c.name, rec)
	}
	// Only now may the buffer from before a stall age out: anything this
	// segment triggered has captured it already.
	c.mu.Lock()
	c.lastSegAt = c.m.now()
	c.mu.Unlock()
}

// addFaults names the record's faults in it, and gives the faults on this
// segment its URI.
func (rec *SegmentRecord) addFaults(faults []analysis.Fault) {
	for i := range faults {
		if faults[i].URI == "" && faults[i].Seq == rec.Seq {
			faults[i].URI = rec.URI
		}
		rec.Faults = append(rec.Faults, faults[i].Type)
	}
}

// eventColumns are data/events.csv's columns, in order.
var eventColumns = []string{
	"time_utc", "channel", "seq", "type", "video_frames", "audio_frames", "gap_ms", "scte35", "scte35_tag", "detail",
}

// cueColumns are data/scte35.csv's columns, in order.
var cueColumns = []string{"time_utc", "channel", "seq", "scte35_tag", "extinf", "tags"}

// logEvents writes a row to data/scte35.csv if the segment carries SCTE-35
// tags, and one to data/events.csv for each irregularity in it. scte35 says
// whether this segment or the one before it carried a SCTE-35 tag;
// scte35_tag names this segment's own (see hls.CueKind).
func (c *Channel) logEvents(rec SegmentRecord) {
	tagged := len(rec.SCTE35) > 0
	cue := tagged || c.cue.tagged && rec.PrevURI != "" && c.cue.uri == rec.PrevURI
	c.cue = cueMark{uri: rec.URI, tagged: tagged}
	kind := hls.CueKind(rec.SCTE35)
	when := rec.Fetch.RequestedAt
	if when.IsZero() { // never fetched: its URI didn't resolve
		when = c.m.now()
	}
	at := when.UTC().Format("2006-01-02T15:04:05.000Z")
	seq := strconv.FormatUint(rec.Seq, 10)
	if tagged {
		c.log.Debug("scte35 cue", "seq", rec.Seq, "tag", kind, "tags", rec.SCTE35)
		row := map[string]string{
			"time_utc": at, "channel": c.name, "seq": seq, "scte35_tag": kind, "extinf": num(rec.ExtInf),
			"tags": strings.Join(rec.SCTE35, " | "),
		}
		if err := c.m.appendCSV("scte35.csv", cueColumns, row); err != nil {
			c.log.Error("cannot append to scte35.csv", "error", err)
		}
	}
	for _, e := range rec.Events {
		c.log.Debug("irregular segment", "seq", e.Seq, "type", e.Type, "gap_ms", e.GapMs, "detail", e.Detail)
		row := map[string]string{
			"time_utc": at, "channel": c.name, "seq": strconv.FormatUint(e.Seq, 10), "type": e.Type,
			"gap_ms": num(e.GapMs), "scte35": strconv.FormatBool(cue), "scte35_tag": kind, "detail": e.Detail,
		}
		if a := rec.Analysis; a != nil && a.Video != nil {
			row["video_frames"] = strconv.Itoa(a.Video.Frames)
		}
		if a := rec.Analysis; a != nil && a.Audio != nil {
			row["audio_frames"] = strconv.Itoa(a.Audio.Frames)
		}
		if err := c.m.appendCSV("events.csv", eventColumns, row); err != nil {
			c.log.Error("cannot append to events.csv", "error", err)
		}
	}
}

// remember counts a processed segment for the health line and notes it as
// the newest.
func (c *Channel) remember(rec SegmentRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeq, c.lastSegURI, c.anyRecord = rec.Seq, rec.URI, true
	c.st.record(rec)
}

// latestSeq returns the newest segment handled, if any.
func (c *Channel) latestSeq() (seq uint64, uri string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeq, c.lastSegURI, c.anyRecord
}

func (c *Channel) count(f func(*stats)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.st)
}

// blackFault turns a segment's black runs into a fault; longest is the
// run through it with the most black frames and frame the video frame
// duration in ticks.
func blackFault(seq uint64, runs []BlackInterval, longest blackSpan, frame int64, bd config.Blackdetect) analysis.Fault {
	var total float64
	list := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		total += r.BlackS
		list = append(list, map[string]any{
			"start_s":          r.Start,
			"end_s":            r.End,
			"duration_s":       r.Duration,
			"start_pts":        r.StartPTS,
			"end_pts":          r.EndPTS,
			"black_frames":     r.Frames,
			"black_frames_s":   r.BlackS,
			"undecoded_frames": r.UndecodedFrames,
			"no_frame_s":       r.NoFrameS,
			"unconfirmed":      r.Unconfirmed,
		})
	}
	noFrameS := noFrame(longest.seconds, longest.blackS, longest.undecoded, frame)
	msg := fmt.Sprintf("%.3f s of black frames (%d frames) over a black run of %.3f s on screen, which has %.3f s with no frame and %d frames ffmpeg could not decode (trigger_min %g s)",
		longest.blackS, longest.frames, longest.seconds, noFrameS, longest.undecoded, bd.TriggerMin)
	if longest.joined > 0 {
		msg += fmt.Sprintf(", including %.3f s on screen before this segment", longest.joined)
	}
	values := map[string]any{
		"intervals":        list,
		"total_black_s":    round3(total),
		"longest_run_s":    round3(longest.seconds),
		"black_frames":     longest.frames,
		"black_frames_s":   round3(longest.blackS),
		"undecoded_frames": longest.undecoded,
		"no_frame_s":       round3(noFrameS),
		"run_start_pts":    longest.start,
		"run_end_pts":      longest.end,
		"trigger_min":      bd.TriggerMin,
		"d":                bd.Duration,
		"pix_th":           bd.PixelThreshold,
		"pic_th":           bd.PictureThreshold,
	}
	if n := len(runs); n > 0 {
		values["pix_fmt"], values["color_range"] = runs[n-1].PixFmt, runs[n-1].ColorRange
	}
	if longest.joined > 0 {
		values["joined_previous_s"] = round3(longest.joined)
	}
	if longest.unconfirmed {
		msg += "; unconfirmed: ffmpeg did not fully decode a segment it spans"
		values["unconfirmed"] = true
	}
	return analysis.Fault{Type: analysis.FaultBlackVideo, Seq: seq, Message: msg, Values: values}
}

// undecodedFault reports frames the parser found that ffmpeg did not
// decode after the first frame it did.
func undecodedFault(seq uint64, d BlackDecode) analysis.Fault {
	return analysis.Fault{
		Type: analysis.FaultUndecodedFrames,
		Seq:  seq,
		Message: fmt.Sprintf("ffmpeg decoded %d of the %d frames the TS parser found: %d frames (%.3f s) after the first one it decoded were not decoded, so the black check did not see them",
			d.Decoded, d.Frames, d.Undecoded, d.UndecodedS),
		Values: map[string]any{
			"frames":          d.Frames,
			"decoded":         d.Decoded,
			"undecoded":       d.Undecoded,
			"undecoded_s":     d.UndecodedS,
			"leading_dropped": d.LeadingDropped,
			"ranges":          d.UndecodedRanges,
		},
	}
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
