package analysis

import (
	"cmp"
	"fmt"
	"math"

	"github.com/amillerrr/stream-analyzer/internal/ts"
)

// Fault types.
const (
	FaultVideoDTSGap           = "video_dts_gap"
	FaultAudioPTSGap           = "audio_pts_gap"
	FaultVideoDTSNotIncreasing = "video_dts_not_increasing"
	FaultPTSBehindPCR          = "pts_behind_pcr"
	FaultPTSPCRJump            = "pts_pcr_jump"
	FaultAVOffset              = "av_offset"
	FaultDurationMismatch      = "duration_mismatch"
	FaultBlackVideo            = "black_video"
	FaultInvalidSegment        = "invalid_segment"
	FaultManual                = "manual"
	FaultDiscontinuity         = "discontinuity"
	FaultStall                 = "stall"
	FaultUnavailable           = "unavailable"
	FaultMediaSequenceBackward = "media_sequence_backward"
	// FaultAudioCoverage: a segment's audio ends well before its video.
	FaultAudioCoverage = "audio_coverage"
	// FaultTSCorruption: bytes that were not TS packets, or packets flagged
	// transport_error_indicator, were skipped.
	FaultTSCorruption = "ts_corruption"
	// FaultVideoPTSError: a picture is due before it is decoded (PTS <
	// DTS).
	FaultVideoPTSError = "video_pts_error"
	// FaultVideoPTSGap: presentation jumps at a boundary by more than the
	// DTS does (a changed reorder delay), by video_gap_fault_ms or more.
	FaultVideoPTSGap = "video_pts_gap"
	// FaultStallEnded records the end of a stall that outlasted its own
	// incident (max_incident), with how long it lasted.
	FaultStallEnded = "stall_ended"
	// FaultStreamEnded: the playlist carries EXT-X-ENDLIST, so the origin
	// has ended the stream. It is not a stall.
	FaultStreamEnded = "stream_ended"
	// FaultPlaylistViolation is a playlist update that breaks the rules for
	// a live playlist; Values["reason"] says how (see hls.CheckUpdate).
	FaultPlaylistViolation = "playlist_violation"
	// FaultUndecodedFrames: ffmpeg did not decode frames the TS parser
	// found, after the first frame it did decode, so the black check could
	// not see them. Frames dropped before the first decodable one (an
	// open GOP, or no IDR at the segment's start) are expected when a
	// segment is decoded on its own, and are not this fault.
	FaultUndecodedFrames = "undecoded_frames"
)

// Fault is one detected problem in one segment. Values holds the timestamp
// evidence: raw 33-bit tick values and derived milliseconds.
type Fault struct {
	Type    string  `json:"type"`
	Seq     uint64  `json:"seq"`
	PrevSeq *uint64 `json:"prev_seq,omitempty"`
	// URI is the playlist URI of the segment the fault is on.
	URI     string         `json:"uri,omitempty"`
	Message string         `json:"message"`
	Values  map[string]any `json:"values,omitempty"`
}

// Thresholds configure the checks.
type Thresholds struct {
	// ContinuityMs is how far the gap to the previous segment may be from one
	// frame (video) or from zero after the last PES (audio).
	ContinuityMs float64
	// PCRJumpMs is the largest allowed change in PTS-PCR between consecutive
	// video PES.
	PCRJumpMs float64
	// AVOffsetMs is how far a segment's A/V start offset may be from the
	// channel baseline.
	AVOffsetMs float64
	// BaselineSegments is how many segments form the A/V baseline (their
	// median). Zero disables the A/V offset check.
	BaselineSegments int
	// RebaselineAfter moves the baseline once this many consecutive
	// out-of-range segments agree on a new offset. Zero never moves it.
	RebaselineAfter int
	// DurationFraction is the allowed relative difference between real
	// duration and EXTINF.
	DurationFraction float64
	// VideoGapFaultMs: a video gap or overlap of this size or more, at a
	// segment boundary or between two frames inside a segment, is a fault;
	// a smaller one (beyond ContinuityMs at a boundary, a skipped frame
	// slot inside) is an event.
	VideoGapFaultMs float64
	// AudioGapFaultFrames: an audio hole or overlap of more than this many
	// AAC frames, at a boundary or between two PES inside a segment, is a
	// fault; a smaller one is an event. Audio that ends more than this
	// before its segment's video is a fault too.
	AudioGapFaultFrames float64
}

// DefaultThresholds returns the documented defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{
		ContinuityMs:     10,
		PCRJumpMs:        500,
		AVOffsetMs:       100,
		BaselineSegments: 5,
		RebaselineAfter:  10,
		DurationFraction: 0.10,
		VideoGapFaultMs:  500,
		// 32 ms at 48 kHz: one missing AAC frame plus jitter is an event.
		AudioGapFaultFrames: 1.5,
	}
}

// Input is one analyzed segment and its playlist entry.
type Input struct {
	Seq           uint64
	ExtInf        float64 // seconds; 0 skips the duration check
	Discontinuity bool    // EXT-X-DISCONTINUITY precedes this segment
	Segment       *Segment
	// FrameTicks is the video frame duration the playlist declares
	// (FRAME-RATE); 0 when it declares none, and the chain learns it from
	// the recent segments.
	FrameTicks int64
	// Order, when set, is where the playlist puts this segment relative to
	// the previous one added. When nil, consecutive numbers are adjacent,
	// as they are in a playlist that is never renumbered.
	Order *Order
}

// Order places a segment relative to the previous one added to a Chain.
type Order struct {
	// Adjacent: the playlist lists this segment right after the previous
	// one added, so the two are compared.
	Adjacent bool
	// Gap: segments between the two that were never analyzed.
	Gap *Gap
}

// Result is what adding a segment to a Chain found.
type Result struct {
	Faults []Fault
	// Gap is set when sequence numbers between the previous segment and this
	// one were never analyzed. That is a monitor gap, not a stream fault, and
	// no continuity comparison is made across it.
	Gap *Gap
	// Events are irregularities that are logged but are not faults.
	Events []Event
}

// Event types.
const (
	// EventFrameGap: a DTS step inside the segment longer than 1.25 frames
	// (a skipped frame slot). GapMs is the missing time.
	EventFrameGap = "frame_gap"
	// EventOddLength: a video frame count unlike this channel's usual one.
	// GapMs is the length difference.
	EventOddLength = "odd_length"
	// EventAudioRetimed: audio PTS steps that don't match the frames'
	// duration. GapMs is the net re-timing (negative: squeezed).
	EventAudioRetimed = "audio_retimed"
	// EventAudioGap: audio that starts at most AudioGapFaultFrames AAC
	// frames away from where the previous segment's ended. GapMs is the gap.
	EventAudioGap = "audio_gap"
	// EventVideoGap: the first frame's DTS is off from one frame after the
	// previous segment's last by more than ContinuityMs but less than
	// VideoGapFaultMs. GapMs is the deviation (negative: an overlap).
	EventVideoGap = "video_gap"
	// EventFrameOverlap: DTS steps inside the segment shorter than 0.75
	// frames. GapMs is the time overlapped.
	EventFrameOverlap = "frame_overlap"
	// EventGapTagged: the playlist marks the segment EXT-X-GAP, so it has
	// no media; it is not fetched.
	EventGapTagged = "gap_tagged"
	// EventPresentationGap: at a boundary, the first picture is due off
	// from one frame after the previous segment's last by more than the
	// DTS step shows, beyond ContinuityMs but under VideoGapFaultMs. GapMs
	// is the difference.
	EventPresentationGap = "presentation_gap"
	// EventDuplicatePTS: pictures in the segment share a PTS: two due in
	// one frame slot. Count is how many.
	EventDuplicatePTS = "duplicate_pts"
)

// Event is an irregular segment worth logging that is not a fault.
type Event struct {
	Type  string  `json:"type"`
	Seq   uint64  `json:"seq"`
	GapMs float64 `json:"gap_ms"`
	Count int     `json:"count,omitzero"` // frame gaps or re-timed audio steps
	// Slots is how many frame slots a frame_gap event's gaps skipped.
	Slots  int    `json:"slots,omitzero"`
	Detail string `json:"detail"`
}

// Thresholds for events. A packager rounds audio PTS by a tick or two and
// video DTS by one, so neither counts.
const (
	retimeTolerance = 10 // ticks
	lengthHistory   = 50 // segments behind a channel's usual frame count
	lengthMinimum   = 5  // segments needed before lengths are judged
)

// Gap is an inclusive range of sequence numbers that were never analyzed.
type Gap struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// Chain holds one channel's comparison state. Segments must be added in
// sequence order; only consecutive sequence numbers are compared.
type Chain struct {
	th      Thresholds
	started bool
	prevSeq uint64
	prev    *Segment // nil after Break
	short   int64    // how early the previous segment's audio ended, when that was a fault
	av      avBaseline
	lengths []int   // recent video frame counts
	frames  []int64 // recent segments' video frame durations
	last    int64   // the frame duration the last segment was checked against
}

// NewChain returns a chain with no history.
func NewChain(th Thresholds) *Chain {
	return &Chain{th: th, av: avBaseline{
		need:   th.BaselineSegments,
		rebase: th.RebaselineAfter,
		tol:    ticks(th.AVOffsetMs),
	}}
}

// Add checks a segment on its own, against the previous segment when that is
// seq-1 and no discontinuity separates them, and against the A/V baseline.
func (c *Chain) Add(in Input) Result {
	var r Result
	frame := c.frame(in)
	c.last = frame
	var consecutive bool
	switch {
	case in.Order != nil:
		consecutive, r.Gap = c.started && in.Order.Adjacent, in.Order.Gap
	default:
		consecutive = c.started && in.Seq == c.prevSeq+1
		if c.started && in.Seq > c.prevSeq+1 {
			r.Gap = &Gap{From: c.prevSeq + 1, To: in.Seq - 1}
		}
	}
	r.Faults = checkSegment(in, c.th)
	if f, ok := corruption(in); ok {
		r.Faults = append(r.Faults, f)
	}
	faults, events := videoSteps(in.Seq, in.Segment.Video, frame, c.th)
	r.Faults, r.Events = append(r.Faults, faults...), append(r.Events, events...)
	if v := in.Segment.Video; v != nil {
		faults, events = checkPTS(in.Seq, v)
		r.Faults, r.Events = append(r.Faults, faults...), append(r.Events, events...)
	}
	if e, ok := c.oddLength(in, frame); ok {
		r.Events = append(r.Events, e)
	}
	faults, events = audioSteps(in.Seq, in.Segment.Audio, c.th)
	r.Faults, r.Events = append(r.Faults, faults...), append(r.Events, events...)
	if consecutive && c.prev != nil && !in.Discontinuity {
		faults, events := compare(c.prev, c.prevSeq, in, c.th, frame, c.short)
		r.Faults, r.Events = append(r.Faults, faults...), append(r.Events, events...)
	}
	base, haveBase := c.Baseline()
	if f, ok := c.checkAV(in); ok {
		r.Faults = append(r.Faults, f)
	}
	c.short = 0
	if f, short, ok := audioCoverage(in.Seq, in.Segment, frame, base, haveBase, c.th); ok {
		r.Faults = append(r.Faults, f)
		c.short = short
	}
	c.started, c.prevSeq, c.prev = true, in.Seq, in.Segment
	return r
}

// presentation compares presentation across a boundary with decoding: the
// next segment's first picture is due one frame after the previous
// segment's last, moved by the DTS deviation dev. What DTS doesn't explain
// (a changed reorder delay) is an event, or a video_pts_gap fault at
// VideoGapFaultMs or more.
func presentation(pv, cv *Video, seq uint64, ps *uint64, frame, dev, tol int64, th Thresholds) (*Fault, Event, bool) {
	expected := ts.Add(pv.MaxPTS(), frame)
	pdev := ts.Diff(cv.MinPTS(), expected)
	switch off := pdev - dev; {
	case abs(off) <= tol:
		return nil, Event{}, false
	case abs(off) < ticks(th.VideoGapFaultMs):
		return nil, Event{
			Type: EventPresentationGap, Seq: seq, GapMs: ms(off),
			Detail: fmt.Sprintf("first picture due %+.3f ms from one frame after the previous segment's last picture, %+.3f ms more than the DTS step shows",
				ms(pdev), ms(off)),
		}, true
	default:
		return &Fault{
			Type: FaultVideoPTSGap, Seq: seq, PrevSeq: ps,
			Message: fmt.Sprintf("first picture due %+.3f ms from one frame after the previous segment's last picture, %+.3f ms more than the DTS step shows",
				ms(pdev), ms(off)),
			Values: map[string]any{
				"prev_max_pts":     pv.MaxPTS(),
				"min_pts":          cv.MinPTS(),
				"expected_pts":     expected,
				"pts_deviation_ms": ms(pdev),
				"dts_deviation_ms": ms(dev),
				"deviation_ms":     ms(off),
				"frame_ms":         ms(frame),
			},
		}, Event{}, true
	}
}

// corruption reports the bytes and packets the analysis had to skip.
func corruption(in Input) (Fault, bool) {
	s := in.Segment
	if s.SkippedBytes == 0 && s.TEIPackets == 0 {
		return Fault{}, false
	}
	return Fault{
		Type: FaultTSCorruption,
		Seq:  in.Seq,
		Message: fmt.Sprintf("%d bytes that were not TS packets and %d packets flagged transport_error_indicator were skipped",
			s.SkippedBytes, s.TEIPackets),
		Values: map[string]any{"skipped_bytes": s.SkippedBytes, "tei_packets": s.TEIPackets, "packets": s.Packets},
	}, true
}

// audioCoverage checks that a segment's audio lasts as long as its video:
// it may end no more than AudioGapFaultFrames AAC frames before the
// video's last frame ends, after the channel's A/V start offset (base,
// video minus audio) when known. short is how early it ended.
func audioCoverage(seq uint64, seg *Segment, frame, base int64, haveBase bool, th Thresholds) (Fault, int64, bool) {
	v, a := seg.Video, seg.Audio
	if v == nil || a == nil || len(v.AUs) == 0 || len(a.PES) == 0 || frame <= 0 || !a.Timed() {
		return Fault{}, 0, false
	}
	if !haveBase {
		base = 0
	}
	videoEnd := ts.Add(v.MaxPTS(), frame)
	short := ts.Diff(videoEnd, a.End()) - base
	if short <= audioLimit(a, th) {
		return Fault{}, 0, false
	}
	return Fault{
		Type: FaultAudioCoverage,
		Seq:  seq,
		Message: fmt.Sprintf("audio ends %.3f ms before the video does (more than %g AAC frames)",
			ms(short), th.AudioGapFaultFrames),
		Values: map[string]any{
			"short_ms":      ms(short),
			"video_end_pts": videoEnd,
			"audio_end_pts": a.End(),
			"av_base_ms":    ms(base),
			"limit_ms":      ms(audioLimit(a, th)),
		},
	}, short, true
}

// audioLimit is the largest audio hole or overlap that is only an event:
// AudioGapFaultFrames AAC frames, and the two ticks a packager rounds by.
func audioLimit(a *Audio, th Thresholds) int64 {
	return int64(math.Round(th.AudioGapFaultFrames*float64(a.frameTicks()))) + 2
}

// Break records that seq exists but could not be analyzed, so the next
// segment is neither compared with it nor reported as a monitor gap.
func (c *Chain) Break(seq uint64) {
	c.started, c.prevSeq, c.prev = true, seq, nil
}

// frame is the video frame duration segments are checked against: the
// playlist's when it declares one, otherwise the most common frame of the
// recent segments, this one included. A few segments at a lower rate (an
// encoder dropping frames during black) don't change it.
func (c *Chain) frame(in Input) int64 {
	if v := in.Segment.Video; v != nil && v.FrameTicks > 0 {
		c.frames = append(c.frames, v.FrameTicks)
		if len(c.frames) > lengthHistory {
			c.frames = c.frames[1:]
		}
	}
	if in.FrameTicks > 0 {
		return in.FrameTicks
	}
	counts := map[int64]int{}
	var best int64
	for _, f := range c.frames {
		counts[f]++
		if n := counts[f]; n > counts[best] || n == counts[best] && f < best {
			best = f
		}
	}
	return best
}

// Frame is the video frame duration the last segment added was checked
// against (see frame), or 0 before any.
func (c *Chain) Frame() int64 { return c.last }

// Baseline returns the A/V offset baseline in ticks, once established.
func (c *Chain) Baseline() (int64, bool) { return c.av.base, c.av.have }

func checkSegment(in Input, th Thresholds) []Fault {
	var out []Fault
	seg := in.Segment
	if v := seg.Video; v != nil {
		out = append(out, checkDTSOrder(in.Seq, v)...)
		out = append(out, checkPCR(in.Seq, v, th)...)
	}
	if in.ExtInf > 0 {
		if d, ok := seg.Duration(); ok {
			if diff := (d - in.ExtInf) / in.ExtInf; math.Abs(diff) > th.DurationFraction {
				out = append(out, Fault{
					Type: FaultDurationMismatch,
					Seq:  in.Seq,
					Message: fmt.Sprintf("real duration %.3f s is %+.1f%% from EXTINF %.3f s",
						d, diff*100, in.ExtInf),
					Values: map[string]any{
						"extinf_s": in.ExtInf,
						"actual_s": round3(d),
						"diff_pct": round3(diff * 100),
					},
				})
			}
		}
	}
	return out
}

func checkDTSOrder(seq uint64, v *Video) []Fault {
	count, first := 0, 0
	for i := 1; i < len(v.AUs); i++ {
		if ts.Diff(v.AUs[i].DTS, v.AUs[i-1].DTS) <= 0 {
			if count == 0 {
				first = i
			}
			count++
		}
	}
	if count == 0 {
		return nil
	}
	cur, prev := v.AUs[first].DTS, v.AUs[first-1].DTS
	delta := ts.Diff(cur, prev)
	return []Fault{{
		Type: FaultVideoDTSNotIncreasing,
		Seq:  seq,
		Message: fmt.Sprintf("video DTS did not increase at frame %d (%d -> %d, %+.3f ms); %d frame(s) affected",
			first, prev, cur, ms(delta), count),
		Values: map[string]any{
			"count":    count,
			"index":    first,
			"prev_dts": prev,
			"dts":      cur,
			"delta_ms": ms(delta),
		},
	}}
}

// checkPTS flags pictures due before they are decoded (PTS < DTS), which
// is a fault whatever the amount. Pictures that share a PTS are two due in
// one frame slot, an overlap of one frame: under the size rule an event.
func checkPTS(seq uint64, v *Video) ([]Fault, []Event) {
	seen := make(map[uint64]int, len(v.AUs))
	dups, before, firstBefore := 0, 0, 0
	var dupDetail string
	for i, au := range v.AUs {
		if au.NoPTS {
			continue
		}
		if j, ok := seen[au.PTS]; ok {
			if dups == 0 {
				dupDetail = fmt.Sprintf("frames %d and %d share PTS %d", j, i, au.PTS)
			}
			dups++
		} else {
			seen[au.PTS] = i
		}
		if ts.Diff(au.PTS, au.DTS) < 0 {
			if before == 0 {
				firstBefore = i
			}
			before++
		}
	}
	var faults []Fault
	var events []Event
	if dups > 0 {
		events = append(events, Event{
			Type: EventDuplicatePTS, Seq: seq, Count: dups,
			Detail: fmt.Sprintf("%d picture(s) due at the same instant as another; %s", dups, dupDetail),
		})
	}
	if before > 0 {
		au := v.AUs[firstBefore]
		faults = append(faults, Fault{
			Type: FaultVideoPTSError,
			Seq:  seq,
			Message: fmt.Sprintf("frame %d is due %.3f ms before it is decoded (PTS %d < DTS %d); %d frame(s) like it",
				firstBefore, ms(ts.Diff(au.DTS, au.PTS)), au.PTS, au.DTS, before),
			Values: map[string]any{
				"pts_before_dts": before,
				"index":          firstBefore,
				"pts":            au.PTS,
				"dts":            au.DTS,
			},
		})
	}
	return faults, events
}

// checkPCR flags video PES whose PTS is not ahead of the last PCR before
// them, and jumps in the PTS-PCR offset between consecutive PES.
func checkPCR(seq uint64, v *Video, th Thresholds) []Fault {
	var out []Fault
	behind, firstBehind := 0, 0
	var worst int64
	jumps, jumpAt := 0, 0
	var jump, jumpPrev int64
	var prevOff int64
	havePrev := false
	for i, au := range v.AUs {
		if !au.HasPCR || au.NoPTS {
			continue
		}
		off := ts.Diff(au.PTS, au.PCR)
		if off <= 0 {
			if behind == 0 {
				firstBehind, worst = i, off
			}
			behind++
			worst = min(worst, off)
		}
		if havePrev {
			if j := off - prevOff; abs(j) > ticks(th.PCRJumpMs) {
				if jumps == 0 || abs(j) > abs(jump) {
					jumpAt, jump, jumpPrev = i, j, prevOff
				}
				jumps++
			}
		}
		prevOff, havePrev = off, true
	}
	if behind > 0 {
		au := v.AUs[firstBehind]
		out = append(out, Fault{
			Type: FaultPTSBehindPCR,
			Seq:  seq,
			Message: fmt.Sprintf("video PTS behind PCR in %d frame(s), first at frame %d (PTS %d, PCR %d); worst %.3f ms",
				behind, firstBehind, au.PTS, au.PCR, ms(worst)),
			Values: map[string]any{
				"count":           behind,
				"index":           firstBehind,
				"pts":             au.PTS,
				"pcr":             au.PCR,
				"offset_ms":       ms(ts.Diff(au.PTS, au.PCR)),
				"worst_offset_ms": ms(worst),
			},
		})
	}
	if jumps > 0 {
		au := v.AUs[jumpAt]
		out = append(out, Fault{
			Type: FaultPTSPCRJump,
			Seq:  seq,
			Message: fmt.Sprintf("PTS-PCR offset jumped %+.3f ms at frame %d (%.3f -> %.3f ms); %d jump(s) over %.0f ms",
				ms(jump), jumpAt, ms(jumpPrev), ms(jumpPrev+jump), jumps, th.PCRJumpMs),
			Values: map[string]any{
				"count":          jumps,
				"index":          jumpAt,
				"pts":            au.PTS,
				"pcr":            au.PCR,
				"prev_offset_ms": ms(jumpPrev),
				"offset_ms":      ms(jumpPrev + jump),
				"jump_ms":        ms(jump),
			},
		})
	}
	return out
}

// compare checks that cur continues prev: video DTS one frame on, audio PTS
// right after the last audio PES, and no PTS-PCR jump at the boundary.
// frame is the channel's video frame duration. A gap or overlap of
// VideoGapFaultMs or more (video), or of more than AudioGapFaultFrames AAC
// frames (audio), is a fault; a smaller one is an event. short is how
// early prev's audio ended when that was already a fault: the hole it
// left is not reported again here.
func compare(prev *Segment, prevSeq uint64, in Input, th Thresholds, frame, short int64) ([]Fault, []Event) {
	var out []Fault
	var events []Event
	cur := in.Segment
	tol := ticks(th.ContinuityMs)
	ps := new(prevSeq)

	if pv := prev.Video; pv != nil {
		if cv := cur.Video; cv == nil {
			out = append(out, Fault{
				Type: FaultVideoDTSGap, Seq: in.Seq, PrevSeq: ps,
				Message: "segment has no video; the previous segment did",
				Values:  map[string]any{"prev_last_dts": pv.LastDTS()},
			})
		} else {
			frame := cmp.Or(frame, pv.FrameTicks, cv.FrameTicks)
			gap := ts.Diff(cv.FirstDTS(), pv.LastDTS())
			dev := gap - frame
			f, e, ok := presentation(pv, cv, in.Seq, ps, frame, dev, tol, th)
			switch {
			case !ok:
			case f != nil:
				out = append(out, *f)
			default:
				events = append(events, e)
			}
			switch {
			case abs(dev) <= tol:
			case abs(dev) < ticks(th.VideoGapFaultMs):
				events = append(events, Event{
					Type: EventVideoGap, Seq: in.Seq, GapMs: ms(dev),
					Detail: fmt.Sprintf("video DTS moved %+.3f ms from the previous segment's last frame; expected one %.3f ms frame (off by %+.3f ms)",
						ms(gap), ms(frame), ms(dev)),
				})
			default:
				out = append(out, Fault{
					Type: FaultVideoDTSGap, Seq: in.Seq, PrevSeq: ps,
					Message: fmt.Sprintf("video DTS moved %+.3f ms from the previous segment's last frame; expected one %.3f ms frame (off by %+.3f ms)",
						ms(gap), ms(frame), ms(dev)),
					Values: map[string]any{
						"prev_last_dts": pv.LastDTS(),
						"first_dts":     cv.FirstDTS(),
						"expected_dts":  ts.Add(pv.LastDTS(), frame),
						"gap_ms":        ms(gap),
						"frame_ms":      ms(frame),
						"deviation_ms":  ms(dev),
					},
				})
			}
		}
	}

	if pa := prev.Audio; pa != nil && pa.Timed() {
		if ca := cur.Audio; ca == nil {
			out = append(out, Fault{
				Type: FaultAudioPTSGap, Seq: in.Seq, PrevSeq: ps,
				Message: "segment has no audio; the previous segment did",
				Values:  map[string]any{"prev_end_pts": pa.End()},
			})
		} else {
			dev := ts.Diff(ca.FirstPTS(), pa.End())
			if short > 0 && abs(dev-short) <= audioLimit(pa, th) {
				dev = 0 // the hole already reported on the previous segment
			}
			switch {
			case abs(dev) <= tol:
			case abs(dev) <= audioLimit(pa, th):
				events = append(events, Event{
					Type: EventAudioGap, Seq: in.Seq, GapMs: ms(dev),
					Detail: fmt.Sprintf("audio starts %+.3f ms from where the previous segment's audio ended (within %g AAC frames of %.3f ms)",
						ms(dev), th.AudioGapFaultFrames, ms(pa.frameTicks())),
				})
			default:
				out = append(out, Fault{
					Type: FaultAudioPTSGap, Seq: in.Seq, PrevSeq: ps,
					Message: fmt.Sprintf("audio PTS starts %+.3f ms from where the previous segment's audio ended",
						ms(dev)),
					Values: map[string]any{
						"prev_last_pts": pa.LastPTS(),
						"prev_end_pts":  pa.End(),
						"first_pts":     ca.FirstPTS(),
						"gap_ms":        ms(ts.Diff(ca.FirstPTS(), pa.LastPTS())),
						"frame_ms":      ms(pa.LastTicks()),
						"deviation_ms":  ms(dev),
					},
				})
			}
		}
	}

	if po, pau, ok := lastPTSPCR(prev); ok {
		if co, cau, ok := firstPTSPCR(cur); ok {
			if j := co - po; abs(j) > ticks(th.PCRJumpMs) {
				out = append(out, Fault{
					Type: FaultPTSPCRJump, Seq: in.Seq, PrevSeq: ps,
					Message: fmt.Sprintf("PTS-PCR offset jumped %+.3f ms across the segment boundary (%.3f -> %.3f ms)",
						ms(j), ms(po), ms(co)),
					Values: map[string]any{
						"count":           1,
						"index":           0,
						"across_boundary": true,
						"prev_pts":        pau.PTS,
						"prev_pcr":        pau.PCR,
						"pts":             cau.PTS,
						"pcr":             cau.PCR,
						"prev_offset_ms":  ms(po),
						"offset_ms":       ms(co),
						"jump_ms":         ms(j),
					},
				})
			}
		}
	}
	return out, events
}

// videoSteps checks the DTS steps inside a segment against the channel's
// frame: steps VideoGapFaultMs or more longer than a frame are one fault
// (the largest named, all counted); shorter skips (steps over 1.25 frames)
// are a frame_gap event, and steps under 0.75 frames a frame_overlap
// event. A step that doesn't advance is checkDTSOrder's.
func videoSteps(seq uint64, v *Video, frame int64, th Thresholds) ([]Fault, []Event) {
	if v == nil {
		return nil, nil
	}
	if frame = cmp.Or(frame, v.FrameTicks); frame <= 0 {
		return nil, nil
	}
	var events []Event
	gaps, slots, overlaps, gapAt, bigs, bigAt := 0, 0, 0, 0, 0, 0
	var missing, overlapped, largest, bigMissing, bigStep int64
	for i := 1; i < len(v.AUs); i++ {
		step := ts.Diff(v.AUs[i].DTS, v.AUs[i-1].DTS)
		switch extra := step - frame; {
		case step <= 0:
		case extra >= ticks(th.VideoGapFaultMs):
			bigs++
			bigMissing += extra
			if step > bigStep {
				bigStep, bigAt = step, i
			}
		case extra > frame/4:
			gaps++
			slots += max(1, int(math.Round(float64(extra)/float64(frame))))
			missing += extra
			if step > largest {
				largest, gapAt = step, i
			}
		case -extra > frame/4:
			overlaps++
			overlapped -= extra
		}
	}
	if gaps > 0 {
		events = append(events, Event{
			Type: EventFrameGap, Seq: seq, GapMs: ms(missing), Count: gaps, Slots: slots,
			Detail: fmt.Sprintf("%d frame gap(s), %d frame slot(s) and %.3f ms missing; largest DTS step %.3f ms, before frame %d",
				gaps, slots, ms(missing), ms(largest), gapAt),
		})
	}
	if overlaps > 0 {
		events = append(events, Event{
			Type: EventFrameOverlap, Seq: seq, GapMs: -ms(overlapped), Count: overlaps,
			Detail: fmt.Sprintf("%d DTS step(s) shorter than 0.75 frames, %.3f ms overlapped", overlaps, ms(overlapped)),
		})
	}
	if bigs == 0 {
		return nil, events
	}
	return []Fault{{
		Type: FaultVideoDTSGap,
		Seq:  seq,
		Message: fmt.Sprintf("video DTS jumps %.3f ms between frames %d and %d inside the segment (%.3f ms of video missing; %d such jump(s), %.3f ms in all)",
			ms(bigStep), bigAt-1, bigAt, ms(bigStep-frame), bigs, ms(bigMissing)),
		Values: map[string]any{
			"inside":       true,
			"count":        bigs,
			"index":        bigAt,
			"prev_dts":     v.AUs[bigAt-1].DTS,
			"dts":          v.AUs[bigAt].DTS,
			"gap_ms":       ms(bigStep),
			"frame_ms":     ms(frame),
			"deviation_ms": ms(bigStep - frame),
			"missing_ms":   ms(bigMissing),
		},
	}}, events
}

// audioSteps checks the audio PTS steps inside a segment against each
// PES's duration: holes and overlaps of more than AudioGapFaultFrames AAC
// frames are one fault (the largest named, all counted); smaller
// re-timing (beyond retimeTolerance) is an audio_retimed event.
func audioSteps(seq uint64, a *Audio, th Thresholds) ([]Fault, []Event) {
	if a == nil || !a.Timed() {
		return nil, nil
	}
	limit := audioLimit(a, th)
	n, pairs, bigs, bigAt := 0, 0, 0, 0
	var net, bigDev, bigNet int64
	lo, hi := int64(math.MaxInt64), int64(math.MinInt64)
	for i := 1; i < len(a.PES); i++ {
		d := a.PES[i-1].Ticks
		if d <= 0 {
			continue
		}
		step := ts.Diff(a.PES[i].PTS, a.PES[i-1].PTS)
		dev := step - d
		if abs(dev) > limit {
			bigs++
			bigNet += dev
			if abs(dev) > abs(bigDev) {
				bigDev, bigAt = dev, i
			}
			continue
		}
		pairs++
		net += dev
		lo, hi = min(lo, step), max(hi, step)
		if abs(dev) > retimeTolerance {
			n++
		}
	}
	var events []Event
	if n > 0 {
		events = append(events, Event{
			Type: EventAudioRetimed, Seq: seq, GapMs: ms(net), Count: n,
			Detail: fmt.Sprintf("%d of %d audio steps off by more than %.3f ms (steps %d..%d ticks); net %+.3f ms",
				n, pairs, ms(retimeTolerance), lo, hi, ms(net)),
		})
	}
	if bigs == 0 {
		return nil, events
	}
	kind := "hole"
	if bigDev < 0 {
		kind = "overlap"
	}
	d := a.PES[bigAt-1].Ticks
	return []Fault{{
		Type: FaultAudioPTSGap,
		Seq:  seq,
		Message: fmt.Sprintf("audio %s of %+.3f ms between PES %d and %d inside the segment (%d hole(s) or overlap(s) over %g AAC frames, net %+.3f ms)",
			kind, ms(bigDev), bigAt-1, bigAt, bigs, th.AudioGapFaultFrames, ms(bigNet)),
		Values: map[string]any{
			"inside":       true,
			"count":        bigs,
			"index":        bigAt,
			"prev_pts":     a.PES[bigAt-1].PTS,
			"prev_end_pts": ts.Add(a.PES[bigAt-1].PTS, d),
			"pts":          a.PES[bigAt].PTS,
			"frame_ms":     ms(a.frameTicks()),
			"deviation_ms": ms(bigDev),
			"net_ms":       ms(bigNet),
			"limit_ms":     ms(limit),
		},
	}}, events
}

// oddLength compares the segment's video frame count with the channel's
// usual one: the most common count among its recent segments.
func (c *Chain) oddLength(in Input, frame int64) (Event, bool) {
	v := in.Segment.Video
	if v == nil {
		return Event{}, false
	}
	n := len(v.AUs)
	history := c.lengths
	c.lengths = append(c.lengths, n)
	if len(c.lengths) > lengthHistory {
		c.lengths = c.lengths[1:]
	}
	if len(history) < lengthMinimum {
		return Event{}, false
	}
	usual := mostCommon(history)
	if n == usual {
		return Event{}, false
	}
	diff := int64(n-usual) * cmp.Or(frame, v.FrameTicks)
	return Event{
		Type: EventOddLength, Seq: in.Seq, GapMs: ms(diff),
		Detail: fmt.Sprintf("%d video frames where this channel's segments usually have %d (%+.3f ms)", n, usual, ms(diff)),
	}, true
}

// mostCommon returns the most frequent value, the larger one on a tie.
func mostCommon(xs []int) int {
	counts := map[int]int{}
	best, bestN := 0, 0
	for _, x := range xs {
		counts[x]++
		if n := counts[x]; n > bestN || n == bestN && x > best {
			best, bestN = x, n
		}
	}
	return best
}

func firstPTSPCR(s *Segment) (int64, AU, bool) {
	if s.Video != nil {
		for _, au := range s.Video.AUs {
			if au.HasPCR {
				return ts.Diff(au.PTS, au.PCR), au, true
			}
		}
	}
	return 0, AU{}, false
}

func lastPTSPCR(s *Segment) (int64, AU, bool) {
	if s.Video != nil {
		for i := len(s.Video.AUs) - 1; i >= 0; i-- {
			if au := s.Video.AUs[i]; au.HasPCR {
				return ts.Diff(au.PTS, au.PCR), au, true
			}
		}
	}
	return 0, AU{}, false
}

func (c *Chain) checkAV(in Input) (Fault, bool) {
	if c.th.BaselineSegments <= 0 {
		return Fault{}, false
	}
	off, ok := in.Segment.AVOffset()
	if !ok {
		return Fault{}, false
	}
	base := c.av.base
	flagged, moved := c.av.observe(off)
	if !flagged {
		return Fault{}, false
	}
	f := Fault{
		Type: FaultAVOffset,
		Seq:  in.Seq,
		Message: fmt.Sprintf("A/V start offset %.3f ms is %+.3f ms from the channel baseline %.3f ms",
			ms(off), ms(off-base), ms(base)),
		Values: map[string]any{
			"offset_ms":       ms(off),
			"baseline_ms":     ms(base),
			"deviation_ms":    ms(off - base),
			"video_start_pts": in.Segment.Video.MinPTS(),
			"audio_start_pts": in.Segment.Audio.MinPTS(),
		},
	}
	if moved {
		f.Message += fmt.Sprintf("; sustained, so the baseline moved to %.3f ms", ms(c.av.base))
		f.Values["new_baseline_ms"] = ms(c.av.base)
	}
	return f, true
}

// avBaseline is the median A/V start offset of the first segments. A run of
// out-of-range offsets that agree with each other moves it, so a permanent
// shift is reported once rather than on every segment forever.
type avBaseline struct {
	need, rebase int
	tol          int64
	samples      []int64
	base         int64
	have         bool
	run          []int64
}

func (b *avBaseline) observe(off int64) (flagged, moved bool) {
	if !b.have {
		b.samples = append(b.samples, off)
		if len(b.samples) >= b.need {
			b.base, b.have, b.samples = median(b.samples), true, nil
		}
		return false, false
	}
	if abs(off-b.base) <= b.tol {
		b.run = b.run[:0]
		return false, false
	}
	if b.rebase <= 0 {
		return true, false
	}
	b.run = append(b.run, off)
	if len(b.run) > b.rebase {
		b.run = b.run[1:]
	}
	if len(b.run) == b.rebase {
		m := median(b.run)
		for _, o := range b.run {
			if abs(o-m) > b.tol {
				return true, false
			}
		}
		b.base, b.run = m, b.run[:0]
		return true, true
	}
	return true, false
}

// ms converts ticks to milliseconds rounded to the microsecond.
func ms(t int64) float64 { return round3(ts.Millis(t)) }

func ticks(ms float64) int64 { return int64(math.Round(ms * ts.Hz / 1000)) }

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
