//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd)

package monitor

// lockDataDir does nothing where flock isn't available: the monitor is run
// on macOS, and a second one on the same data directory goes unnoticed.
func lockDataDir(dir string) (unlock func(), err error) { return func() {}, nil }
