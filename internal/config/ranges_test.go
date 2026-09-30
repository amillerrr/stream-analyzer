package config

import (
	"strings"
	"testing"
)

// A setting outside a sane range is an error naming it, not a silent
// switch-off, a flood or deleted evidence.
func TestSettingsOutsideSaneRangesAreRejected(t *testing.T) {
	for setting, key := range map[string]string{
		"incident_storage_gb: 0.0000000001":         "incident_storage_gb",
		"incident_storage_gb: 1e10":                 "incident_storage_gb",
		"incident_storage_gb: 0.5":                  "incident_storage_gb",
		"min_free_gb: 1e10":                         "min_free_gb",
		"buffer: 1s":                                "buffer",
		"buffer: 100000h":                           "buffer",
		"health_interval: 1ns":                      "health_interval",
		"post_roll: 100h\nmax_incident: 200h":       "post_roll",
		"max_incident: 100h":                        "max_incident",
		"merge_window: 100h":                        "merge_window",
		"stall_target_durations: 0.001":             "stall_target_durations",
		"stall_target_durations: 1e300":             "stall_target_durations",
		"checks:\n  continuity_ms: 1e-9":            "checks.continuity_ms",
		"checks:\n  av_baseline_segments: 1000000":  "checks.av_baseline_segments",
		"checks:\n  duration_tolerance_pct: 100000": "checks.duration_tolerance_pct",
		"checks:\n  video_gap_fault_ms: 5":          "checks.video_gap_fault_ms",
		"checks:\n  audio_gap_fault_frames: 1000":   "checks.audio_gap_fault_frames",
		"blackdetect:\n  d: 1e9":                    "blackdetect.d",
		"blackdetect:\n  d: 1e-9":                   "blackdetect.d",
		"blackdetect:\n  trigger_min: 0.05":         "blackdetect.trigger_min",
		"blackdetect:\n  workers: 1000000000":       "blackdetect.workers",
		"blackdetect:\n  timeout: 1ns":              "blackdetect.timeout",
		`rendition: "-1"`:                           "rendition",
		`rendition: 0x0`:                            "rendition",
		"channels:\n  - name: a\n    url: https://h/x.m3u8\n    rendition: \"-2\"": "channels[0].rendition",
	} {
		doc := setting + "\n"
		if !strings.HasPrefix(setting, "channels:") {
			doc += "channels:\n  - name: a\n    url: https://h/x.m3u8\n"
		}
		_, err := Parse([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: error %v, want one naming %s", setting, err, key)
		}
	}
}

// Values at the edges of the ranges, and the documented "off" values, are
// accepted.
func TestSaneSettingsAreAccepted(t *testing.T) {
	for _, setting := range []string{
		"incident_storage_gb: 1", "min_free_gb: 0", "buffer: 10s", "buffer: 1h", "health_interval: 1s",
		"stall_target_durations: 1.5", "stall_target_durations: 100", "checks:\n  av_baseline_segments: 0",
		"checks:\n  av_rebaseline_after: 0", "blackdetect:\n  timeout: 1s", "blackdetect:\n  trigger_min: 0.1",
		`rendition: " 1 "`, `rendition: 640x360`, `rendition: lowest`, `rendition: avc1_600000`,
	} {
		if _, err := Parse([]byte(setting + "\nchannels:\n  - name: a\n    url: https://h/x.m3u8\n")); err != nil {
			t.Errorf("%q: %v", setting, err)
		}
	}
}

// Settings that would quietly break the monitor or its evidence are
// rejected: a user_agent no HTTP request can carry, no data directory, a
// log file outside it or on one of the monitor's own files, channel names
// that one directory would hold on a case-insensitive file system, and
// credentials that would be copied into every evidence file.
func TestSettingsThatBreakEvidenceAreRejected(t *testing.T) {
	const ch = "channels:\n  - name: a\n    url: https://h/x.m3u8\n"
	for doc, key := range map[string]string{
		"user_agent: \"stream-analyzer/1.0\\n\"\n" + ch: "user_agent",
		"user_agent: \"a\\x7fb\"\n" + ch:                "user_agent",
		"data_dir: \"\"\n" + ch:                         "data_dir",
		"log_file: ../outside.log\n" + ch:               "log_file",
		"log_file: health.csv\n" + ch:                   "log_file",
		"log_file: incidents/x.log\n" + ch:              "log_file",
		"channels:\n  - name: channel1\n    url: https://h/a.m3u8\n  - name: CHANNEL1\n    url: https://h/b.m3u8\n": "channels[1].name",
		"channels:\n  - name: a\n    url: https://user:secret@h/x.m3u8\n":                                           "channels[0].url",
	} {
		_, err := Parse([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%q: error %v, want one naming %s", doc, err, key)
		}
	}
	for _, doc := range []string{
		"log_file: logs/stream-analyzer.log\n" + ch,
		"log_file: /var/log/stream-analyzer.log\n" + ch,
		"log_file: \"\"\n" + ch,
		"user_agent: \"ExamplePlayer/12.0 (12.0.0.4182-88)\"\n" + ch,
	} {
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("%q: %v", doc, err)
		}
	}
}
