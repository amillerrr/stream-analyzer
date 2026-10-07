package blackdetect

import "testing"

// Tests from the false black-video investigation. They fail on purpose:
// the parser guesses, or drops lines silently, where it should fail.

// Lines blackdetect can print that are not a black run: a run that ends
// before it starts (timestamps went back inside the file), a duration that
// disagrees with the times, a missing timestamp (ffmpeg prints NOPTS) and a
// black line cut short. Each must be an error, not a guess and not a
// silent drop, so the segment counts as unchecked.
func TestDetectRejectsOddBlackLines(t *testing.T) {
	for _, tc := range []struct{ name, line string }{
		{"end before start", "black_start:1002.334267 black_end:11.4 black_duration:-990.934267"},
		{"duration disagrees", "black_start:1.5 black_end:2.5 black_duration:7"},
		{"no timestamp", "black_start:NOPTS black_end:1.5 black_duration:NOPTS"},
		{"cut short", "black_start:1.5 black_end:"},
		{"not a number", "black_start:1.5 black_end:nan black_duration:nan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ff := fakeFFmpeg(t, `echo "[blackdetect @ 0x1] `+tc.line+`" >&2
echo "frame=  180 fps=0.0" >&2
`)
			o := Default()
			o.FFmpeg = ff
			iv, err := Detect(t.Context(), "seg.ts", o)
			if err == nil {
				t.Errorf("%q: no error; intervals %+v", tc.line, iv)
			}
		})
	}
}
