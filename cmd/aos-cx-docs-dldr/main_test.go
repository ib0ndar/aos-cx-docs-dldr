//go:build unix

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runMainEnvironment makes the test binary run the real main function, so the
// signal handling below is exercised exactly as shipped.
const runMainEnvironment = "AOSCX_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnvironment) == "1" {
		main()
	}
	os.Exit(m.Run())
}

func TestShutdownSignalsCancelRunWithExit130(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(signal.String(), func(t *testing.T) {
			requestStarted := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/robots.txt":
					w.WriteHeader(http.StatusNotFound)
				case "/slow":
					requestStarted <- struct{}{}
					<-r.Context().Done()
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "--list", "--portal-url", server.URL+"/slow",
				"--raw-cache", t.TempDir(), "--delay", "0", "--timeout", "30", "--attempt-timeout", "30")
			command.Env = append(os.Environ(), runMainEnvironment+"=1")
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requestStarted:
			case <-ctx.Done():
				command.Wait()
				t.Fatalf("executable did not start the expected request: %s", &stderr)
			}
			if err := command.Process.Signal(signal); err != nil {
				command.Process.Kill()
				command.Wait()
				t.Fatal(err)
			}
			err := command.Wait()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 130 || !strings.Contains(stderr.String(), "Cancelled") {
				t.Fatalf("%s exit=%v stderr=%s", signal, err, &stderr)
			}
		})
	}
}
