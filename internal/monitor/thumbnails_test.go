package monitor

import (
	json "encoding/json/v2"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Every black fault leaves a strip of thumbnails in its incident: the frame
// before the run, its first, middle and last black frames, and the frame
// after it, each from the segment that holds it, with a JSON beside it
// saying which frame each tile is.
func TestBlackFaultLeavesAThumbnailStrip(t *testing.T) {
	ff := needFFmpeg(t, "mpeg2video")
	dir := t.TempDir()
	// 25 fps, 2 s segments: black from 1.52 s (in the first) to 2.68 s (in
	// the second), 30 frames: 1.2 s.
	genFFmpeg(t, ff, filepath.Join(dir, "s%d.ts"),
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=25:duration=4,drawbox=color=black:t=fill:enable='between(t,1.5,2.69)',format=yuv420p",
		"-c:v", "mpeg2video", "-g", "25", "-force_key_frames", "expr:gte(t,n_forced*2)",
		"-f", "segment", "-segment_time", "2", "-segment_format", "mpegts", "-segment_start_number", "700")
	o, srv := newOrigin(t)
	o.set("/live/test.m3u8", media(700, seg(700), seg(701)))
	for _, n := range []string{"700", "701"} {
		b, err := os.ReadFile(filepath.Join(dir, "s"+n+".ts"))
		if err != nil {
			t.Fatal(err)
		}
		o.set("/live/s"+n+".ts", b)
	}
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	cfg.Blackdetect.Enabled, cfg.Blackdetect.FFmpeg = true, ff
	run(t, cfg, seen(701))

	dirs := incidentDirs(t, cfg)
	if len(dirs) != 1 {
		t.Fatalf("incidents %v", dirs)
	}
	base := filepath.Join(dirs[0], "thumbnails", "black_"+segmentFile(701, "s701.ts")[len("seg_"):])
	f, err := os.Open(base + ".png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if img.Width != 5*thumbWidth || img.Height != thumbHeight {
		t.Errorf("strip %dx%d, want five %dx%d tiles", img.Width, img.Height, thumbWidth, thumbHeight)
	}
	b, err := os.ReadFile(base + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var strip ThumbnailStrip
	if err := json.Unmarshal(b, &strip); err != nil {
		t.Fatal(err)
	}
	var roles, files []string
	for _, tile := range strip.Tiles {
		roles = append(roles, tile.Role)
		files = append(files, tile.File)
	}
	first, second := "segments/"+segmentFile(700, "s700.ts")+".ts", "segments/"+segmentFile(701, "s701.ts")+".ts"
	if !slices.Equal(roles, []string{"before", "first", "middle", "last", "after"}) ||
		!slices.Equal(files, []string{first, first, second, second, second}) {
		t.Errorf("tiles %+v", strip.Tiles)
	}
	rep := onlyIncident(t, cfg)
	i := slices.IndexFunc(rep.Faults, func(f FaultRecord) bool { return f.Type == "black_video" })
	if i < 0 || rep.Faults[i].Values["thumbnails"] != "thumbnails/"+filepath.Base(base)+".png" {
		t.Errorf("the black fault doesn't name its strip: %+v", rep.Faults)
	}
}
