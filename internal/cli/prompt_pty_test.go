//go:build darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/library"
	"aos-cx-docs-dldr/internal/testutil"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func waitForPTY(t *testing.T, output *lockedBuffer, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(output.String(), text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PTY did not show %q:\n%s", text, output.String())
}

func waitForPTYCount(t *testing.T, output *lockedBuffer, text string, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(output.String(), text) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("PTY did not show %q %d times:\n%s", text, count, output.String())
}

func promptFixtureServer(t *testing.T, advertiseNativePDF ...bool) *httptest.Server {
	t.Helper()
	nativePDF := len(advertiseNativePDF) > 0 && advertiseNativePDF[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.17</option><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">PTY Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/guide.json":
			fmt.Fprintf(w, `{"10.16":{"6300":%q}}`, "http://"+r.Host+"/book/index.html")
		case "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			advertisement := ""
			if nativePDF {
				advertisement = `<link rel="alternate" type="application/pdf" href="original.pdf">`
			}
			io.WriteString(w, `<html><head>`+advertisement+`</head><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`)
		case "/book/original.pdf":
			if !nativePDF {
				t.Errorf("unexpected optional PDF request: %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
			if r.Method == http.MethodGet {
				w.Write(testutil.PDF("PTY native publisher PDF"))
			}
		case "/book/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1><pre>show  vlan</pre></article>`)
		default:
			t.Errorf("unexpected PTY fixture request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func promptMixedPDFFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="native" onclick="openFile('HTML','native','aoscx')">Native-capable Guide</td>
<td id="html" onclick="openFile('HTML','html','aoscx')">HTML-only Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/native.json":
			fmt.Fprintf(w, `{"10.16":{"6300":%q}}`, server.URL+"/native/index.html")
		case "/portal/json/aoscx/html.json":
			fmt.Fprintf(w, `{"10.16":{"6300":%q}}`, server.URL+"/html/index.html")
		case "/native/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><head><link rel="alternate" type="application/pdf" href="guide.pdf"></head>
<body><nav class="wh_publication_toc"><ul><li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Native</h1></main></body></html>`)
		case "/native/guide.pdf":
			if r.Method != http.MethodHead {
				t.Errorf("unexpected optional PDF method: %s", r.Method)
			}
			w.Header().Set("Content-Type", "application/pdf")
		case "/html/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul><li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>HTML</h1></main></body></html>`)
		default:
			t.Errorf("unexpected mixed PDF fixture request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func promptProgressFixtureServer(t *testing.T, topics int) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>8320</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="acl" onclick="openFile('HTML','acl','aoscx')">ACLs 文档 and Classifiers Policy Guide with a very long title</td>
</tr></table></div></html>`)
		case r.URL.Path == "/portal/json/aoscx/acl.json":
			json.NewEncoder(w).Encode(map[string]any{
				"10.18.xxxx": map[string]string{"8320": server.URL + "/book/index.html"},
			})
		case r.URL.Path == "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			var body strings.Builder
			body.WriteString(`<html><body><nav class="wh_publication_toc"><ul>`)
			for index := 1; index <= topics; index++ {
				fmt.Fprintf(&body, `<li><a href="topic-%03d.html">Topic %03d</a></li>`, index, index)
			}
			body.WriteString(`</ul></nav><main class="wh_topic_content"><h1>Home</h1></main></body></html>`)
			io.WriteString(w, body.String())
		case strings.HasPrefix(r.URL.Path, "/book/topic-"):
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<article><h1>%s</h1><pre>show  access-list</pre></article>`,
				strings.TrimSuffix(filepath.Base(r.URL.Path), ".html"))
		default:
			t.Errorf("unexpected progress fixture request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func promptStartupProgressFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			time.Sleep(375 * time.Millisecond)
			io.WriteString(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="ha" onclick="openFile('HTML','ha','aoscx')">High Availability</td>
<td id="job" onclick="openFile('HTML','job','aoscx')">Job Scheduler</td>
<td id="vsx" onclick="openFile('HTML','vsx','aoscx')">VSX</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/ha.json", "/portal/json/aoscx/job.json", "/portal/json/aoscx/vsx.json":
			id := strings.TrimSuffix(filepath.Base(r.URL.Path), ".json")
			delay := map[string]time.Duration{"ha": 150, "job": 300, "vsx": 450}[id]
			time.Sleep(delay * time.Millisecond)
			fmt.Fprintf(w, `{"10.16":{"6300":%q}}`, server.URL+"/book-"+id+"/index.html")
		case "/book-ha/index.html", "/book-job/index.html", "/book-vsx/index.html":
			id := strings.TrimPrefix(strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0], "book-")
			delay := map[string]time.Duration{"ha": 150, "job": 300, "vsx": 450}[id]
			time.Sleep(delay * time.Millisecond)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">%s topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>%s</h1></main></body></html>`, id, id)
		default:
			t.Errorf("unexpected startup-progress request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func promptIncompleteFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>8320</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="acl" onclick="openFile('HTML','acl','aoscx')">ACL Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/acl.json":
			json.NewEncoder(w).Encode(map[string]any{
				"10.18.xxxx": map[string]string{"8320": server.URL + "/book/index.html"},
			})
		case "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`)
		case "/book/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1><img src="missing.png"></article>`)
		case "/book/missing.png":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected incomplete PTY request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func promptCancellationFixtureServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	active := make(chan struct{}, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>8320</option></select>
<select id="ver"><option>10.18.xxxx</option></select><div id="menu1"><table><tr>
<td id="first" onclick="openFile('PDF','first','aoscx')">First PDF</td>
<td id="second" onclick="openFile('PDF','second','aoscx')">Second PDF</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/first.json":
			json.NewEncoder(w).Encode(map[string]any{
				"10.18.xxxx": map[string]string{"8320": server.URL + "/first.pdf"},
			})
		case "/portal/json/aoscx/second.json":
			json.NewEncoder(w).Encode(map[string]any{
				"10.18.xxxx": map[string]string{"8320": server.URL + "/second.pdf"},
			})
		case "/first.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(testutil.PDF("first"))
		case "/second.pdf":
			select {
			case active <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		default:
			t.Errorf("unexpected cancellation PTY request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, active
}

func promptTypeaheadFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6300</option><option>6400</option></select>
<select id="ver"><option>10.17</option><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">PTY Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/guide.json":
			fmt.Fprintf(w, `{"10.17":{"6300":%q},"10.16":{"6300":%q}}`,
				"http://"+r.Host+"/book/index.html", "http://"+r.Host+"/book/index.html")
		case "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`)
		case "/book/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>Topic</h1></article>`)
		default:
			t.Errorf("unexpected PTY typeahead fixture request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func promptBackFixtureServer(t *testing.T) (*httptest.Server, *[]string, *sync.Mutex) {
	t.Helper()
	requests := &[]string{}
	var requestsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		*requests = append(*requests, r.URL.Path)
		requestsMu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<html><select id="platform"><option>6300</option><option>6400</option></select>
<select id="ver"><option>10.17</option><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide-a" onclick="openFile('HTML','guide-a','aoscx')">Guide A</td>
<td id="guide-b" onclick="openFile('HTML','guide-b','aoscx')">Guide B</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/guide-a.json":
			fmt.Fprintf(w, `{"10.17":{"6300":%q},"10.16":{"6400":%q}}`,
				"http://"+r.Host+"/old/index.html", "http://"+r.Host+"/new/index.html")
		case "/portal/json/aoscx/guide-b.json":
			fmt.Fprintf(w, `{"10.17":{"6300":%q}}`, "http://"+r.Host+"/old-b/index.html")
		case "/new/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="topic.html">New Topic</a></li></ul></nav>
<main class="wh_topic_content"><h1>New Home</h1></main></body></html>`)
		case "/new/topic.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<article><h1>New Topic</h1></article>`)
		default:
			if strings.HasPrefix(r.URL.Path, "/old") {
				t.Errorf("Back navigation fetched a stale selection: %s", r.URL)
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, requests, &requestsMu
}

func startPTYCLI(t *testing.T, server *httptest.Server, root string, extra ...string) (*os.File, *exec.Cmd, *lockedBuffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	script := `before="$(stty -g)"; "$@" ; code=$?; after="$(stty -g)"; printf '\nTTY_BEFORE:%s\nTTY_AFTER:%s\n' "$before" "$after"; exit "$code"`
	args := []string{"-c", script, "sh", binary(t),
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--raw-cache", filepath.Join(root, "cache"),
		"--delay", "0", "--retries", "0", "--workers", "4", "--json",
	}
	args = append(args, extra...)
	command := exec.CommandContext(ctx, "/bin/sh", args...)
	command.Dir = root
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	output := &lockedBuffer{}
	go func() {
		_, _ = io.Copy(output, terminal)
	}()
	return terminal, command, output
}

func finishPTYCLI(t *testing.T, terminal *os.File, command *exec.Cmd, output *lockedBuffer, wantCode int) {
	t.Helper()
	err := command.Wait()
	_ = terminal.Close()
	code := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	if code != wantCode {
		t.Fatalf("PTY exit=%d want=%d err=%v\n%s", code, wantCode, err, output.String())
	}
	waitForPTY(t, output, "TTY_AFTER:")
	before, after := "", ""
	for _, candidate := range strings.Split(output.String(), "\n") {
		candidate = strings.TrimSpace(candidate)
		if strings.HasPrefix(candidate, "TTY_BEFORE:") {
			before = strings.TrimPrefix(candidate, "TTY_BEFORE:")
		}
		if strings.HasPrefix(candidate, "TTY_AFTER:") {
			after = strings.TrimPrefix(candidate, "TTY_AFTER:")
		}
	}
	if before == "" || comparableTerminalState(before) != comparableTerminalState(after) {
		t.Fatalf("terminal state was not restored: before=%q after=%q\n%s", before, after, output.String())
	}
}

func comparableTerminalState(state string) string {
	fields := strings.Split(state, ":")
	for index, field := range fields {
		if !strings.HasPrefix(field, "lflag=") {
			continue
		}
		value, err := strconv.ParseUint(strings.TrimPrefix(field, "lflag="), 16, 64)
		if err == nil {
			fields[index] = fmt.Sprintf("lflag=%x", value&^uint64(0x20000000))
		}
	}
	return strings.Join(fields, ":")
}

func TestActualPTYFullySpecifiedCommandDoesNotPrompt(t *testing.T) {
	server := promptFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	terminal, command, output := startPTYCLI(t, server, root,
		"--platform", "6300", "--version", "10.16", "--all", "--destination", destination)
	defer terminal.Close()
	finishPTYCLI(t, terminal, command, output, 0)
	text := output.String()
	for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve", "Save documentation"} {
		if strings.Contains(text, prompt) {
			t.Fatalf("fully specified command prompted for %q:\n%s", prompt, text)
		}
	}

	terminal, command, output = startPTYCLI(t, server, root)
	defer terminal.Close()
	for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
		waitForPTY(t, output, prompt)
		_, _ = terminal.Write([]byte("\r"))
	}
	waitForPTY(t, output, "Save documentation under which directory?")
	_, _ = terminal.Write([]byte(destination + "\r"))
	waitForPTY(t, output, "An existing library was found")
	_, _ = terminal.Write([]byte("\r"))
	finishPTYCLI(t, terminal, command, output, 0)
}

func TestActualPTYAvailabilityFirstLabelsAndPreference(t *testing.T) {
	for _, tc := range []struct {
		name, label string
		advertise   bool
	}{
		{"unavailable", "PTY Guide [HTML (static)]", false},
		{"available", "PTY Guide [HTML (static) / PDF native]", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := promptFixtureServer(t, tc.advertise)
			root := t.TempDir()
			destination := filepath.Join(root, "library")
			terminal, command, output := startPTYCLI(t, server, root)
			defer terminal.Close()
			for _, prompt := range []string{"Switch platform", "Documentation version"} {
				waitForPTY(t, output, prompt)
				_, _ = terminal.Write([]byte("\r"))
			}
			waitForPTY(t, output, "Documents to retrieve")
			_, _ = terminal.Write([]byte("\x1b[B\r"))
			waitForPTY(t, output, "Select guides")
			waitForPTY(t, output, tc.label)
			_, _ = terminal.Write([]byte(" \r"))
			waitForPTY(t, output, "Save documentation under which directory?")
			_, _ = terminal.Write([]byte(destination + "\r"))
			if tc.advertise {
				waitForPTY(t, output, "Prefer a source-verified PDF when available?")
				_, _ = terminal.Write([]byte("\x1b[B\r"))
			} else {
				waitForPTY(t, output, "Generate PDF companions for the 1 HTML guide?")
				_, _ = terminal.Write([]byte("\r"))
			}
			finishPTYCLI(t, terminal, command, output, 0)
			plain := ansi.Strip(output.String())
			if !strings.Contains(plain, tc.label) {
				t.Fatalf("PTY omitted exact availability label %q:\n%s", tc.label, plain)
			}
			hasPreference := strings.Contains(plain, "Prefer a source-verified PDF when available?")
			if hasPreference != tc.advertise {
				t.Fatalf("PTY preference question mismatch: advertise=%v output:\n%s", tc.advertise, plain)
			}
			if tc.advertise && strings.Contains(plain, "Generate PDF companions for") {
				t.Fatalf("all-publisher-PDF flow asked redundant conversion question:\n%s", plain)
			}
			if !tc.advertise && strings.Contains(plain, "Preferred source-verified PDF was unavailable") {
				t.Fatalf("ordinary unavailable guided flow emitted fallback warning:\n%s", plain)
			}
		})
	}
}

func TestActualPTYOffersAllAvailableAndKeepsIndividualDisabled(t *testing.T) {
	server := promptUnavailableFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	terminal, command, output := startPTYCLI(t, server, root)
	defer terminal.Close()
	for _, prompt := range []string{"Switch platform", "Documentation version"} {
		waitForPTY(t, output, prompt)
		_, _ = terminal.Write([]byte("\r"))
	}
	waitForPTY(t, output, "Documents to retrieve")
	waitForPTY(t, output, "Mapped source unavailable for Introduction to the WebUI Guide: mapped source HTTP 404")
	_, _ = terminal.Write([]byte("\x1b[B\r"))
	waitForPTY(t, output, "Select guides")
	waitForPTY(t, output,
		"Introduction to the WebUI Guide [HTML (flare) - unavailable:")
	waitForPTY(t, output, "Other Guide [HTML (static)]")
	_, _ = terminal.Write([]byte(" \r"))
	waitForPTY(t, output, "Save documentation under which directory?")
	_, _ = terminal.Write([]byte(destination + "\r"))
	waitForPTY(t, output, "Generate PDF companions for the 1 HTML guide?")
	_, _ = terminal.Write([]byte("\r"))
	finishPTYCLI(t, terminal, command, output, 0)
	plain := ansi.Strip(output.String())
	if !strings.Contains(plain, "All available mapped guides (1)") ||
		!strings.Contains(plain, "Mapped source unavailable for Introduction to the WebUI Guide: mapped source HTTP 404") {
		t.Fatalf("PTY did not expose available-subset mode:\n%s", plain)
	}
	warning := "Mapped source unavailable for Introduction to the WebUI Guide"
	warningAt, promptAt := strings.Index(plain, warning), strings.Index(plain, "? Documents to retrieve")
	if warningAt < 0 || promptAt <= warningAt ||
		!strings.Contains(plain[warningAt:promptAt], "\r\n") ||
		plain[promptAt-1] != '\r' {
		t.Fatalf("persistent warning/prompt boundary did not reset to column zero:\n%q", plain[warningAt:promptAt+len("? Documents")])
	}
	if _, err := os.Stat(filepath.Join(destination, "6000", "10.17", "other", "index.html")); err != nil {
		t.Fatalf("available guide was not selected around disabled row: %v\n%s", err, plain)
	}
	if _, err := os.Stat(filepath.Join(destination, "6000", "10.17", "webUI")); !os.IsNotExist(err) {
		t.Fatalf("disabled guide was selected: %v\n%s", err, plain)
	}
}

func TestActualPTYMixedPDFSelectionShowsContextualConversionPrompt(t *testing.T) {
	server := promptMixedPDFFixtureServer(t)
	root := t.TempDir()
	terminal, command, output := startPTYCLI(t, server, root)
	defer terminal.Close()
	for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
		waitForPTY(t, output, prompt)
		_, _ = terminal.Write([]byte("\r"))
	}
	waitForPTY(t, output, "Save documentation under which directory?")
	_, _ = terminal.Write([]byte(filepath.Join(root, "library") + "\r"))
	waitForPTY(t, output, "Prefer a source-verified PDF when available?")
	_, _ = terminal.Write([]byte("\x1b[B\r"))
	waitForPTY(t, output, "1 selected guide will use publisher PDFs; 1 will use complete HTML.")
	waitForPTY(t, output, "Generate PDF companions for the 1 HTML guide?")
	_, _ = terminal.Write([]byte("\003"))
	finishPTYCLI(t, terminal, command, output, 130)
	plain := ansi.Strip(output.String())
	contextAt := strings.Index(plain, "1 selected guide will use publisher PDFs; 1 will use complete HTML.")
	promptAt := strings.Index(plain, "? Generate PDF companions for the 1 HTML guide?")
	if contextAt < 0 || promptAt <= contextAt || plain[promptAt-1] != '\n' {
		t.Fatalf("mixed conversion context/prompt alignment failed:\n%s", plain)
	}
	if _, err := os.Stat(filepath.Join(root, "library")); !os.IsNotExist(err) {
		t.Fatalf("mixed selection created output before confirmation: %v", err)
	}
}

func TestActualPTYStartupAndAvailabilityProgressPrecedePrompts(t *testing.T) {
	server := promptStartupProgressFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	if err := os.MkdirAll(filepath.Join(destination, "6300", "10.16"), 0o700); err != nil {
		t.Fatal(err)
	}
	terminal, command, output := startPTYCLI(
		t, server, root, "--destination", destination,
	)
	defer terminal.Close()

	waitForPTY(t, output, "Switch platform")
	portalFrames := matchingProgressFrames(output.String(), "Refreshing Product Documentation catalogue")
	if len(portalFrames) < 2 {
		t.Fatalf("delayed portal did not animate indeterminate progress: %v\n%s", portalFrames, output.String())
	}
	for _, frame := range portalFrames {
		if strings.Contains(frame, "%") || strings.Contains(frame, "/") {
			t.Fatalf("portal progress fabricated determinate state: %q", frame)
		}
	}
	mappingFrames := matchingProgressFrames(output.String(), "Refreshing guide mappings")
	if len(mappingFrames) < 2 ||
		!strings.Contains(mappingFrames[len(mappingFrames)-1], "3/3 (100%)") {
		t.Fatalf("mapping progress did not reach its exact total: %v\n%s", mappingFrames, output.String())
	}
	_, _ = terminal.Write([]byte("\r"))
	waitForPTY(t, output, "Documentation version")
	_, _ = terminal.Write([]byte("\r"))
	waitForPTY(t, output, "Documents to retrieve")
	_, _ = terminal.Write([]byte("\x1b[B\r"))
	waitForPTY(t, output, "Checking guide availability")
	waitForPTY(t, output, "Select guides")

	availabilityFrames := matchingProgressFrames(output.String(), "Checking guide availability")
	if len(availabilityFrames) < 2 ||
		!strings.Contains(availabilityFrames[len(availabilityFrames)-1], "3/3 (100%)") {
		t.Fatalf("availability progress did not reach its exact total: %v\n%s", availabilityFrames, output.String())
	}
	for _, frame := range append(append(portalFrames, mappingFrames...), availabilityFrames...) {
		if ansi.StringWidth(frame) > 79 {
			t.Fatalf("startup progress wrapped into final terminal column: width=%d frame=%q",
				ansi.StringWidth(frame), frame)
		}
	}
	waitForPTY(t, output, "High Availability [HTML (static)]")
	_, _ = terminal.Write([]byte(" \r"))
	waitForPTY(t, output, "An existing library was found")
	if strings.Contains(ansi.Strip(output.String()), "Prefer a source-verified PDF when available?") {
		t.Fatalf("HTML-only selection regained the misleading preference prompt:\n%s", output.String())
	}
	_, _ = terminal.Write([]byte{3})
	finishPTYCLI(t, terminal, command, output, 130)
}

func matchingProgressFrames(text, label string) []string {
	var matches []string
	for _, frame := range strings.Split(text, "\r\x1b[2K") {
		plain := ansi.Strip(frame)
		if newline := strings.IndexByte(plain, '\n'); newline >= 0 {
			plain = plain[:newline]
		}
		plain = strings.TrimSpace(plain)
		if strings.Contains(plain, label) {
			matches = append(matches, plain)
		}
	}
	return matches
}

func TestActualPTYFullySpecifiedProgressIsBounded(t *testing.T) {
	server := promptProgressFixtureServer(t, 104)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	terminal, command, output := startPTYCLI(t, server, root,
		"--platform", "8320", "--version", "10.18.xxxx",
		"--guides", "acl", "--destination", destination)
	defer terminal.Close()
	finishPTYCLI(t, terminal, command, output, 0)

	var frames []string
	for _, part := range strings.Split(output.String(), "\r\x1b[2K") {
		line := strings.SplitN(part, "\n", 2)[0]
		plain := ansi.Strip(strings.TrimSuffix(line, "\r"))
		if !strings.Contains(plain, "topics [") &&
			!strings.Contains(plain, "Building offline") &&
			!strings.Contains(plain, "Hashing generated") &&
			!strings.Contains(plain, "Checking offline") &&
			!strings.Contains(plain, "Writing and syncing") &&
			!strings.Contains(plain, "offline checks finished") {
			continue
		}
		frames = append(frames, plain)
		if width := ansi.StringWidth(line); width > 79 {
			t.Fatalf("progress frame wrapped at width %d:\n%q\n%s", width, line, output.String())
		}
	}
	if len(frames) == 0 || len(frames) > 32 {
		t.Fatalf("fully specified terminal emitted %d progress frames, want 1..32:\n%s",
			len(frames), output.String())
	}
	joined := strings.Join(frames, "\n")
	for _, expected := range []string{
		"105/105", "100%", "Building offline", "Hashing generated",
		"Checking offline", "Writing and syncing", "offline checks finished",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("progress frames omitted %q:\n%s", expected, joined)
		}
	}
	buildingAdvanced := false
	for _, frame := range frames {
		if strings.Contains(frame, "Building offline") &&
			!strings.Contains(frame, " 0/105 ") &&
			!strings.Contains(frame, " 105/105 ") {
			buildingAdvanced = true
		}
	}
	if !buildingAdvanced {
		t.Fatalf("delayed PTY fixture did not visibly advance page emission:\n%s", joined)
	}
	if !strings.Contains(ansi.Strip(output.String()), "Published complete library") {
		t.Fatalf("terminal did not finish with authoritative success:\n%s", output.String())
	}
}

func TestActualPTYDumbTerminalUsesSparsePlainProgress(t *testing.T) {
	t.Setenv("TERM", "dumb")
	server := promptProgressFixtureServer(t, 9)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	terminal, command, output := startPTYCLI(t, server, root,
		"--platform", "8320", "--version", "10.18.xxxx",
		"--guides", "acl", "--destination", destination)
	defer terminal.Close()
	finishPTYCLI(t, terminal, command, output, 0)

	text := output.String()
	plain := ansi.Strip(text)
	if strings.Contains(text, "\r\x1b[2K") || regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(text) {
		t.Fatalf("TERM=dumb emitted terminal animation or SGR styling:\n%q", text)
	}
	for _, expected := range []string{
		"topics 10/10 (100%)", "Building offline guide",
		"Hashing generated files", "Checking offline references",
		"Writing and syncing manifest", "offline checks finished",
		"Published complete library",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("TERM=dumb sparse progress omitted %q:\n%s", expected, plain)
		}
	}
	progressLines := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "[1/1]") || strings.Contains(line, "Product Documentation catalogue") ||
			strings.Contains(line, "Catalogue ready:") || strings.Contains(line, "Publishing accepted guides") ||
			strings.Contains(line, "Published complete library") {
			progressLines++
		}
	}
	if progressLines > 40 {
		t.Fatalf("TERM=dumb progress was not sparse (%d progress lines):\n%s", progressLines, text)
	}
}

func TestActualPTYIncompleteResultShowsReasonsAndWarningStatus(t *testing.T) {
	server := promptIncompleteFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	terminal, command, output := startPTYCLI(t, server, root,
		"--platform", "8320", "--version", "10.18.xxxx",
		"--guides", "acl", "--destination", destination)
	defer terminal.Close()
	finishPTYCLI(t, terminal, command, output, 2)

	text := output.String()
	plain := ansi.Strip(text)
	for _, expected := range []string{
		"Guide acl archive error:",
		"missing.png",
		"Full diagnostics: " + filepath.Join(destination, "8320", "10.18.xxxx", "manifest.json"),
		"Published incomplete library",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("PTY incomplete diagnostics omitted %q:\n%s", expected, plain)
		}
	}
	if strings.Contains(plain, "Published complete library") {
		t.Fatalf("incomplete PTY reported success:\n%s", plain)
	}
	if !regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(text) {
		t.Fatal("incomplete PTY diagnostics were not semantically colored")
	}
}

func TestActualPTYCancellationDuringProgressPublishesAndRestoresTerminal(t *testing.T) {
	server, active := promptCancellationFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "library")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := `before="$(stty -g)"; trap '' INT; "$@"; code=$?; trap - INT; after="$(stty -g)"; printf '\nTTY_BEFORE:%s\nTTY_AFTER:%s\n' "$before" "$after"; exit "$code"`
	args := []string{"-c", script, "sh", binary(t),
		"--transport", "http", "--platform", "8320", "--version", "10.18.xxxx",
		"--all", "--destination", destination, "--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0", "--retries", "0", "--json",
	}
	command := exec.CommandContext(ctx, "/bin/sh", args...)
	command.Dir = root
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	output := &lockedBuffer{}
	go func() { _, _ = io.Copy(output, terminal) }()
	select {
	case <-active:
	case <-ctx.Done():
		t.Fatal("second PDF did not enter the cancellable progress stage")
	}
	_, _ = terminal.Write([]byte{3})
	finishPTYCLI(t, terminal, command, output, 130)
	manifestBytes, err := os.ReadFile(filepath.Join(destination, "8320", "10.18.xxxx", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest library.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "incomplete" || len(manifest.PDFGuides) != 1 ||
		manifest.PDFGuides["first"].Status != "complete" {
		t.Fatalf("PTY cancellation did not publish accepted work: %+v", manifest)
	}
	if !strings.Contains(ansi.Strip(output.String()), "Cancelled; accepted guides") {
		t.Fatalf("PTY cancellation omitted its durable result:\n%s", output.String())
	}
}

func TestActualPTYGuidedAllAndMultiSelect(t *testing.T) {
	server := promptFixtureServer(t)
	for _, mode := range []string{"all", "some"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			destination := filepath.Join(root, "Library 文档")
			terminal, command, output := startPTYCLI(t, server, root)
			defer terminal.Close()
			waitForPTY(t, output, "Switch platform")
			_, _ = terminal.Write([]byte("\r"))
			waitForPTY(t, output, "Documentation version")
			_, _ = terminal.Write([]byte("\r"))
			waitForPTY(t, output, "Documents to retrieve")
			if mode == "some" {
				_, _ = terminal.Write([]byte("\x1b[B\r"))
				waitForPTY(t, output, "Select guides")
				_, _ = terminal.Write([]byte(" \r"))
			} else {
				_, _ = terminal.Write([]byte("\r"))
			}
			waitForPTY(t, output, "Save documentation under which directory?")
			_, _ = terminal.Write([]byte(destination + "\r"))
			finishPTYCLI(t, terminal, command, output, 0)
			manifest := filepath.Join(destination, "6300", "10.16", "manifest.json")
			if _, err := os.Stat(manifest); err != nil {
				t.Fatalf("guided %s selection did not publish expected library: %v\n%s", mode, err, output.String())
			}
		})
	}
}

func TestActualPTYBurstInputCrossesPromptTransitions(t *testing.T) {
	server := promptTypeaheadFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "Burst Library")
	terminal, command, output := startPTYCLI(t, server, root)
	defer terminal.Close()
	waitForPTY(t, output, "Switch platform")
	_, _ = terminal.Write([]byte("\r\x1b[B\r\r" + destination + "\r"))
	finishPTYCLI(t, terminal, command, output, 0)
	manifest := filepath.Join(destination, "6300", "10.16", "manifest.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("burst input did not retain confirmed platform and next-prompt keys: %v\n%s", err, output.String())
	}
}

func TestActualPTYLeftBackRebuildsSelectionAndPreservesDestination(t *testing.T) {
	server, requests, requestsMu := promptBackFixtureServer(t)
	root := t.TempDir()
	destination := filepath.Join(root, "Back Library")
	terminal, command, output := startPTYCLI(t, server, root)
	defer terminal.Close()
	waitForPTY(t, output, "Switch platform")
	_, _ = terminal.Write([]byte(
		"\r" + // accept 6300
			"\x1b[D" + // release -> platform
			"\x1b[B\r" + // select 6400
			"\r" + // accept the now-enabled 10.16
			"\r" + // all mapped guides
			destination + "\x1b[D" + // destination -> mode, retaining draft
			"\r" + // confirm all again
			"\r", // accept restored destination
	))
	finishPTYCLI(t, terminal, command, output, 0)
	manifest := filepath.Join(destination, "6400", "10.16", "manifest.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("Back flow did not publish the final selection: %v\n%s", err, output.String())
	}
	if _, err := os.Stat(filepath.Join(destination, "6300", "10.17")); !os.IsNotExist(err) {
		t.Fatalf("Back flow created stale selection output: %v", err)
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	for _, request := range *requests {
		if strings.HasPrefix(request, "/old") {
			t.Fatalf("Back flow fetched stale route %q: %v", request, *requests)
		}
	}
}

func TestActualPTYPartialFixedGuidesAndExplicitAllFalse(t *testing.T) {
	server, _, _ := promptBackFixtureServer(t)
	t.Run("fixed-guides", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "Fixed Guides")
		terminal, command, output := startPTYCLI(t, server, root,
			"--guides", "guide-a", "--destination", destination)
		defer terminal.Close()
		waitForPTY(t, output, "Switch platform")
		_, _ = terminal.Write([]byte("\x1b[B\r\r"))
		finishPTYCLI(t, terminal, command, output, 0)
		if _, err := os.Stat(filepath.Join(destination, "6400", "10.16", "manifest.json")); err != nil {
			t.Fatalf("fixed partial guide was lost: %v\n%s", err, output.String())
		}
		for _, title := range []string{"Documents to retrieve", "Select guides", "Save documentation"} {
			if strings.Contains(output.String(), title) {
				t.Fatalf("fixed partial invocation prompted for %q:\n%s", title, output.String())
			}
		}
	})

	t.Run("all-false", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "All False")
		terminal, command, output := startPTYCLI(t, server, root,
			"--platform", "6400", "--version", "10.16",
			"--destination", destination, "--all=false")
		defer terminal.Close()
		waitForPTY(t, output, "Documents to retrieve")
		_, _ = terminal.Write([]byte("\r"))
		finishPTYCLI(t, terminal, command, output, 0)
		if _, err := os.Stat(filepath.Join(destination, "6400", "10.16", "manifest.json")); err != nil {
			t.Fatalf("--all=false skipped guide selection: %v\n%s", err, output.String())
		}
	})
}

func TestActualPTYDirectoryAutocompletePublishesAtCompletedPath(t *testing.T) {
	server := promptFixtureServer(t)
	root := t.TempDir()
	browse := filepath.Join(root, "Browse Root")
	targetA := filepath.Join(browse, "Target A")
	targetRoot := filepath.Join(browse, "Target Root")
	child := filepath.Join(targetRoot, "Child Folder")
	for _, directory := range []string{targetA, child} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	terminal, command, output := startPTYCLI(t, server, root)
	defer terminal.Close()
	for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
		waitForPTY(t, output, prompt)
		_, _ = terminal.Write([]byte("\r"))
	}
	waitForPTY(t, output, "Save documentation under which directory?")
	_, _ = terminal.Write([]byte(filepath.Join(browse, "Tar")))
	waitForPTY(t, output, "Target A/")
	waitForPTY(t, output, "Target Root/")
	if !regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(output.String()) {
		t.Fatal("interactive prompt did not use terminal presentation colors")
	}
	foundVerticalMenu := false
	for _, line := range strings.Split(ansi.Strip(output.String()), "\n") {
		if strings.Contains(line, "Target A/") != strings.Contains(line, "Target Root/") {
			foundVerticalMenu = true
		}
		if strings.Contains(line, "Target A/") && strings.Contains(line, "Target Root/") {
			t.Fatalf("two completion candidates were flattened into one row:\n%s", output.String())
		}
	}
	if !foundVerticalMenu {
		t.Fatalf("directory completions were not rendered vertically:\n%s", output.String())
	}
	_, _ = terminal.Write([]byte("\t\t\t\x1b[A\x1b[A\x1b[A"))
	_, _ = terminal.Write([]byte("get Root/Chi"))
	waitForPTY(t, output, "Child Folder/")
	_, _ = terminal.Write([]byte("\r"))
	time.Sleep(100 * time.Millisecond)
	manifest := filepath.Join(child, "6300", "10.16", "manifest.json")
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatalf("first Enter with an active completion menu created output: %v", err)
	}
	if command.ProcessState != nil {
		t.Fatal("first Enter with an active completion menu submitted the destination")
	}
	childCount := strings.Count(output.String(), "Child Folder/")
	_, _ = terminal.Write([]byte("\t"))
	waitForPTYCount(t, output, "Child Folder/", childCount+1)
	_, _ = terminal.Write([]byte("\r"))
	finishPTYCLI(t, terminal, command, output, 0)
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("completed directory was not the publication base: %v\n%s", err, output.String())
	}
	if _, err := os.Stat(filepath.Join(targetA, "6300")); !os.IsNotExist(err) {
		t.Fatalf("arrow/Tab completion published at the wrong suggestion: %v", err)
	}
}

func TestActualPTYDirectoryAutocompleteAllowsNewPathAndCancelsRestoredDraft(t *testing.T) {
	server := promptFixtureServer(t)
	t.Run("blank-current-directory", func(t *testing.T) {
		root := t.TempDir()
		terminal, command, output := startPTYCLI(t, server, root)
		defer terminal.Close()
		for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
			waitForPTY(t, output, prompt)
			_, _ = terminal.Write([]byte("\r"))
		}
		waitForPTY(t, output, "Save documentation under which directory?")
		_, _ = terminal.Write([]byte("\r"))
		finishPTYCLI(t, terminal, command, output, 0)
		if _, err := os.Stat(filepath.Join(root, "6300", "10.16", "manifest.json")); err != nil {
			t.Fatalf("blank destination did not immediately publish under the displayed CWD: %v\n%s",
				err, output.String())
		}
	})

	t.Run("new-path", func(t *testing.T) {
		root := t.TempDir()
		destination := filepath.Join(root, "New Destination")
		terminal, command, output := startPTYCLI(t, server, root)
		defer terminal.Close()
		for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
			waitForPTY(t, output, prompt)
			_, _ = terminal.Write([]byte("\r"))
		}
		waitForPTY(t, output, "Save documentation under which directory?")
		_, _ = terminal.Write([]byte(destination + "\r"))
		finishPTYCLI(t, terminal, command, output, 0)
		if _, err := os.Stat(filepath.Join(destination, "6300", "10.16", "manifest.json")); err != nil {
			t.Fatalf("new typed destination was not created after confirmation: %v\n%s", err, output.String())
		}
	})

	t.Run("back-restore-cancel", func(t *testing.T) {
		root := t.TempDir()
		browse := filepath.Join(root, "Browse")
		destination := filepath.Join(browse, "Restore Target")
		if err := os.MkdirAll(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		terminal, command, output := startPTYCLI(t, server, root)
		defer terminal.Close()
		for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
			waitForPTY(t, output, prompt)
			_, _ = terminal.Write([]byte("\r"))
		}
		waitForPTY(t, output, "Save documentation under which directory?")
		draft := filepath.Join(browse, "Rest")
		_, _ = terminal.Write([]byte(draft))
		waitForPTY(t, output, "Restore Target/")
		_, _ = terminal.Write([]byte("\x1b[D"))
		waitForPTY(t, output, "Documents to retrieve")
		_, _ = terminal.Write([]byte("\r"))
		waitForPTYCount(t, output, "Restore Target/", 2)
		_, _ = terminal.Write([]byte{3})
		finishPTYCLI(t, terminal, command, output, 130)
		if _, err := os.Stat(filepath.Join(destination, "6300")); !os.IsNotExist(err) {
			t.Fatalf("Back/restored destination was created before confirmation: %v", err)
		}
	})
}

func TestActualPTYShowsAllPlatformsAndReleases(t *testing.T) {
	platforms := []string{"4100i", "5420", "6000", "6100", "6200", "6300", "6400", "8100",
		"8320", "8325", "8360", "8400", "9300", "10000", "10040"}
	versions := []string{"10.18.xxxx", "10.17.1000", "10.17", "10.16", "10.15", "10.14", "10.13", "10.10"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			fmt.Fprint(w, `<html><select id="platform">`)
			for _, value := range platforms {
				fmt.Fprintf(w, "<option>%s</option>", value)
			}
			fmt.Fprint(w, `</select><select id="ver">`)
			for _, value := range versions {
				fmt.Fprintf(w, "<option>%s</option>", value)
			}
			fmt.Fprint(w, `</select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">PTY Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/guide.json":
			mappings := map[string]map[string]string{}
			for _, version := range versions {
				mappings[version] = map[string]string{platforms[0]: "http://" + r.Host + "/book/index.html"}
			}
			json.NewEncoder(w).Encode(mappings)
		default:
			t.Errorf("selection-only fixture must not fetch %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	terminal, command, output := startPTYCLI(t, server, t.TempDir())
	defer terminal.Close()
	waitForPTY(t, output, "Switch platform")
	waitForPTY(t, output, "10040")
	if text := output.String(); strings.Contains(text, "earlier choices") || strings.Contains(text, "later choices") {
		t.Fatalf("15 platforms were unnecessarily scrolled in a 24-row terminal:\n%s", text)
	}
	_, _ = terminal.Write([]byte("\r"))
	waitForPTY(t, output, "Documentation version")
	waitForPTY(t, output, "10.10")
	if text := output.String(); strings.Contains(text, "earlier choices") || strings.Contains(text, "later choices") {
		t.Fatalf("eight releases were unnecessarily scrolled:\n%s", text)
	}
	_, _ = terminal.Write([]byte{3})
	finishPTYCLI(t, terminal, command, output, 130)
}

func TestActualPTYCtrlCAtDestinationCreatesNoLibrary(t *testing.T) {
	server := promptFixtureServer(t)
	root := t.TempDir()
	terminal, command, output := startPTYCLI(t, server, root)
	defer terminal.Close()
	for _, prompt := range []string{"Switch platform", "Documentation version", "Documents to retrieve"} {
		waitForPTY(t, output, prompt)
		_, _ = terminal.Write([]byte("\r"))
	}
	waitForPTY(t, output, "Save documentation under which directory?")
	_, _ = terminal.Write([]byte{3})
	finishPTYCLI(t, terminal, command, output, 130)
	if _, err := os.Stat(filepath.Join(root, "6300")); !os.IsNotExist(err) {
		t.Fatalf("prompt cancellation created a library: %v", err)
	}
}
