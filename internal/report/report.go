// Package report summarizes a monitoring run from the CSV files in the data
// directory: per channel, the SCTE-35 splice points, where the irregular
// segments fall relative to them, the ad-break cadence and the faults.
package report

import (
	"bufio"
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
)

// Options selects what to report.
type Options struct {
	DataDir  string
	From, To time.Time // the window: From inclusive, To exclusive
	Channel  string    // "" reports every channel with data in the window
}

// margin is how far outside the window cues, irregular segments and health
// lines are read, so that segments near either end are placed by the break
// around them and the time between breaks can be checked for gaps.
const margin = time.Hour

// maxSegments bounds a health line's segment count: far above any real
// interval, low enough that a corrupt value can't stall the report.
const maxSegments = 100_000

// openLimit is how long after opening an incident can still be open with
// the default max_incident (10m) and post_roll (60s). An incident whose
// report.json still says open, but that neither opened within it nor has
// been written since, was left open by a crash.
const openLimit = 11 * time.Minute

// maxLine is longer than any line the monitor writes.
const maxLine = 64 * 1024

// Places relative to the ad breaks, in table order.
const (
	atSplice      = iota // within one segment of a CUE-OUT or CUE-IN segment
	inBreak              // from CUE-OUT up to CUE-IN otherwise
	inProgramming        // everywhere else
	places
)

// eventTypes are the irregularities in table order. Other types found in
// events.csv follow them, alphabetically.
var eventTypes = []string{
	analysis.EventFrameGap, analysis.EventOddLength, analysis.EventAudioRetimed, analysis.EventAudioGap,
}

// Run writes the report to w.
func Run(w io.Writer, o Options) error {
	d, err := load(o)
	if err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(d.seen))
	where := fmt.Sprintf("from %s to %s in %s", o.From.UTC().Format(time.RFC3339), o.To.UTC().Format(time.RFC3339), o.DataDir)
	switch {
	case len(names) == 0:
		return fmt.Errorf("no data %s", where)
	case o.Channel != "" && !d.seen[o.Channel]:
		return fmt.Errorf("no data for channel %q %s (channels with data: %s)", o.Channel, where, strings.Join(names, ", "))
	case o.Channel != "":
		names = []string{o.Channel}
	}

	p := printer{w: bufio.NewWriter(w), from: o.From, clock: "15:04:05"}
	if !sameDay(o.From, o.To.Add(-time.Nanosecond)) {
		p.clock = "01-02 15:04:05"
	}
	p.header(o, d.notes)
	total := summary{name: "all channels"}
	for _, name := range names {
		s := d.channel(name).summarize(o)
		p.channel(s)
		total.add(s)
	}
	if len(names) > 1 {
		p.total(total)
	}
	return p.w.Flush()
}

// --- reading the data directory ----------------------------------------------

// data is what the data directory holds for the report.
type data struct {
	channels map[string]*channel
	seen     map[string]bool   // channels with a row inside the window
	notes    []string          // what couldn't be read, or isn't there
	strs     map[string]string // interned tag and event types
	// oldEvents is set by an events.csv row in the window without a
	// scte35_tag column: one from before the monitor logged cues.
	oldEvents bool
}

// channel is one channel's rows.
type channel struct {
	name      string
	irregular map[uint64]*segment // irregular segments, by sequence number
	cues      map[uint64]cue      // segments with SCTE-35 tags, by sequence number
	incidents []incident          // incidents that overlap the window
	// From the health lines: the sums over the window, and the segments
	// counted by the lines in the window (analyzed) and by every line read,
	// margin included (monitored).
	gaps, faults        int
	analyzed, monitored []span
}

// span is a run of sequence numbers, lo through hi.
type span struct{ lo, hi uint64 }

// segment is an irregular segment: when it was fetched and its event types.
type segment struct {
	at    time.Time
	types []string
}

// cue is a segment's SCTE-35 tags: when it was fetched, their tag type (see
// hls.CueKind), such as OUT or IN+OUT, the segment's EXTINF and, for a
// CUE-OUT, the break's declared length.
type cue struct {
	at       time.Time
	kind     string
	extinf   float64
	timed    bool // extinf is known; files from before it was logged lack it
	planned  float64
	declared bool // planned is known
}

func (c cue) has(kind string) bool {
	for k := range strings.SplitSeq(c.kind, "+") {
		if k == kind {
			return true
		}
	}
	return false
}

type incident struct {
	id, status, types, dir string
	opened                 time.Time
	faults                 int
}

func (d *data) channel(name string) *channel {
	c := d.channels[name]
	if c == nil {
		c = &channel{name: name, irregular: map[uint64]*segment{}, cues: map[uint64]cue{}}
		d.channels[name] = c
	}
	return c
}

// keep reports whether to keep a channel's row at t: t is in the window, or
// in the margin around it if wide, and the channel is being reported. A row
// inside the window marks the channel as having data.
func (d *data) keep(o Options, name string, t time.Time, wide bool) bool {
	from, to := o.From, o.To
	if wide {
		from, to = from.Add(-margin), to.Add(margin)
	}
	if t.Before(from) || !t.Before(to) {
		return false
	}
	if !t.Before(o.From) && t.Before(o.To) {
		d.seen[name] = true
	}
	return o.Channel == "" || name == o.Channel
}

// intern returns a copy of s shared by every equal string, so that a
// value kept from a row doesn't hold on to the whole line.
func (d *data) intern(s string) string {
	if v, ok := d.strs[s]; ok {
		return v
	}
	s = strings.Clone(s)
	d.strs[s] = s
	return s
}

func load(o Options) (*data, error) {
	if _, err := os.Stat(o.DataDir); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}
	d := &data{channels: map[string]*channel{}, seen: map[string]bool{}, strs: map[string]string{}}

	// Rows outside the window and its margin are skipped by time, unparsed.
	far := func(t time.Time) bool { return t.Before(o.From.Add(-margin)) || !t.Before(o.To.Add(margin)) }
	in := func(t time.Time) bool { return !t.Before(o.From) && t.Before(o.To) }
	// Sequence numbers start over when a stream resets; rows after a reset
	// are a new epoch, so the same number means another segment there.
	resets, err := numberResets(o.DataDir, far)
	if err != nil {
		return nil, err
	}
	epoch := func(name string, t time.Time, seq uint64) uint64 {
		n, _ := slices.BinarySearchFunc(resets[name], t, func(r, t time.Time) int {
			if r.After(t) {
				return 1
			}
			return -1
		})
		return seq + uint64(n)<<epochShift
	}

	cueFiles, err := d.read(o.DataDir, "scte35.csv", far, func(r row) bool {
		name, t, seq, ok := r.stamp()
		c := cue{at: t, kind: r.col("scte35_tag")}
		if !ok || c.kind == "" {
			return false
		}
		seq = epoch(name, t, seq)
		if s := r.col("extinf"); s != "" {
			var err error
			if c.extinf, err = strconv.ParseFloat(s, 64); err != nil || c.extinf < 0 {
				return false
			}
			c.timed = true
		}
		if c.has(hls.CueOut) {
			c.planned, c.declared = hls.CueDuration(strings.Split(r.col("tags"), " | "))
		}
		if d.keep(o, name, t, true) {
			ch := d.channel(name)
			if _, dup := ch.cues[seq]; !dup {
				c.kind = d.intern(c.kind)
				ch.cues[seq] = c
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	_, err = d.read(o.DataDir, "events.csv", far, func(r row) bool {
		name, t, seq, ok := r.stamp()
		typ := r.col("type")
		if !ok || typ == "" {
			return false
		}
		seq = epoch(name, t, seq)
		if _, tagged := r.index["scte35_tag"]; !tagged && in(t) {
			d.oldEvents = true
		}
		if d.keep(o, name, t, true) {
			ch := d.channel(name)
			g := ch.irregular[seq]
			if g == nil {
				g = &segment{at: t}
				ch.irregular[seq] = g
			}
			if !slices.Contains(g.types, typ) {
				g.types = append(g.types, d.intern(typ))
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	_, err = d.read(o.DataDir, "health.csv", far, func(r row) bool {
		name := r.col("channel")
		at, err := time.Parse(time.RFC3339, r.col("time_utc"))
		segs, ok1 := count(r.col("segments"))
		refused, ok2 := count(r.col("segment_errors"))
		gaps, ok3 := count(r.col("monitor_gaps"))
		faults, ok4 := count(r.col("faults"))
		if name == "" || err != nil || !ok1 || !ok2 || !ok3 || !ok4 || segs+refused > maxSegments {
			return false
		}
		var seq uint64
		if s := r.col("seq"); s != "" {
			if seq, err = strconv.ParseUint(s, 10, 64); err != nil {
				return false
			}
			seq = epoch(name, at, seq)
		} else {
			segs, refused = 0, 0 // no segment yet
		}
		if !d.keep(o, name, at, true) {
			return true
		}
		// A health line counts the segments processed since the one before
		// it, which end at its seq: analyzed, or failed to download (their
		// tags were still logged).
		ch := d.channel(name)
		window := in(at)
		if window {
			ch.gaps += gaps
			ch.faults += faults
		}
		if n := uint64(segs + refused); n > 0 {
			ch.monitored = addSpan(ch.monitored, span{seq + 1 - min(n, seq+1), seq})
		}
		if n := uint64(segs); n > 0 && window {
			ch.analyzed = addSpan(ch.analyzed, span{seq + 1 - min(n, seq+1), seq})
		}
		return true
	})
	if err != nil {
		return nil, err
	}

	switch {
	case d.oldEvents:
		d.notes = append(d.notes, "events.csv has rows in the window from before stream-analyzer logged SCTE-35 cues:"+
			" breaks then are unknown, so those segments count as in programming")
	case cueFiles == 0:
		d.notes = append(d.notes, "no scte35.csv: no SCTE-35 cues were logged, so every segment counts as in programming")
	}

	incidents := filepath.Join(o.DataDir, "incidents")
	indexed := map[string]bool{}
	_, err = d.read(incidents, "incidents.csv", nil, func(r row) bool {
		in := incident{id: r.col("id"), status: r.col("status"), types: r.col("fault_types"), dir: r.col("dir")}
		name := r.col("channel")
		var closed time.Time
		var err, err2 error
		in.opened, err = time.Parse(time.RFC3339, r.col("opened_utc"))
		if s := r.col("closed_utc"); s != "" {
			closed, err2 = time.Parse(time.RFC3339, s)
		}
		var ok bool
		in.faults, ok = count(r.col("fault_count"))
		if in.id == "" || name == "" || err != nil || err2 != nil || !ok {
			return false
		}
		indexed[in.id] = true
		if closed.IsZero() {
			closed = in.opened // interrupted by a crash: no end was recorded
		}
		d.addIncident(o, name, in, closed)
		return true
	})
	if err != nil {
		return nil, err
	}
	if err := d.unindexed(o, incidents, indexed); err != nil {
		return nil, err
	}
	return d, nil
}

// epochShift places each epoch of sequence numbers (see numberResets) far
// above the last: real numbers stay far below 2^40.
const epochShift = 40

// numberResets finds, per channel, when its sequence numbers started over
// (a stream reset): the monitor processes segments in order, so a number
// well below the one before in events.csv, scte35.csv or health.csv marks
// one. A reset seen in several files within 15 minutes is one reset, from
// the earliest.
func numberResets(dir string, skip func(time.Time) bool) (map[string][]time.Time, error) {
	found := map[string][]time.Time{}
	for _, name := range []string{"events.csv", "scte35.csv", "health.csv"} {
		last := map[string]uint64{}
		var scratch data
		if _, err := scratch.read(dir, name, skip, func(r row) bool {
			ch, t, seq, ok := r.stamp()
			if !ok {
				return true
			}
			if prev, seen := last[ch]; seen && seq+10 < prev {
				found[ch] = append(found[ch], t)
			}
			last[ch] = seq
			return true
		}); err != nil {
			return nil, err
		}
	}
	for ch, ts := range found {
		slices.SortFunc(ts, time.Time.Compare)
		var merged []time.Time
		for _, t := range ts {
			if n := len(merged); n == 0 || t.Sub(merged[n-1]) > 15*time.Minute {
				merged = append(merged, t)
			}
		}
		found[ch] = merged
	}
	return found, nil
}

// addIncident keeps an incident if it overlaps the window.
func (d *data) addIncident(o Options, name string, in incident, end time.Time) {
	if !in.opened.Before(o.To) || end.Before(o.From) {
		return
	}
	d.seen[name] = true
	if o.Channel == "" || name == o.Channel {
		ch := d.channel(name)
		ch.incidents = append(ch.incidents, in)
	}
}

// unindexed adds the incidents that overlap the window but aren't in
// incidents.csv yet, such as those still open, from their report.json.
func (d *data) unindexed(o Options, dir string, indexed map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		stamp, _, _ := strings.Cut(e.Name(), "_")
		opened, err := time.Parse("20060102T150405Z", stamp)
		if !e.IsDir() || err != nil || indexed[e.Name()] || !opened.Before(o.To) {
			continue
		}
		path := filepath.Join(dir, e.Name(), "report.json")
		b, err := os.ReadFile(path)
		info, err2 := os.Stat(path)
		if err != nil || err2 != nil {
			continue // not written yet, or deleted by the storage cap
		}
		var r struct {
			ID         string    `json:"id"`
			Channel    string    `json:"channel"`
			Status     string    `json:"status"`
			OpenedAt   time.Time `json:"opened_at"`
			ClosedAt   time.Time `json:"closed_at"`
			FaultTypes []string  `json:"fault_types"`
			FaultCount int       `json:"fault_count"`
		}
		if err := json.Unmarshal(b, &r); err != nil || r.Channel == "" {
			d.notes = append(d.notes, fmt.Sprintf("cannot read incidents/%s/report.json", e.Name()))
			continue
		}
		// An open incident runs until its report.json was last written (it
		// is rewritten with every segment) or, since a stall holds it open
		// without segments, for as long as the default limits allow.
		end := r.ClosedAt
		switch {
		case r.Status == "open":
			end = info.ModTime()
			if limit := r.OpenedAt.Add(openLimit); limit.After(end) {
				end = limit
			}
		case end.IsZero():
			end = r.OpenedAt
		}
		d.addIncident(o, r.Channel, incident{id: r.ID, status: r.Status, types: strings.Join(r.FaultTypes, ";"),
			dir: filepath.Join("incidents", e.Name()), opened: r.OpenedAt, faults: r.FaultCount}, end)
	}
	return nil
}

// row is one CSV row, read by column name.
type row struct {
	index  map[string]int
	fields []string
}

func (r row) col(name string) string {
	if i, ok := r.index[name]; ok && i < len(r.fields) {
		return r.fields[i]
	}
	return ""
}

// stamp returns the row's channel, time_utc and seq.
func (r row) stamp() (name string, t time.Time, seq uint64, ok bool) {
	t, err := time.Parse(time.RFC3339, r.col("time_utc"))
	seq, err2 := strconv.ParseUint(r.col("seq"), 10, 64)
	name = r.col("channel")
	return name, t, seq, err == nil && err2 == nil && name != ""
}

// count parses a non-negative count; "" is 0.
func count(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

// read calls fn with each row of dir/name, and of the files the monitor
// moved aside when their columns changed (name.<UTC time>.csv). fn reports
// whether it could read the row; lines that couldn't be read are noted. If
// skip is set and time_utc is the first column, rows it skips by time are
// not parsed further. read returns how many files it found.
func (d *data) read(dir, name string, skip func(time.Time) bool, fn func(row) bool) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var paths []string
	base := strings.TrimSuffix(name, ".csv")
	for _, e := range entries {
		if mid, ok := strings.CutPrefix(e.Name(), base+"."); ok && strings.HasSuffix(mid, ".csv") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	paths = append(paths, filepath.Join(dir, name))
	files, bad := 0, 0
	for _, path := range paths {
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return files, err
		}
		files++
		n, err := readCSV(f, skip, fn)
		f.Close()
		if err != nil {
			return files, fmt.Errorf("%s: %w", path, err)
		}
		bad += n
	}
	if bad > 0 {
		d.notes = append(d.notes, fmt.Sprintf("skipped %s in %s", plural(bad, "unreadable line"), name))
	}
	return files, nil
}

// readCSV calls fn with each row after the header, and returns how many
// lines couldn't be read. Each line is parsed on its own: the monitor never
// writes a field with a line break, so a row torn by a crash, with the next
// row appended to its line, costs only that line.
func readCSV(r io.Reader, skip func(time.Time) bool, fn func(row) bool) (bad int, err error) {
	br := bufio.NewReaderSize(r, maxLine)
	var index map[string]int
	width := 0
	for {
		b, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n') // too long for a row: drop the rest of it
			}
			b, bad = nil, bad+1
		}
		switch {
		case errors.Is(err, io.EOF) && len(b) == 0:
			return bad, nil
		case errors.Is(err, io.EOF):
			// No newline: a row the monitor was writing when it stopped,
			// perhaps cut short.
			return bad + 1, nil
		case err != nil:
			return bad, err
		}
		line := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		if line == "" {
			continue
		}
		if index != nil && skip != nil {
			first, _, _ := strings.Cut(line, ",")
			if t, err := time.Parse(time.RFC3339, first); err == nil && skip(t) {
				continue
			}
		}
		fields, err := splitCSV(line)
		switch {
		case index == nil:
			if err != nil {
				return 0, err
			}
			index, width = make(map[string]int, len(fields)), len(fields)
			for i, h := range fields {
				index[h] = i
			}
			if fields[0] != "time_utc" {
				skip = nil
			}
		case err != nil || len(fields) != width || !fn(row{index, fields}):
			bad++
		}
	}
}

var errQuote = errors.New("bad quoting")

// splitCSV parses one line as encoding/csv writes it: a field with a comma,
// quote or leading space is quoted, with its quotes doubled.
func splitCSV(line string) ([]string, error) {
	var fields []string
	for {
		quoted, ok := strings.CutPrefix(line, `"`)
		if !ok {
			field, rest, more := strings.Cut(line, ",")
			if strings.Contains(field, `"`) {
				return nil, errQuote
			}
			fields = append(fields, field)
			if !more {
				return fields, nil
			}
			line = rest
			continue
		}
		var b strings.Builder
		for {
			i := strings.IndexByte(quoted, '"')
			if i < 0 {
				return nil, errQuote
			}
			b.WriteString(quoted[:i])
			quoted = quoted[i+1:]
			if rest, ok := strings.CutPrefix(quoted, `"`); ok {
				b.WriteByte('"')
				quoted = rest
				continue
			}
			break
		}
		fields = append(fields, b.String())
		if quoted == "" {
			return fields, nil
		}
		rest, ok := strings.CutPrefix(quoted, ",")
		if !ok {
			return nil, errQuote
		}
		line = rest
	}
}

// --- placing segments relative to the breaks ----------------------------------

// breaks places a channel's segments relative to its ad breaks.
type breaks struct {
	cues   map[uint64]cue
	near   map[uint64]bool // within one segment of a CUE-OUT or CUE-IN segment
	ranges [][2]uint64     // [CUE-OUT, CUE-IN) of each break with both ends seen, in order
}

func newBreaks(cues map[uint64]cue) *breaks {
	b := &breaks{cues: cues, near: map[uint64]bool{}}
	var out uint64
	open := false
	for _, seq := range slices.Sorted(maps.Keys(cues)) {
		c := cues[seq]
		if c.has(hls.CueOut) || c.has(hls.CueIn) {
			b.near[seq-1], b.near[seq], b.near[seq+1] = true, true, true
		}
		// A break that ends where the next begins is closed first, whatever
		// order the tags come in.
		if c.has(hls.CueIn) && open {
			b.ranges = append(b.ranges, [2]uint64{out, seq})
			open = false
		}
		if c.has(hls.CueOut) {
			// A CUE-OUT while a break is open means its CUE-IN went unseen.
			out, open = seq, true
		}
	}
	return b
}

// place says where a segment is: at a splice, in a break (between a CUE-OUT
// and its CUE-IN, or tagged CUE-OUT-CONT itself) or in programming.
func (b *breaks) place(seq uint64) int {
	if b.near[seq] {
		return atSplice
	}
	if b.cues[seq].has(hls.CueCont) {
		return inBreak
	}
	// The last break starting at or before seq.
	i, found := slices.BinarySearchFunc(b.ranges, seq, func(r [2]uint64, seq uint64) int { return cmp.Compare(r[0], seq) })
	if found || i > 0 && seq < b.ranges[i-1][1] {
		return inBreak
	}
	return inProgramming
}

// --- summarizing ---------------------------------------------------------------

// summary is one section of the report.
type summary struct {
	name             string
	segments, gaps   int       // monitored segments and monitor gaps (health.csv)
	outs, ins        int       // CUE-OUT and CUE-IN segments in the window
	irregularSplices int       // splice points with an irregular segment within one segment
	cadence          []float64 // minutes between consecutive CUE-OUTs
	planned          []float64 // seconds each break was declared to last
	lengths          []float64 // seconds from each CUE-OUT to its CUE-IN, by EXTINF
	faults           int       // faults on the health lines
	incidents        []incident
	rows             []tally // all monitored, any irregular, then each type
	splices          []splicePoint
}

// tally counts segments by place.
type tally struct {
	label string
	n     [places]int
}

func (t tally) total() int { return t.n[atSplice] + t.n[inBreak] + t.n[inProgramming] }

// splicePoint is a CUE-OUT or CUE-IN segment and the irregular segments
// around it.
type splicePoint struct {
	at   time.Time
	kind string // CUE-OUT or CUE-IN
	seq  uint64
	near [3][]string // event types on the segments before, at and after it
}

// summarize reports one channel over the window.
func (ch *channel) summarize(o Options) summary {
	b := newBreaks(ch.cues)
	in := func(t time.Time) bool { return !t.Before(o.From) && t.Before(o.To) }
	s := summary{name: ch.name, incidents: ch.incidents, gaps: ch.gaps, faults: ch.faults}
	slices.SortFunc(s.incidents, func(a, b incident) int { return a.opened.Compare(b.opened) })

	// The segments the health lines in the window counted give the base
	// rate; those every line read counted show which stretches the monitor
	// saw.
	monitored := merge(ch.monitored)
	all := tally{label: "all monitored"}
	for _, run := range merge(ch.analyzed) {
		for k := range run.hi - run.lo + 1 {
			all.n[b.place(run.lo+k)]++
		}
	}

	anyType := tally{label: "any irregular"}
	byType := map[string]*tally{}
	for _, typ := range eventTypes {
		byType[typ] = &tally{label: typ}
	}
	for seq, g := range ch.irregular {
		if !in(g.at) {
			continue
		}
		p := b.place(seq)
		anyType.n[p]++
		for _, typ := range g.types {
			if byType[typ] == nil {
				byType[typ] = &tally{label: typ}
			}
			byType[typ].n[p]++
		}
	}
	// Counted from the segments seen, not summed from the health lines:
	// after a restart the first playlist's segments are processed again.
	s.segments = all.total()
	s.rows = []tally{all, anyType}
	for _, typ := range slices.SortedFunc(maps.Keys(byType), byRank) {
		s.rows = append(s.rows, *byType[typ])
	}

	var outs []splicePoint
	for _, seq := range slices.Sorted(maps.Keys(ch.cues)) {
		c := ch.cues[seq]
		if !in(c.at) {
			continue
		}
		for k := range strings.SplitSeq(c.kind, "+") {
			sp := splicePoint{at: c.at, seq: seq}
			switch k {
			case hls.CueOut:
				sp.kind = "CUE-OUT"
				s.outs++
			case hls.CueIn:
				sp.kind = "CUE-IN"
				s.ins++
			default:
				continue
			}
			irregular := false
			for i := range sp.near {
				if g := ch.irregular[seq+uint64(i)-1]; g != nil {
					sp.near[i] = slices.SortedFunc(slices.Values(g.types), byRank)
					irregular = true
				}
			}
			if irregular {
				s.irregularSplices++
			}
			s.splices = append(s.splices, sp)
			if k == hls.CueOut {
				outs = append(outs, sp)
				if c.declared {
					s.planned = append(s.planned, c.planned)
				}
			}
		}
	}
	// The time between breaks, only across stretches the monitor saw
	// whole: a restart or a monitor gap could hide a break in between.
	// Segments after the last health line count as seen, since the monitor
	// writes one only every interval.
	seen := func(from, to uint64) bool {
		if len(monitored) == 0 {
			return true
		}
		return covers(monitored, from, min(to, monitored[len(monitored)-1].hi+1))
	}
	slices.SortStableFunc(outs, func(a, b splicePoint) int { return a.at.Compare(b.at) })
	for i := 1; i < len(outs); i++ {
		a, b := outs[i-1], outs[i]
		if b.seq > a.seq && seen(a.seq+1, b.seq) {
			s.cadence = append(s.cadence, b.at.Sub(a.at).Minutes())
		}
	}
	for _, r := range b.ranges {
		if length, ok := ch.breakLength(r[0], r[1]); ok && in(ch.cues[r[0]].at) {
			s.lengths = append(s.lengths, length)
		}
	}
	return s
}

// addSpan adds a run of sequence numbers, joining it to the last run if
// they overlap or touch, as consecutive health lines' runs do.
func addSpan(runs []span, s span) []span {
	if n := len(runs); n > 0 && s.lo <= runs[n-1].hi+1 && runs[n-1].lo <= s.hi+1 {
		runs[n-1] = span{min(runs[n-1].lo, s.lo), max(runs[n-1].hi, s.hi)}
		return runs
	}
	return append(runs, s)
}

// merge sorts runs and joins those that overlap or touch.
func merge(runs []span) []span {
	slices.SortFunc(runs, func(a, b span) int { return cmp.Compare(a.lo, b.lo) })
	var out []span
	for _, s := range runs {
		out = addSpan(out, s)
	}
	return out
}

// covers reports whether merged runs include every sequence number in
// [from, to).
func covers(runs []span, from, to uint64) bool {
	if from >= to {
		return true
	}
	i, found := slices.BinarySearchFunc(runs, from, func(s span, seq uint64) int { return cmp.Compare(s.lo, seq) })
	if !found {
		i-- // the last run starting before from
	}
	return i >= 0 && to-1 <= runs[i].hi
}

// breakLength adds up the EXTINF of a break's segments, from its CUE-OUT up
// to its CUE-IN, if every one of them carries a tag and so has a row. Fetch
// times would overstate it: a segment is listed only once it is complete,
// and the CUE-OUT segment is often cut short.
func (ch *channel) breakLength(out, in uint64) (float64, bool) {
	var sum float64
	for seq := out; seq < in; seq++ {
		c, ok := ch.cues[seq]
		if !ok || !c.timed {
			return 0, false
		}
		sum += c.extinf
	}
	return sum, true
}

// add sums another channel into a total.
func (s *summary) add(c summary) {
	s.segments += c.segments
	s.gaps += c.gaps
	s.outs += c.outs
	s.ins += c.ins
	s.irregularSplices += c.irregularSplices
	s.faults += c.faults
	s.incidents = append(s.incidents, c.incidents...)
	for _, r := range c.rows {
		i := slices.IndexFunc(s.rows, func(t tally) bool { return t.label == r.label })
		if i < 0 {
			s.rows = append(s.rows, tally{label: r.label})
			i = len(s.rows) - 1
		}
		for p := range places {
			s.rows[i].n[p] += r.n[p]
		}
	}
}

// byRank orders event types as eventTypes does, then alphabetically.
func byRank(a, b string) int {
	rank := func(t string) int {
		if i := slices.Index(eventTypes, t); i >= 0 {
			return i
		}
		return len(eventTypes)
	}
	return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a, b))
}

// --- printing --------------------------------------------------------------------

type printer struct {
	w     *bufio.Writer
	from  time.Time // the window's start
	clock string    // layout for times inside the window
}

func (p *printer) header(o Options, notes []string) {
	to := o.To.UTC().Format(time.DateTime)
	if sameDay(o.From, o.To) {
		to = o.To.UTC().Format(time.TimeOnly)
	}
	fmt.Fprintf(p.w, "stream-analyzer report: %s to %s UTC, data %s\n", o.From.UTC().Format(time.DateTime), to, o.DataDir)
	fmt.Fprintln(p.w, "  at a splice: within one segment of a CUE-OUT or CUE-IN segment")
	fmt.Fprintln(p.w, "  in a break: from CUE-OUT up to CUE-IN otherwise; in programming: everywhere else")
	for _, n := range notes {
		fmt.Fprintf(p.w, "note: %s\n", n)
	}
}

func (p *printer) channel(s summary) {
	fmt.Fprintf(p.w, "\n%s\n", s.name)
	p.overview(s)
	p.line("breaks", s.cadenceText())
	if t := s.lengthText(); t != "" {
		p.line("break length", t)
	}
	p.faults(s)
	for _, in := range s.incidents {
		when := in.opened.UTC().Format(p.clock)
		if in.opened.Before(p.from) {
			when = in.opened.UTC().Format(time.DateTime)
		}
		fmt.Fprintf(p.w, "  %-14s %s  %-6s  %s  %s  %s\n", "", when, in.status, in.types, plural(in.faults, "fault"), in.dir)
	}
	p.table(s.rows)
	if len(s.splices) > 0 {
		fmt.Fprintf(p.w, "\n  splice points (UTC)\n")
		for _, sp := range s.splices {
			fmt.Fprintf(p.w, "  %s  %-7s  seq %d  %s\n", sp.at.UTC().Format(p.clock), sp.kind, sp.seq&(1<<epochShift-1), sp.around())
		}
	}
}

func (p *printer) total(s summary) {
	fmt.Fprintf(p.w, "\n%s\n", s.name)
	p.overview(s)
	p.faults(s)
	p.table(s.rows)
}

func (p *printer) overview(s summary) {
	p.line("monitored", fmt.Sprintf("%d segments, %d monitor gaps", s.segments, s.gaps))
	if s.outs+s.ins == 0 {
		p.line("splice points", "none")
		return
	}
	p.line("splice points", fmt.Sprintf("%d CUE-OUT, %d CUE-IN; %d of %d with an irregular segment",
		s.outs, s.ins, s.irregularSplices, s.outs+s.ins))
}

func (p *printer) line(label, text string) { fmt.Fprintf(p.w, "  %-14s %s\n", label, text) }

// faults prints the faults on the health lines and the number of
// incidents.
func (p *printer) faults(s summary) {
	p.line("faults", noneOr(s.faults))
	p.line("incidents", noneOr(len(s.incidents)))
}

func (p *printer) table(rows []tally) {
	fmt.Fprintf(p.w, "\n  %-15s %8s%16s%16s%16s\n", "", "segments", "at a splice", "in a break", "in programming")
	for _, r := range rows {
		fmt.Fprintf(p.w, "  %-15s %8d", r.label, r.total())
		if t := r.total(); t > 0 {
			for _, n := range r.n {
				fmt.Fprintf(p.w, "  %9d %4s", n, fmt.Sprintf("%.0f%%", math.Round(100*float64(n)/float64(t))))
			}
		}
		fmt.Fprintln(p.w)
	}
}

// cadenceText describes the breaks that began in the window.
func (s summary) cadenceText() string {
	if s.outs == 0 {
		return "none"
	}
	text := strconv.Itoa(s.outs)
	if len(s.cadence) > 0 {
		text += ", every " + spread(s.cadence, "min", 1)
	}
	return text
}

// lengthText gives the breaks' declared lengths and their lengths from
// CUE-OUT to CUE-IN by EXTINF. The latter includes any part of the CUE-OUT
// segment before the splice, so it can run up to a segment longer; a break
// that ended early shows as shorter. It is "" if neither is known.
func (s summary) lengthText() string {
	var parts []string
	if len(s.planned) > 0 {
		parts = append(parts, spread(s.planned, "s", 0)+" declared")
	}
	if len(s.lengths) > 0 {
		parts = append(parts, spread(s.lengths, "s", 0)+" from CUE-OUT to CUE-IN")
	}
	return strings.Join(parts, "; ")
}

// around lists the irregular segments within one segment of the splice.
func (sp splicePoint) around() string {
	var parts []string
	for i, types := range sp.near {
		if len(types) > 0 {
			parts = append(parts, []string{"-1:", " 0:", "+1:"}[i]+" "+strings.Join(types, " "))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "   ")
}

// spread gives values as their median and range, or as the one value they
// all round to: "8.0 min (median, range 7.5–8.5)".
func spread(xs []float64, unit string, prec int) string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', prec, 64) }
	lo, hi := f(slices.Min(xs)), f(slices.Max(xs))
	if lo == hi {
		return lo + " " + unit
	}
	return fmt.Sprintf("%s %s (median, range %s–%s)", f(median(xs)), unit, lo, hi)
}

func median(xs []float64) float64 {
	s := slices.Sorted(slices.Values(xs))
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func noneOr(n int) string {
	if n == 0 {
		return "none"
	}
	return strconv.Itoa(n)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func sameDay(a, b time.Time) bool {
	return a.UTC().Format(time.DateOnly) == b.UTC().Format(time.DateOnly)
}
