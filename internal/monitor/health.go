package monitor

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// stats are one channel's counters for the current health interval.
type stats struct {
	playlists, playlistErrors, stale     int
	segments, segmentErrors, blackErrors int
	bytes                                int64
	gaps, faults, suppressed             int
	writeErrors                          int               // evidence files that could not be written
	queueDrops                           int               // segments dropped because the queue was full
	resolveErrors                        int               // channel URL fetches that gave no usable playlist
	panics                               int               // panics recovered
	originRecovered                      int               // refusals (4xx/5xx) cured by the retry
	shortBlack                           int               // black runs that ended below trigger_min
	blackNotChecked                      int               // segments whose black check was incomplete or skipped
	leadingDropped                       int               // frames dropped before the first decodable one
	targetChanges                        int               // EXT-X-TARGETDURATION changes (origin notes)
	frameGaps                            int               // skipped frame slots (DTS gaps) inside segments
	audioRetimed                         int               // segments with re-timed audio
	minPTSPCR, minDTSPCR                 *float64          // smallest PTS-PCR and DTS-PCR, ms
	lastNew                              time.Time         // the playlist last listed a new segment
	last                                 *analysis.Summary // latest analyzed segment
}

func (s *stats) record(rec SegmentRecord) {
	if rec.Error != "" {
		s.segmentErrors++
	} else {
		s.segments++
		s.bytes += int64(rec.Fetch.Bytes)
	}
	s.faults += len(rec.Faults)
	if rec.Gap != nil {
		s.gaps += int(rec.Gap.To - rec.Gap.From + 1)
	}
	for _, e := range rec.Events {
		switch e.Type {
		case analysis.EventFrameGap:
			s.frameGaps += e.Slots
		case analysis.EventAudioRetimed:
			s.audioRetimed++
		}
	}
	if rec.Analysis != nil {
		s.last = rec.Analysis
		if v := rec.Analysis.Video; v != nil {
			s.minPTSPCR = lowest(s.minPTSPCR, v.PTSPCRMinMs)
			s.minDTSPCR = lowest(s.minDTSPCR, v.DTSPCRMinMs)
		}
	}
}

func lowest(a, b *float64) *float64 {
	switch {
	case b == nil:
		return a
	case a == nil || *b < *a:
		return new(*b)
	}
	return a
}

func (m *Monitor) healthLoop(ctx context.Context) {
	t := time.NewTicker(m.cfg.HealthInterval)
	defer t.Stop()
	start := time.Now()
	prevWall, prevMono := start.Round(0), time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			wall, mono := now.Round(0), now.Sub(start)
			if jump := clockJump(prevWall, prevMono, wall, mono); jump > clockJumpLimit || jump < -clockJumpLimit {
				m.clockJumped(jump, wall)
			}
			prevWall, prevMono = wall, mono
			for _, c := range m.channels {
				c.safely("health", c.logHealth)
			}
		}
	}
}

// clockJumpLimit is how far wall time and monotonic time may drift apart
// between two health lines before it is reported.
const clockJumpLimit = 5 * time.Second

// clockJump is how much more wall time than monotonic time passed between
// two readings: the machine slept (macOS stops the monotonic clock while it
// sleeps) or the wall clock was stepped. Timers run on monotonic time, so
// neither shows anywhere else.
func clockJump(prevWall time.Time, prevMono time.Duration, wall time.Time, mono time.Duration) time.Duration {
	return wall.Sub(prevWall) - (mono - prevMono)
}

// clockJumped warns that the machine was suspended or its clock stepped,
// and notes it in every open incident: segments after the gap don't follow
// on from those before it.
func (m *Monitor) clockJumped(jump time.Duration, at time.Time) {
	what := "the machine was suspended, or its clock stepped forward"
	if jump < 0 {
		what = "the wall clock was stepped back"
	}
	m.log.Warn(what, "by", jump.Round(time.Second), "noticed_at", at.UTC())
	m.incidents.noteAll(fmt.Sprintf("%s by %.0f s, noticed at %s: nothing was monitored meanwhile",
		what, jump.Seconds(), at.UTC().Format(time.RFC3339)))
}

// healthColumns are data/health.csv's columns, in order.
var healthColumns = []string{
	"time_utc", "channel", "rendition", "seq", "segments", "mb", "playlists", "playlist_errors",
	"stale_playlists", "segment_errors", "origin_errors_recovered", "monitor_gaps", "faults",
	"suppressed", "target_duration_changes", "short_black_runs", "leading_frames_dropped", "frame_gaps", "audio_retimed", "stalled", "last_new_segment_age_s", "av_offset_ms",
	"av_baseline_ms", "min_pts_pcr_ms", "min_dts_pcr_ms",
	"write_errors", "blackdetect_errors", "black_not_checked", "queue_drops", "resolve_errors", "panics", "last_processed_age_s", "free_gb",
	"incident",
}

// logHealth writes the channel's health line to the log and to
// data/health.csv, then starts a new interval. The line is a warning when
// nothing arrived, fetches failed, or anything stopped the monitor
// collecting evidence or checking the stream: failed writes, blackdetect
// failures, segments whose black check was incomplete, dropped queue entries, monitor gaps, an unresolvable channel
// URL, or free space under min_free_gb.
func (c *Channel) logHealth() { c.writeHealth(false) }

// logFinalHealth writes the line for the part of an interval before the
// monitor stopped. That no segment arrived in it is no reason to warn.
func (c *Channel) logFinalHealth() { c.writeHealth(true) }

func (c *Channel) writeHealth(final bool) {
	c.mu.Lock()
	s := c.st
	c.st = stats{lastNew: s.lastNew, last: s.last}
	rendition := "media-playlist"
	if c.variant != nil {
		rendition = c.variant.Label()
	}
	seq, haveSeq, baseline, stalled, lastSeg := c.lastSeq, c.anyRecord, c.baseline, c.stalled, c.lastSegAt
	c.mu.Unlock()
	incident := c.m.incidents.openID(c.name)
	now := c.m.now()

	row := map[string]string{
		"time_utc": now.UTC().Format(time.RFC3339), "channel": c.name, "rendition": rendition,
		"segments": strconv.Itoa(s.segments), "mb": num(math.Round(float64(s.bytes)/1e5) / 10),
		"playlists": strconv.Itoa(s.playlists), "playlist_errors": strconv.Itoa(s.playlistErrors),
		"stale_playlists": strconv.Itoa(s.stale), "segment_errors": strconv.Itoa(s.segmentErrors),
		"origin_errors_recovered": strconv.Itoa(s.originRecovered), "monitor_gaps": strconv.Itoa(s.gaps),
		"faults": strconv.Itoa(s.faults), "suppressed": strconv.Itoa(s.suppressed),
		"target_duration_changes": strconv.Itoa(s.targetChanges),
		"short_black_runs":        strconv.Itoa(s.shortBlack),
		"leading_frames_dropped":  strconv.Itoa(s.leadingDropped),
		"black_not_checked":       strconv.Itoa(s.blackNotChecked),
		"frame_gaps":              strconv.Itoa(s.frameGaps), "audio_retimed": strconv.Itoa(s.audioRetimed),
		"stalled": strconv.FormatBool(stalled), "incident": incident,
		"write_errors": strconv.Itoa(s.writeErrors), "blackdetect_errors": strconv.Itoa(s.blackErrors),
		"queue_drops": strconv.Itoa(s.queueDrops), "resolve_errors": strconv.Itoa(s.resolveErrors),
		"panics": strconv.Itoa(s.panics),
	}
	attrs := []any{"rendition", rendition}
	if haveSeq {
		attrs = append(attrs, "seq", seq)
		row["seq"] = strconv.FormatUint(seq, 10)
	}
	attrs = append(attrs,
		"segments", s.segments, "mb", math.Round(float64(s.bytes)/1e5)/10,
		"playlists", s.playlists, "playlist_errors", s.playlistErrors, "stale_playlists", s.stale,
		"segment_errors", s.segmentErrors, "origin_errors_recovered", s.originRecovered,
		"monitor_gaps", s.gaps, "faults", s.faults, "suppressed", s.suppressed,
		"target_duration_changes", s.targetChanges,
		"short_black_runs", s.shortBlack, "leading_frames_dropped", s.leadingDropped,
		"frame_gaps", s.frameGaps, "audio_retimed", s.audioRetimed)
	if stalled {
		attrs = append(attrs, "stalled", true)
	}
	if !s.lastNew.IsZero() {
		age := time.Since(s.lastNew).Round(100 * time.Millisecond)
		attrs = append(attrs, "last_new_segment_age", age)
		row["last_new_segment_age_s"] = num(age.Seconds())
	}
	if a := s.last; a != nil && a.AVOffsetMs != nil {
		attrs = append(attrs, "av_offset_ms", *a.AVOffsetMs)
		row["av_offset_ms"] = num(*a.AVOffsetMs)
	}
	if baseline != nil {
		b := math.Round(ts.Millis(*baseline)*10) / 10
		attrs = append(attrs, "av_baseline_ms", b)
		row["av_baseline_ms"] = num(b)
	}
	if s.minPTSPCR != nil {
		attrs = append(attrs, "min_pts_pcr_ms", *s.minPTSPCR)
		row["min_pts_pcr_ms"] = num(*s.minPTSPCR)
	}
	if s.minDTSPCR != nil {
		attrs = append(attrs, "min_dts_pcr_ms", *s.minDTSPCR)
		row["min_dts_pcr_ms"] = num(*s.minDTSPCR)
	}
	attrs = append(attrs, "write_errors", s.writeErrors, "blackdetect_errors", s.blackErrors,
		"black_not_checked", s.blackNotChecked, "queue_drops", s.queueDrops, "resolve_errors", s.resolveErrors, "panics", s.panics)
	if !lastSeg.IsZero() {
		age := now.Sub(lastSeg).Round(100 * time.Millisecond)
		attrs = append(attrs, "last_processed_age", age)
		row["last_processed_age_s"] = num(age.Seconds())
	}
	lowDisk := false
	if free, err := c.m.freeSpace(); err == nil {
		gb := math.Round(float64(free)/1e8) / 10
		attrs = append(attrs, "free_gb", gb)
		row["free_gb"] = num(gb)
		lowDisk = c.m.cfg.MinFreeBytes > 0 && free < uint64(c.m.cfg.MinFreeBytes)
	}
	attrs = append(attrs, "incident", incident)
	if final {
		attrs = append(attrs, "final", true)
	}

	level := slog.LevelInfo
	if s.warns(final, stalled, lowDisk) {
		level = slog.LevelWarn
	}
	c.log.Log(context.Background(), level, "health", attrs...)
	if err := c.m.appendCSV("health.csv", healthColumns, row); err != nil {
		c.log.Error("cannot append to health.csv", "error", err)
	}
}

// warns reports whether an interval's health line is a warning: nothing
// arrived, fetches failed, or anything stopped the monitor collecting
// evidence or checking the stream. final is the line written at shutdown.
func (s stats) warns(final, stalled, lowDisk bool) bool {
	return !final && s.segments == 0 || s.segmentErrors > 0 || s.playlistErrors > 0 || stalled ||
		s.writeErrors > 0 || s.blackErrors > 0 || s.blackNotChecked > 0 || s.queueDrops > 0 ||
		s.resolveErrors > 0 || s.gaps > 0 || s.panics > 0 || lowDisk
}

func num(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// appendCSV adds one row to a CSV file in the data directory, writing the
// header first into a new file. A file whose header differs (written by
// another version) is first renamed to <name>.<UTC time>.csv, so no file
// mixes two sets of columns.
func (m *Monitor) appendCSV(name string, header []string, row map[string]string) error {
	m.csvMu.Lock()
	defer m.csvMu.Unlock()
	if err := os.MkdirAll(m.cfg.DataDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(m.cfg.DataDir, name)
	if err := rotateOnHeaderChange(path, header, m.now()); err != nil {
		return err
	}
	record := make([]string, len(header))
	for i, col := range header {
		record[i] = row[col]
	}
	return appendRecord(path, header, record)
}

// appendRecord appends a record to a CSV file, with the header first in a
// new or empty file. A last line a crash cut short is ended first, so the
// new record doesn't join it.
func appendRecord(path string, header, record []string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w := csv.NewWriter(f)
	last := make([]byte, 1)
	switch {
	case info.Size() == 0:
		w.Write(header)
	case func() bool { _, err := f.ReadAt(last, info.Size()-1); return err == nil && last[0] != '\n' }():
		if _, err := f.Write([]byte("\n")); err != nil {
			f.Close()
			return err
		}
	}
	w.Write(record)
	w.Flush()
	return errors.Join(w.Error(), f.Close())
}

func (c *Channel) targetDuration() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.target
}

// rotateOnHeaderChange renames a CSV file whose first record isn't header.
func rotateOnHeaderChange(path string, header []string, now time.Time) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	first, err := csv.NewReader(f).Read()
	f.Close()
	if errors.Is(err, io.EOF) || err == nil && slices.Equal(first, header) {
		return nil
	}
	moved := strings.TrimSuffix(path, ".csv") + "." + now.UTC().Format("20060102T150405Z") + ".csv"
	return os.Rename(path, moved)
}
