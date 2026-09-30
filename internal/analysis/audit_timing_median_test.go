package analysis

// Audit (2026-09-30): "one frame" is the previous segment's median DTS step.
// Real segments from incident 20260929T211715Z_channel1: every rendition
// has the same boundary (61787630 -> 61787631, a 6005-tick DTS step), but the
// 512x288 rendition dropped every other frame during the black stretch, so
// its median step is 6006 and the same boundary passes there. The
// incident's verdicts say not_reproduced for that rendition only.
//
// Reads ../../data (git-ignored); skipped when it is not there.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAuditMedianFrameVerdict(t *testing.T) {
	inc := filepath.Join("..", "..", "data", "incidents", "20260929T211715Z_channel1")
	dirs := map[string]string{
		"1280x720 (monitored)": filepath.Join(inc, "segments"),
		"640x360":              filepath.Join(inc, "renditions", "1_640x360_1300000"),
		"512x288":              filepath.Join(inc, "renditions", "0_512x288_900000"),
	}
	verdicts := map[string]bool{}
	for name, dir := range dirs {
		var segs []*Segment
		for _, q := range []string{"61787630", "61787631"} {
			b, err := os.ReadFile(filepath.Join(dir, "seg_"+q+".ts"))
			if err != nil {
				t.Skipf("real data not available: %v", err)
			}
			s, err := Analyze(b)
			if err != nil {
				t.Fatal(err)
			}
			segs = append(segs, s)
		}
		c := NewChain(DefaultThresholds())
		c.Add(Input{Seq: 61787630, ExtInf: 6.006, Segment: segs[0]})
		r := c.Add(Input{Seq: 61787631, ExtInf: 6.006, Segment: segs[1]})
		gap := false
		for _, f := range r.Faults {
			gap = gap || f.Type == FaultVideoDTSGap
		}
		verdicts[name] = gap
		t.Logf("%-21s prev last DTS %d, first DTS %d (step %d), prev FrameTicks %d -> video_dts_gap %v",
			name, segs[0].Video.LastDTS(), segs[1].Video.FirstDTS(), int64(segs[1].Video.FirstDTS()-segs[0].Video.LastDTS()), segs[0].Video.FrameTicks, gap)
	}
	if fmt.Sprint(verdicts["512x288"]) != fmt.Sprint(verdicts["1280x720 (monitored)"]) {
		t.Errorf("the same boundary timestamps give different verdicts: %v", verdicts)
	}
}
