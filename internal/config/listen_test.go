package config

import (
	"strings"
	"testing"
)

// The manual trigger listens on this machine only; "" turns it off.
func TestListenIsLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"127.0.0.1:8765": true, "127.0.0.2:9000": true, "localhost:8765": true, "[::1]:8765": true, "": true,
		"0.0.0.0:8765": false, ":8765": false, "192.168.1.10:8765": false, "[::]:8765": false,
		"example.com:80": false, "127.0.0.1": false, "localhost:http": false,
	} {
		_, err := Parse([]byte("listen: \"" + addr + "\"\nchannels:\n  - name: a\n    url: https://h/x.m3u8\n"))
		if (err == nil) != ok {
			t.Errorf("listen %q: error %v, want accepted=%v", addr, err, ok)
		}
		if err != nil && !strings.Contains(err.Error(), "listen") {
			t.Errorf("listen %q: error %q does not name the setting", addr, err)
		}
	}
	if err := CheckListen("0.0.0.0:1"); err == nil {
		t.Error("CheckListen accepted 0.0.0.0:1")
	}
}
