package monitor

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tlsConfig is the client's TLS configuration. TLS defaults to a 1.2
// ceiling because some origins speak only TLS 1.2 and reset the connection
// on Go's TLS 1.3 ClientHello instead of negotiating down. Set
// tls_max_version to "1.3" to remove the ceiling.
func tlsConfig(tlsMax string) *tls.Config {
	cfg := &tls.Config{}
	if tlsMax != "1.3" {
		cfg.MaxVersion = tls.VersionTLS12
	}
	return cfg
}

// newHTTPClient keeps connections to the origin alive across all channels.
func newHTTPClient(tlsMax string) *http.Client {
	return newWireClient(tlsConfig(tlsMax))
}

// newWireClient is an HTTP/1.1 client whose connections keep each
// response's header block as it arrived (see wireConn). Compression is off,
// so segments arrive byte-exact; playlists ask for gzip themselves (see
// fetch). HTTP/2 is not offered: the origin speaks HTTP/1.1, and what
// players get from it is what the evidence should show.
func newWireClient(tlsCfg *tls.Config) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 32
	t.DisableCompression = true
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &wireConn{Conn: c}, nil
	}
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		cfg := tlsCfg.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName, _, _ = net.SplitHostPort(addr)
		}
		cfg.NextProtos = []string{"http/1.1"}
		tc := tls.Client(raw, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return &wireConn{Conn: tc, tls: tc}, nil
	}
	return &http.Client{Transport: t}
}

// maxWireHeader bounds a recorded header block.
const maxWireHeader = 64 << 10

// wireConn is a client connection that keeps the header block of the
// response it is reading, so the headers can be saved as the server sent
// them: names as spelled, repeats in order, and the hop-by-hop ones
// (Transfer-Encoding, Connection) that Go's client takes out. Go reads them
// into a map of canonical names, which loses all three.
type wireConn struct {
	net.Conn
	tls *tls.Conn // nil for plain HTTP

	mu   sync.Mutex
	on   bool   // recording
	head []byte // the header block so far; complete once on is false
}

func (c *wireConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		if c.on {
			c.head = append(c.head, p[:n]...)
			c.cut()
		}
		c.mu.Unlock()
	}
	return n, err
}

// cut stops recording at the end of the final response's header block. A
// 1xx response's block comes first and is dropped.
func (c *wireConn) cut() {
	for {
		i := bytes.Index(c.head, []byte("\r\n\r\n"))
		switch {
		case i < 0 && len(c.head) > maxWireHeader:
			c.head, c.on = nil, false
			return
		case i < 0:
			return
		case bytes.HasPrefix(c.head, []byte("HTTP/1.1 1")) || bytes.HasPrefix(c.head, []byte("HTTP/1.0 1")):
			c.head = c.head[i+4:]
		default:
			c.head, c.on = c.head[:i+4], false
			return
		}
	}
}

// record starts keeping the next response's header block.
func (c *wireConn) record() {
	c.mu.Lock()
	c.on, c.head = true, nil
	c.mu.Unlock()
}

// header returns the header block recorded since record, if it is whole.
func (c *wireConn) header() (WireHeader, bool) {
	c.mu.Lock()
	b, whole := c.head, !c.on && c.head != nil
	c.mu.Unlock()
	if !whole {
		return nil, false
	}
	return parseWireHeader(b)
}

// tlsVersion names the connection's TLS version, or "" for plain HTTP.
func (c *wireConn) tlsVersion() string {
	if c.tls == nil {
		return ""
	}
	return tls.VersionName(c.tls.ConnectionState().Version)
}

// parseWireHeader reads a response's header block (status line, fields,
// blank line) into a header keyed by the names as sent. A folded line
// (obs-fold) continues the field before it.
func parseWireHeader(b []byte) (WireHeader, bool) {
	lines := strings.Split(strings.TrimSuffix(string(b), "\r\n\r\n"), "\r\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "HTTP/") {
		return nil, false
	}
	h := WireHeader{}
	var last string
	for _, line := range lines[1:] {
		if line != "" && (line[0] == ' ' || line[0] == '\t') {
			if vs := h[last]; len(vs) > 0 {
				vs[len(vs)-1] += " " + strings.TrimSpace(line)
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" {
			return nil, false
		}
		last = name
		h[name] = append(h[name], strings.TrimSpace(value))
	}
	return h, true
}
