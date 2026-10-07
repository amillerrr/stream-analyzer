package blackdetect

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Build is the ffmpeg the monitor runs, as found when it starts.
type Build struct {
	// Path is the binary itself (symbolic links resolved). Every run uses
	// it, so the checks below hold for every run.
	Path string `json:"path"`
	// Version is as ffmpeg prints it; Major and Minor are 0 when it can't
	// be read (a build from git).
	Version      string `json:"version"`
	Major, Minor int    `json:"-"`
	// H264 and HEVC are the decoders ffmpeg uses for each codec ("h264",
	// "libopenh264"), "" for none.
	H264 string `json:"h264_decoder"`
	HEVC string `json:"hevc_decoder"`
	// SelfTestDecoder is the decoder the self-test's H.264 clips went
	// through, as ffmpeg names it in its stream mapping ("h264 (native)").
	SelfTestDecoder string `json:"self_test_decoder"`
}

// MinMajor is the oldest ffmpeg major version without known black-check
// problems: before 7.0 times are printed to 6 significant digits, and
// 10-bit full-range video gets the limited-range threshold.
const MinMajor = 7

// Warnings are what's worth logging about the build that doesn't stop the
// monitor.
func (b Build) Warnings() []string {
	switch {
	case b.Major == 0:
		return []string{fmt.Sprintf("cannot read ffmpeg's version from %q; %d.0 or later is recommended", b.Version, MinMajor)}
	case b.Major < MinMajor:
		return []string{fmt.Sprintf("ffmpeg %s is older than %d.0: it prints black times to 6 significant digits (the monitor reads frame timestamps instead) and gives 10-bit full-range video the limited-range threshold", b.Version, MinMajor)}
	}
	return nil
}

// Inspect finds the ffmpeg o names (a name on PATH or a path) and resolves
// it to the binary itself, reads its version and the decoders it uses for
// H.264 and HEVC, and runs the self-test with it. It refuses a build that
// decodes H.264 only with OpenH264 unless allowOpenH264, and any build that
// fails the self-test.
func Inspect(ctx context.Context, o Options, allowOpenH264 bool) (Build, error) {
	name := cmp.Or(o.FFmpeg, "ffmpeg")
	path, err := exec.LookPath(name)
	if err != nil {
		return Build{}, fmt.Errorf("blackdetect needs ffmpeg: %w (install it, set blackdetect.ffmpeg, or set blackdetect.enabled: false)", err)
	}
	if path, err = filepath.Abs(path); err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		return Build{}, fmt.Errorf("blackdetect: resolving %s: %w", name, err)
	}
	b := Build{Path: path}
	out, err := exec.CommandContext(ctx, path, "-hide_banner", "-version").Output()
	if err != nil {
		return b, fmt.Errorf("blackdetect: %s -version: %w", path, err)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	b.Version, b.Major, b.Minor, _ = readVersion(first)
	if out, err = exec.CommandContext(ctx, path, "-hide_banner", "-decoders").Output(); err != nil {
		return b, fmt.Errorf("blackdetect: %s -decoders: %w", path, err)
	}
	dec := readDecoders(string(out))
	b.H264, b.HEVC = dec["h264"], dec["hevc"]
	switch {
	case b.H264 == "":
		return b, fmt.Errorf("blackdetect: ffmpeg %s (%s) has no H.264 decoder", b.Version, path)
	case b.H264 == openH264 && !allowOpenH264:
		return b, openH264Refused(b)
	}
	o.FFmpeg = path
	if b.SelfTestDecoder, err = SelfTest(ctx, o); err != nil {
		return b, fmt.Errorf("blackdetect: ffmpeg %s (%s) failed the self-test: %w", b.Version, path, err)
	}
	if strings.Contains(b.SelfTestDecoder, openH264) && !allowOpenH264 {
		return b, openH264Refused(b)
	}
	return b, nil
}

const openH264 = "libopenh264"

func openH264Refused(b Build) error {
	return fmt.Errorf("blackdetect: ffmpeg %s (%s) decodes H.264 only with OpenH264, which drops full-range video's range tag (dark pictures count as black), mis-times frames on irregular segments and can't decode interlaced, 10-bit or 4:2:2 video; install an ffmpeg with its own H.264 decoder (see the README), or set blackdetect.allow_openh264: true",
		b.Version, b.Path)
}

var versionLine = regexp.MustCompile(`^ffmpeg version (\S+)`)
var versionNumber = regexp.MustCompile(`^n?(\d+)\.(\d+)`)

// readVersion reads ffmpeg -version's first line: the version as printed,
// and its major and minor numbers when it has them.
func readVersion(line string) (version string, major, minor int, ok bool) {
	m := versionLine.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return strings.TrimSpace(line), 0, 0, false
	}
	n := versionNumber.FindStringSubmatch(m[1])
	if n == nil {
		return m[1], 0, 0, false
	}
	major, _ = strconv.Atoi(n[1])
	minor, _ = strconv.Atoi(n[2])
	return m[1], major, minor, true
}

var decoderLine = regexp.MustCompile(`^\s*([VAS.][F.][S.][X.][B.][D.])\s+(\S+)\s+(.*)$`)
var forCodec = regexp.MustCompile(`\(codec (\S+)\)\s*$`)

// readDecoders reads ffmpeg -decoders: for each codec, the decoder ffmpeg
// uses, the first one listed for it that isn't experimental.
func readDecoders(list string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(list))
	for sc.Scan() {
		m := decoderLine.FindStringSubmatch(sc.Text())
		if m == nil || m[1][3] == 'X' {
			continue
		}
		codec := m[2]
		if c := forCodec.FindStringSubmatch(m[3]); c != nil {
			codec = c[1]
		}
		if _, ok := out[codec]; !ok {
			out[codec] = m[2]
		}
	}
	return out
}
