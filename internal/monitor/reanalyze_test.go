package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// snapshot maps every file under dir to its SHA-256.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// faultKeys names each fault by type, segment and the segment before it.
func faultKeys(fs []FaultRecord) []string {
	var out []string
	for _, f := range fs {
		k := fmt.Sprintf("%s@%d", f.Type, f.Seq)
		if f.PrevSeq != nil {
			k += fmt.Sprintf("<%d", *f.PrevSeq)
		}
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// reanalyzed runs Reanalyze on dir and checks that it added report.v2.json,
// holding what it returned, and changed nothing else.
func reanalyzed(t *testing.T, dir string, o Options) Report {
	t.Helper()
	before := snapshot(t, dir)
	r, err := Reanalyze(t.Context(), dir, o)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dir)
	if _, ok := after[reanalyzeFile]; !ok {
		t.Fatalf("no %s written", reanalyzeFile)
	}
	delete(after, reanalyzeFile)
	if !maps.Equal(before, after) {
		t.Errorf("reanalyze changed the incident:\nbefore %v\nafter  %v", before, after)
	}
	b, err := os.ReadFile(filepath.Join(dir, reanalyzeFile))
	if err != nil {
		t.Fatal(err)
	}
	var saved Report
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ID != r.ID || len(saved.Faults) != len(r.Faults) {
		t.Errorf("%s holds %s with %d faults; Reanalyze returned %s with %d", reanalyzeFile, saved.ID, len(saved.Faults), r.ID, len(r.Faults))
	}
	for _, f := range r.Files {
		if f.Path == reanalyzeFile {
			t.Errorf("the report lists itself among the evidence files")
		}
	}
	if !slices.ContainsFunc(r.Files, func(f EvidenceFile) bool { return f.Path == "report.json" }) {
		t.Errorf("the original report.json is not among the evidence files: %v", r.Files)
	}
	return r
}

// Reanalyzing an incident the monitor wrote finds what the monitor found:
// the same faults on the same segments with the same verdicts, and the
// same segments with the same files.
func TestReanalyzeAgreesWithTheMonitor(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(twoVariantMaster))
	jumped := tstest.Base
	jumped.VideoStart += 900000
	jumped.AudioStart += 900000
	content := map[uint64][]byte{
		100: tstest.Base.Segment(0).Bytes(), 101: tstest.Base.Segment(1).Bytes(), 103: tstest.Base.Segment(2).Bytes(),
		104: jumped.Segment(3).Bytes(), 105: jumped.Segment(4).Bytes(), 106: jumped.Segment(5).Bytes(),
	}
	for _, v := range []string{"hi", "lo"} {
		for n, body := range content {
			o.sequence(fmt.Sprintf("/live/%s-begin=0-dur=5340000-seq=%d.ts", v, n), body)
		}
	}
	o.sequence("/live/hi.m3u8",
		numberedPlaylist("hi", 100, 100, 101, 103),
		numberedPlaylist("hi", 100, 100, 101, 103, 104),
		numberedPlaylist("hi", 100, 100, 101, 103, 104),
		numberedPlaylist("hi", 103, 103, 104, 105, 106))
	o.sequence("/live/lo.m3u8", numberedPlaylist("lo", 100, 100, 101, 103, 104))
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.PostRoll, cfg.MergeWindow = 2e9, 2e9
	run(t, cfg, func(m *Monitor, recs map[uint64]SegmentRecord) bool {
		_, ok := recs[106]
		return ok && anyClosed(t, cfg.DataDir)
	})
	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents %v, want 1", dirs)
	}
	live := readReport(t, dirs[0])
	v2 := reanalyzed(t, dirs[0], Options{Config: cfg, Logger: testLogger(t)})

	// The comparison means something only if the monitor found the jump.
	if !slices.Contains(faultKeys(live.Faults), "video_dts_gap@104<103") {
		t.Fatalf("the monitor's faults %v lack the jump at 104", faultKeys(live.Faults))
	}
	if got, want := faultKeys(v2.Faults), faultKeys(live.Faults); !slices.Equal(got, want) {
		t.Errorf("faults\n got  %v\n want %v (the monitor's)", got, want)
	}
	verdicts := func(r Report) map[string]string {
		out := map[string]string{}
		for _, f := range r.Faults {
			for label, v := range f.Renditions {
				out[fmt.Sprintf("%s@%d %s", f.Type, f.Seq, label)] = v
			}
		}
		return out
	}
	if got, want := verdicts(v2), verdicts(live); !maps.Equal(got, want) {
		t.Errorf("verdicts\n got  %v\n want %v (the monitor's)", got, want)
	}
	files := func(r Report) map[uint64]string {
		out := map[uint64]string{}
		for _, s := range r.Segments {
			out[s.Seq] = s.File
		}
		return out
	}
	if got, want := files(v2), files(live); !maps.Equal(got, want) {
		t.Errorf("segments\n got  %v\n want %v (the monitor's)", got, want)
	}
	if v2.ID != live.ID || v2.Channel != live.Channel || !v2.OpenedAt.Equal(live.OpenedAt) {
		t.Errorf("header %s %s %v, want the original's %s %s %v", v2.ID, v2.Channel, v2.OpenedAt, live.ID, live.Channel, live.OpenedAt)
	}
}

// oldIncident writes an incident as the monitor did before segments were
// known by the origin's number: files named by playlist position, so the
// renumbered playlist (MEDIA-SEQUENCE 100 -> 103 while seq=103 sat at
// position 102) made it download seq=104 twice, as positions 103 and 104,
// and report the second copy as a timestamp jump back. The media are
// continuous.
func oldIncident(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"playlists", "segments"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONFile := func(rel string, v any) {
		t.Helper()
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		write(rel, b)
	}
	const base = "http://origin.test/live/"
	t0 := time.Date(2026, 9, 29, 17, 55, 0, 0, time.UTC)
	uri := func(n uint64) string { return fmt.Sprintf("hi-begin=0-dur=5340000-seq=%d.ts", n) }
	// Playlists, one a second.
	for i, body := range [][]byte{
		numberedPlaylist("hi", 100, 100, 101, 103),
		numberedPlaylist("hi", 100, 100, 101, 103, 104),
		numberedPlaylist("hi", 103, 103, 104, 105, 106),
	} {
		at := t0.Add(time.Duration(i) * time.Second)
		pl, err := hls.ParseMedia(body)
		if err != nil {
			t.Fatal(err)
		}
		name := playlistName(at, pl.MediaSequence)
		write("playlists/"+name+".m3u8", body)
		writeJSONFile("playlists/"+name+".json", FetchMeta{URL: base + "hi.m3u8", RequestedAt: at, CompletedAt: at.Add(50 * time.Millisecond), Status: 200})
	}
	// Segments by position: position -> (origin number, media).
	for _, s := range []struct {
		pos, num uint64
		k        int
		at       int // seconds after t0
	}{
		{100, 100, 0, 0}, {101, 101, 1, 0}, {102, 103, 2, 0}, {103, 104, 3, 1}, {104, 104, 3, 2}, {105, 105, 4, 2}, {106, 106, 5, 2},
	} {
		at := t0.Add(time.Duration(s.at)*time.Second + 100*time.Millisecond + time.Duration(s.pos)*time.Millisecond)
		file := fmt.Sprintf("seg_%d.ts", s.pos)
		write("segments/"+file, tstest.Base.Segment(s.k).Bytes())
		// The old sidecar: seq is the position, and there is no msn.
		writeJSONFile(fmt.Sprintf("segments/seg_%d.json", s.pos), map[string]any{
			"seq": s.pos, "uri": uri(s.num), "file": file, "extinf": 0.534, "discontinuity_sequence": 0,
			"fetch": FetchMeta{URL: base + uri(s.num), RequestedAt: at, CompletedAt: at.Add(20 * time.Millisecond), Status: 200},
		})
	}
	writeJSONFile("report.json", map[string]any{
		"id": "20260929T175602Z_test", "channel": "test", "status": "closed",
		"opened_at": "2026-09-29T10:56:02-07:00", "last_fault_at": "2026-09-29T10:56:02-07:00",
		"closed_at": "2026-09-29T10:57:02-07:00", "close_reason": "post_roll_elapsed",
		"stream":      map[string]any{"url": base + "master.m3u8", "media_playlist_url": base + "hi.m3u8", "rendition": "0_1280x720_2000000"},
		"fault_types": []string{"audio_pts_gap", "video_dts_gap"}, "fault_count": 2,
		"faults": []map[string]any{
			{"type": "video_dts_gap", "detected_at": "2026-09-29T10:56:02-07:00", "seq": 104, "message": "video DTS jumped -534.000 ms"},
			{"type": "audio_pts_gap", "detected_at": "2026-09-29T10:56:02-07:00", "seq": 104, "message": "audio PTS jumped -534.000 ms"},
		},
	})
	return dir
}

// An incident saved before the fix is reanalyzed by the origin's numbers:
// the second download of seq=104 is not compared with the first, so the
// artifact jump is gone; the renumbering is reported as a playlist
// violation; each segment's record names the file that holds it.
func TestReanalyzeOldIncident(t *testing.T) {
	dir := oldIncident(t)
	cfg := testConfig(t, "http://origin.test/live/master.m3u8")
	v2 := reanalyzed(t, dir, Options{Config: cfg, Logger: testLogger(t)})

	for _, f := range v2.Faults {
		if f.Type == analysis.FaultVideoDTSGap || f.Type == analysis.FaultAudioPTSGap {
			t.Errorf("fault %s at %d: %s", f.Type, f.Seq, f.Message)
		}
	}
	if !slices.ContainsFunc(v2.Faults, func(f FaultRecord) bool {
		return f.Type == analysis.FaultPlaylistViolation && f.Values["reason"] == hls.ViolationRenumbered
	}) {
		t.Errorf("faults %v: want a playlist_violation for the renumbering", faultKeys(v2.Faults))
	}
	got := map[uint64]string{}
	for _, s := range v2.Segments {
		got[s.Seq] = s.File
	}
	want := map[uint64]string{100: "seg_100.ts", 101: "seg_101.ts", 103: "seg_102.ts", 104: "seg_103.ts", 105: "seg_105.ts", 106: "seg_106.ts"}
	if !maps.Equal(got, want) {
		t.Errorf("segments %v, want %v", got, want)
	}
	if !slices.ContainsFunc(v2.Notes, func(n string) bool { return strings.Contains(n, "seg_104.ts") && strings.Contains(n, "seq=104") }) {
		t.Errorf("notes %q: want seg_104.ts named as a second download of seq=104", v2.Notes)
	}
	if v2.OpenedAt.Location() != time.UTC || v2.OpenedAt.Format(time.RFC3339) != "2026-09-29T17:56:02Z" {
		t.Errorf("opened_at %v, want the original's time in UTC", v2.OpenedAt)
	}
}

// A directory without report.json is not an incident: nothing is written.
func TestReanalyzeRefusesADirectoryWithoutAReport(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, "http://origin.test/live/master.m3u8")
	if _, err := Reanalyze(t.Context(), dir, Options{Config: cfg, Logger: testLogger(t)}); err == nil {
		t.Error("no error for a directory without report.json")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

// Faults found before the original incident opened, in segments saved as
// its pre-roll, are marked pre_roll; the fault that opened it, on the
// segment fetched just before, and everything after, are not.
func TestReanalyzeMarksThePreRoll(t *testing.T) {
	dir := oldIncident(t)
	// The original incident opened on seq=104 (position 103), fetched 1.2 s
	// after the first playlist. Moving seq=101's audio 36 ms later makes a
	// fault in the pre-roll.
	late := tstest.Base.Segment(1)
	for i := range late.Audio {
		late.Audio[i].PTS = ts.Add(late.Audio[i].PTS, 3240)
	}
	if err := os.WriteFile(filepath.Join(dir, "segments", "seg_101.ts"), late.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "report.json"))
	var old map[string]any
	json.Unmarshal(b, &old)
	old["faults"] = []map[string]any{{"type": "video_dts_gap", "detected_at": "2026-09-29T17:55:01.5Z", "seq": 103, "message": "x"}}
	old["opened_at"] = "2026-09-29T17:55:01.5Z"
	old["segments"] = []map[string]any{{"seq": 103, "uri": "hi-begin=0-dur=5340000-seq=104.ts",
		"fetch": map[string]any{"requested_at": "2026-09-29T17:55:01.203Z", "completed_at": "2026-09-29T17:55:01.223Z", "status": 200}}}
	b, _ = json.Marshal(old)
	os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)

	v2 := reanalyzed(t, dir, Options{Config: testConfig(t, "http://origin.test/live/master.m3u8"), Logger: testLogger(t)})
	var pre, after []string
	for _, f := range v2.Faults {
		k := fmt.Sprintf("%s@%d", f.Type, f.Seq)
		if f.PreRoll {
			pre = append(pre, k)
		} else {
			after = append(after, k)
		}
	}
	if !slices.Contains(pre, "audio_pts_gap@101") {
		t.Errorf("pre-roll faults %v, want audio_pts_gap@101", pre)
	}
	if slices.ContainsFunc(after, func(k string) bool { return strings.HasSuffix(k, "@101") }) {
		t.Errorf("faults on 101 not marked pre_roll: %v", after)
	}
	if !slices.ContainsFunc(v2.Notes, func(n string) bool { return strings.Contains(n, "pre_roll") }) {
		t.Errorf("notes %q: want the pre-roll faults counted", v2.Notes)
	}
}
