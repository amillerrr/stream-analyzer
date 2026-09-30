package monitor

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// fetched adds a fetched segment to a rendition, listed after prev, with
// black runs given in seconds from its first frame.
func fetched(t *testing.T, r *rendition, seq uint64, s tstest.Segment, prev *uint64, black ...blackdetect.Interval) {
	t.Helper()
	seg, err := analysis.Analyze(s.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	first := float64(seg.Video.MinPTS()) / ts.Hz
	for i := range black {
		black[i].Start += first
		black[i].End += first
	}
	r.want([]uint64{seq})
	r.segs[seq].Status = statusFetched
	r.data[seq] = renditionData{seg: seg, prev: prev, black: placeBlack(seg, black, tstest.FrameTicks)}
}

func newRendition() *rendition {
	return &rendition{
		variant: hls.Variant{Index: 1, Resolution: "640x360", Bandwidth: 500000},
		segs:    map[uint64]*RenditionSegment{}, data: map[uint64]renditionData{},
	}
}

// A stall is judged by the other rendition's playlist while the monitored
// one was stalled: it moved on (not reproduced), it listed nothing newer
// (reproduced), or it was never fetched then (pending, then inconclusive).
func TestStallVerdictNeedsThePlaylistDuringTheStall(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := FaultRecord{Type: analysis.FaultStall, Seq: 402, DetectedAt: at}
	for _, tc := range []struct {
		name     string
		listings []listing
		ended    time.Time
		closed   bool
		want     string
	}{
		{"never fetched, open", nil, time.Time{}, false, "pending"},
		{"never fetched, closed", nil, time.Time{}, true, "inconclusive"},
		{"fetched only before the stall", []listing{{at.Add(-time.Second), 402}}, time.Time{}, true, "inconclusive"},
		{"moved on", []listing{{at.Add(time.Second), 402}, {at.Add(4 * time.Second), 403}}, time.Time{}, false, "not_reproduced"},
		{"stalled too", []listing{{at.Add(time.Second), 402}}, time.Time{}, false, "reproduced"},
		{"stalled too, both resumed", []listing{{at.Add(time.Second), 402}, {at.Add(time.Minute), 403}}, at.Add(50 * time.Second), true, "reproduced"},
	} {
		r := newRendition()
		r.listings = tc.listings
		got, values := r.stallVerdict(f, tc.ended, tc.closed)
		if got != tc.want {
			t.Errorf("%s: %q, want %q (values %v)", tc.name, got, tc.want, values)
		}
	}
}

// A boundary fault is reproduced when the other rendition shows the same
// fault, at the same boundary, of about the same size. Its own values are
// in the verdict.
func TestBoundaryVerdicts(t *testing.T) {
	small := tstest.Base
	small.VideoStart += 4500 // +50 ms: under the fault size
	small.AudioStart += 4500
	jumped := tstest.Base
	jumped.VideoStart += 54000 // +600 ms
	jumped.AudioStart += 54000
	far := tstest.Base
	far.VideoStart += 900000 // +10 s
	far.AudioStart += 900000
	monitored := func(dev float64) FaultRecord {
		return FaultRecord{Type: analysis.FaultVideoDTSGap, Seq: 201, PrevSeq: new(uint64(200)), Values: map[string]any{"deviation_ms": dev}}
	}
	for _, tc := range []struct {
		name      string
		next      tstest.Segment
		prev      *uint64 // the entry the rendition lists before 201
		f         FaultRecord
		want      string
		valueKey  string
		wantValue float64
	}{
		{"same jump", far.Segment(1), new(uint64(200)), monitored(10000), "reproduced", "deviation_ms", 10000},
		{"a smaller gap", jumped.Segment(1), new(uint64(200)), monitored(10000), "different", "deviation_ms", 600},
		{"a gap under the fault size", small.Segment(1), new(uint64(200)), monitored(10000), "not_reproduced", "deviation_ms", 50},
		{"no gap", tstest.Base.Segment(1), new(uint64(200)), monitored(10000), "not_reproduced", "deviation_ms", 0},
		{"another boundary", far.Segment(1), new(uint64(199)), monitored(10000), "inconclusive", "", 0},
	} {
		r := newRendition()
		fetched(t, r, 200, tstest.Base.Segment(0), nil)
		fetched(t, r, 201, tc.next, tc.prev)
		r.recheck(analysis.DefaultThresholds(), nil)
		got, values := r.checkVerdict(tc.f)
		if got != tc.want {
			t.Errorf("%s: %q, want %q (values %v)", tc.name, got, tc.want, values)
			continue
		}
		if tc.valueKey != "" {
			if v, ok := values[tc.valueKey].(float64); !ok || v < tc.wantValue-0.01 || v > tc.wantValue+0.01 {
				t.Errorf("%s: rendition values %v, want %s %.3f", tc.name, values, tc.valueKey, tc.wantValue)
			}
		}
	}
}

// A black_video fault is reproduced by a black run at least trigger_min
// long that overlaps the monitored run, joined across the rendition's
// segments; a shorter overlapping run is "different", black elsewhere is
// not the same fault.
func TestBlackVerdicts(t *testing.T) {
	seg1, err := analysis.Analyze(tstest.Base.Segment(1).Bytes())
	if err != nil {
		t.Fatal(err)
	}
	start := seg1.Video.MinPTS() // the monitored run covers all of segment 201
	f := FaultRecord{Type: analysis.FaultBlackVideo, Seq: 201, Values: map[string]any{
		"run_start_pts": start, "run_end_pts": ts.Add(start, 48048),
	}}
	// ffmpeg ends a run that lasts to the end at the last frame's PTS.
	whole := blackdetect.Interval{Start: 0, End: 0.5005, Duration: 0.5005}
	for _, tc := range []struct {
		name         string
		b200, b201   []blackdetect.Interval
		want         string
		wantLongestS float64
	}{
		{"joined run long enough", []blackdetect.Interval{{Start: 0.2, End: 0.5005, Duration: 0.3005}}, []blackdetect.Interval{whole}, "reproduced", 0.868},
		{"overlapping but short", nil, []blackdetect.Interval{{Start: 0.1, End: 0.4, Duration: 0.3}}, "different", 0.3},
		{"black elsewhere", []blackdetect.Interval{{Start: 0, End: 0.3, Duration: 0.3}}, nil, "not_reproduced", 0},
	} {
		r := newRendition()
		r.blackMin = 0.5
		fetched(t, r, 200, tstest.Base.Segment(0), nil, tc.b200...)
		fetched(t, r, 201, tstest.Base.Segment(1), new(uint64(200)), tc.b201...)
		r.recheck(analysis.DefaultThresholds(), nil)
		got, values := r.checkVerdict(f)
		if got != tc.want {
			t.Errorf("%s: %q, want %q (values %v)", tc.name, got, tc.want, values)
			continue
		}
		if l, _ := values["longest_run_s"].(float64); tc.wantLongestS > 0 && (l < tc.wantLongestS-0.01 || l > tc.wantLongestS+0.01) {
			t.Errorf("%s: rendition values %v, want longest_run_s %.3f", tc.name, values, tc.wantLongestS)
		}
	}
}

// The monitored rendition stalls while the other keeps listing new
// segments: the stall is not reproduced there.
func TestStallNotReproducedWhenTheOtherRenditionMovesOn(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(twoVariantMaster))
	o.sequence("/live/hi.m3u8", playlistBody(400, -1, seg(400), seg(401), seg(402)))
	o.route("/live/lo.m3u8", func(n int, w http.ResponseWriter) {
		var es []entry
		for k := range 3 {
			es = append(es, entry{uri: fmt.Sprintf("lo%d.ts", 400+n+k)})
		}
		w.Write(media(uint64(400+n), es...))
	})
	o.segments(400, 3)
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	cfg.StallTargetDurations = 3
	cfg.MaxIncident = 5 * time.Second

	run(t, cfg, func(*Monitor, map[uint64]SegmentRecord) bool { return anyClosed(t, cfg.DataDir) })
	r := onlyIncident(t, cfg)
	f := r.Faults[0]
	if f.Type != analysis.FaultStall {
		t.Fatalf("first fault %s, want stall", f.Type)
	}
	if v := f.Renditions["1_640x360_500000"]; v != "not_reproduced" {
		t.Errorf("stall verdict %q (values %v), want not_reproduced", v, f.RenditionValues)
	}
	if n, ok := f.RenditionValues["1_640x360_500000"]["newest_seq"].(float64); !ok || n <= 402 {
		t.Errorf("rendition values %v, want the newer number it listed", f.RenditionValues)
	}
}
