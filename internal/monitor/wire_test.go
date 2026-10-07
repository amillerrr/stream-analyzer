package monitor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Response headers are saved as the server sent them: names as spelled,
// repeats in order, folded lines joined, and the hop-by-hop ones Go's
// client drops. Code still finds them whatever the spelling, and the fetch
// records the address it came from.
func TestHeadersAreSavedAsSent(t *testing.T) {
	base := rawServer(t, func(string) []byte {
		return []byte("HTTP/1.1 200 OK\r\nETag: \"abc\"\r\nVia: 1.1 edge-a\r\nX-Folded: one\r\n two\r\nVia: 1.1 edge-b\r\n" +
			"Transfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n")
	})
	m := newTestMonitor(t, testConfig(t, base+"/x.m3u8"), nil)
	body, meta, err := m.fetch(t.Context(), base+"/seg.ts", false, 5*time.Second)
	if err != nil || string(body) != "hello" {
		t.Fatalf("body %q, err %v", body, err)
	}
	h := meta.Headers
	if !slices.Equal(h["ETag"], []string{`"abc"`}) || !slices.Equal(h["Via"], []string{"1.1 edge-a", "1.1 edge-b"}) ||
		!slices.Equal(h["Transfer-Encoding"], []string{"chunked"}) || !slices.Equal(h["X-Folded"], []string{"one two"}) {
		t.Errorf("saved headers %v", h)
	}
	if got := meta.Headers.Get("etag"); got != `"abc"` {
		t.Errorf(`header("etag") = %q`, got)
	}
	if !strings.HasPrefix(base, "http://"+meta.RemoteAddr) {
		t.Errorf("remote address %q, want the server's (%s)", meta.RemoteAddr, base)
	}
}

// Over TLS too: the headers are read after decryption, and the TLS version
// is recorded.
func TestHeadersAreSavedAsSentOverTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["ETag"] = []string{`"tls"`}
		fmt.Fprint(w, "hello")
	}))
	t.Cleanup(srv.Close)
	m := newTestMonitor(t, testConfig(t, srv.URL+"/x.m3u8"), nil)
	cfg := tlsConfig("1.2")
	cfg.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	m.client = newWireClient(cfg)
	_, meta, err := m.fetch(t.Context(), srv.URL+"/seg.ts", false, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(meta.Headers["ETag"], []string{`"tls"`}) || meta.TLSVersion != "TLS 1.2" || meta.Proto != "HTTP/1.1" {
		t.Errorf("headers %v, TLS %q, proto %q: want ETag as sent over TLS 1.2 and HTTP/1.1", meta.Headers, meta.TLSVersion, meta.Proto)
	}
}

// Every attempt at a segment is kept: the refused ones with their own
// headers in failed_attempts, and each refusal's body in a file next to
// the segment's.
func TestRefusedSegmentAttemptsAreKept(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/test.m3u8", playlistBody(500, -1, seg(500), seg(501), seg(502)))
	o.segments(500, 1)
	o.route("/live/s501.ts", func(n int, w http.ResponseWriter) {
		w.Header().Set("X-Error-Id", fmt.Sprintf("err-%d", n))
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "error page %d", n)
	})
	o.route("/live/s502.ts", func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("X-First-Attempt", "yes")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "busy")
			return
		}
		w.Write(tstestSegment(2))
	})
	cfg := testConfig(t, srv.URL+"/live/test.m3u8")
	_, recs := run(t, cfg, seen(502))
	buf := filepath.Join(cfg.DataDir, "buffer", "test")
	file := func(name, want string) {
		t.Helper()
		if b, err := os.ReadFile(filepath.Join(buf, name)); err != nil || string(b) != want {
			t.Errorf("%s holds %q (%v), want %q", name, b, err, want)
		}
	}
	r501 := recs[501].Fetch
	if len(r501.Failed) != 1 || r501.Failed[0].Headers.Get("X-Error-Id") != "err-1" || r501.Headers.Get("X-Error-Id") != "err-2" {
		t.Errorf("501: failed attempts %+v, last X-Error-Id %q: want err-1 kept apart from err-2", r501.Failed, r501.Headers.Get("X-Error-Id"))
	} else {
		file(r501.Failed[0].BodyFile, "error page 1")
		file(r501.BodyFile, "error page 2")
	}
	r502 := recs[502].Fetch
	if len(r502.Failed) != 1 || r502.Failed[0].Headers.Get("X-First-Attempt") != "yes" || r502.Failed[0].Status != http.StatusServiceUnavailable {
		t.Errorf("502: failed attempts %+v, want the 503 with its headers", r502.Failed)
	} else {
		file(r502.Failed[0].BodyFile, "busy")
	}
}

// A segment's files, and a playlist fetch's, are pruned together.
func TestBufferGroupsAFetchsFiles(t *testing.T) {
	for _, set := range [][]string{
		{"seg_501.ts", "seg_501.json", "seg_501.attempt1.body", "seg_501.attempt2.body"},
		{"playlist_20260102T144854.849Z_msn5.m3u8", "playlist_20260102T144854.849Z_msn5.json"},
		{"playlist_20260102T144854.849Z_msn5.m3u8.gz", "playlist_20260102T144854.849Z_msn5.json"},
		{"playlist_20260102T144854.849Z.json"},
	} {
		for _, name := range set[1:] {
			if fileGroup(name) != fileGroup(set[0]) {
				t.Errorf("%s is in group %q, %s in %q", name, fileGroup(name), set[0], fileGroup(set[0]))
			}
		}
	}
	if fileGroup("seg_501.ts") == fileGroup("seg_5010.ts") || fileGroup("playlist_20260102T144854.849Z_msn5.json") == fileGroup("playlist_20260102T144854.851Z_msn5.json") {
		t.Error("different fetches share a group")
	}
}
