package config

// Scratch audit: which absurd or dangerous settings does validation accept?

import (
	"strings"
	"testing"
)

const auditBase = "channels:\n  - name: a\n    url: https://h/x.m3u8\n"

func TestAuditAcceptedSettings(t *testing.T) {
	for _, extra := range []string{
		`data_dir: ""`,
		`listen: ""`,
		`listen: 0.0.0.0:8765`,
		`listen: "[::]:8765"`,
		`log_file: ../../../../tmp/elsewhere.log`,
		`log_file: /etc/stream-analyzer.log`,
		`log_file: health.csv`,
		`user_agent: "stream-analyzer/1.0\n"`,
		`user_agent: "a\u0000b"`,
		`user_agent: ""`,
		`rendition: "-1"`,
		`rendition: 0x0`,
		`rendition: " 1"`,
		`buffer: 1ns`,
		`buffer: 100000h`,
		`health_interval: 1ns`,
		`post_roll: 0s`,
		`merge_window: 0s`,
		"post_roll: 0s\nmax_incident: 1ns",
		`stall_target_durations: 0.001`,
		`stall_target_durations: 1e300`,
		"checks:\n  continuity_ms: 1e-9",
		"checks:\n  av_baseline_segments: 0",
		"checks:\n  av_baseline_segments: 1000000000",
		"checks:\n  duration_tolerance_pct: 1e-9",
		"checks:\n  duration_tolerance_pct: 100000",
		"blackdetect:\n  d: 1e-9",
		"blackdetect:\n  d: 1e9",
		"blackdetect:\n  trigger_min: 0",
		"blackdetect:\n  trigger_min: 0.05",
		"blackdetect:\n  workers: 1000000000",
		"blackdetect:\n  timeout: 1ns",
		"blackdetect:\n  ffmpeg: \"\"",
		"blackdetect:\n  enabled: ~",
		"blackdetect:\n  enabled:",
		`log_file:`,
		`log_file: ~`,
		`data_dir: null`,
		`listen:`,
		`data_dir: /Volumes/Media Team's SSD/stream-data   # evidence drive`,
		`user_agent: stream-analyzer "lab" build # comment`,
	} {
		c, err := Parse([]byte(auditBase + extra + "\n"))
		if err != nil {
			t.Logf("REJECTED %-45q %v", extra, strings.ReplaceAll(err.Error(), "\n", "; "))
			continue
		}
		t.Logf("accepted %-45q -> data_dir=%q listen=%q log_file=%q ua=%q rendition=%q buffer=%v health=%v stall=%g checks=%+v bd.enabled=%v bd.d=%g trig=%g workers=%d timeout=%v ffmpeg=%q",
			extra, c.DataDir, c.Listen, c.LogFile, c.UserAgent, c.Rendition, c.Buffer, c.HealthInterval, c.StallTargetDurations,
			c.Checks, c.Blackdetect.Enabled, c.Blackdetect.Duration, c.Blackdetect.TriggerMin, c.Blackdetect.Workers, c.Blackdetect.Timeout, c.Blackdetect.FFmpeg)
	}
	for _, chans := range []string{
		"channels:\n  - name: channel1\n    url: https://h/a.m3u8\n  - name: CHANNEL1\n    url: https://h/b.m3u8\n",
		"channels:\n  - name: a\n    url: https://user:secret@h/x.m3u8?token=abc\n",
		"channels:\n  - name: a\n    url: \"https://h/x.m3u8 \"\n",
		"channels:\n  - name: a\n    url: https://h/x.m3u8#frag\n",
		"channels:\n  - name: a\n    url: https://h/x.m3u8\n    rendition: \"-2\"\n",
		"channels:\n  - name: ...\n    url: https://h/x.m3u8\n",
		"channels:\n  - name: -rf\n    url: https://h/x.m3u8\n",
	} {
		c, err := Parse([]byte(chans))
		if err != nil {
			t.Logf("REJECTED %q: %v", chans, err)
			continue
		}
		t.Logf("accepted channels %+v", c.Channels)
	}
}
