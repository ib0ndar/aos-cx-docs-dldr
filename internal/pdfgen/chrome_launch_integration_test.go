//go:build chromepdftests

package pdfgen

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestPinnedChromeHasNoTCPListenerAndExitsWithParent launches the pinned
// Chrome through a child test process exactly as a render does. While it runs,
// no process in Chrome's group may listen on TCP and no DevToolsActivePort
// file may exist. After the parent is SIGKILLed, so none of its cleanup runs,
// the whole Chrome group must exit by itself: on macOS because its DevTools
// pipe closed, on Linux also through the parent-death signal.
func TestPinnedChromeHasNoTCPListenerAndExitsWithParent(t *testing.T) {
	chrome := os.Getenv(ChromePathEnvironment)
	if chrome == "" {
		t.Skip(ChromePathEnvironment + " is not set")
	}
	if _, err := reverifySidecar(chrome); err != nil {
		t.Fatal(err)
	}
	lsof, lsofErr := exec.LookPath("lsof")
	if lsofErr != nil {
		lsof, lsofErr = exec.LookPath("/usr/sbin/lsof")
	}
	if lsofErr != nil && runtime.GOOS == "darwin" {
		t.Fatal("lsof is required to inspect Chrome's sockets on macOS")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(executable)
	parent.Env = append(os.Environ(),
		chromeParentHelperEnvironment+"="+chrome, chromeParentRootEnvironment+"="+t.TempDir())
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	parent.Stderr = &stderr
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		parent.Process.Kill()
		parent.Wait()
	}()
	reported := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		reported <- strings.TrimSpace(line)
	}()
	var line string
	select {
	case line = <-reported:
	case <-time.After(90 * time.Second):
		t.Fatalf("parent did not start Chrome: %s", &stderr)
	}
	pidText, profile, ok := strings.Cut(line, "\t")
	pid, err := strconv.Atoi(pidText)
	if !ok || err != nil || pid <= 0 {
		t.Fatalf("parent did not report Chrome: %q stderr=%s", line, &stderr)
	}
	group, err := syscall.Getpgid(pid)
	if err != nil || group != pid {
		t.Fatalf("Chrome is not in its own process group: pid=%d pgid=%d %v", pid, group, err)
	}
	defer syscall.Kill(-group, syscall.SIGKILL)

	if _, err := os.Stat(filepath.Join(profile, "DevToolsActivePort")); !os.IsNotExist(err) {
		t.Fatalf("Chrome published a DevTools TCP port file: %v", err)
	}
	if lsofErr == nil {
		var listeners bytes.Buffer
		command := exec.Command(lsof, "-nP", "-a", "-g", strconv.Itoa(group), "-iTCP", "-sTCP:LISTEN")
		command.Stdout, command.Stderr = &listeners, &listeners
		if err := command.Run(); err == nil || listeners.Len() != 0 {
			t.Fatalf("Chrome process group listens on TCP: %v\n%s", err, &listeners)
		}
		// Positive control: lsof can see this process, including both
		// DevTools pipe descriptors.
		pipes, err := exec.Command(lsof, "-nP", "-a", "-p", strconv.Itoa(pid), "-d", "3,4").Output()
		if err != nil || strings.Count(string(pipes), "PIPE")+strings.Count(string(pipes), "FIFO") != 2 {
			t.Fatalf("lsof did not show Chrome's DevTools pipes: %v\n%s", err, pipes)
		}
	} else {
		t.Logf("lsof unavailable on %s; checked DevToolsActivePort only", runtime.GOOS)
	}

	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	parent.Wait()
	killed := time.Now()
	deadline := killed.Add(30 * time.Second)
	for {
		err := syscall.Kill(-group, 0)
		if errors.Is(err, syscall.ESRCH) {
			t.Logf("Chrome process group exited %s after its parent was killed", time.Since(killed).Round(time.Millisecond))
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Chrome process group %d outlived its SIGKILLed parent: %v", group, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
