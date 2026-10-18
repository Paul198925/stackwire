//go:build !windows

package storage

import "syscall"

// Space returns the size and the free space of the filesystem that holds path.
func Space(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// The field types differ between platforms, hence the conversions.
	bs := uint64(st.Bsize)
	return uint64(st.Blocks) * bs, uint64(st.Bavail) * bs, nil
}
