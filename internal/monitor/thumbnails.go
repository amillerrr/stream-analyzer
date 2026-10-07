package monitor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// waitDelay is how long makeStrip waits for ffmpeg's output to close once
// it has exited or been killed.
const waitDelay = 5 * time.Second

// The size of one thumbnail in a black fault's strip.
const thumbWidth, thumbHeight = 160, 90

// ThumbnailStrip is the JSON beside a black fault's strip of thumbnails
// (thumbnails/black_<N>_<hash>.png in its incident): which frame each
// tile is, left to right.
type ThumbnailStrip struct {
	Seq   uint64 `json:"seq"`
	URI   string `json:"uri"`
	Image string `json:"image"`
	Tiles []Tile `json:"tiles"`
	// Continues: the run lasts to the end of the segment, so the frame
	// after it isn't known yet and there is no "after" tile.
	Continues bool `json:"continues,omitzero"`
	// Missing lists the tiles left out because their segment's file was
	// gone from both the incident and the buffer.
	Missing []string `json:"missing,omitempty"`
}

// Tile is one thumbnail: the frame before the run, its first, middle and
// last black frames, or the frame after it.
type Tile struct {
	Role string `json:"role"`
	// File is the segment holding the frame, relative to the incident
	// (segments/...), or its name in the buffer when the incident lacks it.
	File string `json:"file"`
	// PTS is the frame's 33-bit PTS.
	PTS uint64 `json:"pts"`
}

// frameRef is a decoded frame: the segment file holding it, the video PID
// decoded and its PTS as ffmpeg gives it for that file.
type frameRef struct {
	file string
	pid  uint16
	pts  int64
}

// maxTileSamples bounds the black frames a run keeps to pick its middle
// one from; past it, every other one is let go.
const maxTileSamples = 4096

// blackTiles follows where a black run's frames are, across segments, for
// its strip.
type blackTiles struct {
	before, after *frameRef
	first, last   frameRef
	samples       []frameRef // every stride-th black frame
	stride, seen  int
}

func (t *blackTiles) add(f frameRef) {
	if t.stride == 0 {
		t.stride = 1
	}
	if t.seen == 0 {
		t.first = f
	}
	t.last = f
	if t.seen%t.stride == 0 {
		t.samples = append(t.samples, f)
		if len(t.samples) > maxTileSamples {
			kept := t.samples[:0]
			for i, s := range t.samples {
				if i%2 == 0 {
					kept = append(kept, s)
				}
			}
			t.samples, t.stride = kept, 2*t.stride
		}
	}
	t.seen++
}

// clone copies t, so a joined run can grow without changing the original.
func (t *blackTiles) clone() *blackTiles {
	if t == nil {
		return &blackTiles{}
	}
	c := *t
	c.samples = append([]frameRef(nil), t.samples...)
	return &c
}

// tiles lists the strip's frames in order.
func (t *blackTiles) tiles() (roles []string, frames []frameRef) {
	add := func(role string, f *frameRef) {
		if f != nil && f.file != "" {
			roles, frames = append(roles, role), append(frames, *f)
		}
	}
	add("before", t.before)
	add("first", &t.first)
	mid := t.samples[min((t.seen/2)/t.stride, len(t.samples)-1)]
	add("middle", &mid)
	add("last", &t.last)
	add("after", t.after)
	return roles, frames
}

// stripJob is a strip to make for a black fault on segment seq.
type stripJob struct {
	seq   uint64
	uri   string
	name  string // thumbnails/black_<N>_<hash>.png
	tiles *blackTiles
}

// stripName is where a black fault's strip goes in its incident.
func stripName(seq uint64, uri string) string {
	return "thumbnails/black_" + strings.TrimPrefix(segmentFile(seq, uri), "seg_") + ".png"
}

// makeStrip writes a black fault's strip into the incident at dir, holding
// one of the ffmpeg worker slots: one ffmpeg run picks each frame by its
// PTS from the segment that holds it (the incident's copy, or the
// buffer's), scales it to a tile and lays the tiles side by side.
func (c *Channel) makeStrip(ctx context.Context, dir string, job stripJob) error {
	roles, frames := job.tiles.tiles()
	strip := ThumbnailStrip{Seq: job.seq, URI: job.uri, Image: job.name, Continues: job.tiles.after == nil}
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error", "-copyts"}
	var graph []string
	for i, f := range frames {
		path, rel := filepath.Join(dir, kindSegments, f.file), kindSegments+"/"+f.file
		if _, err := os.Stat(path); err != nil {
			path, rel = filepath.Join(c.dir, f.file), f.file
			if _, err := os.Stat(path); err != nil {
				strip.Missing = append(strip.Missing, roles[i])
				continue
			}
		}
		k := len(strip.Tiles)
		strip.Tiles = append(strip.Tiles, Tile{Role: roles[i], File: rel, PTS: wrapPTS(f.pts)})
		args = append(args, "-i", path)
		graph = append(graph, fmt.Sprintf("[%d:i:0x%x]settb=1/90000,select=eq(pts\\,%d),scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1[t%d]",
			k, f.pid, f.pts, thumbWidth, thumbHeight, thumbWidth, thumbHeight, k))
	}
	if len(strip.Tiles) == 0 {
		return errors.New("none of its segments' files is left")
	}
	var inputs strings.Builder
	for k := range strip.Tiles {
		fmt.Fprintf(&inputs, "[t%d]", k)
	}
	if n := len(strip.Tiles); n > 1 {
		graph = append(graph, fmt.Sprintf("%shstack=inputs=%d[out]", inputs.String(), n))
	} else {
		graph = append(graph, "[t0]null[out]")
	}
	out := filepath.Join(dir, job.name)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(out), tmpPrefix+filepath.Base(out))
	args = append(args, "-filter_complex", strings.Join(graph, ";"), "-map", "[out]", "-frames:v", "1", "-update", "1", "-y", tmp)

	select {
	case c.m.ffmpeg <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.m.ffmpeg }()
	bd := c.m.cfg.Blackdetect
	if bd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bd.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, cmp.Or(bd.FFmpeg, "ffmpeg"), args...)
	cmd.WaitDelay = waitDelay
	if b, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(b)))
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	return writeJSON(strings.TrimSuffix(out, ".png")+".json", strip)
}
