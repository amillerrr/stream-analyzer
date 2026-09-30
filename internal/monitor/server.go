package monitor

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
)

// Handler serves the manual trigger: POST /capture?channel=NAME opens an
// incident for the channel, or merges into the one already open.
func (m *Monitor) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /capture", m.handleCapture)
	return mux
}

func (m *Monitor) handleCapture(w http.ResponseWriter, r *http.Request) {
	if why := foreign(r); why != "" {
		m.log.Warn("refused a capture request", "reason", why, "host", r.Host, "origin", r.Header.Get("Origin"),
			"remote", r.RemoteAddr)
		respond(w, http.StatusForbidden, map[string]any{"error": why})
		return
	}
	name := r.URL.Query().Get("channel")
	c, ok := m.byName[name]
	if !ok {
		names := make([]string, 0, len(m.channels))
		for _, c := range m.channels {
			names = append(names, c.name)
		}
		respond(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("unknown channel %q", name), "channels": names})
		return
	}
	id, created, err := m.incidents.manual(c)
	switch {
	case errors.Is(err, errStopped):
		respond(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	case err != nil:
		respond(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	status := "merged"
	if created {
		status = "opened"
	}
	respond(w, http.StatusOK, map[string]any{"incident": id, "status": status, "dir": filepath.Join(m.incidentDir, id)})
}

// foreign says why a capture request did not come from this machine by
// hand or by a script, or "" when it did: the Host must be a loopback
// address (not a DNS-rebinding name), and a web page's request (it carries
// Origin, or Sec-Fetch-Site other than none) is refused.
func foreign(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Sprintf("host %q is not a loopback address", r.Host)
	}
	if o := r.Header.Get("Origin"); o != "" {
		return fmt.Sprintf("request from a web page (Origin %s)", o)
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "none" {
		return fmt.Sprintf("request from a web page (Sec-Fetch-Site %s)", s)
	}
	return ""
}

func respond(w http.ResponseWriter, code int, v any) {
	b, err := marshalJSON(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}
