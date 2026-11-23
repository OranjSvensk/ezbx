//go:build linux || darwin

package disk

import "syscall"

func free(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bavail is what an unprivileged user may use (root keeps a reserve).
	return st.Bavail * uint64(st.Bsize), nil
}
