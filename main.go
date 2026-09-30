// Command stream-analyzer watches live HLS channels and saves evidence (raw
// segments, playlists, response headers and a report) when it sees a timing
// fault or black video. "stream-analyzer report" summarizes a run from the CSV
// files it wrote; "stream-analyzer reanalyze" runs today's checks again on
// incidents already saved.
package main

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	rtdebug "runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/monitor"
	"github.com/amillerrr/stream-analyzer/internal/report"
)

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "report":
		err = runReport(os.Args[2:], os.Stdout, os.Stderr)
	case len(os.Args) > 1 && os.Args[1] == "reanalyze":
		err = runReanalyze(os.Args[2:], os.Stdout, os.Stderr)
	default:
		err = run(os.Args[1:], os.Stderr)
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "stream-analyzer:", err)
		os.Exit(1)
	}
}

// errUsage marks a flag error that the flag package has already printed,
// with the usage.
var errUsage = errors.New("usage")

// parse parses a command's flags. It returns flag.ErrHelp for -h, and a
// usage error for anything else the flag package rejects.
func parse(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return fmt.Errorf("%w: %w", errUsage, err)
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("stream-analyzer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "channels.yaml", "YAML config file")
	dataDir := fs.String("data", "", "data directory (overrides data_dir)")
	listen := fs.String("listen", "", "manual trigger address (overrides listen)")
	only := fs.String("channels", "", "comma-separated channel names to monitor (default: all)")
	debug := fs.Bool("debug", false, "log at debug level")
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintln(w, "usage: stream-analyzer [flags]         monitor the channels")
		fmt.Fprintln(w, "       stream-analyzer report -from TIME -to TIME [-channel NAME]")
		fmt.Fprintln(w, "                                       summarize a run (stream-analyzer report -h)")
		fmt.Fprintln(w, "       stream-analyzer reanalyze INCIDENT_DIR...")
		fmt.Fprintln(w, "                                       check saved incidents again (stream-analyzer reanalyze -h)")
		fmt.Fprintln(w)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	// Flag parsing stops at the first argument that isn't a flag, so flags
	// written before a subcommand ("-config x report ...") would otherwise
	// start a second monitor on the same data directory.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: a subcommand comes first, as in stream-analyzer report -config channels.yaml -from TIME -to TIME", fs.Arg(0))
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *listen != "" {
		cfg.Listen = *listen
		if err := config.CheckListen(cfg.Listen); err != nil {
			return fmt.Errorf("-%w", err)
		}
	}
	if *only != "" {
		if cfg.Channels, err = pick(cfg.Channels, strings.Split(*only, ",")); err != nil {
			return err
		}
	}
	if cfg.Blackdetect.Enabled {
		if _, err := exec.LookPath(cfg.Blackdetect.FFmpeg); err != nil {
			return fmt.Errorf("blackdetect needs ffmpeg: %w (install it, set blackdetect.ffmpeg, or set blackdetect.enabled: false)", err)
		}
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	// A write to a closed stderr (a pipe whose reader went away, a closed
	// terminal) must fail, not kill the monitor with incidents open.
	signal.Ignore(syscall.SIGPIPE)
	out := &logWriter{stderr: stderr}
	if cfg.LogFile != "" {
		path := cfg.LogFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(cfg.DataDir, path)
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		out.file = f
		// A crash the monitor can't recover from still leaves its stack in
		// the log file.
		if err := rtdebug.SetCrashOutput(f, rtdebug.CrashOptions{}); err != nil {
			fmt.Fprintln(stderr, "stream-analyzer: crash output stays on stderr:", err)
		}
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := newLogger(out, level)

	m, err := monitor.New(monitor.Options{Config: cfg, Logger: logger})
	if err != nil {
		return err
	}
	// SIGHUP too: closing the terminal window should close incidents
	// cleanly, not leave them to be marked interrupted on the next start.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return m.Run(ctx)
}

// newLogger logs text lines to w with UTC times, like every file the
// monitor writes.
func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 && a.Value.Kind() == slog.KindTime {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	}))
}

// logWriter writes each log line to the log file first, then to stderr.
// Once stderr fails (a closed pipe or terminal) it is left alone, and the
// file still gets every line.
type logWriter struct {
	file       io.Writer // nil without a log file
	stderr     io.Writer
	stderrGone atomic.Bool
}

func (w *logWriter) Write(p []byte) (int, error) {
	var err error
	if w.file != nil {
		_, err = w.file.Write(p)
	}
	if !w.stderrGone.Load() {
		if _, serr := w.stderr.Write(p); serr != nil {
			w.stderrGone.Store(true)
			if w.file == nil {
				err = serr
			}
		}
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// pick keeps the named channels, once each, in config order.
func pick(all []config.Channel, names []string) ([]config.Channel, error) {
	want := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if !slices.ContainsFunc(all, func(c config.Channel) bool { return c.Name == n }) {
			return nil, fmt.Errorf("-channels: no channel named %q in the config", n)
		}
		want[n] = true
	}
	var out []config.Channel
	for _, c := range all {
		if want[c.Name] {
			out = append(out, c)
		}
	}
	return out, nil
}

// runReport is the report subcommand: a summary, per channel, of the data
// directory's CSV files over a time window.
func runReport(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "channels.yaml", "YAML config file, for data_dir")
	dataDir := fs.String("data", "", "data directory (overrides data_dir)")
	from := fs.String("from", "", "start of the window (required)")
	to := fs.String("to", "", "end of the window, exclusive (required)")
	channel := fs.String("channel", "", "report only this channel")
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintln(w, "usage: stream-analyzer report -from TIME -to TIME [-channel NAME] [-data DIR]")
		fmt.Fprintln(w, "\nPer channel: splice points, irregular segments at a splice versus in a break")
		fmt.Fprintln(w, "or in programming, the ad-break cadence and faults, from the CSV files in the")
		fmt.Fprintln(w, "data directory. TIME is UTC unless it has an offset: 2026-09-28T18:35:00Z,")
		fmt.Fprintln(w, "2026-09-28T11:35:00-07:00, \"2026-09-28 18:35\" or 2026-09-28.")
		fmt.Fprintln(w)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("report: unexpected argument %q", fs.Arg(0))
	}
	if *from == "" || *to == "" {
		return errors.New("report: -from and -to are required (see stream-analyzer report -h)")
	}
	start, err := parseTime(*from)
	if err != nil {
		return fmt.Errorf("report: -from: %w", err)
	}
	end, err := parseTime(*to)
	if err != nil {
		return fmt.Errorf("report: -to: %w", err)
	}
	if !end.After(start) {
		return fmt.Errorf("report: -to %s is not after -from %s", *to, *from)
	}
	dir := *dataDir
	if dir == "" {
		cfg, err := config.Load(*configPath)
		if err != nil {
			return fmt.Errorf("report: %w (or name the data directory with -data)", err)
		}
		dir = cfg.DataDir
	}
	if err := report.Run(stdout, report.Options{DataDir: dir, From: start, To: end, Channel: *channel}); err != nil {
		return fmt.Errorf("report: %w", err)
	}
	return nil
}

// runReanalyze is the reanalyze subcommand: today's checks, run again on
// incidents already saved, each written to report.v2.json next to its
// report.json.
func runReanalyze(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("reanalyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "channels.yaml", "YAML config file, for the checks' thresholds and blackdetect")
	fs.Usage = func() {
		w := fs.Output()
		fmt.Fprintln(w, "usage: stream-analyzer reanalyze [-config FILE] INCIDENT_DIR...")
		fmt.Fprintln(w, "\nRuns the current checks again on each incident's saved playlists and")
		fmt.Fprintln(w, "segments, and writes the result to report.v2.json in the incident directory.")
		fmt.Fprintln(w, "Nothing else in the directory changes.")
		fmt.Fprintln(w)
		fs.PrintDefaults()
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("reanalyze: name at least one incident directory (see stream-analyzer reanalyze -h)")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("reanalyze: %w", err)
	}
	if cfg.Blackdetect.Enabled {
		if _, err := exec.LookPath(cfg.Blackdetect.FFmpeg); err != nil {
			return fmt.Errorf("reanalyze: blackdetect needs ffmpeg: %w (install it, set blackdetect.ffmpeg, or set blackdetect.enabled: false)", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := newLogger(stderr, slog.LevelWarn)
	failed := 0
	for _, dir := range fs.Args() {
		r, err := monitor.Reanalyze(ctx, dir, monitor.Options{Config: cfg, Logger: logger})
		if err != nil {
			fmt.Fprintf(stderr, "stream-analyzer reanalyze: %s: %v\n", dir, err)
			failed++
			if ctx.Err() != nil {
				break
			}
			continue
		}
		was := "?"
		if b, err := os.ReadFile(filepath.Join(dir, "report.json")); err == nil {
			var old monitor.Report
			if json.Unmarshal(b, &old) == nil {
				was = monitor.FaultSummary(old.Faults)
			}
		}
		fmt.Fprintf(stdout, "%s: %s; report.json had %s\n", filepath.Join(dir, "report.v2.json"), monitor.FaultSummary(r.Faults), was)
	}
	if failed > 0 {
		return fmt.Errorf("reanalyze: %d of %d incidents failed", failed, fs.NArg())
	}
	return nil
}

// timeLayouts are the forms -from and -to accept. A time without an offset
// is UTC, like the CSV files.
var timeLayouts = []string{
	time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04:05", "2006-01-02T15:04",
	"2006-01-02 15:04:05Z07:00", "2006-01-02 15:04Z07:00", "2006-01-02 15:04:05", "2006-01-02 15:04",
	time.DateOnly,
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read %q as a time; use a form like 2026-09-28T18:35:00Z, \"2026-09-28 18:35\" or 2026-09-28", s)
}
