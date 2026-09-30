// Package blackdetect runs ffmpeg's blackdetect filter over a saved segment.
// ffmpeg only reads the file; it is never used to record.
package blackdetect

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Options configure a blackdetect run.
type Options struct {
	FFmpeg           string
	Duration         float64 // d: shortest black run reported, seconds
	PixelThreshold   float64 // pix_th
	PictureThreshold float64 // pic_th
	Timeout          time.Duration
}

// Default returns d=0.1, pix_th=0.10, pic_th=0.98 with a 30 s timeout.
func Default() Options {
	return Options{
		FFmpeg:           "ffmpeg",
		Duration:         0.1,
		PixelThreshold:   0.10,
		PictureThreshold: 0.98,
		Timeout:          30 * time.Second,
	}
}

// Filter returns the ffmpeg filter expression for these options.
func (o Options) Filter() string {
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }
	return fmt.Sprintf("blackdetect=d=%s:pix_th=%s:pic_th=%s", f(o.Duration), f(o.PixelThreshold), f(o.PictureThreshold))
}

// Interval is one black run in stream time: the video's PTS in seconds.
// Start is the first black frame's PTS; End is the next frame's, or the
// last frame's when the run lasts to the end of the file.
type Interval struct {
	Start    float64 `json:"start_s"`
	End      float64 `json:"end_s"`
	Duration float64 `json:"duration_s"`
}

// Detect decodes the file's video with ffmpeg and returns its black runs.
// ffmpeg runs with -copyts, so times are the stream's own PTS (in seconds)
// and runs in consecutive files can be compared in one time base.
func Detect(ctx context.Context, path string, o Options) ([]Interval, error) {
	return detectWith(ctx, path, o, &logSink{})
}

// waitDelay is how long Detect waits for ffmpeg's stderr to close once
// ffmpeg has exited or been killed: a child it left holding the pipe can't
// hold a worker slot.
const waitDelay = 5 * time.Second

func detectWith(ctx context.Context, path string, o Options, log *logSink) ([]Interval, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, cmp.Or(o.FFmpeg, "ffmpeg"),
		"-hide_banner", "-nostdin", "-nostats", "-loglevel", "info",
		"-copyts", "-i", path, "-an", "-sn", "-dn", "-vf", o.Filter(), "-f", "null", "-")
	cmd.Stderr = log
	cmd.WaitDelay = waitDelay
	// ErrWaitDelay: ffmpeg succeeded and its log was read, but a child
	// kept the pipe open.
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, fmt.Errorf("ffmpeg blackdetect: %w: %s", err, strings.Join(log.last, " | "))
	}
	return ParseLog(strings.Join(log.black, "\n")), nil
}

// logSink reads ffmpeg's log as it is written and keeps only what Detect
// uses: the black lines, and the last few lines for an error. A broken
// file can make ffmpeg log an error for every frame.
type logSink struct {
	partial []byte   // the line being written
	black   []string // blackdetect's lines
	last    []string // the last lines, for an error message
}

// maxLogLine bounds a line kept from ffmpeg's log.
const maxLogLine = 4 << 10

func (s *logSink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.partial = append(s.partial, p[:min(len(p), maxLogLine-len(s.partial))]...)
			break
		}
		s.partial = append(s.partial, p[:min(i, maxLogLine-len(s.partial))]...)
		s.line(string(s.partial))
		s.partial, p = s.partial[:0], p[i+1:]
	}
	return n, nil
}

func (s *logSink) line(l string) {
	if strings.Contains(l, "black_start") {
		s.black = append(s.black, l)
	}
	if l = strings.TrimSpace(l); l != "" {
		s.last = append(s.last, l)
		if len(s.last) > 3 {
			s.last = s.last[1:]
		}
	}
}

// kept is how many bytes of the log are held.
func (s *logSink) kept() int {
	n := len(s.partial)
	for _, l := range slices.Concat(s.black, s.last) {
		n += len(l)
	}
	return n
}

var blackLine = regexp.MustCompile(`black_start:\s*([-0-9.e+]+)\s+black_end:\s*([-0-9.e+]+)\s+black_duration:\s*([-0-9.e+]+)`)

// ParseLog extracts the black intervals from ffmpeg's log output.
func ParseLog(log string) []Interval {
	var out []Interval
	for _, m := range blackLine.FindAllStringSubmatch(log, -1) {
		start, err1 := strconv.ParseFloat(m[1], 64)
		end, err2 := strconv.ParseFloat(m[2], 64)
		dur, err3 := strconv.ParseFloat(m[3], 64)
		if err1 == nil && err2 == nil && err3 == nil {
			out = append(out, Interval{Start: start, End: end, Duration: dur})
		}
	}
	return out
}
