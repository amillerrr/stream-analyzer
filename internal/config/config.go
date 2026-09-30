// Package config loads stream-analyzer's YAML configuration. Unknown keys are
// errors, so a typo can't silently leave a default in place.
package config

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/yamlite"
)

// Config is the whole configuration.
type Config struct {
	DataDir              string
	Listen               string // manual-trigger HTTP listener
	LogFile              string // relative to DataDir; "" logs to stderr only
	UserAgent            string
	TLSMaxVersion        string // "1.2" or "1.3"
	Rendition            string // default variant selection for master playlists
	Buffer               time.Duration
	PostRoll             time.Duration
	MergeWindow          time.Duration
	MaxIncident          time.Duration
	IncidentStorageBytes int64
	// MinFreeBytes is the free space the data directory's filesystem
	// should keep: below it the health line is a warning, and opening an
	// incident logs an error. Zero turns the check off.
	MinFreeBytes   int64
	HealthInterval time.Duration
	// StallTargetDurations is how many target durations may pass without a
	// new segment before the channel counts as stalled.
	StallTargetDurations float64
	Checks               analysis.Thresholds
	Blackdetect          Blackdetect
	Channels             []Channel
}

// Blackdetect configures the ffmpeg black-video check.
type Blackdetect struct {
	Enabled bool
	Workers int // concurrent ffmpeg processes across all channels
	// TriggerMin is the shortest black run, in seconds, that opens an
	// incident on its own. Shorter runs down to d are still recorded.
	TriggerMin float64
	blackdetect.Options
}

// Channel is one monitored channel.
type Channel struct {
	Name      string
	URL       string
	Rendition string // overrides Config.Rendition when set
}

// Default returns the defaults, with no channels.
func Default() Config {
	return Config{
		DataDir:              "./data",
		Listen:               "127.0.0.1:8765",
		LogFile:              "stream-analyzer.log",
		UserAgent:            "stream-analyzer/1.0",
		TLSMaxVersion:        "1.2",
		Rendition:            "highest",
		Buffer:               3 * time.Minute,
		PostRoll:             time.Minute,
		MergeWindow:          time.Minute,
		MaxIncident:          10 * time.Minute,
		IncidentStorageBytes: 20_000_000_000,
		MinFreeBytes:         5_000_000_000,
		HealthInterval:       time.Minute,
		StallTargetDurations: 3,
		Checks:               analysis.DefaultThresholds(),
		Blackdetect:          Blackdetect{Enabled: true, Workers: 4, TriggerMin: 1.0, Options: blackdetect.Default()},
	}
}

// Load reads and parses a config file.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c, err := Parse(data)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse parses a config document over the defaults and validates it.
func Parse(data []byte) (Config, error) {
	doc, err := yamlite.Parse(data)
	if err != nil {
		return Config{}, err
	}
	c := Default()
	var errs []error
	root := newSection("", doc, &errs)

	root.str("data_dir", &c.DataDir)
	root.str("listen", &c.Listen)
	root.str("log_file", &c.LogFile)
	root.str("user_agent", &c.UserAgent)
	root.str("tls_max_version", &c.TLSMaxVersion)
	root.str("rendition", &c.Rendition)
	root.dur("buffer", &c.Buffer)
	root.dur("post_roll", &c.PostRoll)
	root.dur("merge_window", &c.MergeWindow)
	root.dur("max_incident", &c.MaxIncident)
	root.dur("health_interval", &c.HealthInterval)
	root.float("stall_target_durations", &c.StallTargetDurations)
	// Sizes are checked before they become bytes: a tiny cap would round to
	// 0 bytes and delete every incident, a huge one would overflow.
	gb := float64(c.IncidentStorageBytes) / 1e9
	if root.float("incident_storage_gb", &gb) && !(gb >= 1 && gb <= maxGB) {
		root.fail("incident_storage_gb", fmt.Sprintf("must be between 1 and %g, got %g", float64(maxGB), gb))
		gb = 1
	}
	c.IncidentStorageBytes = int64(gb * 1e9)
	free := float64(c.MinFreeBytes) / 1e9
	if root.float("min_free_gb", &free) && !(free >= 0 && free <= maxGB) {
		root.fail("min_free_gb", fmt.Sprintf("must be between 0 (off) and %g, got %g", float64(maxGB), free))
		free = 0
	}
	c.MinFreeBytes = int64(free * 1e9)

	checks := root.sub("checks")
	checks.float("continuity_ms", &c.Checks.ContinuityMs)
	checks.float("pcr_jump_ms", &c.Checks.PCRJumpMs)
	checks.float("av_offset_ms", &c.Checks.AVOffsetMs)
	checks.int("av_baseline_segments", &c.Checks.BaselineSegments)
	checks.int("av_rebaseline_after", &c.Checks.RebaselineAfter)
	pct := c.Checks.DurationFraction * 100
	checks.float("duration_tolerance_pct", &pct)
	c.Checks.DurationFraction = pct / 100
	checks.float("video_gap_fault_ms", &c.Checks.VideoGapFaultMs)
	checks.float("audio_gap_fault_frames", &c.Checks.AudioGapFaultFrames)
	checks.checkUnknown()

	bd := root.sub("blackdetect")
	bd.bool("enabled", &c.Blackdetect.Enabled)
	bd.float("d", &c.Blackdetect.Duration)
	bd.float("pix_th", &c.Blackdetect.PixelThreshold)
	bd.float("pic_th", &c.Blackdetect.PictureThreshold)
	bd.float("trigger_min", &c.Blackdetect.TriggerMin)
	bd.str("ffmpeg", &c.Blackdetect.FFmpeg)
	bd.int("workers", &c.Blackdetect.Workers)
	bd.dur("timeout", &c.Blackdetect.Timeout)
	bd.checkUnknown()

	for i, item := range root.list("channels") {
		path := fmt.Sprintf("channels[%d]", i)
		m, ok := item.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: must be a mapping with name and url", path))
			continue
		}
		cs := newSection(path, m, &errs)
		var ch Channel
		cs.str("name", &ch.Name)
		cs.str("url", &ch.URL)
		cs.str("rendition", &ch.Rendition)
		cs.checkUnknown()
		c.Channels = append(c.Channels, ch)
	}
	root.checkUnknown()

	errs = append(errs, c.validate()...)
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxGB bounds the size settings, far below where bytes overflow int64.
const maxGB = 1e6

// checkSelector checks a rendition selector's syntax: highest, lowest, a
// 0-based index, a WxH resolution, or a URI substring. Whether a variant
// matches is only known once the master playlist is fetched.
func checkSelector(sel string) error {
	sel = strings.TrimSpace(sel)
	if n, err := strconv.Atoi(sel); err == nil && n < 0 {
		return fmt.Errorf("%q: a variant index is 0 or more", sel)
	}
	if w, h, ok := strings.Cut(strings.ToLower(sel), "x"); ok {
		wn, errW := strconv.Atoi(w)
		hn, errH := strconv.Atoi(h)
		if errW == nil && errH == nil && (wn <= 0 || hn <= 0) {
			return fmt.Errorf("%q: a resolution is WIDTHxHEIGHT, both above 0", sel)
		}
	}
	return nil
}

// CheckListen checks the manual trigger's address: "" (off), or a loopback
// host (localhost, 127.0.0.0/8, ::1) with a port number. POST /capture
// opens incidents and fills the disk, so it must not be reachable from the
// network.
func CheckListen(addr string) error {
	if addr == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen: %q is not host:port: %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("listen: %q needs a port number", addr)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen: %q must be a loopback address (127.0.0.1, ::1 or localhost), so only this machine can open incidents", addr)
	}
	return nil
}

// CheckUserAgent reports a User-Agent no HTTP request can carry: with a
// control character in it every fetch fails before it is sent, which looks
// like our own network failing, never like the origin.
func CheckUserAgent(ua string) error {
	if strings.ContainsFunc(ua, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }) {
		return fmt.Errorf("user_agent: %q contains a control character; every request would fail before it is sent", ua)
	}
	return nil
}

// CheckChannelNames reports two channels whose names differ only in case.
// Names are directory names, and on a case-insensitive file system (the
// macOS default) the two would share a buffer and mix their evidence.
func CheckChannelNames(chs []Channel) error {
	seen := map[string]string{}
	for i, ch := range chs {
		k := strings.ToLower(ch.Name)
		if other, ok := seen[k]; ok && other != ch.Name {
			return fmt.Errorf("channels[%d].name: %q and %q differ only in case; on a case-insensitive file system they would share directories", i, other, ch.Name)
		}
		seen[k] = ch.Name
	}
	return nil
}

// reservedFiles are the data directory's entries the monitor writes itself.
var reservedFiles = []string{
	"buffer", "incidents", "health.csv", "events.csv", "scte35.csv", "origin_notes.csv", "stream-analyzer.lock",
}

func (c Config) validate() []error {
	var errs []error
	fail := func(key, format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: %s", key, fmt.Sprintf(format, args...)))
	}
	if len(c.Channels) == 0 {
		fail("channels", "at least one channel is required")
	}
	seen := map[string]bool{}
	for i, ch := range c.Channels {
		key := fmt.Sprintf("channels[%d]", i)
		switch {
		case ch.Name == "":
			fail(key+".name", "required")
		case !safeName.MatchString(ch.Name) || ch.Name == "." || ch.Name == "..":
			fail(key+".name", "%q may only contain letters, digits, '.', '_' and '-'", ch.Name)
		case seen[ch.Name]:
			fail(key+".name", "duplicate channel name %q", ch.Name)
		}
		seen[ch.Name] = true
		if err := checkSelector(ch.Rendition); err != nil {
			fail(key+".rendition", "%v", err)
		}
		switch u, err := url.Parse(ch.URL); {
		case ch.URL == "":
			fail(key+".url", "required")
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			fail(key+".url", "must be an http or https URL, got %q", ch.URL)
		case u.User != nil:
			fail(key+".url", "carries credentials (user:password@); they would be copied into every evidence file")
		}
	}
	if err := CheckChannelNames(c.Channels); err != nil {
		errs = append(errs, err)
	}
	if err := CheckUserAgent(c.UserAgent); err != nil {
		errs = append(errs, err)
	}
	if c.DataDir == "" {
		fail("data_dir", "required: the directory the buffer, incidents and CSV files go in")
	}
	if lf := c.LogFile; lf != "" && !filepath.IsAbs(lf) {
		first, _, _ := strings.Cut(filepath.ToSlash(filepath.Clean(lf)), "/")
		switch {
		case !filepath.IsLocal(lf):
			fail("log_file", "%q is relative to data_dir and must stay inside it", lf)
		case slices.Contains(reservedFiles, first):
			fail("log_file", "%q is one of the monitor's own files or directories in data_dir", lf)
		}
	}
	if err := checkSelector(c.Rendition); err != nil {
		fail("rendition", "%v", err)
	}
	// Durations: each within a range where it still does its job.
	type span struct{ lo, hi time.Duration }
	durations := map[string]struct {
		v time.Duration
		span
	}{
		"buffer":          {c.Buffer, span{10 * time.Second, time.Hour}},
		"post_roll":       {c.PostRoll, span{0, time.Hour}},
		"merge_window":    {c.MergeWindow, span{0, time.Hour}},
		"max_incident":    {c.MaxIncident, span{time.Nanosecond, 24 * time.Hour}},
		"health_interval": {c.HealthInterval, span{time.Second, time.Hour}},
	}
	for _, key := range slices.Sorted(maps.Keys(durations)) {
		if d := durations[key]; d.v < d.lo || d.v > d.hi {
			fail(key, "must be between %v and %v, got %v", d.lo, d.hi, d.v)
		}
	}
	if c.TLSMaxVersion != "1.2" && c.TLSMaxVersion != "1.3" {
		fail("tls_max_version", "must be \"1.2\" or \"1.3\", got %q", c.TLSMaxVersion)
	}
	if err := CheckListen(c.Listen); err != nil {
		errs = append(errs, err)
	}
	if !(c.StallTargetDurations >= 1.5 && c.StallTargetDurations <= 100) {
		fail("stall_target_durations", "must be between 1.5 and 100, got %g", c.StallTargetDurations)
	}
	if c.MaxIncident < c.PostRoll {
		fail("max_incident", "must be at least post_roll (%v)", c.PostRoll)
	}
	k := c.Checks
	type bounds struct{ v, lo, hi float64 }
	for _, key := range []string{
		"checks.continuity_ms", "checks.pcr_jump_ms", "checks.av_offset_ms", "checks.duration_tolerance_pct",
		"checks.video_gap_fault_ms", "checks.audio_gap_fault_frames",
	} {
		b := map[string]bounds{
			"checks.continuity_ms":          {k.ContinuityMs, 1, 1000},
			"checks.pcr_jump_ms":            {k.PCRJumpMs, 1, 60000},
			"checks.av_offset_ms":           {k.AVOffsetMs, 1, 10000},
			"checks.duration_tolerance_pct": {k.DurationFraction * 100, 0.1, 100},
			"checks.video_gap_fault_ms":     {k.VideoGapFaultMs, max(k.ContinuityMs, 1), 60000},
			"checks.audio_gap_fault_frames": {k.AudioGapFaultFrames, 0.1, 100},
		}[key]
		if !(b.v >= b.lo && b.v <= b.hi) {
			fail(key, "must be between %g and %g, got %g", b.lo, b.hi, b.v)
		}
	}
	if k.BaselineSegments < 0 || k.BaselineSegments > 1000 {
		fail("checks.av_baseline_segments", "must be between 0 (the A/V offset check off) and 1000, got %d", k.BaselineSegments)
	}
	if k.RebaselineAfter < 0 || k.RebaselineAfter > 1000 {
		fail("checks.av_rebaseline_after", "must be between 0 (never) and 1000, got %d", k.RebaselineAfter)
	}
	b := c.Blackdetect
	if !(b.Duration >= 0.01 && b.Duration <= 60) {
		fail("blackdetect.d", "must be between 0.01 and 60 seconds, got %g", b.Duration)
	}
	if !(b.TriggerMin >= b.Duration && b.TriggerMin <= 3600) {
		fail("blackdetect.trigger_min", "must be between d (%g) and 3600 seconds, got %g", b.Duration, b.TriggerMin)
	}
	if b.PixelThreshold < 0 || b.PixelThreshold > 1 {
		fail("blackdetect.pix_th", "must be between 0 and 1")
	}
	if b.PictureThreshold < 0 || b.PictureThreshold > 1 {
		fail("blackdetect.pic_th", "must be between 0 and 1")
	}
	if b.Workers < 1 || b.Workers > 64 {
		fail("blackdetect.workers", "must be between 1 and 64, got %d", b.Workers)
	}
	if b.Timeout < time.Second || b.Timeout > 10*time.Minute {
		fail("blackdetect.timeout", "must be between 1s and 10m, got %v", b.Timeout)
	}
	return errs
}

// section decodes one mapping, remembering which keys were read so unknown
// ones can be reported with their full path.
type section struct {
	path string
	m    map[string]any
	used map[string]bool
	errs *[]error
}

func newSection(path string, m map[string]any, errs *[]error) *section {
	return &section{path: path, m: m, used: map[string]bool{}, errs: errs}
}

func (s *section) key(k string) string {
	if s.path == "" {
		return k
	}
	return s.path + "." + k
}

func (s *section) fail(k, msg string) {
	*s.errs = append(*s.errs, fmt.Errorf("%s: %s", s.key(k), msg))
}

func (s *section) scalar(k string) (string, bool) {
	s.used[k] = true
	v, ok := s.m[k]
	if !ok || v == nil {
		return "", false
	}
	str, ok := v.(string)
	if !ok {
		s.fail(k, "must be a single value")
		return "", false
	}
	return str, true
}

func (s *section) str(k string, dst *string) {
	if v, ok := s.scalar(k); ok {
		*dst = v
	}
}

func (s *section) dur(k string, dst *time.Duration) {
	if v, ok := s.scalar(k); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			s.fail(k, fmt.Sprintf("%q is not a duration (use units, like 60s or 3m)", v))
			return
		}
		*dst = d
	}
}

// float reports whether the key was present and valid.
func (s *section) float(k string, dst *float64) bool {
	v, ok := s.scalar(k)
	if !ok {
		return false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		s.fail(k, fmt.Sprintf("%q is not a finite number", v))
		return false
	}
	*dst = f
	return true
}

func (s *section) int(k string, dst *int) {
	if v, ok := s.scalar(k); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			s.fail(k, fmt.Sprintf("%q is not a whole number", v))
			return
		}
		*dst = n
	}
}

func (s *section) bool(k string, dst *bool) {
	if v, ok := s.scalar(k); ok {
		switch strings.ToLower(v) {
		case "true", "yes", "on":
			*dst = true
		case "false", "no", "off":
			*dst = false
		default:
			s.fail(k, fmt.Sprintf("%q is not true or false", v))
		}
	}
}

func (s *section) sub(k string) *section {
	s.used[k] = true
	child := newSection(s.key(k), map[string]any{}, s.errs)
	switch v := s.m[k].(type) {
	case nil:
	case map[string]any:
		child.m = v
	default:
		s.fail(k, "must be a mapping")
	}
	return child
}

func (s *section) list(k string) []any {
	s.used[k] = true
	switch v := s.m[k].(type) {
	case nil:
		return nil
	case []any:
		return v
	default:
		s.fail(k, "must be a list")
		return nil
	}
}

func (s *section) checkUnknown() {
	for _, k := range slices.Sorted(maps.Keys(s.m)) {
		if !s.used[k] {
			s.fail(k, "unknown setting")
		}
	}
}
