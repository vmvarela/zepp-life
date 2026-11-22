//go:build unix

package main

import (
	"os"
	"syscall"
)

// openRegularFile opens path read-only without following a final symlink
// (O_NOFOLLOW) and without blocking on a FIFO (O_NONBLOCK): a candidate
// swapped after the folder was listed fails with ELOOP instead of being
// followed, and open(2) returns immediately for a FIFO so the handle can be
// rejected as non-regular before any read. The caller must still verify that
// the opened handle is a regular file.
func openRegularFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
