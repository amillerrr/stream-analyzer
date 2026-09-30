package hls

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Parses every real playlist under the monitor's data directory (read-only).
func TestAuditAllRealPlaylists(t *testing.T) {
	root := filepath.Join("..", "..", "data") // git-ignored; skipped where absent
	if _, err := os.Stat(root); err != nil {
		t.Skipf("real data not available: %v", err)
	}
	kinds := map[string]int{}
	durs := map[float64]int{}
	var masters, media, errs, trailing, withDSN int
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".m3u8") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if IsMaster(b) {
			masters++
			m, err := ParseMaster(b)
			if err != nil {
				t.Errorf("%s: %v", p, err)
				return nil
			}
			if v, err := SelectVariant(m.Variants, "highest"); err != nil || v.Resolution != "1280x720" {
				t.Errorf("%s: highest = %+v %v", p, v, err)
			}
			return nil
		}
		pl, err := ParseMedia(b)
		if err != nil {
			errs++
			t.Errorf("%s: %v", p, err)
			return nil
		}
		media++
		if strings.Contains(string(b), "DISCONTINUITY-SEQUENCE") {
			withDSN++
		}
		if len(pl.Trailing) > 0 {
			trailing++
		}
		for _, s := range pl.Segments {
			k := CueKind(s.SCTE35)
			kinds[k]++
			if d, ok := CueDuration(s.SCTE35); ok {
				durs[d]++
			}
			if (k == "") != (len(s.SCTE35) == 0) {
				t.Errorf("%s seq %d: kind %q for %q", p, s.Seq, k, s.SCTE35)
			}
		}
		return nil
	})
	var ks []string
	for k, n := range kinds {
		ks = append(ks, k+"="+itoa(n))
	}
	sort.Strings(ks)
	t.Logf("masters=%d media=%d parse_errors=%d with_DSN_tag=%d with_trailing_tags=%d kinds=%v declared_durations=%v", masters, media, errs, withDSN, trailing, ks, durs)
}

func itoa(n int) string { return strconv.Itoa(n) }
