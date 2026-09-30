package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The monitor's column names (internal/monitor).
var (
	eventHeader = []string{"time_utc", "channel", "seq", "type", "video_frames", "audio_frames", "gap_ms",
		"scte35", "scte35_tag", "detail"}
	cueHeader    = []string{"time_utc", "channel", "seq", "scte35_tag", "extinf", "tags"}
	healthHeader = []string{"time_utc", "channel", "rendition", "seq", "segments", "mb", "playlists",
		"playlist_errors", "stale_playlists", "segment_errors", "origin_errors_recovered", "monitor_gaps",
		"faults", "suppressed", "short_black_runs", "frame_gaps", "audio_retimed", "stalled",
		"last_new_segment_age_s", "av_offset_ms", "av_baseline_ms", "min_pts_pcr_ms", "min_dts_pcr_ms",
		"worst_frame_late_ms", "incident"}
	incidentHeader = []string{"id", "channel", "rendition", "status", "opened_utc", "closed_utc", "duration_s",
		"fault_count", "fault_types", "first_fault", "seq_first", "seq_last", "close_reason", "dir"}
)

var noon = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// stream builds a data directory. Segments are 6 s long and segment 1000 is
// fetched at noon.
type stream struct {
	t                               *testing.T
	dir                             string
	events, cues, health, incidents [][]string
}

func newStream(t *testing.T) *stream { return &stream{t: t, dir: t.TempDir()} }

func at(seq uint64) time.Time {
	return noon.Add(time.Duration(int64(seq)-1000) * 6 * time.Second)
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// event logs irregularities of the given types on a segment.
func (s *stream) event(ch string, seq uint64, types ...string) {
	for _, typ := range types {
		row := map[string]string{"time_utc": stamp(at(seq)), "channel": ch, "seq": fmt.Sprint(seq),
			"type": typ, "video_frames": "180", "audio_frames": "281", "gap_ms": "33.367", "scte35": "false"}
		s.events = append(s.events, pick(eventHeader, row))
	}
}

// cue tags one segment of the given EXTINF; each break is declared 120 s.
func (s *stream) cue(ch string, seq uint64, kind, extinf string) {
	tags := map[string]string{
		"OUT":    "#EXT-X-CUE-OUT:120.000",
		"CONT":   "#EXT-X-CUE-OUT-CONT:ElapsedTime=6.006,Duration=120.000",
		"IN":     "#EXT-X-CUE-IN",
		"IN+OUT": "#EXT-X-CUE-IN | #EXT-X-CUE-OUT:120.000",
	}[kind]
	s.cues = append(s.cues, pick(cueHeader, map[string]string{"time_utc": stamp(at(seq)), "channel": ch,
		"seq": fmt.Sprint(seq), "scte35_tag": kind, "extinf": extinf, "tags": tags}))
}

// adBreak tags a break: CUE-OUT on out, cut short to 2 s as the packager
// does, CUE-OUT-CONT on the 6 s segments inside it if cont, and CUE-IN on
// in.
func (s *stream) adBreak(ch string, out, in uint64, cont bool) {
	s.cue(ch, out, "OUT", "2.002")
	for seq := out + 1; cont && seq < in; seq++ {
		s.cue(ch, seq, "CONT", "6")
	}
	s.cue(ch, in, "IN", "6")
}

// monitored writes health rows covering segments first..last, ten per row,
// each a few seconds after its last segment.
func (s *stream) monitored(ch string, first, last uint64) {
	for c := first; c <= last; c += 10 {
		end := min(c+9, last)
		s.healthRow(ch, end, int(end-c+1), 0, at(end).Add(5*time.Second))
	}
}

func (s *stream) healthRow(ch string, seq uint64, segments, faults int, t time.Time) {
	s.health = append(s.health, pick(healthHeader, map[string]string{"time_utc": t.Format(time.RFC3339),
		"channel": ch, "seq": fmt.Sprint(seq), "segments": fmt.Sprint(segments), "monitor_gaps": "0",
		"faults": fmt.Sprint(faults), "incident": "none"}))
}

func (s *stream) write() string {
	s.t.Helper()
	writeCSV(s.t, filepath.Join(s.dir, "events.csv"), eventHeader, s.events)
	writeCSV(s.t, filepath.Join(s.dir, "scte35.csv"), cueHeader, s.cues)
	writeCSV(s.t, filepath.Join(s.dir, "health.csv"), healthHeader, s.health)
	if len(s.incidents) > 0 {
		writeCSV(s.t, filepath.Join(s.dir, "incidents", "incidents.csv"), incidentHeader, s.incidents)
	}
	return s.dir
}

func pick(header []string, row map[string]string) []string {
	out := make([]string, len(header))
	for i, col := range header {
		out[i] = row[col]
	}
	return out
}

func writeCSV(t *testing.T, path string, header []string, rows [][]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.Write(header)
	w.WriteAll(rows)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runReport(t *testing.T, o Options) string {
	t.Helper()
	var b bytes.Buffer
	if err := Run(&b, o); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return b.String()
}

// section returns a section of the report, each line with its spacing
// collapsed: from the line that is just the name to the next line that
// isn't indented.
func section(t *testing.T, out, name string) []string {
	t.Helper()
	var lines []string
	found := false
	for l := range strings.Lines(out) {
		l = strings.TrimRight(l, "\n")
		switch {
		case l == name:
			found = true
			continue
		case found && l != "" && !strings.HasPrefix(l, " "):
			return lines
		}
		if found && strings.TrimSpace(l) != "" {
			lines = append(lines, strings.Join(strings.Fields(l), " "))
		}
	}
	if !found {
		t.Fatalf("no %q section in:\n%s", name, out)
	}
	return lines
}

// counts returns a table row's numbers without its percentages.
func counts(t *testing.T, lines []string, label string) string {
	t.Helper()
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, label+" "); ok {
			var n []string
			for f := range strings.FieldsSeq(rest) {
				if !strings.HasSuffix(f, "%") {
					n = append(n, f)
				}
			}
			return strings.Join(n, " ")
		}
	}
	t.Fatalf("no %q row in %q", label, lines)
	return ""
}

func wantLines(t *testing.T, lines []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(lines, w) {
			t.Errorf("missing line %q in:\n%s", w, strings.Join(lines, "\n"))
		}
	}
}

// Three breaks between noon and 12:20: CUE-OUT on 1010, 1085 and 1170, each
// CUE-IN 20 segments (120 s) later. At a splice means within one segment of
// a CUE-OUT or CUE-IN segment; in a break means from CUE-OUT to CUE-IN
// otherwise. The irregular segments:
//
//	1010 odd_length audio_retimed  CUE-OUT          at a splice
//	1011 audio_retimed             after CUE-OUT    at a splice
//	1020 frame_gap                 mid-break        in a break
//	1029 odd_length frame_gap      before CUE-IN    at a splice
//	1030 frame_gap audio_retimed   CUE-IN           at a splice
//	1050 audio_retimed                              in programming
//	1060 frame_gap audio_gap                        in programming
//	1084 odd_length                before CUE-OUT   at a splice
//	1105 frame_gap                 CUE-IN           at a splice
//
// 990 and 1205 are outside the window. The breaks at 1170-1190 have none.
func TestReportPlacesIrregularSegmentsAroundSplices(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1199)
	s.adBreak("alpha", 1010, 1030, true)
	s.adBreak("alpha", 1085, 1105, true)
	s.adBreak("alpha", 1170, 1190, true)
	s.event("alpha", 990, "audio_retimed")
	s.event("alpha", 1010, "odd_length", "audio_retimed")
	s.event("alpha", 1011, "audio_retimed")
	s.event("alpha", 1020, "frame_gap")
	s.event("alpha", 1029, "odd_length", "frame_gap")
	s.event("alpha", 1030, "frame_gap", "audio_retimed")
	s.event("alpha", 1050, "audio_retimed")
	s.event("alpha", 1060, "frame_gap", "audio_gap")
	s.event("alpha", 1084, "odd_length")
	s.event("alpha", 1105, "frame_gap")
	s.event("alpha", 1205, "frame_gap")
	dir := s.write()

	out := runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(20 * time.Minute)})
	alpha := section(t, out, "alpha")
	wantLines(t, alpha,
		"monitored 200 segments, 0 monitor gaps",
		"splice points 3 CUE-OUT, 3 CUE-IN; 4 of 6 with an irregular segment",
		"breaks 3, every 8.0 min (median, range 7.5–8.5)",
		"break length 120 s declared; 116 s from CUE-OUT to CUE-IN",
		"faults none",
		"incidents none",
		"segments at a splice in a break in programming",
		"12:01:00 CUE-OUT seq 1010 0: odd_length audio_retimed +1: audio_retimed",
		"12:03:00 CUE-IN seq 1030 -1: frame_gap odd_length 0: frame_gap audio_retimed",
		"12:08:30 CUE-OUT seq 1085 -1: odd_length",
		"12:10:30 CUE-IN seq 1105 0: frame_gap",
		"12:17:00 CUE-OUT seq 1170 none",
		"12:19:00 CUE-IN seq 1190 none",
	)
	for label, want := range map[string]string{
		// 18 segments within one of a splice, 3 x 17 inside breaks otherwise.
		"all monitored": "200 18 51 131",
		"any irregular": "9 6 1 2",
		"frame_gap":     "5 3 1 1",
		"odd_length":    "3 3 0 0",
		"audio_retimed": "4 3 0 1",
		"audio_gap":     "1 0 0 1",
	} {
		if got := counts(t, alpha, label); got != want {
			t.Errorf("%s = %s, want %s (segments, at a splice, in a break, in programming)", label, got, want)
		}
	}
	if !strings.Contains(out, "2026-09-28 12:00:00 to 12:20:00 UTC") {
		t.Errorf("the report doesn't state its window:\n%s", out)
	}
}

// Breaks that cross the window's edges, in a stream that doesn't repeat
// CUE-OUT-CONT: the segments inside the window are still placed in the
// break, whether its CUE-OUT (990) or its CUE-IN (1060) is outside.
func TestReportPlacesSegmentsInBreaksAcrossTheWindowEdges(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.adBreak("alpha", 990, 1010, false)
	s.adBreak("alpha", 1040, 1060, false)
	s.event("alpha", 1003, "frame_gap")
	s.event("alpha", 1030, "frame_gap")
	s.event("alpha", 1041, "odd_length")
	s.event("alpha", 1045, "frame_gap")
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)}), "alpha")
	wantLines(t, alpha, "splice points 1 CUE-OUT, 1 CUE-IN; 1 of 2 with an irregular segment")
	for label, want := range map[string]string{
		"frame_gap":  "3 0 2 1",
		"odd_length": "1 1 0 0",
		// 1009-1011 and 1039-1041 at a splice; 1000-1008 and 1042-1049 in a break.
		"all monitored": "50 6 17 27",
	} {
		if got := counts(t, alpha, label); got != want {
			t.Errorf("%s = %s, want %s", label, got, want)
		}
	}
}

// One break ends and the next begins on the same segment (IN+OUT): it is a
// splice point twice, and each break has its own range and length.
func TestReportBackToBackBreaks(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1069)
	s.cue("alpha", 1010, "OUT", "2.002")
	for seq := uint64(1011); seq < 1050; seq++ {
		if seq == 1030 {
			s.cue("alpha", seq, "IN+OUT", "2.002")
			continue
		}
		s.cue("alpha", seq, "CONT", "6")
	}
	s.cue("alpha", 1050, "IN", "6")
	s.event("alpha", 1030, "odd_length")
	s.event("alpha", 1040, "frame_gap")
	s.event("alpha", 1060, "frame_gap")
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(7 * time.Minute)}), "alpha")
	wantLines(t, alpha,
		"splice points 2 CUE-OUT, 2 CUE-IN; 2 of 4 with an irregular segment",
		"breaks 2, every 2.0 min",
		"break length 120 s declared; 116 s from CUE-OUT to CUE-IN",
		"12:03:00 CUE-IN seq 1030 0: odd_length",
		"12:03:00 CUE-OUT seq 1030 0: odd_length",
	)
	if got := counts(t, alpha, "frame_gap"); got != "2 0 1 1" {
		t.Errorf("frame_gap = %s, want 2 0 1 1", got)
	}
}

// The time between breaks is only measured across stretches the monitor
// saw: with 1100-1199 unmonitored (a restart), the 16 minutes from the
// CUE-OUT at 1070 to the one at 1230 would hide the break in between.
func TestReportCadenceSkipsStretchesTheMonitorMissed(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1099)
	s.monitored("alpha", 1200, 1299)
	s.adBreak("alpha", 1010, 1030, true)
	s.adBreak("alpha", 1070, 1090, true)
	s.adBreak("alpha", 1230, 1250, true)
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(30 * time.Minute)}), "alpha")
	wantLines(t, alpha, "breaks 3, every 6.0 min")
}

// A segment the origin refused was still processed (its tags logged), so
// it is no hole in what the monitor saw.
func TestReportCadenceCountsRefusedSegmentsAsSeen(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1099)
	s.adBreak("alpha", 1010, 1030, true)
	s.adBreak("alpha", 1070, 1090, true)
	// The line for 1040-1049 counts nine segments and one refused.
	for _, r := range s.health {
		if r[slices.Index(healthHeader, "seq")] == "1049" {
			r[slices.Index(healthHeader, "segments")] = "9"
			r[slices.Index(healthHeader, "segment_errors")] = "1"
		}
	}
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(15 * time.Minute)}), "alpha")
	wantLines(t, alpha, "breaks 2, every 6.0 min")
}

// Segments after the last health line count as seen: the monitor writes a
// line a minute, so it may have stopped (or still be running) with the
// last CUE-OUT (1170) past the last line (1164).
func TestReportCadenceCountsTheStretchAfterTheLastHealthLine(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1164)
	s.adBreak("alpha", 1010, 1030, true)
	s.adBreak("alpha", 1085, 1105, true)
	s.adBreak("alpha", 1170, 1190, true)
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(30 * time.Minute)}), "alpha")
	wantLines(t, alpha, "breaks 3, every 8.0 min (median, range 7.5–8.5)")
}

// -channel reports one channel; without it every channel with data gets a
// section, then a total. A channel with no data is an error naming the ones
// that have some.
func TestReportChannels(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.monitored("beta", 1000, 1029)
	s.adBreak("beta", 1010, 1020, true)
	s.event("alpha", 1040, "frame_gap")
	s.event("beta", 1010, "odd_length")
	dir := s.write()
	window := Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)}

	all := runReport(t, window)
	wantLines(t, section(t, all, "alpha"), "splice points none", "breaks none")
	total := section(t, all, "all channels")
	if got := counts(t, total, "all monitored"); got != "80 6 7 67" {
		t.Errorf("all channels: all monitored = %s, want 80 6 7 67", got)
	}
	if got := counts(t, total, "any irregular"); got != "2 1 0 1" {
		t.Errorf("all channels: any irregular = %s, want 2 1 0 1", got)
	}

	one := window
	one.Channel = "beta"
	out := runReport(t, one)
	section(t, out, "beta")
	if strings.Contains(out, "alpha") || strings.Contains(out, "all channels") {
		t.Errorf("-channel beta reported more than beta:\n%s", out)
	}

	one.Channel = "gamma"
	err := Run(new(bytes.Buffer), one)
	if err == nil || !strings.Contains(err.Error(), `"gamma"`) || !strings.Contains(err.Error(), "alpha, beta") {
		t.Errorf("Run(-channel gamma) = %v, want an error naming gamma and the channels with data", err)
	}
	one.Channel, one.From, one.To = "", noon.Add(time.Hour), noon.Add(2*time.Hour)
	if err := Run(new(bytes.Buffer), one); err == nil {
		t.Error("Run over a window with no data succeeded, want an error")
	}
}

// Faults are counted from health.csv. Incidents are listed if they overlap
// the window, from incidents.csv or, for one not indexed yet (such as one
// still open), from its report.json. One that opened before the window
// shows its date.
func TestReportListsFaultsAndIncidents(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.healthRow("alpha", 1030, 0, 3, noon.Add(3*time.Minute+30*time.Second))
	closed := func(id, opened, closed, count, types string) []string {
		return pick(incidentHeader, map[string]string{"id": id, "channel": "alpha", "status": "closed",
			"opened_utc": opened, "closed_utc": closed, "fault_count": count, "fault_types": types,
			"dir": "incidents/" + id})
	}
	s.incidents = append(s.incidents,
		closed("20260928T110000Z_alpha", "2026-09-28T11:00:00Z", "2026-09-28T11:05:00Z", "1", "stall"),
		closed("20260928T115800Z_alpha", "2026-09-28T11:58:00Z", "2026-09-28T12:02:00Z", "4", "pts_pcr_jump"),
		closed("20260928T120130Z_alpha", "2026-09-28T12:01:30Z", "2026-09-28T12:03:00Z", "2", "audio_pts_gap;video_dts_gap"),
		closed("20260928T120600Z_alpha", "2026-09-28T12:06:00Z", "2026-09-28T12:07:00Z", "1", "stall"))
	dir := s.write()
	open := filepath.Join(dir, "incidents", "20260928T120400Z_alpha")
	if err := os.MkdirAll(open, 0o755); err != nil {
		t.Fatal(err)
	}
	report := `{"id": "20260928T120400Z_alpha", "channel": "alpha", "status": "open",
		"opened_at": "2026-09-28T12:04:00Z", "fault_types": ["stall"], "fault_count": 1, "segments": []}`
	if err := os.WriteFile(filepath.Join(open, "report.json"), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	// Left open by a crash three days earlier, with no restart since to mark
	// it interrupted: it is no incident now.
	stale := filepath.Join(dir, "incidents", "20260925T120000Z_beta")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	report = `{"id": "20260925T120000Z_beta", "channel": "beta", "status": "open",
		"opened_at": "2026-09-25T12:00:00Z", "fault_types": ["stall"], "fault_count": 1, "segments": []}`
	if err := os.WriteFile(filepath.Join(stale, "report.json"), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
	crashed := time.Date(2026, 9, 25, 12, 1, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(stale, "report.json"), crashed, crashed); err != nil {
		t.Fatal(err)
	}

	out := runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)})
	if strings.Contains(out, "beta") {
		t.Errorf("listed an incident left open days before the window:\n%s", out)
	}
	alpha := section(t, out, "alpha")
	wantLines(t, alpha,
		"faults 3",
		"incidents 3",
		"2026-09-28 11:58:00 closed pts_pcr_jump 4 faults incidents/20260928T115800Z_alpha",
		"12:01:30 closed audio_pts_gap;video_dts_gap 2 faults incidents/20260928T120130Z_alpha",
		"12:04:00 open stall 1 fault incidents/20260928T120400Z_alpha",
	)
	for _, l := range alpha {
		if strings.Contains(l, "20260928T110000Z_alpha") || strings.Contains(l, "20260928T120600Z_alpha") {
			t.Errorf("listed an incident outside the window: %q", l)
		}
	}
}

// The report says what it couldn't use: a missing scte35.csv, one that
// starts after the window does, and lines it couldn't read. A row torn by
// a crash, with the next row appended to the same line, costs that line
// only; an absurd segment count is unreadable, not a hang.
func TestReportNotes(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.healthRow("alpha", 1049, 1000000000, 0, noon.Add(4*time.Minute))
	s.event("alpha", 1010, "frame_gap")
	dir := s.write()
	if err := os.Remove(filepath.Join(dir, "scte35.csv")); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(dir, "events.csv"))
	if err != nil {
		t.Fatal(err)
	}
	torn := stamp(at(1020)) + `,alpha,1020,frame_gap,180,281,33.367,false,,"1 frame gap(s), 33.3`
	events = fmt.Appendf(events, "%s%s,alpha,1021,frame_gap,180,281,33.367,false,,x\n%s,alpha,1030,odd_length,179,281,-33.367,false,,\"1 frame, short\"\n",
		torn, stamp(at(1021)), stamp(at(1030)))
	if err := os.WriteFile(filepath.Join(dir, "events.csv"), events, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)})
	for _, want := range []string{
		"note: no scte35.csv",
		"note: skipped 1 unreadable line in events.csv",
		"note: skipped 1 unreadable line in health.csv",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	alpha := section(t, out, "alpha")
	for label, want := range map[string]string{"frame_gap": "1 0 0 1", "odd_length": "1 0 0 1"} {
		if got := counts(t, alpha, label); got != want {
			t.Errorf("%s = %s, want %s: 1010 and 1030 read, the torn line not", label, got, want)
		}
	}

	// A first break some minutes into a run is no reason for a note.
	s.cue("alpha", 1040, "OUT", "2.002")
	s.write()
	out = runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)})
	if strings.Contains(out, "note: no scte35.csv") || strings.Contains(out, "before stream-analyzer logged") {
		t.Errorf("noted missing cues in a run that logged them:\n%s", out)
	}

	// events.csv rows without scte35_tag in the window come from a version
	// that didn't log cues: breaks then are unknown.
	round4 := []string{"time_utc", "channel", "seq", "type", "video_frames", "audio_frames", "gap_ms", "scte35", "detail"}
	writeCSV(t, filepath.Join(dir, "events.20260928T120200Z.csv"), round4, [][]string{
		{stamp(at(1005)), "alpha", "1005", "audio_retimed", "180", "281", "-0.889", "false", "12 steps off"},
	})
	out = runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)})
	if !strings.Contains(out, "note: events.csv has rows in the window from before stream-analyzer logged SCTE-35 cues") {
		t.Errorf("no note about rows from before cues were logged:\n%s", out)
	}
}

// A line longer than any the monitor writes costs only itself.
func TestReportSkipsOverlongLines(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.event("alpha", 1010, "frame_gap")
	dir := s.write()
	events, err := os.ReadFile(filepath.Join(dir, "events.csv"))
	if err != nil {
		t.Fatal(err)
	}
	events = fmt.Appendf(events, "%s\n%s,alpha,1030,odd_length,179,281,-33.367,false,,short\n",
		strings.Repeat("x", 2<<20), stamp(at(1030)))
	if err := os.WriteFile(filepath.Join(dir, "events.csv"), events, 0o644); err != nil {
		t.Fatal(err)
	}

	out := runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)})
	if !strings.Contains(out, "note: skipped 1 unreadable line in events.csv") {
		t.Errorf("no note about the long line:\n%s", out)
	}
	if got := counts(t, section(t, out, "alpha"), "any irregular"); got != "2 0 0 2" {
		t.Errorf("any irregular = %s, want 2 0 0 2: 1010 and 1030 read", got)
	}
}

// A file the monitor moved aside when its columns changed
// (events.<UTC time>.csv) is still read, by column name.
func TestReportReadsRotatedFilesWithOlderColumns(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.event("alpha", 1040, "frame_gap")
	dir := s.write()
	round4 := []string{"time_utc", "channel", "seq", "type", "video_frames", "audio_frames", "gap_ms", "scte35", "detail"}
	writeCSV(t, filepath.Join(dir, "events.20260928T120200Z.csv"), round4, [][]string{
		{stamp(at(1005)), "alpha", "1005", "audio_retimed", "180", "281", "-0.889", "false", "12 steps off"},
	})

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)}), "alpha")
	if got := counts(t, alpha, "any irregular"); got != "2 0 0 2" {
		t.Errorf("any irregular = %s, want 2 0 0 2", got)
	}
}

// A break's length is shown two ways: as declared on its CUE-OUT tag, and
// as the sum of EXTINF from CUE-OUT up to CUE-IN, which fetch times would
// overstate (each segment is listed once it is complete, and the CUE-OUT
// segment is often cut short). A break with an untagged segment inside it
// has no EXTINF length.
func TestReportBreakLengthsAddUpEXTINF(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1099)
	s.adBreak("alpha", 1010, 1030, true)  // 2.002 + 19 x 6 s
	s.adBreak("alpha", 1060, 1080, false) // no CUE-OUT-CONT
	dir := s.write()

	alpha := section(t, runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(10 * time.Minute)}), "alpha")
	wantLines(t, alpha, "breaks 2, every 5.0 min", "break length 120 s declared; 116 s from CUE-OUT to CUE-IN")
}

// splitCSV reads a line as encoding/csv writes it, and rejects what
// encoding/csv would: an unterminated quote, text after a closing quote, a
// quote inside an unquoted field.
func TestSplitCSVMatchesEncodingCSV(t *testing.T) {
	for _, rec := range [][]string{
		{"a", "b", "c"},
		{"", "", ""},
		{"1 frame gap(s), 33.367 ms missing", "x"},
		{`say "hi"`, `""`, `"`},
		{" leading space", "trailing space ", "tab\there"},
		{"#EXT-X-CUE-OUT-CONT:ElapsedTime=6.006,Duration=120.000 | #EXT-OATCLS-SCTE35:/DAl+/="},
	} {
		var b bytes.Buffer
		w := csv.NewWriter(&b)
		w.Write(rec)
		w.Flush()
		line := strings.TrimSuffix(b.String(), "\n")
		got, err := splitCSV(line)
		if err != nil || !slices.Equal(got, rec) {
			t.Errorf("splitCSV(%q) = %q, %v; want %q", line, got, err, rec)
		}
	}
	for _, line := range []string{`"unterminated`, `"a"b,c`, `a"b,c`, `a,"b"x`, `"a""`} {
		if got, err := splitCSV(line); err == nil {
			t.Errorf("splitCSV(%q) = %q, want an error", line, got)
		}
	}
}

// A file that went through a tool writing CRLF line ends reads the same.
func TestReportReadsCRLFFiles(t *testing.T) {
	s := newStream(t)
	s.monitored("alpha", 1000, 1049)
	s.adBreak("alpha", 1010, 1030, true)
	s.event("alpha", 1010, "odd_length")
	s.event("alpha", 1040, "frame_gap")
	dir := s.write()
	for _, name := range []string{"events.csv", "scte35.csv", "health.csv"} {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n")), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := runReport(t, Options{DataDir: dir, From: noon, To: noon.Add(5 * time.Minute)})
	if strings.Contains(out, "unreadable") {
		t.Errorf("CRLF lines read as unreadable:\n%s", out)
	}
	alpha := section(t, out, "alpha")
	wantLines(t, alpha, "break length 120 s declared; 116 s from CUE-OUT to CUE-IN")
	if got := counts(t, alpha, "any irregular"); got != "2 1 0 1" {
		t.Errorf("any irregular = %s, want 2 1 0 1", got)
	}
}
