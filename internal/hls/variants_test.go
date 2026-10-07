package hls

import "testing"

// An audio-only variant (CODECS lists no video codec) is never picked by
// highest or lowest: watching it would turn the black check off.
func TestHighestAndLowestSkipAudioOnlyVariants(t *testing.T) {
	m, err := ParseMaster([]byte("#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=64000,CODECS=\"mp4a.40.2\"\naudio.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000,CODECS=\"avc1.4d401e,mp4a.40.2\"\nlow.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720,CODECS=\"avc1.64001f,mp4a.40.2\"\nhigh.m3u8\n"))
	if err != nil {
		t.Fatal(err)
	}
	for sel, want := range map[string]string{"lowest": "low.m3u8", "highest": "high.m3u8", "0": "audio.m3u8"} {
		v, err := SelectVariant(m.Variants, sel)
		if err != nil || v.URI != want {
			t.Errorf("%s: %s, %v; want %s", sel, v.URI, err, want)
		}
	}
	if _, err := SelectVariant(m.Variants[:1], "lowest"); err == nil {
		t.Error("only an audio-only variant: lowest picked it")
	}
}

// A variant is labelled audio only when its CODECS say so: a video variant
// without RESOLUTION is not audio.
func TestVariantLabels(t *testing.T) {
	for _, tc := range []struct {
		v    Variant
		want string
	}{
		{Variant{Index: 0, Bandwidth: 64000, Codecs: "mp4a.40.2"}, "0_audio_64000"},
		{Variant{Index: 1, Bandwidth: 800000, Codecs: "avc1.4d401e,mp4a.40.2"}, "1_unknown_800000"},
		{Variant{Index: 2, Bandwidth: 800000}, "2_unknown_800000"},
		{Variant{Index: 3, Bandwidth: 3000000, Resolution: "1280x720"}, "3_1280x720_3000000"},
	} {
		if got := tc.v.Label(); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.v, got, tc.want)
		}
	}
}
