//go:build !unix

package storage

import "os"

// Owner reports that numeric owners are unavailable on this platform.
func Owner(os.FileInfo) (uid int, ok bool) {
	return 0, false
}
