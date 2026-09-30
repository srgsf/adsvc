//go:build unix

package library

import (
	"os"
	"syscall"
)

// mapFile maps the first size bytes of f into memory, shared with the file (writes through
// a writable mapping land in the file's pages; fsync makes them durable).
func mapFile(f *os.File, size int, writable bool) ([]byte, error) {
	prot := syscall.PROT_READ
	if writable {
		prot |= syscall.PROT_WRITE
	}
	return syscall.Mmap(int(f.Fd()), 0, size, prot, syscall.MAP_SHARED)
}

// unmapFile releases a mapping of mapFile. A writable one is written to f first where
// mappings are emulated.
func unmapFile(_ *os.File, b []byte, _ bool) error { return syscall.Munmap(b) }
