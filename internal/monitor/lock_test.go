//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package monitor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A second monitor on a data directory refuses to start before it touches
// anything: the first one's open incident stays open, where the second's
// crash recovery would have marked it interrupted. Once the first lets go,
// the directory is free, and a monitor releases it when it stops.
func TestSecondMonitorOnADataDirectoryRefusesToStart(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	unlock, err := lockDataDir(cfg.DataDir) // the first monitor
	if err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(cfg.DataDir, "incidents", "20260102T120000Z_test")
	if err := os.MkdirAll(open, 0o755); err != nil {
		t.Fatal(err)
	}
	report := `{"id":"20260102T120000Z_test","channel":"test","status":"open","opened_at":"2026-01-02T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(open, "report.json"), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestMonitor(t, cfg, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = m.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "in use by another stream-analyzer") ||
		!strings.Contains(err.Error(), "pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("second Run = %v, want an error naming the lock and its holder", err)
	}
	if r := readReport(t, open); r.Status != "open" {
		t.Errorf("the first monitor's open incident is now %q", r.Status)
	}

	unlock()
	stopped, stop := context.WithCancel(t.Context())
	stop()
	if err := newTestMonitor(t, cfg, nil).Run(stopped); err != nil {
		t.Fatalf("Run once the first monitor let go = %v, want it to start", err)
	}
	again, err := lockDataDir(cfg.DataDir)
	if err != nil {
		t.Fatalf("lock after Run returned = %v, want the monitor to have released it", err)
	}
	again()
}

// A lock file this user can't write, such as one a sudo run left, still
// locks: flock doesn't need write access.
func TestReadOnlyLockFileStillLocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockFile), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lockDataDir with a read-only lock file = %v, want the lock", err)
	}
	defer unlock()
	if _, err := lockDataDir(dir); err == nil || !strings.Contains(err.Error(), "in use by another stream-analyzer") {
		t.Errorf("second lockDataDir = %v, want it refused", err)
	}
}

// A symlink in the lock file's place isn't followed: its target keeps its
// contents, and the monitor doesn't start.
func TestLockFileSymlinkIsNotFollowed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, lockFile)); err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockDataDir(dir); err == nil {
		unlock()
		t.Error("lockDataDir took the lock through a symlink")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep me\n" {
		t.Errorf("the symlink's target now holds %q", b)
	}
}

// On a filesystem without flock, such as some network mounts, the monitor
// can't lock its data directory: it warns and runs, as it did before.
func TestDataDirectoryWithoutFlockStillRuns(t *testing.T) {
	flock = func(int, int) error { return syscall.ENOTSUP }
	defer func() { flock = syscall.Flock }()
	cfg := testConfig(t, "http://127.0.0.1:1/never.m3u8")
	logs := &lockedBuffer{}
	m, err := New(Options{Config: cfg, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	if err := m.Run(stopped); err != nil {
		t.Fatalf("Run = %v, want it to run without the lock", err)
	}
	if !strings.Contains(logs.String(), "cannot lock the data directory") {
		t.Errorf("no warning that the data directory isn't locked:\n%s", logs.String())
	}
}
