package monitor

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// POST /capture answers only requests made on this machine, by hand or by
// a script: the Host must be a loopback address, and a request a web page
// made (it carries Origin, or Sec-Fetch-Site other than none) is refused.
func TestCaptureOnlyFromThisMachine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		url    string
		header map[string]string
		want   int
	}{
		{"curl to 127.0.0.1", "http://127.0.0.1:8765/capture?channel=test", nil, http.StatusOK},
		{"curl to localhost", "http://localhost:8765/capture?channel=test", nil, http.StatusOK},
		{"curl to ::1", "http://[::1]:8765/capture?channel=test", nil, http.StatusOK},
		{"typed in the address bar", "http://127.0.0.1:8765/capture?channel=test", map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"DNS rebinding", "http://rebind.attacker.example:8765/capture?channel=test", nil, http.StatusForbidden},
		{"a page elsewhere", "http://127.0.0.1:8765/capture?channel=test", map[string]string{"Origin": "https://attacker.example"}, http.StatusForbidden},
		{"a page on another local port", "http://127.0.0.1:8765/capture?channel=test", map[string]string{"Origin": "http://127.0.0.1:3000"}, http.StatusForbidden},
		{"cross-site fetch", "http://127.0.0.1:8765/capture?channel=test", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	} {
		m := newTestMonitor(t, testConfig(t, "http://127.0.0.1:1/never.m3u8"), nil)
		req := httptest.NewRequest(http.MethodPost, tc.url, nil)
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, req)
		m.incidents.shutdown()
		if rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body.String(), tc.want)
		}
		if n := len(incidentDirs(t, m.cfg)); (n > 0) != (tc.want == http.StatusOK) {
			t.Errorf("%s: %d incidents", tc.name, n)
		}
	}
}
