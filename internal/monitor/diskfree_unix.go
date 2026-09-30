//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package monitor

import "syscall"

// diskFree returns the bytes available to this user on dir's filesystem.
func diskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
