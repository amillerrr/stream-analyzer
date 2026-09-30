package yamlite

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseConfigShapedDocument(t *testing.T) {
	doc := `
# stream-analyzer settings
data_dir: ./data            # trailing comment
listen: "127.0.0.1:8765"
empty:
title: 'it''s # not a comment'
checks:
  pcr_jump_ms: 500
  nested:
    deep: yes
args: [-hwaccel, "video toolbox", '']
none: []
channels:
  - name: channel1
    url: https://origin.example.com/live/cluster1/feed01/hls/index.m3u8#frag
  -   name: channel2
      rendition: lowest
tags:
- a
- b
-
  - nested-list-item
`
	got, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"data_dir": "./data",
		"listen":   "127.0.0.1:8765",
		"empty":    nil,
		"title":    "it's # not a comment",
		"checks": map[string]any{
			"pcr_jump_ms": "500",
			"nested":      map[string]any{"deep": "yes"},
		},
		"args": []any{"-hwaccel", "video toolbox", ""},
		"none": []any{},
		"channels": []any{
			map[string]any{
				"name": "channel1",
				"url":  "https://origin.example.com/live/cluster1/feed01/hls/index.m3u8#frag",
			},
			map[string]any{"name": "channel2", "rendition": "lowest"},
		},
		"tags": []any{"a", "b", []any{"nested-list-item"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseEmptyDocument(t *testing.T) {
	got, err := Parse([]byte("# only a comment\n---\n"))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want empty map", got, err)
	}
}

func TestParseRejectsUnsupportedOrMalformedInput(t *testing.T) {
	for name, tc := range map[string]struct{ doc, wantErr string }{
		"tab indent":        {"a:\n\tb: 1\n", "line 2"},
		"over-indented key": {"a: 1\n  b: 2\n", "line 2"},
		"duplicate key":     {"a: 1\na: 2\n", "line 2"},
		"no colon":          {"just text\n", "line 1"},
		"unterminated":      {"a: \"open\n", "line 1"},
		"flow mapping":      {"a: {b: 1}\n", "line 1"},
		"block scalar":      {"a: |\n  text\n", "line 1"},
		"anchor":            {"a: &x 1\n", "line 1"},
		"top-level list":    {"- a\n- b\n", "mapping"},
		"trailing garbage":  {"a: \"x\" y\n", "line 1"},
	} {
		_, err := Parse([]byte(tc.doc))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.wantErr)
		}
	}
}

// A quote inside a plain value doesn't quote anything: the comment after
// it is still a comment.
func TestApostropheInAPlainValue(t *testing.T) {
	doc, err := Parse([]byte("data_dir: /Volumes/Media Team's SSD/stream-data   # evidence drive\nuser_agent: stream-analyzer \"lab\" build # comment\nquoted: \"a # b\"  # c\n"))
	if err != nil {
		t.Fatal(err)
	}
	m := doc
	for key, want := range map[string]string{
		"data_dir":   "/Volumes/Media Team's SSD/stream-data",
		"user_agent": `stream-analyzer "lab" build`,
		"quoted":     "a # b",
	} {
		if m[key] != want {
			t.Errorf("%s = %q, want %q", key, m[key], want)
		}
	}
}
