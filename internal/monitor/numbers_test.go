package monitor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/hls"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

func parsed(t *testing.T, body string) *hls.Media {
	t.Helper()
	pl, err := hls.ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n" + body))
	if err != nil {
		t.Fatal(err)
	}
	return pl
}

// An ad break numbered from 1 at the end of a playlist whose content is
// numbered 500 is new segments, not a stale copy of an older playlist: the
// playlist lists the newest segment queued, so the numbers don't matter.
func TestLowerNumbersAfterTheNewestQueuedAreNew(t *testing.T) {
	var p pollState
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	content := "#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:6.006,\nfeed-seq=499.ts\n#EXTINF:6.006,\nfeed-seq=500.ts\n"
	p.update(parsed(t, content), "http://h.test/", "p1", FetchMeta{Status: 200}, at, 3)
	u := p.update(parsed(t, content+"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nhttps://ads.example.com/break-seq=1.ts\n"),
		"http://h.test/", "p2", FetchMeta{Status: 200}, at.Add(3*time.Second), 3)
	if u.stale || u.restart || len(u.faults) > 0 || len(u.queue) != 1 || u.queue[0].seg.URI != "https://ads.example.com/break-seq=1.ts" {
		t.Errorf("update %+v: want the ad queued, nothing stale and no fault", u)
	}
}

// When a window jump's two ends carry numbers from different packagers,
// the numbers between them say nothing: no fault.
func TestWindowJumpNeedsOneNumbering(t *testing.T) {
	var p pollState
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p.update(parsed(t, "#EXT-X-MEDIA-SEQUENCE:10\n#EXTINF:6.006,\nfeed-seq=499.ts\n#EXTINF:6.006,\nhttps://ads.example.com/break-seq=3.ts\n"),
		"http://h.test/", "p1", FetchMeta{Status: 200}, at, 3)
	u := p.update(parsed(t, "#EXT-X-MEDIA-SEQUENCE:12\n#EXTINF:6.006,\nfeed-seq=520.ts\n#EXTINF:6.006,\nfeed-seq=521.ts\n"),
		"http://h.test/", "p2", FetchMeta{Status: 200}, at.Add(time.Second), 3)
	if len(u.faults) > 0 {
		t.Errorf("faults %+v", u.faults)
	}
}

// Two segments listed with one number are two segments: each gets its own
// files and record, and the repeated number raises nothing.
func TestSegmentsSharingANumberKeepTheirOwnFiles(t *testing.T) {
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(700, entry{uri: "a-seq=7.ts"}, entry{uri: "b/a-seq=7.ts"}, discSeg(702)))
	o.set("/live/a-seq=7.ts", tstest.Base.Segment(0).Bytes())
	o.set("/live/b/a-seq=7.ts", tstest.Base.Segment(1).Bytes())
	o.set("/live/s702.ts", tstest.Base.Segment(2).Bytes())
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	run(t, cfg, seen(702))
	buf := filepath.Join(cfg.DataDir, "buffer", "test")
	for _, uri := range []string{"a-seq=7.ts", "b/a-seq=7.ts"} {
		for _, ext := range []string{".ts", ".json"} {
			if _, err := os.Stat(filepath.Join(buf, segmentFile(7, uri)+ext)); err != nil {
				t.Errorf("%s: %v", uri, err)
			}
		}
	}
	rep := onlyIncident(t, cfg)
	var uris []string
	for _, s := range rep.Segments {
		if s.Seq == 7 {
			uris = append(uris, s.URI)
		}
	}
	slices.Sort(uris)
	if !slices.Equal(uris, []string{"a-seq=7.ts", "b/a-seq=7.ts"}) {
		t.Errorf("report.json lists %v as segment 7, want both", uris)
	}
	if !slices.Equal(rep.FaultTypes, []string{analysis.FaultDiscontinuity}) {
		t.Errorf("fault types %v, want only the discontinuity", rep.FaultTypes)
	}
}
