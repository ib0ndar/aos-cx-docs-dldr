package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type cancelOnWrite struct {
	cancel context.CancelFunc
}

func (w cancelOnWrite) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestCancellationOnFinalSuccessfulOutputReturns130(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/aoscx.html":
			io.WriteString(w, fixturePortal)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	defer server.Close()
	for _, jsonOutput := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		args := []string{"--list", "--portal-url", server.URL + "/aoscx.html", "--raw-cache", t.TempDir(), "--delay", "0"}
		if jsonOutput {
			args = append(args, "--json")
		}
		var stderr bytes.Buffer
		code := runContract(ctx, args, cancelOnWrite{cancel}, &stderr)
		cancel()
		if code != 130 || !strings.Contains(stderr.String(), "Cancelled") {
			t.Fatalf("final output cancellation lost: json=%v exit=%d stderr=%s", jsonOutput, code, &stderr)
		}
	}
}

func TestCancellationWinsEvenWhenHelpCommandReturnsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	code := runContract(ctx, []string{"--help"}, cancelOnWrite{cancel}, &stderr)
	if code != 130 || !strings.Contains(stderr.String(), "Cancelled") {
		t.Fatalf("successful help completion hid cancellation: exit=%d stderr=%s", code, &stderr)
	}
}

func TestAlreadyCancelledContextWritesNoCompletedOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := runContract(ctx, []string{"--app-version"}, &stdout, &stderr)
	if code != 130 || stdout.Len() != 0 {
		t.Fatalf("cancelled command wrote completed output: exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}
