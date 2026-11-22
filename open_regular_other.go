//go:build !unix

package main

import "os"

// openRegularFile opens path for reading. This platform has no O_NOFOLLOW:
// candidates are rejected as symlinks while the folder is listed (Lstat
// never follows), and the opened handle is verified to be a regular file
// before anything is read. A symlink swapped in between those two steps
// would be followed — the documented limitation on non-unix systems.
func openRegularFile(path string) (*os.File, error) {
	return os.Open(path)
}
