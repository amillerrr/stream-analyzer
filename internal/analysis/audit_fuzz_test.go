package analysis

// Audit: fuzz targets for segment analysis (TS, PES, PCR and
// ADTS parsing together) and for the checks that run on its output.

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/ts"
	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

func addFixtureSeeds(f *testing.F) {
	paths, _ := filepath.Glob(tstest.Path("*.ts"))
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("<html><body>504 Gateway Timeout</body></html>"))
}

func FuzzAnalyze(f *testing.F) {
	addFixtureSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		start := time.Now()
		seg, err := Analyze(b)
		if err != nil {
			return
		}
		sum := seg.Summary() // must not panic
		if math.IsNaN(sum.DurationS) || math.IsInf(sum.DurationS, 0) {
			t.Fatalf("duration %v", sum.DurationS)
		}
		if v := seg.Video; v != nil {
			if len(v.AUs) == 0 {
				t.Fatal("video with no AUs")
			}
			for i, au := range v.AUs {
				if au.PTS >= ts.Wrap || au.DTS >= ts.Wrap || au.PCR >= ts.Wrap {
					t.Fatalf("AU %d has a timestamp wider than 33 bits: %+v", i, au)
				}
			}
			if v.FrameTicks < 0 {
				t.Fatalf("negative frame ticks %d", v.FrameTicks)
			}
		}
		if a := seg.Audio; a != nil {
			for _, p := range a.PES {
				if p.Frames < 0 || p.Ticks < 0 {
					t.Fatalf("audio PES %+v", p)
				}
			}
		}
		// The checks must cope with whatever Analyze returns, including a
		// segment compared with itself and with a copy one frame later.
		c := NewChain(DefaultThresholds())
		for i := range uint64(7) {
			c.Add(Input{Seq: 100 + i, ExtInf: 6.006, Segment: seg})
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("took %v for %d bytes", d, len(b))
		}
	})
}

func FuzzParseADTS(f *testing.F) {
	f.Add([]byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0xFF, 0xFC, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		frames, rate := parseADTS(b)
		if frames < 0 || frames > len(b) {
			t.Fatalf("%d frames in %d bytes", frames, len(b))
		}
		if frames > 0 && rate <= 0 {
			t.Fatalf("%d frames at rate %d", frames, rate)
		}
	})
}
