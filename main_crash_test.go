package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the command itself when the test binary is started as a
// subprocess by the tests below.
func TestMain(m *testing.M) {
	if args := os.Getenv("STREAM_ANALYZER_TEST_ARGS"); args != "" {
		os.Args = append([]string{"stream-analyzer"}, strings.Split(args, "\n")...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	w.n++
	return 0, syscall.EPIPE
}

// The log file gets every line even when stderr is gone (a closed pipe or
// terminal), and a failing stderr is not retried for every line.
func TestLogWriterKeepsTheFileWhenStderrFails(t *testing.T) {
	var file bytes.Buffer
	stderr := &failingWriter{}
	w := &logWriter{file: &file, stderr: stderr}
	for _, line := range []string{"one\n", "two\n", "three\n"} {
		if n, err := w.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("Write(%q) = %d, %v", line, n, err)
		}
	}
	if file.String() != "one\ntwo\nthree\n" {
		t.Errorf("log file got %q", file.String())
	}
	if stderr.n != 1 {
		t.Errorf("stderr written %d times after it failed, want 1", stderr.n)
	}
}

// With stderr piped into a reader that has gone away (stream-analyzer ... |
// head -1, a closed terminal), the monitor keeps running, logs to its file
// and stops cleanly on SIGTERM instead of dying of SIGPIPE.
func TestBrokenStderrPipeDoesNotKillTheMonitor(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "channels.yaml")
	os.WriteFile(cfg, []byte("listen: 127.0.0.1:0\nblackdetect:\n  enabled: false\nchannels:\n  - name: test\n    url: http://127.0.0.1:1/never.m3u8\n"), 0o644)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // nobody reads stderr
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "STREAM_ANALYZER_TEST_ARGS=-config\n"+cfg+"\n-data\n"+filepath.Join(dir, "data"))
	cmd.Stderr = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	time.Sleep(1500 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	err = cmd.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		t.Errorf("the monitor exited with %v", exit)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "data", "stream-analyzer.log"))
	if !strings.Contains(string(log), "stream-analyzer stopped") {
		t.Errorf("the log file has no shutdown line:\n%s", log)
	}
}
