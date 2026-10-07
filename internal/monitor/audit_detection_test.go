package monitor

// Detection-logic audit (scratch). Each test logs what the monitor does in a
// constructed case; t.Error marks the behavior the audit considers wrong.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// A continuous black run over four 6 s segments, cut by ffmpeg's segment
// muxer, run through the real Analyze, real ffmpeg blackdetect and
// blackTrigger: at the last segment the run is joined from all four.
func TestAuditDetBlackJoinsAcrossFourSegments(t *testing.T) {
	ff := needFFmpeg(t, "mpeg2video")
	dir := t.TempDir()
	genFFmpeg(t, ff, filepath.Join(dir, "seg_%d.ts"),
		"-f", "lavfi", "-i", "color=c=black:size=160x90:rate=30000/1001:duration=24",
		"-c:v", "mpeg2video", "-g", "30", "-force_key_frames", "expr:gte(t,n_forced*6)",
		"-f", "segment", "-segment_time", "6", "-segment_format", "mpegts")
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Blackdetect.Enabled, cfg.Blackdetect.FFmpeg = true, ff
	m := newTestMonitor(t, cfg, nil)
	ch := m.channels[0]
	var last float64
	for seq := range uint64(4) {
		path := filepath.Join(dir, fmt.Sprintf("seg_%d.ts", seq))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		seg, err := analysis.Analyze(body)
		if err != nil {
			t.Fatal(err)
		}
		o := cfg.Blackdetect.Options
		o.VideoPID = seg.Video.PID
		res, err := blackdetect.Detect(context.Background(), path, o)
		if err != nil {
			t.Fatal(err)
		}
		f, ok := ch.blackTrigger(seq, true, seg, placeBlack(seg, res, 3003)) // consecutive playlist entries
		longest := 0.0
		if ok {
			longest = f.Values["black_frames_s"].(float64)
		}
		t.Logf("seq %d: carried=%v; fault black_frames_s=%.3f %q", seq, ch.black.open, longest, f.Message)
		last = longest
	}
	if last < 23.9 {
		t.Errorf("a continuous 24 s black run over 4 segments was reported with %.3f s of black frames at its last segment", last)
	}
}

// A stall that outlasts max_incident: the incident closes at the cap while
// the stall goes on, and when segments resume nothing records it.
func TestAuditDetStallOutlastingMaxIncidentLosesItsEnd(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.PostRoll, cfg.MergeWindow, cfg.MaxIncident = time.Minute, time.Minute, 10*time.Minute
	cfg.StallTargetDurations = 3
	t0 := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	clock := t0
	m := newTestMonitor(t, cfg, func() time.Time { return clock })
	ch := m.channels[0]
	os.MkdirAll(ch.dir, 0o755)
	ch.mu.Lock()
	ch.target = 6 * time.Second
	ch.mediaURL = "http://127.0.0.1:1/live/hi.m3u8"
	ch.mu.Unlock()
	pl := func(msn uint64, n int) *hls.Media {
		var es []entry
		for i := range n {
			es = append(es, seg(msn+uint64(i)))
		}
		p, err := hls.ParseMedia(media(msn, es...))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var p pollState
	meta := FetchMeta{Status: 200}
	ch.handlePlaylist(&p, pl(100, 3), "http://127.0.0.1:1/live/hi.m3u8", meta)
	clock = t0.Add(20 * time.Second)
	ch.checkStall(&p, meta) // 20 s > 18 s: stall
	for s := 30; s <= 11*60; s += 30 {
		clock = t0.Add(time.Duration(s) * time.Second)
		m.incidents.tick(clock)
		ch.checkStall(&p, meta)
	}
	clock = t0.Add(15 * time.Minute) // the stream resumes after 15 minutes
	ch.handlePlaylist(&p, pl(101, 3), "http://127.0.0.1:1/live/hi.m3u8", meta)
	m.incidents.tick(clock.Add(2 * time.Minute))

	dirs := incidentDirs(t, cfg)
	var ended bool
	for _, d := range dirs {
		r := readReport(t, d)
		t.Logf("%s: status=%s reason=%s types=%v notes=%v", filepath.Base(d), r.Status, r.CloseReason, r.FaultTypes, r.Notes)
		ended = ended || slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "stall ended") })
	}
	if !ended {
		t.Error("a 15-minute stall: no incident records that it ended (or how long it lasted)")
	}
	m.incidents.shutdown()
}

// Between two polls the playlist window jumps from 100-102 to 110-112 (the
// origin never listed 103-109) and the media jumps with it. The monitor
// calls this its own monitor gap; no fault, no incident.
func TestAuditDetOriginSkipIsReportedAsMonitorGap(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8",
		playlistBody(100, -1, seg(100), seg(101), seg(102)),
		playlistBody(110, -1, seg(110), seg(111), seg(112)))
	for k := range 13 {
		o.sequence(fmt.Sprintf("/live/s%d.ts", 100+k), tstest.Base.Segment(k).Bytes())
	}
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(112))
	t.Logf("110: gap=%+v faults=%v", recs[110].Gap, recs[110].Faults)
	if n := len(incidentDirs(t, cfg)); n == 0 {
		t.Errorf("the origin skipped 103-109 (never listed) and no fault or incident was raised; it is logged as our monitor gap %+v", recs[110].Gap)
	}
}

// A later playlist rewrites earlier entries: 100 and 101 get new URIs and
// EXTINF, and 101 gains a CUE-IN tag. Nothing notices.
func TestAuditDetPlaylistRewriteIsUnnoticed(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	rewritten := strings.NewReplacer("s100.ts", "x100.ts", "#EXTINF:0.534,\ns101.ts", "#EXT-X-CUE-IN\n#EXTINF:0.300,\nx101.ts").
		Replace(string(playlistBody(100, -1, seg(100), seg(101), seg(102), seg(103))))
	o.sequence("/live/hi.m3u8", playlistBody(100, -1, seg(100), seg(101), seg(102)), []byte(rewritten))
	o.segments(100, 4)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(103))
	t.Logf("second playlist:\n%s", rewritten)
	t.Logf("records: 100 uri=%s extinf=%v; 101 uri=%s extinf=%v scte35=%v", recs[100].URI, recs[100].ExtInf, recs[101].URI, recs[101].ExtInf, recs[101].SCTE35)
	if len(incidentDirs(t, cfg)) == 0 {
		t.Error("earlier playlist entries were rewritten (URI, EXTINF, a new CUE-IN) and no fault was raised")
	}
}

// After max_incident, black_video is suppressed. A pts_pcr_jump then opens
// incident B; a new black fault while B is open is dropped from B's fault
// list entirely.
func TestAuditDetSuppressedFaultIsDroppedFromAnotherOpenIncident(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.PostRoll, cfg.MergeWindow, cfg.MaxIncident = time.Minute, time.Minute, 5*time.Minute
	clock := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	m := newTestMonitor(t, cfg, func() time.Time { return clock })
	in, ch := m.incidents, m.channels[0]
	start := clock
	for i := range 55 { // black every 6 s for 5.5 min: capped at 5 min
		clock = start.Add(time.Duration(i) * 6 * time.Second)
		in.fault(ch, []analysis.Fault{{Type: analysis.FaultBlackVideo, Seq: uint64(i)}}, SegmentRecord{Seq: uint64(i), URI: "u"})
		in.tick(clock)
	}
	clock = clock.Add(6 * time.Second)
	in.fault(ch, []analysis.Fault{{Type: analysis.FaultPTSPCRJump, Seq: 100}}, SegmentRecord{Seq: 100, URI: "u"})
	clock = clock.Add(20 * time.Second)
	in.fault(ch, []analysis.Fault{{Type: analysis.FaultBlackVideo, Seq: 104, Message: "black run of 30.000 s"}},
		SegmentRecord{Seq: 104, URI: "u", Faults: []string{analysis.FaultBlackVideo}})
	in.shutdown()
	dirs := incidentDirs(t, cfg)
	for _, d := range dirs {
		r := readReport(t, d)
		t.Logf("%s: reason=%s types=%v count=%d", filepath.Base(d), r.CloseReason, r.FaultTypes, r.FaultCount)
	}
	r := readReport(t, dirs[len(dirs)-1])
	if !slices.Contains(r.FaultTypes, analysis.FaultBlackVideo) {
		t.Error("a 30 s black run on segment 104, inside open incident B, is not in B's faults")
	}
}

// Rendition verdicts match by fault type only: the monitored rendition
// jumped 10 s (video_dts_gap), the other rendition's copy has a 50 ms gap:
// "reproduced".
func TestAuditDetReproducedOnADifferentFault(t *testing.T) {
	a0 := mustSeg(t, tstest.Base.Segment(0))
	s1 := tstest.Base.Segment(1)
	for i := range s1.Video {
		s1.Video[i].DTS = ts.Add(s1.Video[i].DTS, 4500) // +50 ms
		s1.Video[i].PTS = ts.Add(s1.Video[i].PTS, 4500)
	}
	a1 := mustSeg(t, s1)
	r := &rendition{segs: map[uint64]*RenditionSegment{}, data: map[uint64]renditionData{}}
	r.want([]uint64{200, 201})
	for seq, s := range map[uint64]*analysis.Segment{200: a0, 201: a1} {
		r.segs[seq].Status = statusFetched
		r.data[seq] = renditionData{seg: s}
	}
	r.recheck(analysis.DefaultThresholds(), nil)
	prev := uint64(200)
	monitored := FaultRecord{Type: analysis.FaultVideoDTSGap, Seq: 201, PrevSeq: &prev,
		Values: map[string]any{"deviation_ms": 10000.0}}
	v, _ := r.checkVerdict(monitored)
	t.Logf("monitored: +10 s DTS jump; rendition: +50 ms gap; verdict=%s (rendition faults %v)", v, r.segs[201].Faults)
	if v == "reproduced" {
		t.Error("a +50 ms gap in the other rendition counts as reproducing a +10 s jump")
	}
}

func mustSeg(t *testing.T, s tstest.Segment) *analysis.Segment {
	t.Helper()
	a, err := analysis.Analyze(s.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A packager restart whose new numbering is within 3*len+10 of the old
// newest is taken for a stale CDN copy: nothing is queued until the new
// numbers pass the old ones, then the first new segment is compared with
// the old timeline's last one.
func TestAuditDetRestartTakenForStaleCopy(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8",
		playlistBody(1000, -1, seg(1000), seg(1001), seg(1002)),
		playlistBody(995, -1, seg(995), seg(996), seg(997)),
		playlistBody(1001, -1, seg(1001), seg(1002), seg(1003)))
	restarted := tstest.Base
	restarted.VideoStart += 50 * 90000 // a new timeline 50 s on
	restarted.AudioStart += 50 * 90000
	for k := range 3 {
		o.sequence(fmt.Sprintf("/live/s%d.ts", 1000+k), tstest.Base.Segment(k).Bytes())
	}
	for k, s := range []uint64{995, 996, 997, 998, 999, 1000, 1001, 1002, 1003} {
		_ = s
		if s >= 1003 {
			o.sequence(fmt.Sprintf("/live/s%d.ts", s), restarted.Segment(k).Bytes())
		}
	}
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(1003))
	for _, d := range incidentDirs(t, cfg) {
		r := readReport(t, d)
		for _, f := range r.Faults {
			t.Logf("fault %s seq %d: %s", f.Type, f.Seq, f.Message)
		}
	}
	t.Logf("1003: gap=%+v faults=%v", recs[1003].Gap, recs[1003].Faults)
}

// One audio_pts_gap 30 s before an incident's max_incident cap is treated
// as "still firing": its post-roll is cut to 30 s and its type suppressed,
// so a second, separate audio_pts_gap 50 s later opens nothing.
func TestAuditDetSingleLateFaultGetsSuppressed(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.PostRoll, cfg.MergeWindow, cfg.MaxIncident = time.Minute, time.Minute, 10*time.Minute
	clock := time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)
	m := newTestMonitor(t, cfg, func() time.Time { return clock })
	in, ch := m.incidents, m.channels[0]
	start := clock
	step := func(d time.Duration) { clock = clock.Add(d); in.tick(clock) }
	// black every 50 s keeps one incident open to the cap
	for i := 0; clock.Sub(start) <= 9*time.Minute; i++ {
		in.fault(ch, []analysis.Fault{{Type: analysis.FaultBlackVideo, Seq: uint64(i)}}, SegmentRecord{Seq: uint64(i), URI: "u"})
		step(30 * time.Second)
	}
	for clock.Sub(start) < 9*time.Minute+30*time.Second {
		step(5 * time.Second)
	}
	in.fault(ch, []analysis.Fault{{Type: analysis.FaultAudioPTSGap, Seq: 500}}, SegmentRecord{Seq: 500, URI: "u"})
	for clock.Sub(start) < 10*time.Minute+20*time.Second {
		step(5 * time.Second)
	}
	in.fault(ch, []analysis.Fault{{Type: analysis.FaultAudioPTSGap, Seq: 510}}, SegmentRecord{Seq: 510, URI: "u"})
	in.shutdown()
	var seen510 bool
	for _, d := range incidentDirs(t, cfg) {
		r := readReport(t, d)
		t.Logf("%s: reason=%s opened=%s closed=%s types=%v", filepath.Base(d), r.CloseReason, r.OpenedAt.Format("15:04:05"), r.ClosedAt.Format("15:04:05"), r.FaultTypes)
		for _, f := range r.Faults {
			seen510 = seen510 || f.Seq == 510
		}
	}
	if !seen510 {
		t.Error("an audio_pts_gap 50 s after a single one near the cap is in no incident (suppressed)")
	}
}

// A stall verdict for a rendition whose playlist could never be fetched:
// N+1 stays "pending" (with a playlist error), and at close that reads as
// "reproduced", i.e. "this rendition stalled too".
func TestAuditDetStallVerdictFromUnfetchablePlaylist(t *testing.T) {
	r := &rendition{segs: map[uint64]*RenditionSegment{}, data: map[uint64]renditionData{}}
	r.want([]uint64{402, 403})
	r.segs[402].Error = "playlist: Get \"http://origin/lo.m3u8\": dial tcp: connection refused"
	r.segs[403].Error = r.segs[402].Error
	v, _ := r.stallVerdict(FaultRecord{Type: analysis.FaultStall, Seq: 402}, time.Time{}, true)
	t.Logf("rendition playlist never fetched; stall verdict at close = %q", v)
	if v == "reproduced" {
		t.Error("a rendition whose playlist was never seen is reported as stalled too")
	}
}
