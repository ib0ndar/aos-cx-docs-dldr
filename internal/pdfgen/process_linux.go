package pdfgen

import "syscall"

// sidecarProcessAttributes places a sidecar in its own process group and has
// the kernel SIGKILL it when this process dies without running its cleanup,
// for example after SIGKILL. A process-group sidecar never receives the
// terminal's hang-up itself. The death signal follows the thread that started
// the child; this program never locks goroutines to threads, so the runtime
// does not retire that thread while the sidecar runs.
func sidecarProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
