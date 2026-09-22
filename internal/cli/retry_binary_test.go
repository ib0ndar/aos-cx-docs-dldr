//go:build reqexperiment

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestActualBinaryRetriesHeaderAndBodyAttemptTimeouts(t *testing.T) {
	var topicAttempts atomic.Int32
	var imageAttempts atomic.Int32
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	if err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<select id="platform"><option>6300</option></select><select id="ver"><option>10.16</option></select>
<div id="menu1"><table><tr><td id="retry" onclick="openFile('HTML','retry','aoscx')">Retry Guide</td></tr></table></div>`)
		case "/portal/json/aoscx/retry.json":
			io.WriteString(w, `{"10.16":{"6300":"`+server.URL+`/guide/Content/home.htm"}}`)
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><body><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul>
<main id="mc-main-content"><h1>Home</h1><a href="second.htm">Second</a></main></body></html>`)
		case "/guide/Data/Tocs/Guide.js":
			io.WriteString(w, `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0},{i:1,c:0}]}});`)
		case "/guide/Data/Tocs/Chunk0.js":
			io.WriteString(w, `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/second.htm':{i:[1],t:['Second'],b:['']}});`)
		case "/guide/Content/second.htm":
			if topicAttempts.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main id="mc-main-content"><h1>Second</h1><img src="../Resources/image.png"></main>`)
		case "/guide/Resources/image.png":
			w.Header().Set("Content-Type", "image/png")
			if imageAttempts.Add(1) == 1 {
				w.Header().Set("Content-Length", "256")
				w.Write(png[:16])
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			w.Write(png)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	binary := filepath.Join(root, "aos-cx-docs-dldr")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "../../cmd/aos-cx-docs-dldr")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build actual binary: %v\n%s", err, output)
	}
	command := exec.Command(binary,
		"--transport", "http",
		"--platform", "6300", "--release", "10.16", "--guides", "retry",
		"--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "raw-v2"),
		"--portal-url", server.URL+"/portal/aoscx.html",
		"--delay", "0", "--timeout", "2.5", "--attempt-timeout", "0.05",
		"--retries", "2", "--workers", "1", "--json",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("actual binary failed: %v\nstdout=%s\nstderr=%s", err, &stdout, &stderr)
	}
	var output DownloadOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode actual-binary output: %v\n%s", err, &stdout)
	}
	guide := output.Manifest.HTMLGuides["retry"]
	if output.Manifest.Status != "complete" || guide.Status != "complete" ||
		guide.HTML == nil || len(guide.HTML.MissingResources) != 0 ||
		topicAttempts.Load() != 2 || imageAttempts.Load() != 2 {
		t.Fatalf("actual binary retry result=%+v topic_attempts=%d image_attempts=%d",
			output.Manifest, topicAttempts.Load(), imageAttempts.Load())
	}
	log := stderr.String()
	if !strings.Contains(log, "per-attempt deadline exceeded at headers") ||
		!strings.Contains(log, "per-attempt deadline exceeded at body") ||
		strings.Count(log, "retry 1/2") < 2 {
		t.Fatalf("actual binary omitted retry progress diagnostics:\n%s", log)
	}
}
