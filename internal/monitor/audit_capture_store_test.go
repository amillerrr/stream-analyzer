package monitor

// Scratch audit tests (store, CSV, lock, recovery). Not part of the audited
// code.

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
)

// CAP: the same sequence number downloaded twice while an incident is open
// (e.g. after a media-sequence reset): the incident keeps the first bytes,
// but report.json describes the second download.
func TestAuditSameSeqTwiceInOpenIncident(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	in, c := m.incidents, m.channels[0]
	os.MkdirAll(c.dir, 0o755)
	if _, _, err := in.manual(c); err != nil {
		t.Fatal(err)
	}
	a, b := bytes.Repeat([]byte{'A'}, 1000), bytes.Repeat([]byte{'B'}, 2000)
	for _, body := range [][]byte{a, b} {
		rec := SegmentRecord{Seq: 7, URI: "s7.ts", File: "seg_7.ts", Fetch: FetchMeta{Bytes: len(body)}}
		c.persist(kindSegments, "seg_7.ts", body)
		c.persistJSON(kindSegments, "seg_7.json", rec)
		in.segment(c, rec)
	}
	in.shutdown()
	dir := incidentDirs(t, cfg)[0]
	ts, _ := os.ReadFile(filepath.Join(dir, "segments", "seg_7.ts"))
	var side SegmentRecord
	sb, _ := os.ReadFile(filepath.Join(dir, "segments", "seg_7.json"))
	json.Unmarshal(sb, &side)
	var inReport int
	for _, s := range readReport(t, dir).Segments {
		if s.Seq == 7 {
			inReport = s.Fetch.Bytes
		}
	}
	buf, _ := os.ReadFile(filepath.Join(c.dir, "seg_7.ts"))
	t.Logf("incident seg_7.ts=%d bytes, incident seg_7.json says %d, report.json says %d, buffer seg_7.ts=%d", len(ts), side.Fetch.Bytes, inReport, len(buf))
	if len(ts) != inReport {
		t.Errorf("report.json describes a %d-byte download; the incident holds the %d-byte file", inReport, len(ts))
	}
}

// CAP: a row cut short (disk full, power loss) makes the next append
// continue the same line; both rows are lost to a line-by-line reader.
func TestAuditTornRowGluesNextRow(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	m := newTestMonitor(t, cfg, nil)
	path := filepath.Join(cfg.DataDir, "scte35.csv")
	os.MkdirAll(cfg.DataDir, 0o755)
	os.WriteFile(path, []byte(strings.Join(cueColumns, ",")+"\n2026-01-03T00:00:00.000Z,test,1,OU"), 0o644)
	if err := m.appendCSV("scte35.csv", cueColumns, map[string]string{"time_utc": "2026-01-03T00:00:06.000Z", "channel": "test", "seq": "2", "scte35_tag": "CONT"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	t.Logf("file:\n%s", b)
	if len(lines) != 3 {
		t.Errorf("%d lines; the new row was appended onto the torn one: %q", len(lines), lines[len(lines)-1])
	}
}

// CAP: deleting the lock file while a monitor holds it lets a second
// monitor lock the same data directory.
func TestAuditLockFileDeletedWhileHeld(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	os.Remove(filepath.Join(dir, lockFile))
	second, err := lockDataDir(dir)
	if err == nil {
		second()
		t.Errorf("a second lock was granted after the lock file was deleted")
	}
}

// CAP: recover() appends a CSV row even when it could not mark the report
// interrupted, so every restart adds another row for the same incident.
func TestAuditRecoverDoubleCountsWhenReportUnwritable(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	dir := filepath.Join(cfg.DataDir, "incidents", "20260101T000000Z_test")
	os.MkdirAll(dir, 0o755)
	b, _ := json.Marshal(Report{ID: filepath.Base(dir), Channel: "test", Status: "open",
		Faults: []FaultRecord{{Type: analysis.FaultPTSBehindPCR, Seq: 7}}})
	os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644)
	os.Chmod(dir, 0o555) // e.g. a directory left by a sudo run, or a full disk
	defer os.Chmod(dir, 0o755)
	for range 3 { // three restarts
		newTestMonitor(t, cfg, nil).incidents.recover()
	}
	rows := readCSV(t, cfg)
	t.Logf("incidents.csv rows after 3 restarts: %d (1 header)", len(rows))
	if len(rows) != 2 {
		t.Errorf("one interrupted incident has %d rows", len(rows)-1)
	}
}
