package library

import (
	"os"
	"syscall"
)

// fallocate reserves the blocks of the first size bytes of f; ok is false where the
// filesystem cannot, and zeros have to be written instead.
func fallocate(f *os.File, size int64) (ok bool, err error) {
	switch err := syscall.Fallocate(int(f.Fd()), 0, 0, size); err {
	case nil:
		return true, nil
	case syscall.EOPNOTSUPP, syscall.ENOSYS, syscall.EINVAL:
		return false, nil
	default:
		return false, &os.PathError{Op: "fallocate", Path: f.Name(), Err: err}
	}
}
