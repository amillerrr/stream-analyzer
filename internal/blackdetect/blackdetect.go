// Package blackdetect runs ffmpeg's blackdetect filter over a saved segment.
// ffmpeg only reads the file; it is never used to record.
package blackdetect

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
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
	// Range, when "limited" or "full", is the color range blackdetect reads
	// the luma in, whatever the stream signals; "" takes the stream's. An
	// override decodes at 8 bits.
	Range string
	// VideoPID is the only stream ffmpeg decodes: the video PID the TS
	// parser analyzed. ffmpeg fails when the file doesn't carry it. With 0,
	// ffmpeg picks a video stream itself, which may be another one.
	VideoPID uint16
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

// frameKey is the metadata key Chain adds to every frame, so that the
// metadata filter prints a line for each one.
const frameKey = "sa.frame"

// Chain is the filter graph Detect runs: timestamps in 90 kHz ticks,
// blackdetect, then a line for every frame with its PTS and the marks
// blackdetect left on it (lavfi.black_start on the first black frame of a
// run, lavfi.black_end on the first frame after it).
func (o Options) Chain() string {
	return "settb=1/90000," + rangeFilters[o.Range] + o.Filter() + ",metadata=mode=add:key=" + frameKey + ":value=1,metadata=mode=print"
}

// rangeFilters relabel the frames' color range for blackdetect without
// changing their values: scaling with the same range in and out, then
// setting the range. Tried on ffmpeg 5.1, 7.1 and 9.0, with limited and
// full range input; the order differs because a yuvj format is taken as
// full range by scale whatever the frames say.
var rangeFilters = map[string]string{
	"":        "",
	"limited": "scale=in_range=pc:out_range=pc,format=yuv420p,setparams=range=tv,",
	"full":    "setparams=range=pc,scale=in_range=pc:out_range=pc,format=yuvj420p,",
}

// Ranges are the values Options.Range takes besides "".
var Ranges = []string{"limited", "full"}

// Interval is one black run in stream time: the video's PTS in seconds.
// Start is the first black frame's PTS; End is the next frame's, or the
// last frame's when the run lasts to the end of the file.
type Interval struct {
	Start    float64 `json:"start_s"`
	End      float64 `json:"end_s"`
	Duration float64 `json:"duration_s"`
}

// Frame is one frame ffmpeg decoded, in the order it was output
// (presentation order).
type Frame struct {
	// PTS is in 90 kHz ticks as ffmpeg -copyts gives it: libavformat
	// unwraps 33-bit timestamps inside a file, so it can be negative or
	// past 2^33. Reduce it modulo 2^33 for the stream's own PTS.
	PTS int64 `json:"pts"`
	// Black: blackdetect counted the frame black. The marks don't depend
	// on d, so a frame can be black in a run shorter than d.
	Black bool `json:"black,omitzero"`
}

// Result is what one blackdetect run found.
type Result struct {
	// Intervals are the black runs as blackdetect printed them, d and
	// longer.
	Intervals []Interval
	// Frames are every frame ffmpeg decoded.
	Frames []Frame
	// LeadingErrors and DecodeErrors count the errors ffmpeg logged
	// reading or decoding the video, before and after it output its first
	// frame. Before it, they are frames it dropped while it looked for one
	// it could decode. A demuxer's errors don't count. FirstDecodeError is
	// the first line after the first frame.
	LeadingErrors, DecodeErrors int
	FirstDecodeError            string
	// Decoder is the decoder ffmpeg used, as its stream mapping names it:
	// "h264 (native)", "h264 (libopenh264)".
	Decoder string
	// PixFmt and ColorRange are the pixel format and color range ("tv",
	// "pc" or "unknown") blackdetect judged the frames in: an untagged
	// range is read as limited.
	PixFmt, ColorRange string
}

// Detect decodes the file's video with ffmpeg and returns its black runs
// and every frame it decoded. ffmpeg runs with -copyts, so times are the
// stream's own PTS and runs in consecutive files can be compared in one
// time base.
func Detect(ctx context.Context, path string, o Options) (Result, error) {
	return detectWith(ctx, path, o, &logSink{})
}

// waitDelay is how long Detect waits for ffmpeg's stderr to close once
// ffmpeg has exited or been killed: a child it left holding the pipe can't
// hold a worker slot.
const waitDelay = 5 * time.Second

// maxFrames bounds the frames kept from one file: far more than a segment
// holds.
const maxFrames = 1 << 20

func detectWith(ctx context.Context, path string, o Options, log *logSink) (Result, error) {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	// level+info marks each line with its level, so decoder errors can be
	// told from the rest.
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "level+info", "-copyts", "-i", path}
	if o.VideoPID != 0 {
		args = append(args, "-map", fmt.Sprintf("0:i:0x%x", o.VideoPID))
	}
	args = append(args, "-an", "-sn", "-dn", "-vf", o.Chain(), "-f", "null", "-")
	cmd := exec.CommandContext(ctx, cmp.Or(o.FFmpeg, "ffmpeg"), args...)
	cmd.Stderr = log
	cmd.WaitDelay = waitDelay
	// ErrWaitDelay: ffmpeg succeeded and its log was read, but a child
	// kept the pipe open.
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return Result{}, fmt.Errorf("ffmpeg blackdetect: %w: %s", err, strings.Join(log.last, " | "))
	}
	if log.tooMany {
		return Result{}, fmt.Errorf("ffmpeg blackdetect: more than %d frames in one file", maxFrames)
	}
	if log.bad != nil {
		return Result{}, log.bad
	}
	if err := agree(log.runs, log.frames, o.Duration); err != nil {
		return Result{}, err
	}
	res := Result{
		Frames:        log.frames,
		LeadingErrors: log.leadingErrors, DecodeErrors: log.decodeErrors, FirstDecodeError: log.firstError,
		Decoder: log.decoder, PixFmt: log.pixFmt, ColorRange: log.colorRange,
	}
	for _, r := range log.runs {
		res.Intervals = append(res.Intervals, r.Interval)
	}
	return res, nil
}

// FormatError is output from ffmpeg that Detect can't read with certainty:
// a blackdetect line it doesn't know or that doesn't hold together, a
// frame with no timestamp, or black runs the frames' marks don't show. The
// segment's black check is then incomplete; nothing is guessed.
type FormatError struct {
	Line string // the line, or "" when the runs and frames disagree
	Why  string
}

func (e *FormatError) Error() string {
	if e.Line == "" {
		return "ffmpeg blackdetect: " + e.Why
	}
	return fmt.Sprintf("ffmpeg blackdetect: %s: %q", e.Why, e.Line)
}

// printedRun is a black run blackdetect printed, with how far each time may
// be from the true value, given the digits ffmpeg printed.
type printedRun struct {
	Interval
	startTol, endTol float64
}

var blackRun = regexp.MustCompile(`^black_start:\s*(\S+)\s+black_end:\s*(\S+)\s+black_duration:\s*(\S+)$`)

// parseRun reads a line blackdetect printed for a run: all three times,
// finite, the end not before the start, and the duration their difference.
func parseRun(msg string) (printedRun, string) {
	m := blackRun.FindStringSubmatch(strings.TrimSpace(msg))
	if m == nil {
		return printedRun{}, "not a black run line"
	}
	var v [3]float64
	var tol [3]float64
	for i, s := range m[1:] {
		x, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return printedRun{}, fmt.Sprintf("%q is not a time", s)
		}
		v[i], tol[i] = x, printedTolerance(s)
	}
	r := printedRun{Interval: Interval{Start: v[0], End: v[1], Duration: v[2]}, startTol: tol[0], endTol: tol[1]}
	switch {
	case r.End < r.Start-tol[0]-tol[1]:
		return printedRun{}, "the run ends before it starts"
	case math.Abs(r.Duration-(r.End-r.Start)) > tol[0]+tol[1]+tol[2]+2.0/ticksPerSecond:
		return printedRun{}, "the duration is not the time between start and end"
	}
	return r, ""
}

const ticksPerSecond = 90000

// printedTolerance is how far a time ffmpeg printed may be from the value
// it stands for: half a unit in its last digit, and one 90 kHz tick. ffmpeg
// before 7.0 prints 6 significant digits.
func printedTolerance(s string) float64 {
	mant, exp, _ := strings.Cut(strings.ToLower(s), "e")
	decimals := 0
	if _, frac, ok := strings.Cut(mant, "."); ok {
		decimals = len(frac)
	}
	e, _ := strconv.Atoi(exp)
	return 0.5*math.Pow10(e-decimals) + 1.0/ticksPerSecond
}

// agree checks blackdetect's printed runs against the marks on the frames:
// each printed run starts at a run of marked frames and ends at the frame
// after it (or at the last frame, for a run that lasts to the end), and
// every marked run at least d long (by ffmpeg's measure) was printed.
func agree(printed []printedRun, frames []Frame, d float64) error {
	type run struct{ start, end int64 } // ffmpeg's start and end, ticks
	var marked []run
	for i := 0; i < len(frames); i++ {
		if !frames[i].Black {
			continue
		}
		j := i
		for j+1 < len(frames) && frames[j+1].Black {
			j++
		}
		r := run{start: frames[i].PTS, end: frames[j].PTS}
		if j+1 < len(frames) {
			r.end = frames[j+1].PTS
		}
		if float64(r.end-r.start) >= d*ticksPerSecond-1 {
			marked = append(marked, r)
		}
		i = j
	}
	if len(printed) != len(marked) {
		return &FormatError{Why: fmt.Sprintf("blackdetect printed %d black runs, but the frames' marks show %d", len(printed), len(marked))}
	}
	for k, p := range printed {
		m := marked[k]
		if math.Abs(p.Start-float64(m.start)/ticksPerSecond) > p.startTol || math.Abs(p.End-float64(m.end)/ticksPerSecond) > p.endTol {
			return &FormatError{Why: fmt.Sprintf("blackdetect printed a run at %g-%g s, but the frames' marks show one at %g-%g s",
				p.Start, p.End, float64(m.start)/ticksPerSecond, float64(m.end)/ticksPerSecond)}
		}
	}
	return nil
}

// logSink reads ffmpeg's log as it is written and keeps only what Detect
// uses: blackdetect's lines, a record per decoded frame, and the last few
// lines for an error. A broken file can make ffmpeg log an error for
// every frame.
type logSink struct {
	partial []byte // the line being written
	runs    []printedRun
	bad     *FormatError // the first line that couldn't be read
	frames  []Frame
	inBlack bool     // the frames now being printed are black
	tooMany bool     // more than maxFrames
	last    []string // the last lines, for an error message
	// Decode errors before and after the first frame, and the first after.
	leadingErrors, decodeErrors int
	firstError                  string
	decoder                     string // from the stream mapping
	pixFmt, colorRange          string // from the output stream's line
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
	ctx, level, msg := splitLine(l)
	switch {
	case slices.ContainsFunc(ctx, isPrinter):
		if why := s.frameLine(msg); why != "" {
			s.fail(l, why)
		}
	case slices.ContainsFunc(ctx, isBlackdetect) || strings.Contains(l, "black_start"):
		r, why := parseRun(msg)
		if why != "" {
			s.fail(l, why)
			break
		}
		s.runs = append(s.runs, r)
	case s.decoder == "" && mapping.MatchString(msg):
		s.decoder = mapping.FindStringSubmatch(msg)[1]
	case s.pixFmt == "" && output.MatchString(msg):
		m := output.FindStringSubmatch(msg)
		s.pixFmt, s.colorRange = m[1], "unknown"
		for p := range strings.SplitSeq(m[2], ",") {
			if p = strings.TrimSpace(p); p == "tv" || p == "pc" {
				s.colorRange = p
			}
		}
		if s.colorRange == "unknown" && strings.HasPrefix(s.pixFmt, "yuvj") {
			s.colorRange = "pc"
		}
	case decodeError(ctx, level):
		if len(s.frames) == 0 {
			s.leadingErrors++
			break
		}
		if s.decodeErrors++; s.firstError == "" {
			s.firstError = strings.TrimSpace(l)
		}
	}
	if l = strings.TrimSpace(l); l != "" {
		s.last = append(s.last, l)
		if len(s.last) > 3 {
			s.last = s.last[1:]
		}
	}
}

// mapping matches the line of ffmpeg's stream mapping that names the
// decoder: "Stream #0:0 -> #0:0 (h264 (native) -> wrapped_avframe (native))".
var mapping = regexp.MustCompile(`^\s*Stream #\d+:\d+ -> #\d+:\d+ \((\S+ \([^)]+\)) -> `)

// output matches the line ffmpeg prints for the output stream, which has
// the filter graph's pixel format and color range: "Stream #0:0: Video:
// wrapped_avframe, yuv420p(tv, bt709, progressive), 1280x720".
var output = regexp.MustCompile(`^\s*Stream #\d+:\d+: Video: wrapped_avframe, (\w+)(?:\(([^)]*)\))?`)

// fail keeps the first line that couldn't be read.
func (s *logSink) fail(line, why string) {
	if s.bad == nil {
		s.bad = &FormatError{Line: strings.TrimSpace(line), Why: why}
	}
}

// isBlackdetect reports blackdetect's own context.
func isBlackdetect(ctx string) bool {
	name, _, _ := strings.Cut(ctx, " @ ")
	return name == "blackdetect" || strings.HasPrefix(name, "Parsed_blackdetect_")
}

// isPrinter reports the context of the metadata filter that prints the
// frames (Parsed_metadata_<index in the chain>).
func isPrinter(ctx string) bool {
	name, _, _ := strings.Cut(ctx, " @ ")
	return strings.HasPrefix(name, "Parsed_metadata_")
}

var (
	frameHeader = regexp.MustCompile(`^frame:\s*\d+\s+pts:\s*(\S+)\s+pts_time:\s*\S+$`)
	metaEntry   = regexp.MustCompile(`^[^=\s]+=`)
)

// frameLine reads one line the metadata filter printed: a frame's header
// (frame:N pts:P pts_time:T), or one of its metadata entries. It says why
// a line can't be read.
func (s *logSink) frameLine(msg string) string {
	msg = strings.TrimSpace(msg)
	switch {
	case strings.HasPrefix(msg, "frame:"):
		m := frameHeader.FindStringSubmatch(msg)
		if m == nil {
			return "not a frame line"
		}
		pts, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return "a frame with no timestamp"
		}
		if len(s.frames) >= maxFrames {
			s.tooMany = true
			return ""
		}
		s.frames = append(s.frames, Frame{PTS: pts, Black: s.inBlack})
	case strings.HasPrefix(msg, "lavfi.black_start="), strings.HasPrefix(msg, "lavfi.black_end="):
		key, value, _ := strings.Cut(msg, "=")
		if x, err := strconv.ParseFloat(value, 64); err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Sprintf("%s is not a time", key)
		}
		if len(s.frames) == 0 {
			return "a black mark before any frame"
		}
		s.mark(key == "lavfi.black_start")
	case !metaEntry.MatchString(msg):
		return "not a frame or metadata line"
	}
	return ""
}

// mark applies blackdetect's mark to the frame just printed, and to the
// frames after it until the next mark.
func (s *logSink) mark(black bool) {
	s.inBlack = black
	if n := len(s.frames); n > 0 {
		s.frames[n-1].Black = black
	}
}

// notDecoding are the contexts whose errors are not about decoding the
// video: the demuxer, the input and output, and the null muxer.
var notDecoding = []string{"mpegts", "in#", "out#", "null", "AVIOContext", "file"}

// decodeError reports a line at error level or worse from reading or
// decoding the video.
func decodeError(ctx []string, level string) bool {
	if level != "error" && level != "fatal" && level != "panic" {
		return false
	}
	if len(ctx) == 0 {
		return true // ffmpeg's own "Error while decoding stream"
	}
	name, _, _ := strings.Cut(ctx[0], " @ ")
	return !slices.ContainsFunc(notDecoding, func(p string) bool { return strings.HasPrefix(name, p) })
}

// levels are the names ffmpeg's level flag prints.
var levels = []string{"quiet", "panic", "fatal", "error", "warning", "info", "verbose", "debug", "trace"}

// splitLine splits a log line printed with -loglevel level+info into its
// contexts ("h264 @ 0x55d0", outermost first), its level and its message:
// "[h264 @ 0x55d0] [error] no frame!".
func splitLine(l string) (ctx []string, level, msg string) {
	msg = strings.TrimLeft(l, " ")
	for strings.HasPrefix(msg, "[") {
		inner, rest, ok := strings.Cut(msg[1:], "]")
		if !ok {
			break
		}
		msg = strings.TrimPrefix(rest, " ")
		if slices.Contains(levels, inner) {
			level = inner
			break
		}
		ctx = append(ctx, inner)
	}
	return ctx, level, msg
}

// kept is how many bytes of the log are held, frames and runs aside.
func (s *logSink) kept() int {
	n := len(s.partial)
	for _, l := range s.last {
		n += len(l)
	}
	return n
}
