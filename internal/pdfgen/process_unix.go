//go:build unix

package pdfgen

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processPeakRSS reports the peak resident set size of one finished sidecar
// process. Darwin reports Maxrss in bytes; other Unix kernels report kibibytes.
func processPeakRSS(state *os.ProcessState) int64 {
	if state == nil {
		return 0
	}
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok || usage.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	if usage.Maxrss > (1<<63-1)/1024 {
		return 1<<63 - 1
	}
	return usage.Maxrss * 1024
}

// processGroupRSS totals the resident set size of every process in one process
// group, so a renderer that forks helper processes is accounted for as a whole.
func processGroupRSS(ctx context.Context, processGroup int) (int64, error) {
	sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	command := exec.CommandContext(sampleCtx, "/bin/ps", "-axo", "pgid=,rss=")
	var output bytes.Buffer
	command.Stdout = &limitedBuffer{buffer: &output, limit: 16 << 20}
	command.Stderr = &limitedBuffer{buffer: &bytes.Buffer{}, limit: 64 << 10}
	if err := command.Run(); err != nil {
		if sampleCtx.Err() != nil {
			return 0, sampleCtx.Err()
		}
		return 0, err
	}
	var total int64
	for _, line := range strings.Split(output.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		group, groupErr := strconv.Atoi(fields[0])
		rssKB, rssErr := strconv.ParseInt(fields[1], 10, 64)
		if groupErr != nil || rssErr != nil {
			return 0, errors.New("unexpected /bin/ps process accounting output")
		}
		if group == processGroup {
			if rssKB > (1<<63-1)/1024 {
				return 0, errors.New("Chrome RSS accounting overflow")
			}
			bytes := rssKB * 1024
			if total > 1<<63-1-bytes {
				return 0, errors.New("Chrome RSS accounting overflow")
			}
			total += bytes
		}
	}
	return total, nil
}

// configureProcessGroup isolates a sidecar in its own process group so that
// cancellation reliably reaps the whole renderer tree rather than only the
// process this package launched directly.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
}
