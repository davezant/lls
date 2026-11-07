//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package index

import "os"

// mapFile reads the whole file on platforms without a portable mmap.
func mapFile(path string) ([]byte, func() error, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return b, nil, nil
}
