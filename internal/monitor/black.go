package monitor

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// BlackInterval is one black run blackdetect found in a segment, placed in
// PTS. Only frames ffmpeg decoded and called black are black: frames the
// TS parser found that ffmpeg did not decode, and time with no frame at
// all, are counted apart and never as black.
type BlackInterval struct {
	// Start and End are seconds from the segment's first frame (its
	// earliest video PTS); Duration is End - Start, the time on screen from
	// the first black frame to the end of the last.
	Start    float64 `json:"start_s"`
	End      float64 `json:"end_s"`
	Duration float64 `json:"duration_s"`
	// StartPTS is the first black frame's PTS; EndPTS is one frame past
	// the last black frame's.
	StartPTS uint64 `json:"start_pts"`
	EndPTS   uint64 `json:"end_pts"`
	// FromStart: the run begins at the segment's first frame. ToEnd: its
	// last black frame is the segment's last, so it may go on in the next.
	FromStart bool `json:"from_start,omitzero"`
	ToEnd     bool `json:"to_end,omitzero"`
	// Frames counts the black frames ffmpeg decoded in the run, and BlackS
	// is their time on screen, one frame each at most: the run's length.
	Frames int     `json:"black_frames"`
	BlackS float64 `json:"black_frames_s"`
	// UndecodedFrames are frames the parser found inside the run that
	// ffmpeg did not decode; NoFrameS is the rest of Duration, time with no
	// frame at all.
	UndecodedFrames int     `json:"undecoded_frames,omitzero"`
	NoFrameS        float64 `json:"no_frame_s"`
	// Unconfirmed: the segment was not fully checked (see
	// BlackDecode.NotChecked), so the run may be longer or broken by
	// pictures ffmpeg did not decode.
	Unconfirmed bool `json:"unconfirmed,omitzero"`
	// PixFmt and ColorRange are what blackdetect judged the frames in.
	PixFmt     string `json:"pix_fmt,omitempty"`
	ColorRange string `json:"color_range,omitempty"`

	// For the thumbnails, as ffmpeg gave them: the black frames' PTS, and
	// the frames decoded right before and after the run, if any.
	black         []int64
	before, after *int64
}

// maxBlackJoinGap is the most time with no frame at all that a black run
// may span at a segment boundary and still be one run: the frames missing
// there show the last black frame held. That time is not black.
const maxBlackJoinGap = ts.Hz / 2

// fallbackFrameTicks is the frame duration used when neither the master
// playlist nor the segments give one: 29.97 fps.
const fallbackFrameTicks = 3003

// BlackDecode says how much of a segment's video ffmpeg decoded for the
// black check, against the frames the TS parser found.
type BlackDecode struct {
	// Decoder is the decoder ffmpeg used, as it names it, and PixFmt and
	// ColorRange what blackdetect judged the frames in ("tv", "pc" or
	// "unknown", which it reads as limited).
	Decoder    string `json:"decoder,omitempty"`
	PixFmt     string `json:"pix_fmt,omitempty"`
	ColorRange string `json:"color_range,omitempty"`
	Frames     int    `json:"frames"`  // frames the parser found
	Decoded    int    `json:"decoded"` // frames ffmpeg decoded
	// LeadingDropped are frames before the first one ffmpeg could decode:
	// expected when a segment is decoded on its own and it has an open GOP
	// or doesn't start on an IDR.
	LeadingDropped int `json:"leading_dropped,omitzero"`
	// Undecoded are frames after the first decoded one that ffmpeg did not
	// decode, UndecodedS their time, and UndecodedRanges where they are.
	Undecoded       int             `json:"undecoded,omitzero"`
	UndecodedS      float64         `json:"undecoded_s,omitzero"`
	UndecodedRanges []UndecodedSpan `json:"undecoded_ranges,omitempty"`
	// DecodeErrors are errors ffmpeg logged decoding after its first frame,
	// FirstDecodeError the first of them; LeadingErrors came before it.
	DecodeErrors     int    `json:"decode_errors,omitzero"`
	FirstDecodeError string `json:"first_decode_error,omitempty"`
	LeadingErrors    int    `json:"leading_errors,omitzero"`
	// NotChecked says why the segment's black check is incomplete: no
	// frame decoded, frames or errors after the first decoded one, or no
	// check at all (no video stream, no saved file). "" when it was
	// complete. Black found in such a segment is unconfirmed.
	NotChecked string `json:"not_checked,omitempty"`
}

// UndecodedSpan is a stretch of frames ffmpeg did not decode, in seconds
// from the segment's first frame and in PTS (to one frame past the last).
type UndecodedSpan struct {
	Start    float64 `json:"start_s"`
	End      float64 `json:"end_s"`
	StartPTS uint64  `json:"start_pts"`
	EndPTS   uint64  `json:"end_pts"`
	Frames   int     `json:"frames"`
}

// maxUndecodedSpans bounds the spans listed.
const maxUndecodedSpans = 10

// placeBlack places the black runs blackdetect found in a segment; frame
// is the channel's video frame duration in ticks. A run is the black frames
// ffmpeg output one after another, placed by their own PTS, each snapped to
// the parser's frame with that PTS, so neither ffmpeg's rounding of times
// nor its unwrapping of 33-bit timestamps moves it. It ends one frame
// after its last black frame: what follows on screen, a frame ffmpeg could
// not decode or no frame at all, is not black. A run that starts at the
// segment's first frame (within half a frame) is FromStart; one whose last
// black frame is the last decoded, at the segment's last PTS, is ToEnd.
func placeBlack(seg *analysis.Segment, res blackdetect.Result, frame int64) []BlackInterval {
	v := seg.Video
	if v == nil || len(v.AUs) == 0 {
		return nil
	}
	frames := decodedFrames(v, res, frame)
	_, missing := missingFrames(v, frames, frame)
	first, last := v.MinPTS(), v.MaxPTS()
	half := frame / 2
	var out []BlackInterval
	for i := 0; i < len(frames); i++ {
		if !frames[i].Black {
			continue
		}
		j := i
		for j+1 < len(frames) && frames[j+1].Black {
			j++
		}
		b := BlackInterval{
			StartPTS: frames[i].pts, EndPTS: ts.Add(frames[j].pts, frame), Frames: j - i + 1,
			PixFmt: res.PixFmt, ColorRange: res.ColorRange,
		}
		for k := i; k <= j; k++ {
			b.black = append(b.black, frames[k].raw)
		}
		if i > 0 {
			b.before = new(frames[i-1].raw)
		}
		if j+1 < len(frames) {
			b.after = new(frames[j+1].raw)
		}
		b.ToEnd = j == len(frames)-1 && ts.Diff(frames[j].pts, last) >= -half
		b.FromStart = ts.Diff(b.StartPTS, first) <= half
		var shown int64
		for k := i; k <= j; k++ {
			shown += frames[k].shown(frames, k, frame)
		}
		b.BlackS = round6(float64(shown) / ts.Hz)
		b.Start = round6(float64(ts.Diff(b.StartPTS, first)) / ts.Hz)
		b.End = round6(float64(ts.Diff(b.EndPTS, first)) / ts.Hz)
		b.Duration = round6(b.End - b.Start)
		for _, au := range missing {
			if ts.Diff(au, b.StartPTS) >= 0 && ts.Diff(au, b.EndPTS) < 0 {
				b.UndecodedFrames++
			}
		}
		b.NoFrameS = noFrame(b.Duration, b.BlackS, b.UndecodedFrames, frame)
		out = append(out, b)
		i = j
	}
	return out
}

// decodedFrame is a frame ffmpeg decoded, at its 33-bit PTS; raw is its
// PTS as ffmpeg gave it.
type decodedFrame struct {
	pts   uint64
	raw   int64
	Black bool
}

// shown is how long frames[k] is on screen: until the next frame, one
// frame at most.
func (f decodedFrame) shown(frames []decodedFrame, k int, frame int64) int64 {
	if k+1 < len(frames) {
		if d := ts.Diff(frames[k+1].pts, f.pts); d > 0 {
			return min(d, frame)
		}
	}
	return frame
}

// decodedFrames are the frames ffmpeg decoded, at their PTS snapped to the
// parser's frames.
func decodedFrames(v *analysis.Video, res blackdetect.Result, frame int64) []decodedFrame {
	snap := snapper(v, frame/2)
	out := make([]decodedFrame, len(res.Frames))
	for i, f := range res.Frames {
		out[i] = decodedFrame{pts: snap(f.PTS), raw: f.PTS, Black: f.Black}
	}
	return out
}

// missingFrames lists the parser's frames ffmpeg did not decode, by PTS,
// in presentation order: those before the first frame it decoded
// (leading), and the rest. Frames are missing only when ffmpeg output
// fewer than the parser found; which ones is found by pairing each decoded
// frame with a parser frame within half a frame of it. ffmpeg re-times
// frames that share or jitter their PTS, so pairing alone would take a
// re-timed frame for a missing one.
func missingFrames(v *analysis.Video, frames []decodedFrame, frame int64) (leading, after []uint64) {
	missing := len(v.AUs) - len(frames)
	if missing <= 0 {
		return nil, nil
	}
	ref := v.MinPTS()
	var aus, dec []int64
	for _, au := range v.AUs {
		if !au.NoPTS {
			aus = append(aus, ts.Diff(au.PTS, ref))
		}
	}
	for _, f := range frames {
		dec = append(dec, ts.Diff(f.pts, ref))
	}
	slices.Sort(aus)
	slices.Sort(dec)
	var unpaired []int64
	j := 0
	for _, a := range aus {
		for j < len(dec) && dec[j] < a-frame/2 {
			j++ // a decoded frame with no parser frame near it
		}
		if j < len(dec) && dec[j]-a <= frame/2 {
			j++
			continue
		}
		unpaired = append(unpaired, a)
	}
	for _, a := range unpaired {
		switch {
		case len(dec) == 0 || a < dec[0]:
			if len(leading) < missing {
				leading = append(leading, ts.Add(ref, a))
			}
		case len(leading)+len(after) < missing:
			after = append(after, ts.Add(ref, a))
		}
	}
	return leading, after
}

// decodeSummary compares what ffmpeg decoded with the frames the parser
// found in seg.
func decodeSummary(seg *analysis.Segment, res blackdetect.Result, frame int64) BlackDecode {
	v := seg.Video
	if v == nil {
		return BlackDecode{}
	}
	frames := decodedFrames(v, res, frame)
	d := BlackDecode{
		Decoder: res.Decoder, PixFmt: res.PixFmt, ColorRange: res.ColorRange, Frames: len(v.AUs), Decoded: len(frames),
		DecodeErrors: res.DecodeErrors, FirstDecodeError: res.FirstDecodeError, LeadingErrors: res.LeadingErrors,
	}
	if len(frames) == 0 {
		d.DecodeErrors, d.LeadingErrors = 0, res.LeadingErrors+res.DecodeErrors
		d.NotChecked = "ffmpeg decoded no frame"
		return d
	}
	ref := v.MinPTS()
	leading, after := missingFrames(v, frames, frame)
	d.LeadingDropped = len(leading)
	var span *UndecodedSpan
	for _, au := range after {
		d.Undecoded++
		switch {
		case span != nil && ts.Diff(au, span.EndPTS) <= frame/2:
			span.EndPTS = ts.Add(au, frame)
			span.Frames++
		case len(d.UndecodedRanges) < maxUndecodedSpans:
			d.UndecodedRanges = append(d.UndecodedRanges, UndecodedSpan{StartPTS: au, EndPTS: ts.Add(au, frame), Frames: 1})
			span = &d.UndecodedRanges[len(d.UndecodedRanges)-1]
		default:
			span = nil
		}
	}
	for i := range d.UndecodedRanges {
		r := &d.UndecodedRanges[i]
		r.Start = round6(float64(ts.Diff(r.StartPTS, ref)) / ts.Hz)
		r.End = round6(float64(ts.Diff(r.EndPTS, ref)) / ts.Hz)
	}
	d.UndecodedS = round6(float64(int64(d.Undecoded)*frame) / ts.Hz)
	var why []string
	if d.Undecoded > 0 {
		why = append(why, fmt.Sprintf("ffmpeg did not decode %d frames after the first one it decoded", d.Undecoded))
	}
	if d.DecodeErrors > 0 {
		why = append(why, fmt.Sprintf("ffmpeg logged %d decode errors after the first frame it decoded (%s)", d.DecodeErrors, d.FirstDecodeError))
	}
	d.NotChecked = strings.Join(why, "; ")
	return d
}

// snapper returns a function that turns a decoded frame's PTS, as ffmpeg
// gives it, into a 33-bit PTS: the parser's frame within half a frame of
// it, or its own value when there is none.
func snapper(v *analysis.Video, half int64) func(int64) uint64 {
	ref := v.MinPTS()
	var at []int64 // the parser's frames' PTS, from ref, in order
	for _, au := range v.AUs {
		if !au.NoPTS {
			at = append(at, ts.Diff(au.PTS, ref))
		}
	}
	slices.Sort(at)
	return func(raw int64) uint64 {
		p := wrapPTS(raw)
		d := ts.Diff(p, ref)
		i, _ := slices.BinarySearch(at, d)
		best := int64(-1)
		for _, k := range []int{i - 1, i} {
			if k >= 0 && k < len(at) && abs(at[k]-d) <= half && (best < 0 || abs(at[k]-d) < abs(at[best]-d)) {
				best = int64(k)
			}
		}
		if best < 0 {
			return p
		}
		return ts.Add(ref, at[best])
	}
}

// framesFromIntervals stands for a detector that reports only black runs
// (Options.BlackDetector): every frame the parser found counts as decoded,
// and black when a run covers it. A run that ends within half a frame of
// the segment's last frame, or after it, covers that frame too.
func framesFromIntervals(v *analysis.Video, iv []blackdetect.Interval, frame int64) []blackdetect.Frame {
	ref, last := v.MinPTS(), v.MaxPTS()
	var out []blackdetect.Frame
	for _, au := range v.AUs {
		if au.NoPTS {
			continue
		}
		f := blackdetect.Frame{PTS: int64(au.PTS)}
		for _, i := range iv {
			start, end := wrapPTS(secondsToTicks(i.Start)), wrapPTS(secondsToTicks(i.End))
			toEnd := ts.Diff(end, last) >= -frame/2
			if ts.Diff(au.PTS, start) >= 0 && (ts.Diff(au.PTS, end) < 0 || toEnd) {
				f.Black = true
			}
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b blackdetect.Frame) int {
		return cmp.Compare(ts.Diff(uint64(a.PTS), ref), ts.Diff(uint64(b.PTS), ref))
	})
	return out
}

// keepBlack drops runs with less than d (blackdetect's d) of black frames
// that neither start at the segment's first frame nor last to its end:
// those can't join a run in the next or previous segment. ffmpeg is asked
// for every run, however short, so that d applies to whole runs.
func keepBlack(runs []BlackInterval, d float64) []BlackInterval {
	return slices.DeleteFunc(runs, func(b BlackInterval) bool { return b.BlackS < d && !b.FromStart && !b.ToEnd })
}

// secondsToTicks converts seconds of stream time, as ffmpeg -copyts
// reports them, to 90 kHz ticks.
func secondsToTicks(s float64) int64 { return int64(math.Round(s * ts.Hz)) }

// wrapPTS reduces a timestamp ffmpeg unwrapped (it can be negative, or
// past 2^33, near the 33-bit wrap) to the stream's 33-bit PTS.
func wrapPTS(t int64) uint64 {
	const w = int64(ts.Wrap)
	return uint64((t%w + w) % w)
}

func abs(x int64) int64 { return max(x, -x) }

// joinsBlack reports whether a black run that lasted to the end of the
// previous segment, ending on screen at end, goes on in a run starting at
// start: within half a frame, or after frames that are missing altogether
// (no more than maxBlackJoinGap).
func joinsBlack(end, start uint64, frame int64) bool {
	d := ts.Diff(start, end)
	return d >= -frame/2 && d <= maxBlackJoinGap
}

// noFrame is the part of a run of seconds on screen with no frame at all:
// neither a black frame (black seconds of them) nor one of the undecoded
// frames the parser found, of frame ticks each.
func noFrame(seconds, black float64, undecoded int, frame int64) float64 {
	return round6(max(0, seconds-black-float64(int64(undecoded)*frame)/ts.Hz))
}

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }
