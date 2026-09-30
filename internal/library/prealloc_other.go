//go:build !linux

package library

import "os"

// fallocate reserves the blocks of the first size bytes of f; ok is false where it
// cannot, and zeros have to be written instead.
func fallocate(*os.File, int64) (ok bool, err error) { return false, nil }
