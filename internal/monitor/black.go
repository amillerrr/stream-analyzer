package monitor

import (
	"math"
	"slices"

	"github.com/amillerrr/stream-analyzer/internal/analysis"
	"github.com/amillerrr/stream-analyzer/internal/blackdetect"
	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// BlackInterval is one black run blackdetect found in a segment, placed in
// PTS.
type BlackInterval struct {
	// Start and End are seconds from the segment's first frame (its
	// earliest video PTS); Duration is End - Start, the time on screen.
	Start    float64 `json:"start_s"`
	End      float64 `json:"end_s"`
	Duration float64 `json:"duration_s"`
	// StartPTS is the first black frame's PTS. EndPTS is where the black
	// ends on screen: the next frame's PTS, or one frame past the last
	// frame when the run lasts to the end of the segment.
	StartPTS uint64 `json:"start_pts"`
	EndPTS   uint64 `json:"end_pts"`
	// FromStart: the run begins at the segment's first frame. ToEnd: it
	// lasts through the segment's last frame, so it may go on in the next.
	FromStart bool `json:"from_start,omitzero"`
	ToEnd     bool `json:"to_end,omitzero"`
	// Frames counts the black frames in the run. ffmpeg counts time with
	// no frame at all as black too (the last frame stays on screen);
	// NoFrameS is that part of Duration.
	Frames   int     `json:"black_frames"`
	NoFrameS float64 `json:"no_frame_s"`
}

// maxBlackJoinGap is the most time with no frame at all that a black run
// may span at a segment boundary and still be one run: the frames missing
// there show the last black frame held.
const maxBlackJoinGap = ts.Hz / 2

// placeBlack places blackdetect's intervals (stream time, seconds) in a
// segment; frame is the channel's video frame duration in ticks. Edges are
// matched within half a frame: a run that starts at the first frame is
// FromStart, one that ends at (or after) the last frame is ToEnd and lasts
// one frame past it, since ffmpeg ends such a run at the last frame's PTS.
func placeBlack(seg *analysis.Segment, iv []blackdetect.Interval, frame int64) []BlackInterval {
	v := seg.Video
	if v == nil || len(v.AUs) == 0 || len(iv) == 0 {
		return nil
	}
	first, last := v.MinPTS(), v.MaxPTS()
	half := frame / 2
	out := make([]BlackInterval, 0, len(iv))
	for _, i := range iv {
		b := BlackInterval{StartPTS: streamPTS(i.Start), EndPTS: streamPTS(i.End)}
		b.FromStart = ts.Diff(b.StartPTS, first) <= half
		if ts.Diff(b.EndPTS, last) >= -half {
			b.ToEnd, b.EndPTS = true, ts.Add(last, frame)
		}
		b.Start = round6(float64(ts.Diff(b.StartPTS, first)) / ts.Hz)
		b.End = round6(float64(ts.Diff(b.EndPTS, first)) / ts.Hz)
		b.Duration = round6(b.End - b.Start)
		for _, au := range v.AUs {
			if ts.Diff(au.PTS, b.StartPTS) >= 0 && ts.Diff(au.PTS, b.EndPTS) < 0 {
				b.Frames++
			}
		}
		b.NoFrameS = noFrame(b.Duration, b.Frames, frame)
		out = append(out, b)
	}
	return out
}

// keepBlack drops runs shorter than d (blackdetect's d) that neither start
// at the segment's first frame nor last to its end: those can't join a run
// in the next or previous segment. ffmpeg is asked for every run, however
// short, so that d applies to whole runs.
func keepBlack(runs []BlackInterval, d float64) []BlackInterval {
	return slices.DeleteFunc(runs, func(b BlackInterval) bool { return b.Duration < d && !b.FromStart && !b.ToEnd })
}

// streamPTS converts seconds of stream time, as ffmpeg -copyts reports
// them, to a 33-bit PTS. ffmpeg unwraps timestamps inside a file, so a time
// can pass 2^33 ticks.
func streamPTS(s float64) uint64 {
	return uint64(max(0, math.Round(s*ts.Hz))) % ts.Wrap
}

// joinsBlack reports whether a black run that lasted to the end of the
// previous segment, ending on screen at end, goes on in a run starting at
// start: within half a frame, or after frames that are missing altogether
// (no more than maxBlackJoinGap).
func joinsBlack(end, start uint64, frame int64) bool {
	d := ts.Diff(start, end)
	return d >= -frame/2 && d <= maxBlackJoinGap
}

// noFrame is the part of a run of seconds on screen with no frame at all,
// given its black frames of frame ticks each.
func noFrame(seconds float64, frames int, frame int64) float64 {
	return round6(max(0, seconds-float64(int64(frames)*frame)/ts.Hz))
}

func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }
