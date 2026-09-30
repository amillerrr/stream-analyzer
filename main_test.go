package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/config"
)

func TestPickKeepsConfigOrderAndDropsDuplicates(t *testing.T) {
	all := []config.Channel{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got, err := pick(all, []string{"c", " a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"a", "c"}) {
		t.Errorf("picked %v, want [a c]", names)
	}
	if _, err := pick(all, []string{"a", "nope"}); err == nil {
		t.Error("unknown channel: expected an error")
	}
}

func TestParseReportTime(t *testing.T) {
	at := time.Date(2026, 9, 28, 18, 35, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-09-28T18:35:00Z":      at,
		"2026-09-28T11:35:00-07:00": at,
		"2026-09-28T18:35:46.123Z":  at.Add(46123 * time.Millisecond),
		"2026-09-28T18:35":          at,
		"2026-09-28 18:35":          at,
		"2026-09-28 18:35:00":       at,
		"2026-09-28 18:35Z":         at,
		"2026-09-28":                time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseTime(in)
		if err != nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "18:35", "yesterday", "2026-09-28T25:00"} {
		if _, err := parseTime(in); err == nil {
			t.Errorf("parseTime(%q): expected an error", in)
		}
	}
}

// The report subcommand reads the data directory named by -data, or else
// the config's data_dir, over the -from/-to window.
func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	health := "time_utc,channel,seq,segments,monitor_gaps,faults\n2026-09-28T12:00:30Z,alpha,1004,5,0,0\n"
	if err := os.WriteFile(filepath.Join(dir, "health.csv"), []byte(health), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "channels.yaml")
	yaml := "data_dir: " + dir + "\nchannels:\n  - name: alpha\n    url: https://example.com/a.m3u8\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"-data", dir, "-from", "2026-09-28T12:00:00Z", "-to", "2026-09-28T12:05:00Z"},
		{"-config", cfg, "-from", "2026-09-28 12:00", "-to", "2026-09-28 12:05", "-channel", "alpha"},
	} {
		var out bytes.Buffer
		if err := runReport(args, &out, io.Discard); err != nil {
			t.Errorf("runReport(%q): %v", args, err)
			continue
		}
		if !strings.Contains(out.String(), "\nalpha\n") || !strings.Contains(out.String(), "5 segments") {
			t.Errorf("runReport(%q) output:\n%s", args, out.String())
		}
	}
	for _, args := range [][]string{
		{"-data", dir, "-from", "2026-09-28T12:00:00Z"},                                     // no -to
		{"-data", dir, "-from", "2026-09-28T12:05:00Z", "-to", "2026-09-28T12:00:00Z"},      // backward
		{"-data", dir, "-from", "noon", "-to", "2026-09-28T12:05:00Z"},                      // not a time
		{"-data", dir, "-from", "2026-09-28T12:00:00Z", "-to", "2026-09-28T12:05:00Z", "x"}, // stray argument
	} {
		if err := runReport(args, io.Discard, io.Discard); err == nil {
			t.Errorf("runReport(%q): expected an error", args)
		}
	}
	if err := runReport([]string{"-h"}, io.Discard, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("runReport(-h) = %v, want flag.ErrHelp", err)
	}
	if err := runReport([]string{"-bogus"}, io.Discard, io.Discard); !errors.Is(err, errUsage) {
		t.Errorf("runReport(-bogus) = %v, want a usage error", err)
	}
}

// An argument left after the monitor's flags, such as flags written before
// "report", is an error before anything starts: it must not run a second
// monitor on the same data directory.
func TestRunRejectsLeftoverArguments(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.yaml")
	err := run([]string{"-config", missing, "report", "-from", "2026-09-28"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `"report"`) {
		t.Errorf("run(-config x report ...) = %v, want an error naming the argument \"report\"", err)
	}
	if err := run([]string{"-bogus"}, io.Discard); !errors.Is(err, errUsage) {
		t.Errorf("run(-bogus) = %v, want a usage error", err)
	}
}

// -listen is held to the same rule as the config: loopback only. (The data
// directory can't be made, so a missing check fails instead of starting a
// monitor that never returns.)
func TestRunRejectsNonLoopbackListenFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "channels.yaml")
	os.WriteFile(cfg, []byte("blackdetect:\n  enabled: false\nchannels:\n  - name: a\n    url: http://127.0.0.1:1/x.m3u8\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "file"), nil, 0o644)
	err := run([]string{"-config", cfg, "-data", filepath.Join(dir, "file", "data"), "-listen", "0.0.0.0:0"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("run(-listen 0.0.0.0:8765) = %v, want a listen error", err)
	}
}

// Log lines carry UTC times, like everything else the monitor writes.
func TestLogTimesAreUTC(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("PDT", -7*3600)
	defer func() { time.Local = saved }()
	var b bytes.Buffer
	newLogger(&b, slog.LevelInfo).Info("hello")
	if !regexp.MustCompile(`time=\d{4}-\d\d-\d\dT[0-9:.]+Z `).Match(b.Bytes()) {
		t.Errorf("log line time is not UTC: %s", b.String())
	}
}

// reanalyze writes report.v2.json into each incident directory named, says
// what it found against the original report, and carries on past a
// directory that is not an incident.
func TestReanalyzeCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "channels.yaml")
	os.WriteFile(cfg, []byte("blackdetect:\n  enabled: false\nchannels:\n  - name: alpha\n    url: http://127.0.0.1:1/x.m3u8\n"), 0o644)
	inc := filepath.Join(dir, "20260929T175600Z_alpha")
	os.MkdirAll(inc, 0o755)
	os.WriteFile(filepath.Join(inc, "report.json"), []byte(`{"id":"20260929T175600Z_alpha","channel":"alpha","status":"closed",
		"opened_at":"2026-09-29T10:56:00-07:00","stream":{"url":"http://127.0.0.1:1/x.m3u8"},
		"faults":[{"type":"video_dts_gap","seq":104,"message":"x"}]}`), 0o644)
	notIncident := filepath.Join(dir, "empty")
	os.MkdirAll(notIncident, 0o755)

	var out bytes.Buffer
	err := runReanalyze([]string{"-config", cfg, notIncident, inc}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("runReanalyze = %v, want an error counting 1 of 2 incidents failed", err)
	}
	if _, err := os.Stat(filepath.Join(inc, "report.v2.json")); err != nil {
		t.Errorf("no report.v2.json after the failed directory: %v", err)
	}
	if got := out.String(); !strings.Contains(got, inc) || !strings.Contains(got, "0 faults") || !strings.Contains(got, "video_dts_gap 1") {
		t.Errorf("output %q: want the incident, its new fault count and the original's", got)
	}
	if err := runReanalyze([]string{"-config", cfg}, io.Discard, io.Discard); err == nil {
		t.Error("no incident directory named: expected an error")
	}
	if err := runReanalyze([]string{"-h"}, io.Discard, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("runReanalyze(-h) = %v, want flag.ErrHelp", err)
	}
}
