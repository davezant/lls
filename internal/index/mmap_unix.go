//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package index

import (
	"os"
	"syscall"
)

// mapFile maps path read-only into memory. The kernel pages data in on
// demand, so untouched modules never consume RAM.
func mapFile(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	size := st.Size()
	if size == 0 {
		return nil, nil, ErrFormat
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		// Fall back to reading the file (e.g. on file systems without mmap).
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, nil, err
		}
		return b, nil, nil
	}
	return data, func() error { return syscall.Munmap(data) }, nil
}
