package monitor

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

// FetchMeta records one HTTP fetch: when it was made, how long it took and
// what came back. It is saved next to every playlist and segment.
type FetchMeta struct {
	URL         string    `json:"url"`
	FinalURL    string    `json:"final_url,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
	ElapsedMs   float64   `json:"elapsed_ms"`
	Status      int       `json:"status,omitzero"`
	Proto       string    `json:"proto,omitempty"`
	// RemoteAddr and TLSVersion describe the connection the response came
	// on ("" when there was none, or for plain HTTP).
	RemoteAddr string `json:"remote_addr,omitempty"`
	TLSVersion string `json:"tls_version,omitempty"`
	// Headers are the response headers as the server sent them: names as
	// spelled, repeats in order, Transfer-Encoding included.
	Headers   WireHeader `json:"headers,omitempty"`
	Bytes     int        `json:"bytes"`
	WireBytes int        `json:"wire_bytes,omitzero"` // gzip size when the body was compressed
	// Encoded: the playlist body is kept as sent, still gzip-encoded,
	// because it was cut short or didn't decode; it is saved as .m3u8.gz.
	Encoded        bool  `json:"encoded,omitzero"`
	Attempts       int   `json:"attempts,omitzero"`
	FailedStatuses []int `json:"failed_statuses,omitempty"`
	// Failed are the attempts before this one, oldest first: each refusal
	// or network error, with its own response headers.
	Failed []FetchMeta `json:"failed_attempts,omitempty"`
	// BodyFile names the file a refused attempt's body was saved to, next
	// to the segment's files.
	BodyFile string `json:"body_file,omitempty"`
	Error    string `json:"error,omitempty"`
}

// WireHeader holds a response's header fields as the server sent them:
// names as spelled, values in order. Unlike http.Header's, its names are
// not canonical, so look one up with Get, which ignores case.
type WireHeader map[string][]string

// Get returns the named field's first value, however the server spelled
// its name.
func (h WireHeader) Get(name string) string {
	if vs := h[name]; len(vs) > 0 {
		return vs[0]
	}
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// The largest bodies read into memory. A segment from the origin is about
// 2.8 MB; 64 MB leaves room for any bitrate this monitor is likely to meet,
// with every channel and rendition fetching at once.
const (
	maxPlaylistBytes = 16 << 20
	maxSegmentBytes  = 64 << 20
)

// fetch GETs url. Playlists are requested with gzip, as players do, and
// decoded here so the saved headers are exactly what the server sent; a
// body that doesn't decode is returned as sent, with an error. Segments
// are read byte for byte and never decoded. A non-2xx status is an error,
// but its body is still returned so it can be saved.
func (m *Monitor) fetch(ctx context.Context, url string, playlist bool, timeout time.Duration) ([]byte, FetchMeta, error) {
	start := time.Now()
	meta := FetchMeta{URL: url, RequestedAt: start.UTC()}
	done := func(err error) error {
		meta.CompletedAt = time.Now().UTC()
		meta.ElapsedMs = math.Round(float64(time.Since(start).Microseconds())) / 1000
		if err != nil {
			meta.Error = err.Error()
		}
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, meta, done(err)
	}
	req.Header.Set("User-Agent", m.cfg.UserAgent)
	if playlist {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	// The connection each request goes out on (the last one, after
	// redirects) keeps the response's header block as it arrived.
	var mu sync.Mutex
	var conn *wireConn
	var remote string
	req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			if conn, _ = info.Conn.(*wireConn); conn != nil {
				conn.record()
			}
			if a := info.Conn.RemoteAddr(); a != nil {
				remote = a.String()
			}
		},
	}))
	resp, err := m.client.Do(req)
	mu.Lock()
	meta.RemoteAddr = remote
	wc := conn
	mu.Unlock()
	if err != nil {
		return nil, meta, done(err)
	}
	defer resp.Body.Close()
	meta.Status, meta.Proto = resp.StatusCode, resp.Proto
	var wire WireHeader
	if wc != nil {
		wire, _ = wc.header()
		meta.TLSVersion = wc.tlsVersion()
	}
	if wire != nil {
		meta.Headers = wire
	} else {
		// Not through a wireConn (a proxy, or a client a test swapped in).
		meta.Headers = WireHeader(resp.Header.Clone())
		if len(resp.TransferEncoding) > 0 {
			meta.Headers["Transfer-Encoding"] = resp.TransferEncoding
		}
	}
	if final := resp.Request.URL.String(); final != url {
		meta.FinalURL = final // redirected
	}

	limit := maxSegmentBytes
	if playlist {
		limit = maxPlaylistBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err == nil && len(body) > limit {
		err = fmt.Errorf("response larger than %d bytes", limit)
	}
	if playlist && strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		meta.WireBytes = len(body)
		switch decoded, gerr := gunzip(body); {
		case err != nil: // cut short: kept as sent
			meta.Encoded = true
		case gerr != nil:
			err, meta.Encoded = gerr, true
		default:
			body = decoded
		}
	}
	meta.Bytes = len(body)
	if err == nil && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		err = fmt.Errorf("HTTP %s", resp.Status)
	}
	return body, meta, done(err)
}

// base is the URL relative references in the body resolve against: where
// the response actually came from.
func (m FetchMeta) base() string { return cmp.Or(m.FinalURL, m.URL) }

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(io.LimitReader(r, maxPlaylistBytes+1))
	if err == nil && len(out) > maxPlaylistBytes {
		err = fmt.Errorf("decoded playlist larger than %d bytes", maxPlaylistBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	return out, nil
}

// playlistExt is a saved playlist body's extension: .m3u8, or .m3u8.gz for
// a gzip body kept as sent.
func playlistExt(meta FetchMeta) string {
	if meta.Encoded {
		return ".m3u8.gz"
	}
	return ".m3u8"
}

// playlistTimeout allows one target duration, within 3-10 s.
func playlistTimeout(target time.Duration) time.Duration {
	return min(max(target, 3*time.Second), 10*time.Second)
}

// segmentTimeout allows three target durations, within 15-60 s.
func segmentTimeout(target time.Duration) time.Duration {
	return min(max(3*target, 15*time.Second), time.Minute)
}
