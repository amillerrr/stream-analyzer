package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseAppliesDefaults(t *testing.T) {
	c, err := Parse([]byte("channels:\n  - name: channel1\n    url: https://h.example/a.m3u8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "./data" || c.Listen != "127.0.0.1:8765" || c.Rendition != "highest" || c.TLSMaxVersion != "1.2" {
		t.Errorf("data=%q listen=%q rendition=%q tls=%q", c.DataDir, c.Listen, c.Rendition, c.TLSMaxVersion)
	}
	if c.Buffer != 3*time.Minute || c.PostRoll != time.Minute || c.MergeWindow != time.Minute || c.HealthInterval != time.Minute {
		t.Errorf("buffer=%v post_roll=%v merge=%v health=%v", c.Buffer, c.PostRoll, c.MergeWindow, c.HealthInterval)
	}
	if c.IncidentStorageBytes != 20_000_000_000 || c.MinFreeBytes != 5_000_000_000 {
		t.Errorf("storage cap = %d, min free = %d", c.IncidentStorageBytes, c.MinFreeBytes)
	}
	k := c.Checks
	if k.PCRJumpMs != 500 || k.AVOffsetMs != 100 || k.DurationFraction != 0.10 || k.ContinuityMs != 10 ||
		k.VideoGapFaultMs != 500 || k.AudioGapFaultFrames != 1.5 {
		t.Errorf("checks = %+v", k)
	}
	b := c.Blackdetect
	if !b.Enabled || b.Duration != 0.1 || b.PixelThreshold != 0.10 || b.FFmpeg != "ffmpeg" || b.Workers < 1 || b.TriggerMin != 1.0 {
		t.Errorf("blackdetect = %+v", b)
	}
	if c.StallTargetDurations != 3 {
		t.Errorf("stall_target_durations = %v, want 3", c.StallTargetDurations)
	}
	if len(c.Channels) != 1 || c.Channels[0].Name != "channel1" || c.Channels[0].URL != "https://h.example/a.m3u8" {
		t.Errorf("channels = %+v", c.Channels)
	}
}

func TestParseOverrides(t *testing.T) {
	doc := `
data_dir: /tmp/rm
listen: 127.0.0.1:9000
log_file: ""
user_agent: test-agent
tls_max_version: "1.3"
rendition: lowest
buffer: 90s
post_roll: 30s
merge_window: 45s
max_incident: 5m
incident_storage_gb: 1.5
min_free_gb: 2.5
health_interval: 30s
stall_target_durations: 5
checks:
  continuity_ms: 5
  pcr_jump_ms: 250
  av_offset_ms: 50
  av_baseline_segments: 3
  av_rebaseline_after: 0
  duration_tolerance_pct: 20
  video_gap_fault_ms: 250
  audio_gap_fault_frames: 2
blackdetect:
  enabled: false
  d: 0.5
  pix_th: 0.2
  pic_th: 0.9
  trigger_min: 0.5
  ffmpeg: /opt/ffmpeg
  workers: 2
  timeout: 10s
channels:
  - name: a
    url: https://h.example/a.m3u8
    rendition: 640x360
  - name: b
    url: http://h.example/b.m3u8
`
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/tmp/rm" || c.Listen != "127.0.0.1:9000" || c.LogFile != "" || c.UserAgent != "test-agent" ||
		c.Rendition != "lowest" || c.TLSMaxVersion != "1.3" {
		t.Errorf("top level = %+v", c)
	}
	if c.Buffer != 90*time.Second || c.PostRoll != 30*time.Second || c.MergeWindow != 45*time.Second ||
		c.MaxIncident != 5*time.Minute || c.HealthInterval != 30*time.Second {
		t.Errorf("durations = %+v", c)
	}
	if c.IncidentStorageBytes != 1_500_000_000 || c.MinFreeBytes != 2_500_000_000 {
		t.Errorf("storage cap = %d, min free = %d", c.IncidentStorageBytes, c.MinFreeBytes)
	}
	k := c.Checks
	if k.ContinuityMs != 5 || k.PCRJumpMs != 250 || k.AVOffsetMs != 50 || k.BaselineSegments != 3 ||
		k.RebaselineAfter != 0 || k.DurationFraction != 0.20 || k.VideoGapFaultMs != 250 || k.AudioGapFaultFrames != 2 {
		t.Errorf("checks = %+v", k)
	}
	b := c.Blackdetect
	if b.Enabled || b.Duration != 0.5 || b.PixelThreshold != 0.2 || b.PictureThreshold != 0.9 ||
		b.FFmpeg != "/opt/ffmpeg" || b.Workers != 2 || b.Timeout != 10*time.Second || b.TriggerMin != 0.5 {
		t.Errorf("blackdetect = %+v", b)
	}
	if c.StallTargetDurations != 5 {
		t.Errorf("stall_target_durations = %v", c.StallTargetDurations)
	}
	if len(c.Channels) != 2 || c.Channels[0].Rendition != "640x360" || c.Channels[1].Rendition != "" {
		t.Errorf("channels = %+v", c.Channels)
	}
}

func TestParseRejectsBadConfig(t *testing.T) {
	const ch = "channels:\n  - name: a\n    url: https://h.example/a.m3u8\n"
	for name, tc := range map[string]struct{ doc, want string }{
		"unknown key":         {"bufer: 3m\n" + ch, "bufer"},
		"unknown nested key":  {"checks:\n  pcr_jmp_ms: 1\n" + ch, "checks.pcr_jmp_ms"},
		"bad duration":        {"buffer: 3 minutes\n" + ch, "buffer"},
		"duration no unit":    {"post_roll: 60\n" + ch, "post_roll"},
		"bad number":          {"checks:\n  pcr_jump_ms: lots\n" + ch, "checks.pcr_jump_ms"},
		"bad bool":            {"blackdetect:\n  enabled: maybe\n" + ch, "blackdetect.enabled"},
		"no channels":         {"buffer: 3m\n", "channel"},
		"duplicate name":      {ch + "  - name: a\n    url: https://h.example/b.m3u8\n", "duplicate"},
		"unsafe name":         {"channels:\n  - name: channel/1\n    url: https://h.example/a.m3u8\n", "name"},
		"missing url":         {"channels:\n  - name: a\n", "url"},
		"non-http url":        {"channels:\n  - name: a\n    url: ftp://h.example/a.m3u8\n", "url"},
		"unknown channel key": {"channels:\n  - name: a\n    url: https://h.example/a.m3u8\n    rendtion: 1\n", "channels[0].rendtion"},
		"negative storage":    {"incident_storage_gb: -1\n" + ch, "incident_storage_gb"},
		"max below post roll": {"max_incident: 30s\n" + ch, "max_incident"},
		"pix_th out of range": {"blackdetect:\n  pix_th: 1.5\n" + ch, "pix_th"},
		"bad tls version":     {"tls_max_version: \"1.1\"\n" + ch, "tls_max_version"},
		"NaN threshold":       {"checks:\n  pcr_jump_ms: NaN\n" + ch, "checks.pcr_jump_ms"},
		"zero video gap size": {"checks:\n  video_gap_fault_ms: 0\n" + ch, "checks.video_gap_fault_ms"},
		"negative audio size": {"checks:\n  audio_gap_fault_frames: -1\n" + ch, "checks.audio_gap_fault_frames"},
		"infinite d":          {"blackdetect:\n  d: +Inf\n" + ch, "blackdetect.d"},
		"zero stall":          {"stall_target_durations: 0\n" + ch, "stall_target_durations"},
		"negative trigger":    {"blackdetect:\n  trigger_min: -1\n" + ch, "blackdetect.trigger_min"},
	} {
		_, err := Parse([]byte(tc.doc))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

// The shipped channels.example.yaml must hold its two example channels.
func TestShippedChannelsFile(t *testing.T) {
	c, err := Load("../../channels.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"channel1", "channel2"}
	if len(c.Channels) != len(want) {
		t.Fatalf("channels = %d, want %d", len(c.Channels), len(want))
	}
	for i, name := range want {
		url := fmt.Sprintf("https://origin.example.com/live/%s/index.m3u8", name)
		if got := c.Channels[i]; got.Name != name || got.URL != url {
			t.Errorf("channel %d = %+v, want %s %s", i, got, name, url)
		}
	}
}
