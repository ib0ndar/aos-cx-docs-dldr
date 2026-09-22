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
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type topicConcurrency struct {
	mu     sync.Mutex
	active int
	max    int
	count  int
}

func (s *topicConcurrency) begin() func() {
	s.mu.Lock()
	s.active++
	s.count++
	if s.active > s.max {
		s.max = s.active
	}
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}
}

func (s *topicConcurrency) reset() {
	s.mu.Lock()
	s.active, s.max, s.count = 0, 0, 0
	s.mu.Unlock()
}

func (s *topicConcurrency) snapshot() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max, s.count
}

func (s *topicConcurrency) current() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func TestPlannedTopicWorkersImproveRealCLIPipeline(t *testing.T) {
	const topics = 24
	state := &topicConcurrency{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">Performance Guide</td>
</tr></table></div></html>`)
		case r.URL.Path == "/portal/json/aoscx/guide.json":
			json.NewEncoder(w).Encode(map[string]any{"10.16": map[string]string{"6300": server.URL + "/book/index.html"}})
		case r.URL.Path == "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			var toc strings.Builder
			for index := range topics {
				fmt.Fprintf(&toc, `<li><a href="topic-%02d.html">Topic %02d</a></li>`, index, index)
			}
			fmt.Fprintf(w, `<html><head><link rel="stylesheet" href="/style.css"></head><body>
<nav class="wh_publication_toc"><ul>%s</ul></nav>
<main class="wh_topic_content"><h1>Home</h1><img src="/image.png"></main></body></html>`, toc.String())
		case strings.HasPrefix(r.URL.Path, "/book/topic-"):
			done := state.begin()
			defer done()
			time.Sleep(200 * time.Millisecond)
			w.Header().Set("Content-Type", "text/html")
			index := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/book/topic-"), ".html")
			fmt.Fprintf(w, `<article><h1 id="topic-%s">Topic %s</h1><p>Deterministic content.</p>
<img src="/image.png"><a href="index.html">Home</a></article>`, index, index)
		case r.URL.Path == "/style.css":
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, `.wh_topic_content{max-width:70rem}`)
		case r.URL.Path == "/image.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNGFixture)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runFixture := func(workers int) (time.Duration, DownloadOutput, int, int) {
		t.Helper()
		state.reset()
		root := t.TempDir()
		args := []string{
			"--platform", "6300", "--release", "10.16", "--guides", "guide",
			"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "cache"),
			"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0.001",
			"--timeout", "10", "--attempt-timeout", "10", "--retries", "0", "--workers", fmt.Sprint(workers), "--json",
		}
		var stdout, stderr bytes.Buffer
		started := time.Now()
		code := run(context.Background(), args, &stdout, &stderr, newNativeClient)
		elapsed := time.Since(started)
		if code != 0 {
			t.Fatalf("workers=%d failed: code=%d stdout=%s stderr=%s", workers, code, stdout.String(), stderr.String())
		}
		var output DownloadOutput
		if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
			t.Fatal(err)
		}
		maximum, count := state.snapshot()
		return elapsed, output, maximum, count
	}

	serialTime, serial, serialMax, serialCount := runFixture(1)
	parallelTime, parallel, parallelMax, parallelCount := runFixture(4)
	if serialMax != 1 || parallelMax < 2 || parallelMax > 4 || serialCount != topics || parallelCount != topics {
		t.Fatalf("worker bounds not enforced: serial=%d/%d parallel=%d/%d",
			serialMax, serialCount, parallelMax, parallelCount)
	}
	if serialTime < 2*parallelTime {
		t.Fatalf("latency-dominated pipeline did not improve by 2x: workers1=%s workers4=%s", serialTime, parallelTime)
	}
	serialGuide := serial.Manifest.HTMLGuides["guide"].HTML
	parallelGuide := parallel.Manifest.HTMLGuides["guide"].HTML
	if serialGuide == nil || parallelGuide == nil || serialGuide.Status != "complete" || parallelGuide.Status != "complete" {
		t.Fatal("performance fixture archive was incomplete")
	}
	if !reflect.DeepEqual(serialGuide.Topics, parallelGuide.Topics) ||
		!reflect.DeepEqual(serialGuide.Assets, parallelGuide.Assets) ||
		serialGuide.Integrity.CheckedLinks != parallelGuide.Integrity.CheckedLinks {
		t.Fatal("worker count changed archive ordering, source hashes, content hashes, assets, or link integrity")
	}
	t.Logf("workers1=%s workers4=%s speedup=%.2fx max-inflight=%d topics=%d",
		serialTime, parallelTime, float64(serialTime)/float64(parallelTime), parallelMax, topics)
}

func TestPlannedTopicPrefetchDoesNotMultiplyRefresh(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	conditionals := map[string]int{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		if r.Header.Get("If-None-Match") != "" {
			conditionals[r.URL.Path]++
		}
		mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", `"fixture-v1"`)
		if r.Header.Get("If-None-Match") == `"fixture-v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		switch r.URL.Path {
		case "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">Refresh Guide</td>
</tr></table></div></html>`)
		case "/portal/json/aoscx/guide.json":
			json.NewEncoder(w).Encode(map[string]any{"10.16": map[string]string{"6300": server.URL + "/book/index.html"}})
		case "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><body><nav class="wh_publication_toc"><ul>
<li><a href="one.html">One</a></li><li><a href="two.html">Two</a></li></ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`)
		case "/book/one.html", "/book/two.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<article><h1>%s</h1></article>`, filepath.Base(r.URL.Path))
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	args := []string{
		"--platform", "6300", "--release", "10.16", "--guides", "guide",
		"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0",
		"--timeout", "10", "--attempt-timeout", "10", "--retries", "0", "--workers", "4", "--json",
	}
	runOnce := func(extra ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), append(append([]string{}, args...), extra...), &stdout, &stderr, newNativeClient); code != 0 {
			t.Fatalf("archive failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
	}
	runOnce()
	mu.Lock()
	counts, conditionals = map[string]int{}, map[string]int{}
	mu.Unlock()
	runOnce("--refresh")
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/book/index.html", "/book/one.html", "/book/two.html"} {
		if counts[path] != 1 || conditionals[path] != 1 {
			t.Errorf("%s refresh requests=%d conditional=%d, want one conditional request", path, counts[path], conditionals[path])
		}
	}
}

func TestPrefetchLookaheadStopsBeforeWholeOversizedInventory(t *testing.T) {
	const topics = 24
	var requested int
	var mu sync.Mutex
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">Bounded Guide</td>
</tr></table></div></html>`)
		case r.URL.Path == "/portal/json/aoscx/guide.json":
			json.NewEncoder(w).Encode(map[string]any{"10.16": map[string]string{"6300": server.URL + "/book/index.html"}})
		case r.URL.Path == "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			var toc strings.Builder
			for index := range topics {
				fmt.Fprintf(&toc, `<li><a href="topic-%02d.html">Topic %02d</a></li>`, index, index)
			}
			fmt.Fprintf(w, `<html><body><nav class="wh_publication_toc"><ul>%s</ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`, toc.String())
		case strings.HasPrefix(r.URL.Path, "/book/topic-"):
			mu.Lock()
			requested++
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<article><h1>Topic</h1><p>%s</p></article>`, strings.Repeat("x", 1200))
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--platform", "6300", "--release", "10.16", "--guides", "guide",
		"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "cache"),
		"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0",
		"--timeout", "10", "--attempt-timeout", "10", "--retries", "0", "--workers", "4",
		"--max-resource-mb", "1", "--max-archive-mb", "0.002",
	}, &stdout, &stderr, newNativeClient)
	if code != 2 || !strings.Contains(stderr.String(), "archive aggregate source-byte limit exceeded") {
		t.Fatalf("aggregate limit did not fail closed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	mu.Lock()
	count := requested
	mu.Unlock()
	if count < 1 || count > 8 {
		t.Fatalf("bounded lookahead fetched %d/%d topics before aggregate failure", count, topics)
	}
}

func TestCancellationStopsQueuedAndActiveTopicPrefetch(t *testing.T) {
	started := make(chan struct{}, 8)
	state := &topicConcurrency{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/portal/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, `<html><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">Cancellation Guide</td>
</tr></table></div></html>`)
		case r.URL.Path == "/portal/json/aoscx/guide.json":
			json.NewEncoder(w).Encode(map[string]any{"10.16": map[string]string{"6300": server.URL + "/book/index.html"}})
		case r.URL.Path == "/book/index.html":
			w.Header().Set("Content-Type", "text/html")
			var toc strings.Builder
			for index := range 8 {
				fmt.Fprintf(&toc, `<li><a href="topic-%d.html">Topic %d</a></li>`, index, index)
			}
			fmt.Fprintf(w, `<html><body><nav class="wh_publication_toc"><ul>%s</ul></nav>
<main class="wh_topic_content"><h1>Home</h1></main></body></html>`, toc.String())
		case strings.HasPrefix(r.URL.Path, "/book/topic-"):
			done := state.begin()
			defer done()
			started <- struct{}{}
			<-r.Context().Done()
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		code   int
		stderr string
	}
	finished := make(chan outcome, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := run(ctx, []string{
			"--platform", "6300", "--release", "10.16", "--guides", "guide",
			"--destination", filepath.Join(root, "library"), "--raw-cache", filepath.Join(root, "cache"),
			"--portal-url", server.URL + "/portal/aoscx.html", "--delay", "0",
			"--timeout", "10", "--attempt-timeout", "10", "--retries", "0", "--workers", "4", "--json",
		}, &stdout, &stderr, newNativeClient)
		finished <- outcome{code: code, stderr: stderr.String()}
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("planned topic workers did not reach bounded concurrency")
		}
	}
	cancel()
	select {
	case result := <-finished:
		if result.code != 130 {
			t.Fatalf("cancellation exit=%d stderr=%s", result.code, result.stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not stop workers and close the cache")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		active := state.current()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("topic requests remained active after CLI exit: %d", active)
		}
		time.Sleep(time.Millisecond)
	}
	for _, name := range []string{".10.16.lock", ".10.16.transaction.json"} {
		if _, err := os.Stat(filepath.Join(root, "library", "6300", name)); !os.IsNotExist(err) {
			t.Fatalf("publication control file leaked after cancellation: %s", name)
		}
	}
	parts, err := filepath.Glob(filepath.Join(root, "cache", "*.part"))
	if err != nil || len(parts) != 0 {
		t.Fatalf("cache partial files leaked after cancellation: %v %v", parts, err)
	}
}
