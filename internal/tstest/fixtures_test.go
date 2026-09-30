package tstest

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the fixtures in testdata/")

// TestFixturesUpToDate fails when a committed fixture no longer matches its
// generator, so edits to the generator can't silently diverge from the files
// the other packages' tests read.
func TestFixturesUpToDate(t *testing.T) {
	for name, want := range Fixtures() {
		path := Path(name)
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run with -update to generate)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run go test ./internal/tstest -run TestFixturesUpToDate -update", name)
		}
	}
}
