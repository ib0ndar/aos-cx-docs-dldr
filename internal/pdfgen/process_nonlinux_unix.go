//go:build unix && !linux

package pdfgen

import "syscall"

// sidecarProcessAttributes places a sidecar in its own process group. macOS
// has no parent-death signal: Chrome instead shuts down when its DevTools pipe
// closes with this process, and qpdf is a bounded batch process that exits on
// its own.
func sidecarProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
