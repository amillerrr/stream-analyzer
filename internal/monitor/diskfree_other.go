//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd)

package monitor

import "errors"

// diskFree is not available on this platform.
func diskFree(string) (uint64, error) { return 0, errors.New("free space unknown on this platform") }
