package monitor

// Audit: regression tests for five ways the monitor went
// wrong. The comment above each test describes the failure, and the test
// fails if it comes back: a renumbered playlist reported as a timestamp
// jump, rendition files written outside the incident, blackdetect
// failures missing from the health line, a truncated copy kept as
// evidence, and an unwritable buffer that looks healthy. Run them with:
//
//	go test ./internal/monitor -run TestAudit -v

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// renumberingOrigin serves two renditions whose playlists do what an
// origin was seen to do: segment number 102 is skipped, so
// the segment named 103 sits at position 102, and a later playlist starts
// at EXT-X-MEDIA-SEQUENCE 103 with that same segment, renumbering every
// segment after it by one. The media itself is continuous: file uN holds
// the timeline's segment N-100, except that u103 onwards hold N-101.
func renumberingOrigin(t *testing.T) (*origin, string, func(phase int)) {
	o, srv := newOrigin(t)
	o.set("/live/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nhi.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=640x360\nlo.m3u8\n"))
	content := map[int]int{100: 0, 101: 1, 103: 2, 104: 3, 105: 4, 106: 5}
	for _, v := range []string{"hi", "lo"} {
		for uri, k := range content {
			o.set(fmt.Sprintf("/live/%s%d.ts", v, uri), tstest.Base.Segment(k).Bytes())
		}
	}
	phases := [][]int{
		{100, 101, 103},      // MSN 100: position 102 holds u103
		{100, 101, 103, 104}, // MSN 100: position 103 holds u104
		{103, 104, 105, 106}, // MSN 103: u103 is position 103 now
	}
	msn := []uint64{100, 100, 103}
	set := func(phase int) {
		for _, v := range []string{"hi", "lo"} {
			var es []entry
			for _, u := range phases[phase] {
				es = append(es, entry{uri: fmt.Sprintf("%s%d.ts", v, u)})
			}
			o.set("/live/"+v+".m3u8", media(msn[phase], es...))
		}
	}
	set(0)
	return o, srv.URL + "/live/master.m3u8", set
}

// TestAuditRenumberedPlaylistIsNotATimestampJump: a segment the origin lists
// again under a new sequence number is downloaded twice and reported as a
// video_dts_gap and audio_pts_gap (the media going back one segment), and
// the other rendition, fetched after the renumbering, is judged
// not_reproduced. The media timeline never went back.
func TestAuditRenumberedPlaylistIsNotATimestampJump(t *testing.T) {
	_, url, set := renumberingOrigin(t)
	cfg := testConfig(t, url)
	cfg.PostRoll, cfg.MergeWindow = 2*time.Second, 2*time.Second

	var phase int
	done := func(m *Monitor, recs map[uint64]SegmentRecord) bool {
		switch _, ok102 := recs[102]; {
		case phase == 0 && ok102:
			phase = 1
			set(1)
		case phase == 1 && recs[103].URI != "":
			phase = 2
			set(2)
		}
		if _, ok := recs[105]; !ok {
			return false
		}
		return anyClosed(t, cfg.DataDir) || len(incidentDirs(t, cfg)) == 0 && time.Since(start(recs)) > 3*time.Second
	}
	_, recs := run(t, cfg, done)

	if recs[103].URI == recs[104].URI {
		t.Errorf("sequence numbers 103 and 104 are the same origin segment %q, downloaded twice", recs[104].URI)
	}
	if slices.Contains(recs[104].Faults, analysis.FaultVideoDTSGap) || slices.Contains(recs[104].Faults, analysis.FaultAudioPTSGap) {
		t.Errorf("104 faults = %v: a timestamp fault for a segment that was only listed again", recs[104].Faults)
	}
	for _, d := range incidentDirs(t, cfg) {
		for _, f := range readReport(t, d).Faults {
			if f.Type == analysis.FaultVideoDTSGap {
				t.Errorf("incident %s: %s at %d, other rendition says %v", filepath.Base(d), f.Type, f.Seq, f.Renditions)
			}
		}
	}
}

// anyClosed reports whether an incident's report.json says closed. A
// report not written yet is skipped rather than failing the test.
func anyClosed(t *testing.T, dataDir string) bool {
	entries, _ := os.ReadDir(filepath.Join(dataDir, "incidents"))
	for _, e := range entries {
		var r Report
		b, err := os.ReadFile(filepath.Join(dataDir, "incidents", e.Name(), "report.json"))
		if err == nil && json.Unmarshal(b, &r) == nil && r.Status == "closed" {
			return true
		}
	}
	return false
}

// start is when the first record was fetched.
func start(recs map[uint64]SegmentRecord) time.Time {
	var first time.Time
	for _, r := range recs {
		if first.IsZero() || r.Fetch.RequestedAt.Before(first) {
			first = r.Fetch.RequestedAt
		}
	}
	return first
}

// TestAuditRenditionLabelStaysInsideTheIncident: rendition directories are
// named from the master playlist's RESOLUTION attribute, which the origin
// controls. A value with "../" puts the rendition's files outside the
// incident, and outside the data directory.
func TestAuditRenditionLabelStaysInsideTheIncident(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/master.m3u8", []byte("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720\nhi.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=../../../../../../escaped\nlo.m3u8\n"))
	before, after := tstest.Load(t, "cont_0.ts"), tstest.Load(t, "jump_1.ts")
	for _, v := range []string{"hi", "lo"} {
		o.set("/live/"+v+".m3u8", media(200, entry{uri: v + "200.ts"}, entry{uri: v + "201.ts"}))
		o.set("/live/"+v+"200.ts", before)
		o.set("/live/"+v+"201.ts", after)
	}
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.PostRoll = time.Second
	run(t, cfg, func(*Monitor, map[uint64]SegmentRecord) bool { return anyClosed(t, cfg.DataDir) })

	root := filepath.Dir(cfg.DataDir)
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && path != root && path != cfg.DataDir && !strings.HasPrefix(path, cfg.DataDir+string(filepath.Separator)) {
			t.Errorf("written outside the data directory: %s", path)
		}
		return nil
	})
}

// TestAuditBlackdetectFailureShowsInHealth: when ffmpeg fails on every
// segment, the health line should not look healthy. It is logged at INFO,
// and health.csv has no column that records the failures.
func TestAuditBlackdetectFailureShowsInHealth(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(100, entry{uri: "s100.ts"}, entry{uri: "s101.ts"}, entry{uri: "s102.ts"}))
	for i, name := range []string{"s100.ts", "s101.ts", "s102.ts"} {
		o.set("/live/"+name, tstest.Load(t, fmt.Sprintf("cont_%d.ts", i)))
	}
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled = true
	cfg.HealthInterval = 700 * time.Millisecond

	var mu sync.Mutex
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil))
	busyHealth := func(line string) bool {
		return strings.Contains(line, "msg=health") && !strings.Contains(line, " segments=0 ")
	}
	health := func(*Monitor, map[uint64]SegmentRecord) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(strings.Split(logs.String(), "\n"), busyHealth)
	}
	run(t, cfg, health, func(o *Options) {
		o.Logger = logger
		o.BlackDetector = func(context.Context, string) ([]blackdetect.Interval, error) {
			return nil, fmt.Errorf("ffmpeg: exit status 1")
		}
	})

	mu.Lock()
	out := logs.String()
	mu.Unlock()
	for line := range strings.Lines(out) {
		if busyHealth(line) && !strings.Contains(line, "level=WARN") {
			t.Errorf("blackdetect failed on every segment, but the health line looks healthy:\n%s", line)
		}
	}
	b, err := os.ReadFile(filepath.Join(cfg.DataDir, "health.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if header, _, _ := strings.Cut(string(b), "\n"); !strings.Contains(header, "black") || !strings.Contains(header, "error") {
		t.Errorf("health.csv has no column for blackdetect failures: %s", header)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestAuditLinkOrCopyKeepsAPartialCopy: when hard links fail, linkOrCopy
// copies. A copy cut short (disk full, crash) leaves a truncated file at
// dst, and every later call returns nil without replacing it, because an
// existing dst "is left as it is".
func TestAuditLinkOrCopyKeepsAPartialCopy(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "seg_1.ts"), filepath.Join(dir, "incident", "seg_1.ts")
	full := bytes.Repeat([]byte{0x47}, 188*100)
	os.WriteFile(src, full, 0o644)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(dst, full[:188*10], 0o644) // what an interrupted copy leaves

	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, full) {
		t.Errorf("incident copy has %d bytes, the buffer file %d: the truncated copy was kept", len(got), len(full))
	}
}

// TestAuditUnwritableBufferLooksHealthy: when evidence can't be written
// (disk full, a directory left read-only), each write logs an ERROR, but the
// segments are analyzed from memory and counted, the health line stays
// INFO, health.csv shows nothing, and the segment records name files that
// were never written.
func TestAuditUnwritableBufferLooksHealthy(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(100, entry{uri: "s100.ts"}, entry{uri: "s101.ts"}, entry{uri: "s102.ts"}))
	for i, name := range []string{"s100.ts", "s101.ts", "s102.ts"} {
		o.set("/live/"+name, tstest.Load(t, fmt.Sprintf("cont_%d.ts", i)))
	}
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.HealthInterval = 700 * time.Millisecond
	buf := filepath.Join(cfg.DataDir, "buffer", "test")
	os.MkdirAll(buf, 0o755)
	os.Chmod(buf, 0o555)
	t.Cleanup(func() { os.Chmod(buf, 0o755) })

	var mu sync.Mutex
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, w: &logs}, nil))
	busy := func(line string) bool {
		return strings.Contains(line, "msg=health") && !strings.Contains(line, " segments=0 ")
	}
	_, recs := run(t, cfg, func(*Monitor, map[uint64]SegmentRecord) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.ContainsFunc(strings.Split(logs.String(), "\n"), busy)
	}, func(o *Options) { o.Logger = logger })

	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if !strings.Contains(out, "cannot write buffer file") {
		t.Skip("buffer writes did not fail (running as root?)")
	}
	for line := range strings.Lines(out) {
		if busy(line) && !strings.Contains(line, "level=WARN") {
			t.Errorf("no evidence could be written, but the health line looks healthy:\n%s", line)
		}
	}
	for seq, r := range recs {
		if r.File == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(buf, r.File)); err != nil {
			t.Errorf("segment %d's record names %s, which was never written", seq, r.File)
		}
	}
}
