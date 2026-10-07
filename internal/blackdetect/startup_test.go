package blackdetect

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "regenerate the self-test clips in selftest/ (needs ffmpeg with libx264)")

// The self-test clips are made from lavfi sources with libx264. Each
// argument list ends where the output file goes.
var clipSources = map[string][]string{
	// 30 frames, limited range tagged bt709, B-frames. Frames 10-19 are
	// black (luma 16), 20-22 dark but not black (luma 40). The 33-bit PTS
	// wrap falls between frames 14 and 15.
	"limited.ts": {
		"-f", "lavfi", "-i", "testsrc2=s=160x90:r=30000/1001:d=1.001,format=yuv420p," +
			"geq=lum='if(between(N,10,19),16,if(between(N,20,22),40,lum(X,Y)))':cb='if(between(N,10,22),128,cb(X,Y))':cr='if(between(N,10,22),128,cr(X,Y))'",
		"-c:v", "libx264", "-bf", "2", "-g", "30", "-qp", "10",
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv",
		"-output_ts_offset", "95441.833667", "-f", "mpegts",
	},
	// 20 frames, full range. Frames 5-9 are black (luma 0), 10-14 dark but
	// not black (luma 30 of 255: black if the range tag is lost).
	"full.ts": {
		"-f", "lavfi", "-i", "testsrc2=s=160x90:r=30000/1001:d=0.667,format=yuv420p," +
			"geq=lum='if(between(N,5,9),0,if(between(N,10,14),30,lum(X,Y)))':cb='if(between(N,5,14),128,cb(X,Y))':cr='if(between(N,5,14),128,cr(X,Y))'," +
			"setparams=range=pc",
		"-c:v", "libx264", "-bf", "0", "-g", "30", "-qp", "10", "-color_range", "pc", "-f", "mpegts",
	},
	// Two video PIDs: the picture (160x90) on 0x100, black (320x180, the
	// one ffmpeg would pick by itself) on 0x101.
	"twopid.ts": {
		"-f", "lavfi", "-i", "testsrc2=s=160x90:r=30000/1001:d=0.5",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=30000/1001:d=0.5",
		"-map", "0", "-map", "1", "-c:v", "libx264", "-bf", "2", "-g", "30", "-qp", "20", "-f", "mpegts",
	},
}

// The self-test passes with the ffmpeg on PATH, a build with native
// decoders. With -update it first makes the clips again.
func TestSelfTestClips(t *testing.T) {
	ff := requireFFmpeg(t)
	if *update {
		for name, args := range clipSources {
			out := filepath.Join("selftest", name)
			cmd := exec.CommandContext(t.Context(), ff, append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...), out)...)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", name, err, b)
			}
		}
	}
	o := Default()
	o.FFmpeg = ff
	decoder, err := SelfTest(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(decoder, "h264 ") {
		t.Errorf("decoder %q, want an H.264 one", decoder)
	}
}

// wrapFFmpeg writes a script that runs the real ffmpeg with its arguments
// changed by edit, a shell case body over "$a" ("drop" to leave it out).
func wrapFFmpeg(t *testing.T, ff, edit string) string {
	t.Helper()
	return fakeFFmpeg(t, `for a do
  shift
  case $a in
`+edit+`
  esac
  if [ "$a" = drop ]; then continue; fi
  set -- "$@" "$a"
done
exec '`+ff+`' "$@"
`)
}

// The self-test catches a build that decodes the wrong stream, misreads
// full range, or shifts timestamps.
func TestSelfTestCatchesWrongAnswers(t *testing.T) {
	ff := requireFFmpeg(t)
	for _, tc := range []struct{ name, edit, want string }{
		// Without -map ffmpeg decodes the larger, black stream.
		{"other video stream", `    -map) a=drop; skip=1;;
    *) if [ -n "$skip" ]; then a=drop; skip=; fi;;`, "twopid.ts"},
		// Full range relabelled limited, as OpenH264 leaves it: the dark
		// frames (luma 30 of 255) turn black.
		{"full range tag lost", `    settb=*) a=$(echo "$a" | sed 's/^settb=1\/90000,/settb=1\/90000,scale=in_range=pc:out_range=pc,format=yuv420p,setparams=range=tv,/');;`, "full.ts"},
		// Timestamps moved by a second.
		{"timestamps shifted", `    -copyts) set -- "$@" -copyts -itsoffset 1; a=drop;;`, "PTS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Default()
			o.FFmpeg = wrapFFmpeg(t, ff, tc.edit)
			if _, err := SelfTest(t.Context(), o); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("self-test error %v, want one about %s", err, tc.want)
			}
		})
	}
}

func TestReadVersion(t *testing.T) {
	for _, tc := range []struct {
		line         string
		major, minor int
		ok           bool
	}{
		{"ffmpeg version 7.1.5 Copyright (c) 2000-2026 the FFmpeg developers", 7, 1, true},
		{"ffmpeg version 5.1.9-0+deb12u1 Copyright (c) 2000-2026 the FFmpeg developers", 5, 1, true},
		{"ffmpeg version n7.0.2 Copyright (c) 2000-2024 the FFmpeg developers", 7, 0, true},
		{"ffmpeg version 9.0.2 Copyright (c) 2000-2026 the FFmpeg developers", 9, 0, true},
		{"ffmpeg version N-117000-g0123abc Copyright (c) 2000-2026 the FFmpeg developers", 0, 0, false},
	} {
		v, major, minor, ok := readVersion(tc.line)
		if major != tc.major || minor != tc.minor || ok != tc.ok || v == "" {
			t.Errorf("%q: %q %d.%d %v, want %d.%d %v", tc.line, v, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

// The decoder ffmpeg uses for a codec is the first one it lists for it
// that isn't experimental.
func TestReadDecoders(t *testing.T) {
	const native = ` VFS..D h264                 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10
 V..... h264_v4l2m2m         V4L2 mem2mem H.264 decoder wrapper (codec h264)
 V....D libopenh264          OpenH264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 VFS..D hevc                 HEVC (High Efficiency Video Coding)
 V.S.BD mpeg2video           MPEG-2 video
`
	const free = ` V....D libopenh264          OpenH264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V.S.BD mpeg2video           MPEG-2 video
`
	const experimental = ` V..X.D h264_new             H.264 (codec h264)
 V....D libopenh264          OpenH264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
`
	for _, tc := range []struct{ name, list, h264, hevc string }{
		{"native", native, "h264", "hevc"},
		{"OpenH264 only", free, "libopenh264", ""},
		{"experimental skipped", experimental, "libopenh264", ""},
	} {
		d := readDecoders(tc.list)
		if d["h264"] != tc.h264 || d["hevc"] != tc.hevc {
			t.Errorf("%s: %v, want h264 %q, hevc %q", tc.name, d, tc.h264, tc.hevc)
		}
	}
}

// A build whose only H.264 decoder is OpenH264 is refused unless
// allow_openh264 says otherwise; then the self-test still has to pass.
func TestInspectRefusesOpenH264Only(t *testing.T) {
	ff := fakeFFmpeg(t, `case "$*" in
*-version*) echo "ffmpeg version 7.1.5 Copyright (c) 2000-2026 the FFmpeg developers";;
*-decoders*) echo " V....D libopenh264          OpenH264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)";;
esac
`)
	o := Default()
	o.FFmpeg = ff
	if _, err := Inspect(t.Context(), o, false); err == nil || !strings.Contains(err.Error(), "allow_openh264") {
		t.Errorf("not allowed: %v, want a refusal naming allow_openh264", err)
	}
	if _, err := Inspect(t.Context(), o, true); err == nil || !strings.Contains(err.Error(), "self-test") {
		t.Errorf("allowed: %v, want the self-test to fail", err)
	}
}

// ffmpeg is looked up once: every run uses the binary found then, by its
// real path, whatever happens to PATH or the link later.
func TestInspectResolvesTheBinary(t *testing.T) {
	real := requireFFmpeg(t)
	dir := t.TempDir()
	wrapper := wrapFFmpeg(t, real, "")
	if err := os.Symlink(wrapper, filepath.Join(dir, "ffmpeg")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	o := Default()
	o.FFmpeg = "ffmpeg"
	b, err := Inspect(t.Context(), o, false)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(wrapper)
	if b.Path != want || b.Version == "" || b.H264 != "h264" || b.SelfTestDecoder == "" {
		t.Errorf("build %+v, want path %s, a version, h264 and the self-test's decoder", b, want)
	}
}

// No ffmpeg at all says how to go on.
func TestInspectWithoutFFmpeg(t *testing.T) {
	o := Default()
	o.FFmpeg = filepath.Join(t.TempDir(), "no-ffmpeg")
	_, err := Inspect(t.Context(), o, false)
	if !errors.Is(err, os.ErrNotExist) && (err == nil || !strings.Contains(err.Error(), "blackdetect.enabled: false")) {
		t.Errorf("err %v, want one saying how to turn black detection off", err)
	}
}
