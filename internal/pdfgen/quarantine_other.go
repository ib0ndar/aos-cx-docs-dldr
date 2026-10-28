//go:build !darwin

package pdfgen

import (
	"errors"
	"io/fs"
	"os"
)

// Only macOS quarantines downloaded files.
const quarantineSupported = false

var errNoQuarantine = errors.New("download quarantine exists only on macOS")

func openPinnedNoFollow(string) (*os.File, error) { return nil, errNoQuarantine }

func hasQuarantine(*os.File) (bool, error) { return false, nil }

func removeQuarantine(*os.File, fs.FileInfo) error { return errNoQuarantine }

func attributesNeedPrivilege(fs.FileInfo) bool { return false }
