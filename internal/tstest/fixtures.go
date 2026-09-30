package tstest

import "github.com/amillerrr/stream-analyzer/internal/ts"

// Base is the timeline behind the cont_*, jump_1, pcr_*, dts_repeat and
// avshift fixtures: video from 10 s, audio starting 16 ms after the first
// video PTS (like the origin's streams), PCR 700 ms behind DTS, and 16-frame
// (0.534 s) segments.
var Base = Timeline{
	VideoStart:       900000,
	AudioStart:       900000 + 2*FrameTicks + 1440,
	FramesPerSegment: 16,
	PCRLead:          63000,
}

// Wrapping is Base moved to the 33-bit wrap point: video DTS wraps five
// frames into segment 0, audio wraps two PES in, and the PCR (700 ms behind)
// wraps ten frames into segment 1.
var Wrapping = Timeline{
	VideoStart:       ts.Wrap - 5*FrameTicks,
	AudioStart:       ts.Wrap - 5*FrameTicks + 2*FrameTicks + 1440,
	FramesPerSegment: 16,
	PCRLead:          63000,
}

// WrapAtBoundary puts the 33-bit wrap exactly on a segment boundary: the
// last frame of segment 0 has DTS 2^33-3003 and segment 1 starts at DTS 0.
var WrapAtBoundary = Timeline{
	VideoStart:       ts.Wrap - 16*FrameTicks,
	AudioStart:       ts.Wrap - 16*FrameTicks + 2*FrameTicks + 1440,
	FramesPerSegment: 16,
	PCRLead:          63000,
}

// Fixtures returns every committed fixture by file name.
func Fixtures() map[string][]byte {
	jumped := Base
	jumped.VideoStart += 900000 // everything 10 s later: an encoder restart
	jumped.AudioStart += 900000

	pcrBehind := Base
	pcrBehind.PCRLead = -45000 // PCR 500 ms ahead of DTS, so every PTS is behind it

	pcrJump := Base.Segment(0)
	for i := 8; i < len(pcrJump.Video); i++ {
		// The PCR lead drops from 700 ms to 100 ms at frame 8.
		pcrJump.Video[i].PCR = new(ts.Add(pcrJump.Video[i].DTS, -9000))
	}

	dtsRepeat := Base.Segment(0)
	dtsRepeat.Video[7].DTS = dtsRepeat.Video[6].DTS

	avShift := Base.Segment(3)
	for i := range avShift.Audio {
		avShift.Audio[i].PTS = ts.Add(avShift.Audio[i].PTS, 18000) // audio 200 ms late
	}

	return map[string][]byte{
		"cont_0.ts":     Base.Segment(0).Bytes(),
		"cont_1.ts":     Base.Segment(1).Bytes(),
		"cont_2.ts":     Base.Segment(2).Bytes(),
		"wrap_0.ts":     Wrapping.Segment(0).Bytes(),
		"wrap_1.ts":     Wrapping.Segment(1).Bytes(),
		"wrapb_0.ts":    WrapAtBoundary.Segment(0).Bytes(),
		"wrapb_1.ts":    WrapAtBoundary.Segment(1).Bytes(),
		"jump_1.ts":     jumped.Segment(1).Bytes(),
		"pcr_behind.ts": pcrBehind.Segment(0).Bytes(),
		"pcr_jump.ts":   pcrJump.Bytes(),
		"dts_repeat.ts": dtsRepeat.Bytes(),
		"avshift.ts":    avShift.Bytes(),
	}
}
