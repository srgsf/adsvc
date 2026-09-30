//go:build !unix

package library

import (
	"io"
	"os"
)

// mapFile emulates a mapping with a heap copy where there is no mmap.
func mapFile(f *os.File, size int, writable bool) ([]byte, error) {
	b := make([]byte, size)
	if writable {
		return b, nil
	}
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, int64(size)), b); err != nil {
		return nil, err
	}
	return b, nil
}

// unmapFile writes a writable emulated mapping back to f.
func unmapFile(f *os.File, b []byte, writable bool) error {
	if !writable {
		return nil
	}
	_, err := f.WriteAt(b, 0)
	return err
}
