//go:build !darwin && !linux

package pdfgen

import "io/fs"

// fileSnapshotOf reports no snapshot support, so every check re-hashes.
// Generated-PDF conversion is pinned only for macOS arm64 and Linux amd64.
func fileSnapshotOf(fs.FileInfo) (fileSnapshot, bool) { return fileSnapshot{}, false }
