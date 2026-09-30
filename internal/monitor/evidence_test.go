package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
)

// An incident holds its own copies of the evidence, never links to the
// buffer's files: it is self-contained, and the storage cap counts what it
// really uses.
func TestIncidentFilesAreCopies(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, c := m.incidents, m.channels[0]
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.persist(kindSegments, "seg_1.ts", []byte("before")); err != nil { // buffered before the incident
		t.Fatal(err)
	}
	in.fault(c, []analysis.Fault{{Type: analysis.FaultPTSBehindPCR, Seq: 2}}, SegmentRecord{Seq: 2, URI: "s2.ts"})
	if err := c.persist(kindSegments, "seg_2.ts", []byte("during")); err != nil { // written while it is open
		t.Fatal(err)
	}
	in.shutdown()
	dir := incidentDirs(t, cfg)[0]
	for _, name := range []string{"seg_1.ts", "seg_2.ts"} {
		inc, err := os.Stat(filepath.Join(dir, kindSegments, name))
		if err != nil {
			t.Fatal(err)
		}
		buf, err := os.Stat(filepath.Join(c.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(inc, buf) {
			t.Errorf("%s in the incident is the buffer's file (a hard link), not a copy", name)
		}
	}
}

// A closed incident's report.json lists every evidence file in it, with its
// size and SHA-256, so a copy sent to a vendor can be checked.
func TestReportListsEveryEvidenceFileWithItsSHA256(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, c := m.incidents, m.channels[0]
	os.MkdirAll(c.dir, 0o755)
	c.persist(kindPlaylists, "playlist_x.m3u8", []byte("#EXTM3U\n"))
	in.fault(c, []analysis.Fault{{Type: analysis.FaultPTSBehindPCR, Seq: 2}}, SegmentRecord{Seq: 2, URI: "s2.ts"})
	c.persist(kindSegments, "seg_2.ts", []byte("segment bytes"))
	in.shutdown()
	dir := incidentDirs(t, cfg)[0]
	r := readReport(t, dir)
	want := map[string]string{
		"playlists/playlist_x.m3u8": "#EXTM3U\n",
		"segments/seg_2.ts":         "segment bytes",
	}
	got := map[string]EvidenceFile{}
	for _, f := range r.Files {
		got[f.Path] = f
	}
	for path, body := range want {
		f, ok := got[path]
		sum := sha256.Sum256([]byte(body))
		if !ok || f.Bytes != int64(len(body)) || f.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: listed as %+v, want %d bytes, sha256 %x", path, f, len(body), sum)
		}
	}
	if _, ok := got["report.json"]; ok {
		t.Error("report.json lists itself")
	}
	if len(r.Files) != len(want) {
		t.Errorf("files %+v, want exactly %v", r.Files, want)
	}
}

// An incident's report lists exactly the segments whose files it holds: the
// opening report is built from the sidecars copied from the buffer, not
// from records pruned by another clock.
func TestOpeningReportMatchesTheCopiedFiles(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, c := m.incidents, m.channels[0]
	os.MkdirAll(c.dir, 0o755)
	for _, seq := range []uint64{1, 2} {
		rec := SegmentRecord{Seq: seq, URI: fmt.Sprintf("s%d.ts", seq), File: fmt.Sprintf("seg_%d.ts", seq)}
		c.persist(kindSegments, rec.File, []byte("ts"))
		c.persistJSON(kindSegments, fmt.Sprintf("seg_%d.json", seq), rec)
	}
	c.remember(SegmentRecord{Seq: 0, URI: "s0.ts", File: "seg_0.ts"}) // its files already pruned
	in.fault(c, []analysis.Fault{{Type: analysis.FaultPTSBehindPCR, Seq: 2}}, SegmentRecord{Seq: 2, URI: "s2.ts", File: "seg_2.ts"})
	in.shutdown()
	var got []uint64
	for _, s := range readReport(t, incidentDirs(t, cfg)[0]).Segments {
		got = append(got, s.Seq)
	}
	if !slices.Equal(got, []uint64{1, 2}) {
		t.Errorf("report segments %v, want [1 2]: the files the incident holds", got)
	}
}

// A segment's files leave the buffer together, by the newer of their times,
// so an incident never gets a sidecar without its segment or the reverse.
func TestPruneKeepsASegmentsFilesTogether(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{
		"seg_5.ts": 4 * time.Minute, "seg_5.json": 2 * time.Minute, // the sidecar is written after the check
		"seg_6.ts": 5 * time.Minute, "seg_6.json": 5 * time.Minute,
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, now.Add(-age), now.Add(-age))
	}
	if err := pruneBuffer(dir, now, 3*time.Minute, time.Time{}); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"seg_5.json", "seg_5.ts"}; !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}
