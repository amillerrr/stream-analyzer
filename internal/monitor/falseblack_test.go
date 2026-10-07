package monitor

// Tests from the false black-video investigation. Each builds its streams
// at test time from ffmpeg's lavfi sources and skips when ffmpeg or the
// encoder it needs is missing. Several fail on purpose: they show a way the
// monitor can report black, or misplace it, when the picture isn't black.

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/config"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// needFFmpeg returns ffmpeg's path and skips the test unless every named
// encoder is built in.
func needFFmpeg(t *testing.T, encoders ...string) string {
	t.Helper()
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	out, err := exec.CommandContext(t.Context(), ff, "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Skipf("ffmpeg -encoders: %v", err)
	}
	for _, e := range encoders {
		if !strings.Contains(string(out), " "+e+" ") {
			t.Skipf("ffmpeg has no %s encoder", e)
		}
	}
	return ff
}

// genFFmpeg runs ffmpeg with args, writing out; it skips the test when
// ffmpeg can't make the clip.
func genFFmpeg(t *testing.T, ff, out string, args ...string) string {
	t.Helper()
	args = append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), out)
	if b, err := exec.CommandContext(t.Context(), ff, args...).CombinedOutput(); err != nil {
		t.Skipf("cannot generate %s: %v\n%s", filepath.Base(out), err, b)
	}
	return out
}

// blackChannel is a channel with blackdetect on, using ff.
func blackChannel(t *testing.T, ff string, edit ...func(*config.Config)) *Channel {
	t.Helper()
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	cfg.Blackdetect.Enabled = true
	cfg.Blackdetect.FFmpeg = ff
	for _, e := range edit {
		e(&cfg)
	}
	ch := newTestMonitor(t, cfg, nil).channels[0]
	if err := os.MkdirAll(ch.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return ch
}

// checkSegment runs the monitor's own segment check (analysis, blackdetect
// and blackTrigger) on the file at path as segment seq, and returns the
// record and the black_video fault, if any.
func checkSegment(t *testing.T, ch *Channel, seq uint64, path string, adjacent bool) (SegmentRecord, *analysis.Fault) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := SegmentRecord{Seq: seq, File: fmt.Sprintf("seg_%d.ts", seq)}
	if err := os.WriteFile(filepath.Join(ch.dir, rec.File), body, 0o644); err != nil {
		t.Fatal(err)
	}
	faults, ok := ch.check(t.Context(), &rec, body, analysis.Order{Adjacent: adjacent})
	if !ok {
		t.Fatalf("segment %d is not usable TS: %v", seq, faults)
	}
	for _, f := range faults {
		if f.Type == analysis.FaultBlackVideo {
			return rec, &f
		}
	}
	return rec, nil
}

// tsPayload returns the payload of a 188-byte packet and whether it starts
// a PES or section.
func tsPayload(p []byte) (payload []byte, pid uint16, pusi bool) {
	pid = uint16(p[1]&0x1f)<<8 | uint16(p[2])
	start := 4
	if p[3]&0x20 != 0 {
		start += 1 + int(p[4])
	}
	if p[3]&0x10 == 0 || start >= ts.PacketSize {
		return nil, pid, false
	}
	return p[start:], pid, p[1]&0x40 != 0
}

// breakSlices rewrites every H.264 slice in the PES on pid whose index (in
// file order) sel selects, so its header names a PPS the stream doesn't
// have (pic_parameter_set_id 5): a decoder drops the slice ("non-existing
// PPS 5 referenced") and outputs no frame, while the TS parser still sees
// each access unit and its PTS. It stands for frames a decoder fails on,
// as when it meets a feature it doesn't support.
func breakSlices(b []byte, pid uint16, sel func(i int) bool) []byte {
	out := slices.Clone(b)
	var pes [][][]byte // each PES's payload, as the packet slices holding it
	for off := 0; off+ts.PacketSize <= len(out); off += ts.PacketSize {
		pl, p, pusi := tsPayload(out[off : off+ts.PacketSize])
		switch {
		case p != pid || pl == nil:
		case pusi:
			pes = append(pes, [][]byte{pl[9+int(pl[8]):]})
		case len(pes) > 0:
			pes[len(pes)-1] = append(pes[len(pes)-1], pl)
		}
	}
	for i, parts := range pes {
		if !sel(i) {
			continue
		}
		buf := slices.Concat(parts...)
		for k := 0; k+3 < len(buf); k++ {
			if buf[k] != 0 || buf[k+1] != 0 || buf[k+2] != 1 {
				continue
			}
			nal := k + 3
			end := len(buf)
			for j := nal; j+2 < len(buf); j++ {
				if buf[j] == 0 && buf[j+1] == 0 && buf[j+2] == 1 {
					end = j
					if buf[j-1] == 0 {
						end--
					}
					break
				}
			}
			if typ := buf[nal] & 0x1f; (typ == 1 || typ == 5) && end-nal >= 3 {
				// first_mb_in_slice 0, slice_type P, pic_parameter_set_id 5.
				buf[nal+1] = 0xcd
				for j := nal + 2; j < end-1; j++ {
					buf[j] = 0xff
				}
				buf[end-1] = 0x80
			}
			k = end - 1
		}
		for _, part := range parts {
			buf = buf[copy(part, buf):]
		}
	}
	return out
}

// cutTS cuts b the way a segmenter that ignores GOPs would: the file's
// first PAT and PMT, then every packet from the start of video PES from
// (in file order) up to video PES from+n.
func cutTS(t *testing.T, b []byte, pid uint16, from, n int) []byte {
	t.Helper()
	var psi []byte
	pmt := -1
	var starts []int
	for off := 0; off+ts.PacketSize <= len(b); off += ts.PacketSize {
		pkt := b[off : off+ts.PacketSize]
		pl, p, pusi := tsPayload(pkt)
		switch {
		case !pusi:
		case p == 0 && pmt < 0:
			s := pl[1+int(pl[0]):]
			pmt = int(s[10]&0x1f)<<8 | int(s[11])
			psi = append(psi, pkt...)
		case int(p) == pmt && len(psi) == ts.PacketSize:
			psi = append(psi, pkt...)
		case p == pid:
			starts = append(starts, off)
		}
	}
	if from+n > len(starts) || len(psi) != 2*ts.PacketSize {
		t.Fatalf("cannot cut video PES %d-%d from %d", from, from+n-1, len(starts))
	}
	end := len(b)
	if from+n < len(starts) {
		end = starts[from+n]
	}
	return append(psi, b[starts[from]:end]...)
}

// 1. Stream selection. The TS parser analyzes the first program's video
// stream with the lowest PID. ffmpeg, run with no -map, decodes the video
// stream it rates best in the whole file (the largest picture). When a
// segment carries two, blackdetect judges a picture the monitor never
// analyzed, and its times are placed on the other stream's PTS.
func TestFalseBlackFFmpegDecodesAnotherVideoStream(t *testing.T) {
	ff := needFFmpeg(t, "mpeg2video")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"second video PID", nil},
		{"second program", []string{"-program", "title=main:st=0", "-program", "title=other:st=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// PID 0x100: the picture, 640x360. PID 0x101: black, 1280x720.
			args := []string{
				"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30000/1001:d=3",
				"-f", "lavfi", "-i", "color=black:size=1280x720:rate=30000/1001:d=3",
				"-map", "0", "-map", "1", "-c:v", "mpeg2video", "-g", "15",
			}
			path := genFFmpeg(t, ff, filepath.Join(t.TempDir(), "two.ts"), append(append(args, tc.args...), "-f", "mpegts")...)
			ch := blackChannel(t, ff)
			rec, f := checkSegment(t, ch, 1, path, false)
			if rec.Analysis == nil || rec.Analysis.Video == nil {
				t.Fatal("no video analyzed")
			}
			if len(rec.Black) > 0 || f != nil {
				t.Errorf("the TS parser analyzed video PID 0x%x (testsrc2, not black), but ffmpeg decoded the larger black stream: black %+v, fault %v",
					rec.Analysis.Video.PID, rec.Black, f)
			}
		})
	}
}

// 2. Time mapping near the 33-bit PTS wrap. ffmpeg -copyts keeps the
// stream's PTS, but libavformat unwraps them from the program's first
// timestamp: when that is in the last 60 s before the wrap, every time
// before the wrap comes out negative (black_start:-55.8). streamPTS clamps
// negative seconds to 0, so the run is misplaced. Audio starting before the
// video is the control: with -copyts it changes nothing.
func TestFalseBlackRunPlacedNearPTSWrap(t *testing.T) {
	ff := needFFmpeg(t, "mpeg2video", "mp2")
	for _, tc := range []struct {
		name   string
		offset string // -output_ts_offset, seconds
	}{
		{"control, audio 1.5 s before video", "0"},
		{"control, video after the wrap, audio before it", "95441.5"},
		{"first timestamp 52 s before the wrap", "95390"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Video black from frame 30 (1.001 s) to frame 75 (2.5025 s).
			path := genFFmpeg(t, ff, filepath.Join(t.TempDir(), "wrap.ts"),
				"-f", "lavfi", "-i", "sine=f=440:d=5.5",
				"-itsoffset", "1.5", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30000/1001:d=4,drawbox=color=black:t=fill:enable='between(t,1.0,2.49)'",
				"-map", "1:v", "-map", "0:a", "-c:v", "mpeg2video", "-g", "15", "-c:a", "mp2",
				"-output_ts_offset", tc.offset, "-f", "mpegts")
			ch := blackChannel(t, ff)
			rec, _ := checkSegment(t, ch, 1, path, false)
			if len(rec.Black) != 1 {
				t.Fatalf("black runs %+v, want one", rec.Black)
			}
			b := rec.Black[0]
			if math.Abs(b.Start-1.001) > 0.02 || math.Abs(b.Duration-1.5015) > 0.04 || b.Frames != 45 {
				t.Errorf("run %+v: want start 1.001 s, 1.5015 s and 45 black frames", b)
			}
		})
	}
}

// 3. Time with no decoded frame counted as black frames. ffmpeg's black run
// lasts until the next frame it decodes that isn't black, so frames it
// can't decode (an unsupported feature, corruption) after a black frame
// stretch the run. The monitor counts the parser's access units in that
// span as black frames, so the fault claims black pictures ffmpeg never
// saw, and trigger_min is reached by frames that were not black.
//
// Here 15 black frames (0.5 s) are followed by 45 frames no decoder can
// output, then by the picture again at the next IDR.
func TestFalseBlackUndecodedFramesCountAsBlackFrames(t *testing.T) {
	ff := needFFmpeg(t, "libx264")
	dir := t.TempDir()
	src := genFFmpeg(t, ff, filepath.Join(dir, "src.ts"),
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30000/1001:d=3,drawbox=color=black:t=fill:enable='lt(n,15)'",
		"-c:v", "libx264", "-bf", "0", "-g", "60", "-keyint_min", "60", "-sc_threshold", "0", "-f", "mpegts")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "undecodable.ts")
	if err := os.WriteFile(path, breakSlices(b, 0x100, func(i int) bool { return i >= 15 && i < 60 }), 0o644); err != nil {
		t.Fatal(err)
	}
	ch := blackChannel(t, ff)
	rec, f := checkSegment(t, ch, 1, path, false)
	t.Logf("black %+v", rec.Black)
	if f != nil {
		t.Errorf("0.5 s of black frames raised black_video (trigger_min %g s): %s", ch.m.cfg.Blackdetect.TriggerMin, f.Message)
	}
	for _, r := range rec.Black {
		if r.Frames > 15 {
			t.Errorf("run %+v counts %d black frames; ffmpeg decoded 15 black frames, the other %d were never decoded", r, r.Frames, r.Frames-15)
		}
	}
}

// 4. A PTS jump inside a segment (an ad splice cut into one segment, which
// no EXT-X-DISCONTINUITY can mark) right after a black frame. ffmpeg ends
// the run at the next frame that isn't black, so the jump becomes black on
// screen: two black frames and a 3 s jump make a 3 s black_video fault. A
// jump back makes black_end come before black_start.
func TestFalseBlackPTSJumpInsideSegment(t *testing.T) {
	ff := needFFmpeg(t, "mpeg2video")
	for _, tc := range []struct {
		name string
		a, b string // -output_ts_offset of each half, seconds
	}{
		{"3 s forward", "0", "4"},
		// Control: ffmpeg unwraps the restart into a 94452 s run; the
		// monitor's own duration is negative and the run is dropped.
		{"restart 990 s back", "1000", "10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// One second of picture whose last two frames are black, then one
			// second of picture.
			a := genFFmpeg(t, ff, filepath.Join(dir, "a.ts"),
				"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30000/1001:d=1,drawbox=color=black:t=fill:enable='gte(n,28)'",
				"-c:v", "mpeg2video", "-g", "15", "-output_ts_offset", tc.a, "-f", "mpegts")
			bb := genFFmpeg(t, ff, filepath.Join(dir, "b.ts"),
				"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30000/1001:d=1",
				"-c:v", "mpeg2video", "-g", "15", "-output_ts_offset", tc.b, "-f", "mpegts")
			var joined []byte
			for _, p := range []string{a, bb} {
				x, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				joined = append(joined, x...)
			}
			path := filepath.Join(dir, "spliced.ts")
			if err := os.WriteFile(path, joined, 0o644); err != nil {
				t.Fatal(err)
			}
			ch := blackChannel(t, ff)
			rec, f := checkSegment(t, ch, 1, path, false)
			t.Logf("black %+v", rec.Black)
			if f != nil {
				t.Errorf("two black frames raised black_video: %s", f.Message)
			}
			for _, r := range rec.Black {
				if r.Duration < 0 || r.End < r.Start || r.Duration > 0.2 {
					t.Errorf("run %+v: two black frames should be about 0.067 s", r)
				}
			}
		})
	}
}

// 5. ffmpeg that decodes nothing, or fails on part of a segment, still
// exits 0 (ffmpeg only fails a run when more than 2/3 of the frames fail
// to decode). The monitor reads only the black lines, so such a segment
// counts as checked and clear, with no blackdetect error and no WARN.
// Frames dropped before the first decodable keyframe are expected when a
// segment is decoded on its own, and leave it checked.
func TestFalseBlackDecodeProblemsAreNotChecks(t *testing.T) {
	ff := needFFmpeg(t, "libx264")
	for _, tc := range []struct {
		name     string
		src      string // the lavfi source, up to its size option
		x264     string
		from, n  int
		corrupt  func(i int) bool // video PES (in the cut) whose slice data to corrupt
		wantWhat string           // "" when the segment is checked
	}{
		// Intra refresh: no IDR at all after the first frame. A segment cut
		// from the middle has none, and the native decoder outputs nothing.
		{"intra refresh, no IDR", "color=c=black:", "intra-refresh=1:keyint=30", 45, 30, nil, "decoded no frame"},
		// Cut mid-GOP: 15 frames before the next IDR can't be decoded (no
		// SPS/PPS, no reference); ffmpeg logs errors and carries on. Those
		// frames are expected to go, and the rest is checked.
		{"mid-GOP start", "color=c=black:", "keyint=60:min-keyint=60:scenecut=0", 45, 90, nil, ""},
		// Damaged slices after the first frame: ffmpeg logs errors, conceals
		// them and outputs every frame.
		{"decode errors after the first frame", "testsrc2=", "keyint=60:min-keyint=60:scenecut=0", 60, 60, between(30, 33), "logged decode errors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := genFFmpeg(t, ff, filepath.Join(dir, "src.ts"),
				"-f", "lavfi", "-i", tc.src+"size=320x240:rate=30000/1001:d=6",
				"-c:v", "libx264", "-bf", "0", "-x264-params", tc.x264, "-f", "mpegts")
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			cut := cutTS(t, b, 0x100, tc.from, tc.n)
			if tc.corrupt != nil {
				cut = corruptSlices(cut, 0x100, tc.corrupt)
			}
			path := filepath.Join(dir, "cut.ts")
			if err := os.WriteFile(path, cut, 0o644); err != nil {
				t.Fatal(err)
			}
			seg, err := analysis.Analyze(cut)
			if err != nil {
				t.Fatal(err)
			}
			ch := blackChannel(t, ff)
			runs, err := ch.m.blackRuns(context.Background(), path, seg, 3003, "")
			t.Logf("runs %+v, error %v", runs, err)
			switch {
			case tc.wantWhat != "" && err == nil:
				t.Errorf("ffmpeg %s on this segment, but blackdetect reported success: the segment counts as checked", tc.wantWhat)
			case tc.wantWhat == "" && err != nil:
				t.Errorf("only frames before the first decodable keyframe were dropped, but the segment is not checked: %v", err)
			}
		})
	}
}

// 6. Settings that make normal video black pass validation: pix_th and
// pic_th swapped (their names differ by one letter), or pic_th 0.
func TestFalseBlackFromThresholdsConfigAccepts(t *testing.T) {
	ff := needFFmpeg(t, "mpeg2video")
	path := genFFmpeg(t, ff, filepath.Join(t.TempDir(), "picture.ts"),
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30000/1001:d=2", "-c:v", "mpeg2video", "-g", "15", "-f", "mpegts")
	for _, tc := range []struct{ name, yaml string }{
		{"pix_th and pic_th swapped", "  pix_th: 0.98\n  pic_th: 0.10\n"},
		{"pic_th 0", "  pic_th: 0\n"},
		{"pix_th 1", "  pix_th: 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := "blackdetect:\n" + tc.yaml + "channels:\n  - name: test\n    url: http://127.0.0.1:1/x.m3u8\n"
			parsed, err := config.Parse([]byte(doc))
			if err != nil {
				return // rejected, as it should be
			}
			ch := blackChannel(t, ff, func(c *config.Config) {
				c.Blackdetect.PixelThreshold, c.Blackdetect.PictureThreshold = parsed.Blackdetect.PixelThreshold, parsed.Blackdetect.PictureThreshold
			})
			rec, f := checkSegment(t, ch, 1, path, false)
			t.Errorf("accepted %q; ffmpeg then calls testsrc2 black: %+v (fault: %v)", strings.TrimSpace(tc.yaml), rec.Black, f != nil)
		})
	}
}
