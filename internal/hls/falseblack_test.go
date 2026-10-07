package hls

import (
	"strings"
	"testing"
)

// Tests from the false black-video investigation. They fail on purpose.

// A segment's number is its packager's own convention: a "seq=N" in its
// file name, or else its playlist position. HLS doesn't require numbers to
// be unique, and playlists from another delivery path can mix the two kinds
// (ad segments with no seq= among content with one), number each ad break
// from 1, or carry a "_seq=" in a token. A repeated number is no violation,
// and the key the monitor names its files and records by (the number and a
// hash of the URI) still differs. A seq= outside the file name is not a
// number at all.
func TestSegmentKeysAreUniqueWithinAPlaylist(t *testing.T) {
	for _, tc := range []struct{ name, prev, body string }{
		{"a token in the query ends in _seq=", "", "#EXT-X-MEDIA-SEQUENCE:100\n" +
			"#EXTINF:6.006,\nchunk_00101.ts?session_seq=7\n" +
			"#EXTINF:6.006,\nchunk_00102.ts?session_seq=7\n"},
		{"inserted ads without seq= among content with seq=", "", "#EXT-X-MEDIA-SEQUENCE:100\n" +
			"#EXTINF:6.006,\nfeed-seq=103.ts\n" +
			"#EXTINF:6.006,\nfeed-seq=104.ts\n" +
			"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nhttps://ads.example.com/creative/1080p_0001.ts\n" +
			"#EXTINF:6.006,\nhttps://ads.example.com/creative/1080p_0002.ts\n"},
		{"each ad break numbers its segments from 1", "", "#EXT-X-MEDIA-SEQUENCE:100\n" +
			"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nhttps://ads.example.com/break-seq=1.ts\n" +
			"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nfeed-seq=500.ts\n" +
			"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nhttps://ads.example.com/break-seq=1.ts?break=2\n"},
		{"an ad break's number again after the first left the window",
			"#EXT-X-MEDIA-SEQUENCE:100\n" +
				"#EXTINF:6.006,\nhttps://ads.example.com/break-seq=1.ts\n" +
				"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nfeed-seq=500.ts\n",
			"#EXT-X-MEDIA-SEQUENCE:101\n" +
				"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nfeed-seq=500.ts\n" +
				"#EXT-X-DISCONTINUITY\n#EXTINF:6.006,\nhttps://ads.example.com/break-seq=1.ts?break=2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parse := func(body string) *Media {
				pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n" + body))
				if err != nil {
					t.Fatal(err)
				}
				return pl
			}
			pl := parse(tc.body)
			seen := map[string]string{}
			for _, s := range pl.Segments {
				if other, ok := seen[s.Key()]; ok {
					t.Errorf("%s and %s share the key %s", other, s.URI, s.Key())
				}
				seen[s.Key()] = s.URI
			}
			// Each is new in turn, as the playlist grows, or after prev.
			if tc.prev != "" {
				if v := CheckUpdate(parse(tc.prev), pl); len(v) > 0 {
					t.Errorf("violations %+v", v)
				}
				return
			}
			prev := *pl
			for n := range len(pl.Segments) {
				prev.Segments = pl.Segments[:n]
				if v := CheckUpdate(&prev, pl); len(v) > 0 {
					t.Errorf("with %d entries before: violations %+v", n, v)
				}
			}
		})
	}
	pl, err := ParseMedia([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:6.006,\nchunk_00101.ts?session_seq=7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s := pl.Segments[0]; s.HasURISeq || s.Number() != 100 {
		t.Errorf("a seq= in the query was read as the number: %+v", s)
	}
}

// The media playlist's EXT-X-KEY is refused unless METHOD=NONE. A master
// playlist's EXT-X-SESSION-KEY is ignored, so a variant whose media
// playlist leaves its key out (it shouldn't) is decoded encrypted: with
// SAMPLE-AES the TS and PES headers are clear and only the slice data is
// scrambled, so the parser is content and ffmpeg decodes noise.
func TestMasterSessionKeyIsRefused(t *testing.T) {
	m, err := ParseMaster([]byte("#EXTM3U\n" +
		`#EXT-X-SESSION-KEY:METHOD=SAMPLE-AES,URI="skd://key",KEYFORMAT="com.apple.streamingkeydelivery"` + "\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720\nv720.m3u8\n"))
	if err == nil {
		t.Errorf("master with EXT-X-SESSION-KEY METHOD=SAMPLE-AES accepted: %+v", m.Variants)
	} else if !strings.Contains(err.Error(), "SESSION-KEY") {
		t.Logf("refused: %v", err)
	}
}
