//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package monitor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// flock is syscall.Flock; tests replace it.
var flock = syscall.Flock

// lockDataDir takes an exclusive lock on the data directory, so a second
// monitor can't share it: its crash recovery would mark this one's open
// incidents interrupted, and it would double every CSV row. The locks are
// the kernel's (flock), so they go away with the process, even after a
// crash.
//
// The directory itself is locked, so deleting the lock file (a cleanup
// script, a hand) can't let a second monitor in. The lock file is locked
// too, for a monitor that locks only the file, and it holds the holder's
// pid for the error the next one gets.
//
// On a filesystem without flock it returns errCannotLock, with an unlock
// that does nothing.
func lockDataDir(dir string) (unlock func(), err error) {
	path := filepath.Join(dir, lockFile)
	d, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot lock data directory %s: %w", dir, err)
	}
	if err := flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		d.Close()
		holder, _ := os.ReadFile(path)
		return refusedLock(dir, path, holder, err)
	}
	unlockFile, err := lockFileIn(dir, path)
	if unlockFile == nil {
		d.Close()
		return nil, err
	}
	return func() { unlockFile(); d.Close() }, err
}

// lockFileIn locks the lock file and writes this process's pid into it.
func lockFileIn(dir, path string) (unlock func(), err error) {
	// Not through a symlink: the pid would overwrite whatever it points at.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	writable := err == nil
	if errors.Is(err, fs.ErrPermission) {
		// A lock file left by another user: flock needs no write access.
		if ro, err2 := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0); err2 == nil {
			f, err = ro, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cannot lock data directory %s: %w", dir, err)
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := io.ReadAll(f)
		f.Close()
		return refusedLock(dir, path, holder, err)
	}
	if writable {
		if err := f.Truncate(0); err == nil {
			f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
		}
	}
	return func() { f.Close() }, nil
}

// refusedLock turns a failed flock into the error lockDataDir returns;
// holder is the lock file's contents, the holder's pid.
func refusedLock(dir, path string, holder []byte, err error) (func(), error) {
	switch {
	case errors.Is(err, syscall.EWOULDBLOCK):
		msg := "data directory " + dir + " is in use by another stream-analyzer"
		if pid := strings.TrimSpace(string(holder)); pid != "" {
			msg += " (pid " + pid + ")"
		}
		return nil, fmt.Errorf("%s; lock file %s", msg, path)
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOLCK):
		return func() {}, fmt.Errorf("%w: %s: %w", errCannotLock, path, err)
	}
	return nil, fmt.Errorf("cannot lock data directory %s: %w", dir, err)
}
