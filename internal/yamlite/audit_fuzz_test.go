package yamlite

// Audit: fuzz target for the YAML subset parser.

import (
	"os"
	"testing"
	"time"
)

// checkTree fails unless v is made only of the types Parse documents.
func checkTree(t *testing.T, v any) {
	t.Helper()
	switch v := v.(type) {
	case nil, string:
	case map[string]any:
		for _, x := range v {
			checkTree(t, x)
		}
	case []any:
		for _, x := range v {
			checkTree(t, x)
		}
	default:
		t.Fatalf("unexpected %T in parse tree", v)
	}
}

func FuzzParse(f *testing.F) {
	if b, err := os.ReadFile("../../channels.example.yaml"); err == nil {
		f.Add(string(b))
	}
	f.Add("a: 1\nb:\n  - x\n  - y: z\n    w: [1, \"2\", '3']\n")
	f.Add("checks:\n  continuity_ms: 10 # comment\n")
	f.Fuzz(func(t *testing.T, s string) {
		start := time.Now()
		doc, err := Parse([]byte(s))
		if err != nil {
			return
		}
		checkTree(t, doc)
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("took %v for %d bytes", d, len(s))
		}
	})
}
