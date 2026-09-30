package config

// Audit (2026-09-30): fuzz target for config parsing. Whatever the input,
// Parse must either fail or return a config that satisfies its own rules.

import (
	"net/url"
	"os"
	"testing"
)

func FuzzConfigParse(f *testing.F) {
	if b, err := os.ReadFile("../../channels.example.yaml"); err == nil {
		f.Add(string(b))
	}
	f.Add("channels:\n  - name: a\n    url: https://h/x.m3u8\n")
	f.Fuzz(func(t *testing.T, s string) {
		c, err := Parse([]byte(s))
		if err != nil {
			return
		}
		if len(c.Channels) == 0 {
			t.Fatal("no channels")
		}
		for _, ch := range c.Channels {
			u, err := url.Parse(ch.URL)
			if !safeName.MatchString(ch.Name) || ch.Name == "." || ch.Name == ".." || err != nil || u.Host == "" {
				t.Fatalf("accepted channel %+v", ch)
			}
		}
		// The storage cap is not checked here: TestAuditStorageCap shows
		// the known failure, and the fuzzer would stop on it.
		if c.Buffer <= 0 || c.MaxIncident <= 0 || c.HealthInterval <= 0 || c.PostRoll < 0 || c.MergeWindow < 0 {
			t.Fatalf("accepted durations %+v", c)
		}
	})
}

// TestAuditStorageCap: incident_storage_gb is only checked for being
// positive before it is converted to bytes, so a tiny value becomes a cap of
// 0 bytes (every closed incident is deleted at once), and a huge one
// overflows int64 (on amd64 the conversion gives math.MinInt64).
func TestAuditStorageCap(t *testing.T) {
	for _, gb := range []string{"0.0000000001", "1e10"} {
		c, err := Parse([]byte("channels:\n  - name: a\n    url: https://h/x.m3u8\nincident_storage_gb: " + gb + "\n"))
		if err != nil {
			continue // rejected: fine
		}
		if c.IncidentStorageBytes <= 0 {
			t.Errorf("incident_storage_gb: %s was accepted as a cap of %d bytes", gb, c.IncidentStorageBytes)
		}
	}
}

// TestAuditListenMustBeLoopback: the manual-capture listener is documented
// as localhost only, but any address is accepted, so a typo or ":8765"
// exposes POST /capture (which opens incidents and fills disk) to the
// network. (main.go's -listen flag also overrides the config after this
// validation.)
func TestAuditListenMustBeLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8765", ":8765", "192.168.1.10:8765"} {
		_, err := Parse([]byte("listen: \"" + addr + "\"\nchannels:\n  - name: a\n    url: https://h/x.m3u8\n"))
		if err == nil {
			t.Errorf("listen: %q was accepted", addr)
		}
	}
}
