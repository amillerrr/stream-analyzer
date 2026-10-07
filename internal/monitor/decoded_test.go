package monitor

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// noReorder is a synthetic segment whose frames are presented in decode
// order (PTS = DTS + 2 frames): frame i is in slot i.
func noReorder(t *testing.T) *analysis.Segment {
	t.Helper()
	s := tstest.Base.Segment(0)
	for i := range s.Video {
		s.Video[i].PTS = ts.Add(s.Video[i].DTS, 2*tstest.FrameTicks)
	}
	seg, err := analysis.Analyze(s.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

// decoded is what ffmpeg would report for seg when it decodes the frames
// in slots dec (in order), the ones in black black.
func decoded(seg *analysis.Segment, dec, black func(i int) bool) blackdetect.Result {
	var res blackdetect.Result
	for i := range seg.Video.AUs {
		if dec(i) {
			res.Frames = append(res.Frames, blackdetect.Frame{
				PTS:   int64(ts.Add(seg.Video.MinPTS(), int64(i)*tstest.FrameTicks)),
				Black: black(i),
			})
		}
	}
	return res
}

func between(lo, hi int) func(int) bool { return func(i int) bool { return i >= lo && i < hi } }

// Only frames ffmpeg decoded count as black. Frames the parser found but
// ffmpeg did not decode end a black run: they are not black pictures.
func TestBlackRunHoldsOnlyDecodedFrames(t *testing.T) {
	seg := noReorder(t)
	n := len(seg.Video.AUs)
	// Black in slots 0-4, slots 5-9 not decoded, then the picture.
	res := decoded(seg, func(i int) bool { return i < 5 || i >= 10 }, between(0, 5))
	runs := placeBlack(seg, res, tstest.FrameTicks)
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	r := runs[0]
	want := float64(5*tstest.FrameTicks) / ts.Hz
	if r.Frames != 5 || r.UndecodedFrames != 0 || math.Abs(r.BlackS-want) > 1e-6 || math.Abs(r.Duration-want) > 1e-6 || r.ToEnd {
		t.Errorf("run %+v: want 5 black frames, %.6f s, ending at slot 5", r, want)
	}
	d := decodeSummary(seg, res, tstest.FrameTicks)
	if d.Frames != n || d.Decoded != n-5 || d.Undecoded != 5 || d.LeadingDropped != 0 {
		t.Errorf("decode %+v: want 5 of %d frames not decoded", d, n)
	}
}

// Black frames ffmpeg output one after another are one run even with
// frames it could not decode between them; those are counted apart, and
// only the black frames make the run's black time.
func TestBlackRunCountsUndecodedFramesInsideItApart(t *testing.T) {
	seg := noReorder(t)
	res := decoded(seg, func(i int) bool { return i < 4 || i >= 7 }, between(0, 10))
	runs := placeBlack(seg, res, tstest.FrameTicks)
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	r := runs[0]
	if r.Frames != 7 || r.UndecodedFrames != 3 || r.NoFrameS != 0 ||
		math.Abs(r.BlackS-float64(7*tstest.FrameTicks)/ts.Hz) > 1e-6 || math.Abs(r.Duration-float64(10*tstest.FrameTicks)/ts.Hz) > 1e-6 {
		t.Errorf("run %+v: want 7 black frames and 3 undecoded over 10 slots", r)
	}
}

// Frames dropped before the first one ffmpeg could decode, when a segment
// is decoded on its own (open GOP, or no IDR at its start), are expected:
// counted apart from frames lost after it, which raise undecoded_frames.
func TestUndecodedFramesAfterTheFirstDecodedOneAreAFault(t *testing.T) {
	ff := needFFmpeg(t, "libx264")
	dir := t.TempDir()
	src := genFFmpeg(t, ff, filepath.Join(dir, "src.ts"),
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30000/1001:d=6",
		"-c:v", "libx264", "-bf", "0", "-x264-params", "keyint=60:min-keyint=60:scenecut=0", "-f", "mpegts")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		body               []byte
		leading, undecoded int // undecoded: at least
	}{
		// 15 frames before the IDR at 60 can't be decoded on their own.
		{"cut mid-GOP", cutTS(t, b, 0x100, 45, 90), 15, 0},
		// Frames 70-79 lost after the picture started (and the frames that
		// refer to them).
		{"frames lost in the middle", breakSlices(cutTS(t, b, 0x100, 60, 60), 0x100, between(10, 20)), 0, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "seg.ts")
			if err := os.WriteFile(path, tc.body, 0o644); err != nil {
				t.Fatal(err)
			}
			ch := blackChannel(t, ff)
			rec, faults := checkAll(t, ch, 1, path)
			d := rec.BlackDecode
			if d == nil || !strings.HasPrefix(d.Decoder, "h264 (") || d.LeadingDropped != tc.leading || d.Undecoded < tc.undecoded || tc.undecoded == 0 && d.Undecoded != 0 {
				t.Errorf("decode %+v: want %d dropped before the first decoded frame, %d or more after", d, tc.leading, tc.undecoded)
			}
			got := slices.ContainsFunc(faults, func(f analysis.Fault) bool { return f.Type == analysis.FaultUndecodedFrames })
			if got != (tc.undecoded > 0) {
				t.Errorf("faults %v: undecoded_frames %v, want %v", faults, got, tc.undecoded > 0)
			}
		})
	}
}

// checkAll runs the monitor's segment check on the file at path as
// segment seq, after no other, and returns the record and every fault.
func checkAll(t *testing.T, ch *Channel, seq uint64, path string) (SegmentRecord, []analysis.Fault) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := SegmentRecord{Seq: seq, File: filepath.Base(path)}
	if err := os.WriteFile(filepath.Join(ch.dir, rec.File), body, 0o644); err != nil {
		t.Fatal(err)
	}
	faults, ok := ch.check(t.Context(), &rec, body, analysis.Order{})
	if !ok {
		t.Fatalf("segment %d is not usable TS: %v", seq, faults)
	}
	return rec, faults
}

// Another rendition is judged by the same rule: a run there is only as
// long as its decoded black frames, and frames it couldn't decode after
// the first are its own undecoded_frames fault.
func TestRenditionBlackCountsOnlyDecodedFrames(t *testing.T) {
	seg := noReorder(t)
	start := seg.Video.MinPTS()
	f := FaultRecord{Type: analysis.FaultBlackVideo, Seq: 201, Values: map[string]any{
		"run_start_pts": start, "run_end_pts": ts.Add(start, 15*tstest.FrameTicks),
	}}
	// Slots 0-14 on screen, slots 5-9 not decoded: 10 black frames.
	res := decoded(seg, func(i int) bool { return i < 5 || i >= 10 }, between(0, 15))
	r := newRendition()
	r.blackMin = 0.4 // 15 slots are 0.5 s, 10 frames 0.334 s
	r.want([]uint64{201})
	r.segs[201].Status = statusFetched
	r.data[201] = renditionData{seg: seg, black: placeBlack(seg, res, tstest.FrameTicks), decode: decodeSummary(seg, res, tstest.FrameTicks)}
	r.recheck(analysis.DefaultThresholds(), nil)
	if got, values := r.checkVerdict(f); got != "different" || values["black_frames"] != 10 {
		t.Errorf("verdict %q (values %v), want different with 10 black frames", got, values)
	}
	if !slices.Contains(r.segs[201].Faults, analysis.FaultUndecodedFrames) || slices.Contains(r.segs[201].Faults, analysis.FaultBlackVideo) {
		t.Errorf("rendition faults %v: want undecoded_frames and no black_video", r.segs[201].Faults)
	}
}

// corruptSlices garbles the second half of the last packet of every video
// PES on pid whose index (in file order) sel selects: the slice headers
// stay intact, so a decoder logs errors, conceals them and still outputs
// the frame.
func corruptSlices(b []byte, pid uint16, sel func(i int) bool) []byte {
	out := slices.Clone(b)
	var last []int // offset of the last packet of each video PES
	for off := 0; off+ts.PacketSize <= len(out); off += ts.PacketSize {
		if pl, p, pusi := tsPayload(out[off : off+ts.PacketSize]); p == pid && pl != nil {
			if pusi {
				last = append(last, off)
			} else if len(last) > 0 {
				last[len(last)-1] = off
			}
		}
	}
	for i, off := range last {
		if !sel(i) {
			continue
		}
		pl, _, _ := tsPayload(out[off : off+ts.PacketSize])
		for k := len(pl) / 2; k < len(pl); k++ {
			pl[k] ^= 0x5a
		}
	}
	return out
}

// A segment whose black check didn't run, for want of a video stream or of
// a saved file, is not checked; with the black check turned off it is.
func TestSkippedBlackCheckIsNotChecked(t *testing.T) {
	video := tstest.Base.Segment(0)
	audio := video
	audio.Video = nil
	for _, tc := range []struct {
		name    string
		seg     tstest.Segment
		saved   bool
		enabled bool
		want    int
	}{
		{"no video stream", audio, true, true, 1},
		{"file not saved", video, false, true, 1},
		{"black check off", audio, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
			cfg.Blackdetect.Enabled = tc.enabled
			ch := newTestMonitor(t, cfg, nil).channels[0]
			if err := os.MkdirAll(ch.dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := tc.seg.Bytes()
			rec := SegmentRecord{Seq: 1}
			if tc.saved {
				rec.File = "seg_1.ts"
				if err := os.WriteFile(filepath.Join(ch.dir, rec.File), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, ok := ch.check(t.Context(), &rec, body, analysis.Order{}); !ok {
				t.Fatal("not usable TS")
			}
			if ch.st.blackNotChecked != tc.want {
				t.Errorf("black_not_checked = %d, want %d (record %+v)", ch.st.blackNotChecked, tc.want, rec.BlackDecode)
			}
		})
	}
}

// Frames dropped before the first decodable one are counted in the health
// line and leave it INFO; a segment not checked makes it a warning.
func TestHealthCountsDroppedLeadingFramesAndUncheckedSegments(t *testing.T) {
	for _, col := range []string{"leading_frames_dropped", "black_not_checked"} {
		if !slices.Contains(healthColumns, col) {
			t.Errorf("health.csv has no %s column", col)
		}
	}
	for _, tc := range []struct {
		name string
		st   stats
		warn bool
	}{
		{"leading frames dropped", stats{segments: 10, leadingDropped: 15}, false},
		{"not checked", stats{segments: 10, blackNotChecked: 1}, true},
	} {
		if got := tc.st.warns(false, false, false); got != tc.warn {
			t.Errorf("%s: warning %v, want %v", tc.name, got, tc.warn)
		}
	}
}

// In another rendition, a segment not fully checked can still reproduce a
// black fault (unconfirmed), but its lack of black proves nothing.
func TestRenditionBlackVerdictOnAnUncheckedSegment(t *testing.T) {
	seg := noReorder(t)
	start := seg.Video.MinPTS()
	f := FaultRecord{Type: analysis.FaultBlackVideo, Seq: 201, Values: map[string]any{
		"run_start_pts": start, "run_end_pts": ts.Add(start, 15*tstest.FrameTicks),
	}}
	for _, tc := range []struct {
		name        string
		black       func(int) bool
		want        string
		unconfirmed bool
	}{
		{"black there", between(0, 15), "reproduced", true},
		{"no black there", between(0, 0), "inconclusive", false},
	} {
		res := decoded(seg, func(int) bool { return true }, tc.black)
		runs := placeBlack(seg, res, tstest.FrameTicks)
		for i := range runs {
			runs[i].Unconfirmed = true
		}
		r := newRendition()
		r.blackMin = 0.4
		r.want([]uint64{201})
		r.segs[201].Status = statusFetched
		r.data[201] = renditionData{seg: seg, black: runs, notChecked: "ffmpeg logged 3 decode errors after the first frame it decoded"}
		r.recheck(analysis.DefaultThresholds(), nil)
		got, values := r.checkVerdict(f)
		if got != tc.want || (values["unconfirmed"] == true) != tc.unconfirmed {
			t.Errorf("%s: verdict %q (values %v), want %q, unconfirmed %v", tc.name, got, values, tc.want, tc.unconfirmed)
		}
	}
}

// ffmpeg output the monitor can't read makes the segment not checked, with
// no black from it: not an ffmpeg failure, and nothing guessed.
func TestUnreadableBlackdetectOutputIsNotChecked(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Blackdetect.Enabled = true
	m, err := New(Options{Config: cfg, Logger: testLogger(t), BlackDetector: func(context.Context, string) ([]blackdetect.Interval, error) {
		return nil, &blackdetect.FormatError{Line: "[blackdetect @ 0x1] [info] black_start:NOPTS", Why: "not a time"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ch := m.channels[0]
	if err := os.MkdirAll(ch.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ch.dir, "seg_1.ts")
	if err := os.WriteFile(path, tstest.Base.Segment(0).Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, faults := checkAll(t, ch, 1, path)
	if ch.st.blackNotChecked != 1 || ch.st.blackErrors != 0 || len(rec.Black) != 0 || len(faults) != 0 {
		t.Errorf("not checked %d, blackdetect errors %d, black %+v, faults %v; want one segment not checked and nothing else",
			ch.st.blackNotChecked, ch.st.blackErrors, rec.Black, faults)
	}
	if d := rec.BlackDecode; d == nil || !strings.Contains(d.NotChecked, "black_start:NOPTS") {
		t.Errorf("black_decode %+v does not say why", d)
	}
}

// Every report.json says how black was checked: the ffmpeg build, the
// decoder each segment went through, and every black setting, the fixed
// ones too.
func TestReportRecordsHowBlackWasChecked(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(700, seg(700), discSeg(701)))
	for k := range 2 {
		o.set(fmt.Sprintf("/live/s%d.ts", 700+k), tstest.Base.Segment(k).Bytes())
	}
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled = true
	build := blackdetect.Build{Path: "/opt/ffmpeg/bin/ffmpeg", Version: "7.1.5", H264: "h264", HEVC: "hevc", SelfTestDecoder: "h264 (native)"}
	run(t, cfg, seen(701), fakeBlack(nil), func(o *Options) { o.FFmpeg = build })
	b := onlyIncident(t, cfg).Blackdetect
	if b == nil {
		t.Fatal("report.json has no blackdetect")
	}
	bd := cfg.Blackdetect
	if !b.Enabled || b.FFmpeg != build || b.D != bd.Duration || b.PixTh != bd.PixelThreshold || b.PicTh != bd.PictureThreshold ||
		b.TriggerMin != bd.TriggerMin || b.FFmpegD != 0 || b.MaxJoinGapS != 0.5 || b.EdgeToleranceFrames != 0.5 ||
		b.FallbackFrameTicks != 3003 || !strings.Contains(b.Filter, "blackdetect=d=0:pix_th=0.1:pic_th=0.98") {
		t.Errorf("blackdetect %+v", *b)
	}
}

// A report lists the decoders its segments went through.
func TestReportListsTheDecodersUsed(t *testing.T) {
	r := Report{Blackdetect: &BlackdetectInfo{}, Segments: []SegmentRecord{
		{Seq: 1, BlackDecode: &BlackDecode{Decoder: "h264 (native)"}},
		{Seq: 2, BlackDecode: &BlackDecode{Decoder: "h264 (native)"}},
		{Seq: 3},
		{Seq: 4, BlackDecode: &BlackDecode{Decoder: "hevc (native)"}},
	}}
	r.derive()
	if got := r.Blackdetect.DecodersUsed; !slices.Equal(got, []string{"h264 (native)", "hevc (native)"}) {
		t.Errorf("decoders used %v", got)
	}
}

// A channel's color_range decides how its luma is read; every black run
// records the pixel format and range it was judged in.
func TestChannelColorRangeOverride(t *testing.T) {
	ff := needFFmpeg(t, "libx264")
	// 1.5 s of full-range luma 30: not black as signaled, black when read
	// as limited range.
	path := genFFmpeg(t, ff, filepath.Join(t.TempDir(), "dark.ts"),
		"-f", "lavfi", "-i", "color=c=black:s=160x90:r=30000/1001:d=1.5,format=yuv420p,geq=lum=30:cb=128:cr=128,setparams=range=pc",
		"-c:v", "libx264", "-qp", "10", "-color_range", "pc", "-f", "mpegts")
	for _, tc := range []struct {
		rng, want string
		fault     bool
	}{
		{"", "pc", false},
		{"limited", "tv", true},
	} {
		ch := blackChannel(t, ff, func(c *config.Config) { c.Channels[0].ColorRange = tc.rng })
		rec, f := checkSegment(t, ch, 1, path, false)
		if d := rec.BlackDecode; d == nil || d.ColorRange != tc.want || d.PixFmt == "" {
			t.Errorf("color_range %q: black_decode %+v, want range %q", tc.rng, d, tc.want)
		}
		if (f != nil) != tc.fault {
			t.Errorf("color_range %q: fault %v, want %v", tc.rng, f, tc.fault)
		}
		if f != nil && (f.Values["color_range"] != "tv" || f.Values["pix_fmt"] == "" || rec.Black[0].ColorRange != "tv") {
			t.Errorf("color_range %q: fault values %v, run %+v", tc.rng, f.Values, rec.Black[0])
		}
	}
}

// Frames are missing only when ffmpeg output fewer than the parser found.
// A stream whose frames share or jitter their PTS gets some re-timed by
// ffmpeg; every frame was still decoded.
func TestRetimedFramesAreNotUndecoded(t *testing.T) {
	seg := noReorder(t)
	res := decoded(seg, func(int) bool { return true }, func(int) bool { return false })
	res.Frames[7].PTS += 2 * tstest.FrameTicks / 3 // ffmpeg's own timestamp for it
	d := decodeSummary(seg, res, tstest.FrameTicks)
	if d.Undecoded != 0 || d.LeadingDropped != 0 || d.NotChecked != "" {
		t.Errorf("decode %+v: every frame was decoded", d)
	}
}
