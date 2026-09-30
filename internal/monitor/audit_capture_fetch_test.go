package monitor

// Scratch audit tests (capture fidelity). Not part of the audited code.

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amillerrr/stream-analyzer/internal/tstest"
)

// rawServer answers every connection with the exact bytes resp(req) returns.
func rawServer(t *testing.T, resp func(path string) []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					req, err := http.ReadRequest(br)
					if err != nil {
						return
					}
					io.Copy(io.Discard, req.Body)
					c.Write(resp(req.URL.Path))
				}
			}(c)
		}
	}()
	return "http://" + ln.Addr().String()
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// CAP: the saved headers are not exactly what the server sent: names are
// canonicalized and Transfer-Encoding is dropped.
func TestAuditSavedHeadersAreNotWireHeaders(t *testing.T) {
	base := rawServer(t, func(string) []byte {
		return []byte("HTTP/1.1 200 OK\r\nETag: \"abc\"\r\nX-IP-TOS: 0\r\nTransfer-Encoding: chunked\r\nContent-Type: video/MP2T\r\n\r\n5\r\nhello\r\n0\r\n\r\n")
	})
	m := newTestMonitor(t, testConfig(t, base+"/x.m3u8"), nil)
	m.client = newHTTPClient("1.2")
	body, meta, err := m.fetch(context.Background(), base+"/seg.ts", false, 5*time.Second)
	keys := make([]string, 0, len(meta.Headers))
	for k := range meta.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("body=%q err=%v saved header names=%v", body, err, keys)
	if _, ok := meta.Headers["Transfer-Encoding"]; !ok {
		t.Errorf("wire had 'Transfer-Encoding: chunked'; saved headers have no Transfer-Encoding")
	}
	if _, ok := meta.Headers["ETag"]; !ok {
		t.Errorf("wire had 'ETag'; saved as %v", keys)
	}
}

// CAP: a playlist served with a corrupt gzip body: the raw bytes the origin
// sent are not saved anywhere.
func TestAuditCorruptGzipPlaylistBodyIsNotSaved(t *testing.T) {
	wire := []byte("\x1f\x8b\x08\x00garbage-from-origin")
	base := rawServer(t, func(string) []byte {
		return append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n", len(wire))), wire...)
	})
	cfg := testConfig(t, base+"/x.m3u8")
	m := newTestMonitor(t, cfg, nil)
	c := m.channels[0]
	c.mediaURL = base + "/x.m3u8"
	os.MkdirAll(c.dir, 0o755)
	_, _, meta, err := c.fetchPlaylist(context.Background())
	t.Logf("err=%v meta.Bytes=%d wire=%d error=%q", err, meta.Bytes, meta.WireBytes, meta.Error)
	entries, _ := os.ReadDir(c.dir)
	var names []string
	found := false
	for _, e := range entries {
		names = append(names, e.Name())
		b, _ := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if bytes.Contains(b, []byte("garbage-from-origin")) {
			found = true
		}
	}
	t.Logf("buffer files: %v", names)
	if !found {
		t.Errorf("the %d bytes the origin sent are not saved (only the .json)", len(wire))
	}
}

// CAP: a gzip playlist that decodes past maxPlaylistBytes is truncated
// silently: no error, 16 MiB + 1 bytes.
func TestAuditOversizeGzipPlaylistTruncatedSilently(t *testing.T) {
	plain := bytes.Repeat([]byte("#COMMENT-LINE-XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\n"), (17<<20)/78)
	wire := gz(plain)
	base := rawServer(t, func(string) []byte {
		return append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n", len(wire))), wire...)
	})
	m := newTestMonitor(t, testConfig(t, base+"/x.m3u8"), nil)
	m.client = newHTTPClient("1.2")
	body, meta, err := m.fetch(context.Background(), base+"/x.m3u8", true, 20*time.Second)
	t.Logf("decoded=%d served(decoded)=%d wire=%d err=%v", len(body), len(plain), meta.WireBytes, err)
	if err == nil && len(body) != len(plain) {
		t.Errorf("decoded playlist truncated from %d to %d bytes with no error", len(plain), len(body))
	}
}

// CAP: a segment answered with Content-Encoding: gzip is decoded before it is
// saved (and cut at 16 MiB), although segments are meant to be byte-exact.
func TestAuditGzipSegmentIsDecodedAndTruncated(t *testing.T) {
	plain := bytes.Repeat([]byte{0x47, 0x1f, 0xff, 0x10}, (20<<20)/4) // 20 MiB
	wire := gz(plain)
	base := rawServer(t, func(string) []byte {
		return append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Type: video/MP2T\r\nContent-Length: %d\r\n\r\n", len(wire))), wire...)
	})
	m := newTestMonitor(t, testConfig(t, base+"/x.m3u8"), nil)
	m.client = newHTTPClient("1.2")
	body, meta, err := m.fetch(context.Background(), base+"/seg.ts", false, 20*time.Second)
	t.Logf("wire=%d saved=%d decoded-size=%d err=%v", len(wire), len(body), len(plain), err)
	if !bytes.Equal(body, wire) {
		t.Errorf("segment bytes saved (%d) are not the bytes on the wire (%d); decoded length %d, meta.Bytes=%d", len(body), len(wire), len(plain), meta.Bytes)
	}
}

// CAP: a refused segment (404 then 404): the error body is dropped; a 503
// cured on retry: the 503's headers are dropped.
func TestAuditRefusedSegmentBodyAndHeadersAreDropped(t *testing.T) {
	o, srv := newScriptOrigin(t)
	o.sequence("/live/master.m3u8", []byte(singleVariantMaster))
	o.sequence("/live/hi.m3u8", playlistBody(500, -1, seg(500), seg(501), seg(502)))
	o.segments(500, 1)
	o.route("/live/s501.ts", func(n int, w http.ResponseWriter) {
		w.Header().Set("X-Origin-Error-Id", fmt.Sprintf("err-%d", n))
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("<html>origin error page, request id 42</html>"))
	})
	body502 := func() []byte { return nil }
	_ = body502
	o.route("/live/s502.ts", func(n int, w http.ResponseWriter) {
		if n == 1 {
			w.Header().Set("X-First-Attempt", "503-was-here")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("busy"))
			return
		}
		w.Write(tstestSegment(2))
	})
	cfg := testConfig(t, srv.URL+"/live/master.m3u8")
	_, recs := run(t, cfg, seen(502))
	r501, r502 := recs[501], recs[502]
	t.Logf("501: status=%d failed=%v faults=%v headers X-Origin-Error-Id=%q", r501.Fetch.Status, r501.Fetch.FailedStatuses, r501.Faults, r501.Fetch.Headers.Get("X-Origin-Error-Id"))
	t.Logf("502: status=%d failed=%v X-First-Attempt=%q", r502.Fetch.Status, r502.Fetch.FailedStatuses, r502.Fetch.Headers.Get("X-First-Attempt"))
	// Is the 404 body anywhere in the data directory?
	found := false
	filepath.Walk(cfg.DataDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("request id 42")) {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Errorf("the origin's 404 body for the unavailable segment is not saved anywhere")
	}
	// Earlier attempts are kept whole in failed_attempts, each with its
	// own headers, rather than merged into the last attempt's.
	if len(r501.Fetch.Failed) == 0 || r501.Fetch.Failed[0].Headers.Get("X-Origin-Error-Id") != "err-1" {
		t.Errorf("first refusal's headers lost: saved X-Origin-Error-Id=%q (only the retry's)", r501.Fetch.Headers.Get("X-Origin-Error-Id"))
	}
	if len(r502.Fetch.Failed) == 0 || r502.Fetch.Failed[0].Headers.Get("X-First-Attempt") == "" {
		t.Errorf("recovered 503's headers lost; only failed_statuses=%v kept", r502.Fetch.FailedStatuses)
	}
	var dirs []string
	for _, d := range incidentDirs(t, cfg) {
		dirs = append(dirs, filepath.Base(d))
	}
	t.Logf("incidents: %v", strings.Join(dirs, ","))
}

func tstestSegment(k int) []byte { return tstest.Base.Segment(k).Bytes() }

// CAP: a gzip playlist cut short on the wire is saved as its raw partial
// gzip bytes under .m3u8, while a complete-but-corrupt one is not saved.
func TestAuditTruncatedGzipPlaylistSavedAsRawBytes(t *testing.T) {
	full := gz([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:7\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:6.006,\na.ts\n"))
	base := rawServer(t, func(string) []byte {
		return append([]byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(full))), full[:20]...)
	})
	cfg := testConfig(t, base+"/x.m3u8")
	m := newTestMonitor(t, cfg, nil)
	c := m.channels[0]
	c.mediaURL = base + "/x.m3u8"
	os.MkdirAll(c.dir, 0o755)
	_, _, meta, err := c.fetchPlaylist(context.Background())
	entries, _ := os.ReadDir(c.dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if strings.HasSuffix(e.Name(), ".m3u8") {
			t.Logf("%s: %d bytes, gzip magic=%v; meta bytes=%d wire_bytes=%d err=%v", e.Name(), len(b), bytes.HasPrefix(b, []byte{0x1f, 0x8b}), meta.Bytes, meta.WireBytes, err)
			if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
				t.Errorf("%s holds raw gzip bytes, not a playlist", e.Name())
			}
		}
	}
}
