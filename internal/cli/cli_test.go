package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

const fixturePortal = `<html><select id="platform"><option>6300</option><option>6400</option></select>
<select id="ver"><option>10.16</option><option>10.18.xxxx</option></select>
<div id="menu1"><table><tr>
<td id="fundamentals" onclick="openFile('HTML', 'fundamentals', 'aoscx')">Fundamentals Guide</td>
<td id="cli" onclick="openFile('HTML', 'cli', 'aoscx')">CLI</td>
</tr></table></div></html>`

func TestEndToEndFreshListingAndConditionalCache(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	validators := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		counts[r.URL.Path]++
		if r.Header.Get("User-Agent") != "aos-cx-docs-dldr/"+model.Version || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected request headers: %v", r.Header)
		}

		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		if r.Method == http.MethodGet && r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("catalogue request was not refreshed")
		}
		if r.URL.Path == "/portal" {
			http.Redirect(w, r, "/current/aoscx.html", 302)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			validators++
			w.WriteHeader(304)
			return
		}
		switch r.URL.Path {
		case "/current/aoscx.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, fixturePortal)
		case "/current/json/aoscx/fundamentals.json":
			io.WriteString(w, `{"10.16":{"6300":"`+server.URL+`/guide/index.html","6400":"fund_6300-6400"},"10.18.xxxx":{"6300":"sd00007433en_us"}}`)
		case "/current/json/aoscx/cli.json":
			io.WriteString(w, `{"10.16":{"6300":"`+server.URL+`/guide/cli.pdf"}}`)
		case "/guide/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main><h1>Fundamentals</h1></main>`)
		case "/guide/cli.pdf":
			if r.Method != http.MethodHead {
				t.Errorf("direct PDF availability used %s instead of HEAD", r.Method)
			}
			w.Header().Set("Content-Type", "application/pdf")
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	dir := filepath.Join(t.TempDir(), "raw")
	args := []string{"--list", "--json", "--platform", "6300", "--version", "10.16",
		"--portal-url", server.URL + "/portal", "--raw-cache", dir, "--delay", "0", "--retries", "0"}
	for range 2 {
		var out, stderr bytes.Buffer
		if code := runContract(context.Background(), args, &out, &stderr); code != 0 {
			t.Fatalf("exit=%d stderr=%s stdout=%s", code, &stderr, &out)
		}
		var listing Listing
		if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
			t.Fatal(err)
		}
		if !listing.Complete || len(listing.Documents) != 2 || listing.Catalog.SourceURL != server.URL+"/current/aoscx.html" ||
			listing.Documents[0].URL != server.URL+"/guide/index.html" ||
			listing.Documents[0].RouteOrigin != "publisher-url" ||
			listing.Documents[1].RouteOrigin != "publisher-url" {
			t.Fatalf("wrong end-to-end catalogue: %+v", listing)
		}
		if availability := listing.PDFAvailability["fundamentals"]; !availability.Checked || availability.Available ||
			availability.Origin != model.PDFOriginSourceAdvertised {
			t.Fatalf("static listing availability was not checked: %+v", availability)
		}
		if availability := listing.SourceAvailability["cli"]; !availability.Checked || !availability.Available ||
			availability.Format != "PDF native" {
			t.Fatalf("direct PDF source availability was not body-free verified: %+v", availability)
		}
		if _, found := listing.PDFAvailability["cli"]; found {
			t.Fatalf("direct mapped PDF was reported as an optional PDF: %+v", listing.PDFAvailability)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if validators != 4 || counts["/robots.txt"] != 2 || len(counts) != 7 {
		t.Fatalf("not all portal/mapping requests revalidated: counts=%v validators=%d", counts, validators)
	}
}

func TestListingReportsSourceBackedNativePDFAvailability(t *testing.T) {
	var server *httptest.Server
	var mu sync.Mutex
	methods := map[string]int{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<select id="platform"><option>8360</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="ha" onclick="openFile('HTML','ha','aoscx')">High Availability</td>
</tr></table></div>`)
		case "/portal/json/aoscx/ha.json":
			fmt.Fprintf(w, `{"10.16":{"8360":%q}}`, server.URL+"/guide/index.html")
		case "/guide/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><head><link rel="alternate" type="application/pdf" href="native.pdf"></head>
<body><main><h1>High Availability</h1></main></body></html>`)
		case "/guide/native.pdf":
			if r.Method != http.MethodHead {
				t.Errorf("listing fetched optional PDF body with %s", r.Method)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/pdf")
		default:
			t.Errorf("unexpected listing request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	args := []string{
		"--list", "--platform", "8360", "--version", "10.16",
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--raw-cache", filepath.Join(t.TempDir(), "raw"), "--delay", "0", "--retries", "0",
	}
	var human, humanErr bytes.Buffer
	if code := runContract(context.Background(), args, &human, &humanErr); code != 0 {
		t.Fatalf("human availability listing exit=%d stderr=%s", code, &humanErr)
	}
	if !strings.Contains(human.String(), "[HTML (static) / PDF native]") ||
		strings.Contains(human.String(), "[HTML / PDF]") {
		t.Fatalf("human listing availability label is ambiguous: %s", &human)
	}
	var out, stderr bytes.Buffer
	if code := runContract(context.Background(), append(args, "--json"), &out, &stderr); code != 0 {
		t.Fatalf("JSON availability listing exit=%d stderr=%s stdout=%s", code, &stderr, &out)
	}
	var listing Listing
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	availability := listing.PDFAvailability["ha"]
	if !listing.Complete || !availability.Checked || !availability.Available ||
		availability.Origin != model.PDFOriginSourceAdvertised ||
		availability.URL != server.URL+"/guide/native.pdf" {
		t.Fatalf("JSON listing omitted checked availability: %+v", listing)
	}
	mu.Lock()
	defer mu.Unlock()
	if methods["GET /guide/native.pdf"] != 0 || methods["HEAD /guide/native.pdf"] != 2 ||
		methods["GET /guide/index.html"] != 2 {
		t.Fatalf("listing availability was not fresh front plus body-free HEAD per run: %v", methods)
	}
}

func TestDefinitiveMappedSourceAbsenceIsListedAndRejectedBeforeOutput(t *testing.T) {
	var server *httptest.Server
	var mu sync.Mutex
	methods := map[string]int{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal/aoscx.html":
			io.WriteString(w, `<select id="platform"><option>6000</option></select>
<select id="ver"><option>10.17</option><option>10.17.1000</option></select>
<div id="menu1"><table><tr>
<td id="webUI" onclick="openFile('HTML','webUI','aoscx')">Introduction to the WebUI Guide</td>
<td id="cli" onclick="openFile('HTML','cli','aoscx')">CLI Guide</td>
</tr></table></div>`)
		case "/portal/json/aoscx/webUI.json":
			fmt.Fprintf(w, `{"10.17":{"6000":%q},"10.17.1000":{"6000":%q}}`,
				server.URL+"/missing/Content/home.htm", server.URL+"/working/index.html")
		case "/portal/json/aoscx/cli.json":
			fmt.Fprintf(w, `{"10.17":{"6000":%q}}`, server.URL+"/missing/cli.pdf")
		case "/missing/Content/home.htm":
			w.WriteHeader(http.StatusNotFound)
		case "/missing/cli.pdf":
			if r.Method != http.MethodHead {
				t.Errorf("direct mapped PDF probe used %s", r.Method)
			}
			w.WriteHeader(http.StatusGone)
		case "/working/index.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html><body><main>Working</main></body></html>`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	baseArgs := []string{
		"--platform", "6000", "--version", "10.17",
		"--portal-url", server.URL + "/portal/aoscx.html",
		"--raw-cache", filepath.Join(t.TempDir(), "raw-v2"),
		"--delay", "0", "--retries", "0",
	}
	var out, stderr bytes.Buffer
	if code := runContract(context.Background(), append([]string{"--list", "--json"}, baseArgs...), &out, &stderr); code != 2 {
		t.Fatalf("definitively unavailable listing exit=%d stderr=%s stdout=%s", code, &stderr, &out)
	}
	var listing Listing
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	webUI := listing.SourceAvailability["webUI"]
	cli := listing.SourceAvailability["cli"]
	if listing.Complete || !webUI.Checked || webUI.Available ||
		webUI.Reason != "mapped source HTTP 404" ||
		webUI.URL != server.URL+"/missing/Content/home.htm" ||
		!cli.Checked || cli.Available || cli.Reason != "mapped source HTTP 410" ||
		cli.Format != "PDF native" {
		t.Fatalf("listing conflated mapped source and optional PDF availability: %+v", listing)
	}
	if _, found := listing.PDFAvailability["webUI"]; found {
		t.Fatalf("missing HTML front produced optional-PDF fallback state: %+v", listing.PDFAvailability)
	}
	var human bytes.Buffer
	printListingStyled(&human, listing, options{platform: "6000", version: "10.17"}, newHumanPresentation(&human))
	if !strings.Contains(human.String(), "[HTML (flare) - unavailable: mapped source HTTP 404]") ||
		!strings.Contains(human.String(), "[PDF native - unavailable: mapped source HTTP 410]") ||
		strings.Contains(human.String(), "shown as HTML") {
		t.Fatalf("human listing hid or conflated source absence: %s", &human)
	}

	destination := filepath.Join(t.TempDir(), "must-not-exist")
	out.Reset()
	stderr.Reset()
	downloadArgs := append([]string{}, baseArgs...)
	downloadArgs = append(downloadArgs, "--guides", "webUI", "--destination", destination)
	if code := runContract(context.Background(), downloadArgs, &out, &stderr); code == 0 {
		t.Fatalf("unavailable fixed selection succeeded: stdout=%s stderr=%s", &out, &stderr)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unavailable preflight created output destination: %v", err)
	}
	for _, required := range []string{
		"mapped source is unavailable", "mapped source HTTP 404",
		server.URL + "/missing/Content/home.htm", "10.17.1000",
	} {
		if !strings.Contains(stderr.String(), required) {
			t.Fatalf("actionable rejection omitted %q: %s", required, &stderr)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if methods["GET /missing/Content/home.htm"] != 2 ||
		methods["HEAD /missing/cli.pdf"] != 1 ||
		methods["GET /missing/cli.pdf"] != 0 {
		t.Fatalf("availability traffic was repeated or fetched PDF bytes: %v", methods)
	}
}

func TestEndToEndFlareHTMLDownload(t *testing.T) {
	var server *httptest.Server
	counts := map[string]int{}
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts[r.URL.Path]++
		switch r.URL.Path {
		case "/robots.txt":
			io.WriteString(w, "User-agent: *\nAllow: /\n")
		case "/aoscx.html":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<select id="platform"><option>6300</option></select><select id="ver"><option>10.16</option></select>
<div id="menu1"><table><tr><td id="job" onclick="openFile('HTML','job','aoscx')">Job Scheduler Guide</td></tr></table></div>`)
		case "/json/aoscx/job.json":
			io.WriteString(w, `{"10.16":{"6300":"`+server.URL+`/guide/Content/home.htm"}}`)
		case "/guide/Content/home.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><head><link rel="stylesheet" href="../Resources/main.css"></head>
<body><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul><main id="mc-main-content"><h1 id="home">Home</h1>
<p>Offline fixture guide.</p><a href="second.htm#second">Second</a><img src="../Resources/icon.png"></main></body></html>`)
		case "/guide/Data/Tocs/Guide.js":
			io.WriteString(w, `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0},{i:1,c:0}]}});`)
		case "/guide/Data/Tocs/Chunk0.js":
			io.WriteString(w, `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/second.htm':{i:[1],t:['Second'],b:['second']}});`)
		case "/guide/Content/second.htm":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<main id="mc-main-content"><h1 id="second">Second</h1><table><tr><th>State</th><td>ready</td></tr></table></main>`)
		case "/guide/Resources/main.css":
			w.Header().Set("Content-Type", "text/css")
			io.WriteString(w, `.icon{background:url("icon.png")}`)
		case "/guide/Resources/icon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNGFixture)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	base := t.TempDir()
	var out, stderr bytes.Buffer
	args := []string{"--json", "--platform", "6300", "--version", "10.16", "--guides", "job",
		"--destination", base, "--portal-url", server.URL + "/aoscx.html", "--raw-cache", filepath.Join(base, "raw"),
		"--delay", "0", "--retries", "0", "--max-resource-mb", "1", "--max-archive-mb", "4", "--zip"}
	budget, err := fetch.NewAttemptBudget(9, 1)
	if err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), args, &out, &stderr,
		func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
			config.Attempts = budget
			return fetch.New(config, notify)
		}); code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, &stderr, &out)
	}
	var result DownloadOutput
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	htmlGuide := result.Manifest.HTMLGuides["job"]
	if result.Manifest.Status != "complete" || htmlGuide.HTML == nil || len(htmlGuide.HTML.Topics) != 2 ||
		len(htmlGuide.HTML.Assets) != 2 || result.ZIP != result.Library+".zip" ||
		strings.Contains(strings.Join(htmlGuide.Warnings, "\n"), "Preferred source-verified PDF") {
		t.Fatalf("incomplete executable HTML result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(result.Library, "job", "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(result.ZIP); err != nil {
		t.Fatal(err)
	}
	if counts["/guide/Content/home.htm"] != 1 || counts["/guide/Resources/icon.png"] != 1 {
		t.Fatalf("raw cache did not prevent duplicate transmissions: %v", counts)
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	if stats := budget.Stats(); stats.AttemptedTransmissions != 9 || stats.ObservedHeaderWrites != 9 ||
		stats.AdmittedRequests != 9 || stats.ReservedAttempts != 0 || stats.Exhausted ||
		total != stats.AttemptedTransmissions {
		t.Fatalf("catalogue/plan/topic/asset budget was not shared: %+v hits=%d counts=%v", stats, total, counts)
	}
}

var tinyPNGFixture = []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 0}

func TestOfflineCommandsAndInvalidConfigurationNeverFetchOrCreateCache(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--help"}, 0, "Native AOS-CX"},
		{[]string{"--app-version"}, 0, model.ExecutableName + " " + model.Version},
		{[]string{"-V"}, 0, model.ExecutableName + " " + model.Version},
		{nil, 1, "non-interactive downloads require"},
		{[]string{"--list", "--platform", "6300"}, 1, "both --platform and --version"},
		{[]string{"--list", "--delay", "NaN"}, 1, "finite"},
		{[]string{"--list", "--timeout", "+Inf"}, 1, "finite"},
		{[]string{"--list", "--timeout", "0"}, 1, "finite"},
		{[]string{"--list", "--attempt-timeout", "NaN"}, 1, "finite"},
		{[]string{"--list", "--attempt-timeout", "0"}, 1, "finite"},
		{[]string{"--list", "--attempt-timeout", "151"}, 1, "must not exceed"},
		{[]string{"--list", "--timeout", "44"}, 1, "must not exceed"},
		{[]string{"--list", "--max-resource-mb", "-1"}, 1, "finite"},
		{[]string{"--list", "--retries", "11"}, 1, "network limits"},
		{[]string{"--list", "--workers", "0"}, 1, "--workers must be between"},
		{[]string{"--list", "--workers", "65"}, 1, "--workers must be between"},
		{[]string{"--list", "--transport", "auto"}, 1, "supported retrieval transports"},
		{[]string{"--list", "--transport", "browser"}, 1, "supported retrieval transports"},
		{[]string{"--list", "--transport", "req"}, 1, "supported retrieval transports"},
		{[]string{"--all"}, 1, "non-interactive downloads require"},
		{[]string{"--list", "--zip"}, 1, "--list cannot be combined with --zip"},
		{[]string{"--app-version", "--zip"}, 1, "--app-version cannot be combined with --zip"},
		{[]string{"--prefer-pdf", "--no-prefer-pdf"}, 1, "mutually exclusive"},
		{[]string{"--offline-catalog"}, 1, "unknown flag"},
	} {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
			cache := filepath.Join(t.TempDir(), "must-not-exist")
			args := append(append([]string{}, tc.args...), "--raw-cache", cache, "--portal-url", "http://127.0.0.1:1/never")
			var out, stderr bytes.Buffer
			if code := runContract(context.Background(), args, &out, &stderr); code != tc.code ||
				!strings.Contains(out.String()+stderr.String(), tc.want) {
				t.Fatalf("code=%d out=%s stderr=%s", code, &out, &stderr)
			}

			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatal("offline command created cache")
			}
		})
	}
}

func TestDefaultAndExplicitRetrievalDeadlines(t *testing.T) {
	defaults := options{
		list: true, portal: "https://example.test/portal", transport: "compatible",
		delay: 0.1, timeout: 150, attemptTimeout: 45, retries: 2,
		workers: 4, maxMB: 256, maxArchiveMB: 1024,
	}
	config, err := defaults.validate()
	if err != nil {
		t.Fatal(err)
	}
	if config.Timeout != 150*time.Second || config.AttemptTimeout != 45*time.Second ||
		config.Retries != 2 {
		t.Fatalf("unexpected retrieval defaults: %+v", config)
	}
	defaults.timeout = 44
	if _, err := defaults.validate(); err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("overall below attempt timeout was accepted: %v", err)
	}
	defaults.timeout = 45
	defaults.attemptTimeout = 45
	config, err = defaults.validate()
	if err != nil || config.Timeout != 45*time.Second || config.AttemptTimeout != 45*time.Second {
		t.Fatalf("explicit strict 45-second total was rejected: config=%+v err=%v", config, err)
	}
}

func TestHelpDocumentsOverallAndAttemptTimeoutDefaults(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := runContract(context.Background(), []string{"--help"}, &out, &stderr); code != 0 {
		t.Fatalf("help exit=%d stderr=%s", code, &stderr)
	}
	text := out.String()
	for _, expected := range []string{
		"This native application supports",
		"--timeout float", "Overall deadline for a retrieval", "(default 150)",
		"--attempt-timeout float", "Deadline for each retrieval attempt", "(default 45)",
		"--retries int", "Retries after transient failures", "(default 2)",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("help omitted %q:\\n%s", expected, text)
		}
	}
	if strings.Contains(text, "native checkpoint") {
		t.Fatalf("production help retained checkpoint framing:\n%s", text)
	}
}

func TestHumanDownloadReportsZIPPath(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/portal":
			io.WriteString(w, `<select id="platform"><option>6300</option></select>
<select id="ver"><option>10.10</option></select><div id="menu1"><table><tr>
<td id="job" onclick="openFile('PDF','job','aoscx')">Job Scheduler</td></tr></table></div>`)
		case "/json/aoscx/job.json":
			io.WriteString(w, `{"10.10":{"6300":"`+server.URL+`/job.pdf"}}`)
		case "/job.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write(testutil.PDF("human ZIP"))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base := t.TempDir()
	args := []string{
		"--transport", "http", "--platform", "6300", "--version", "10.10",
		"--guides", "job", "--destination", base, "--raw-cache", filepath.Join(base, "raw"),
		"--portal-url", server.URL + "/portal", "--delay", "0", "--retries", "0", "--zip",
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), args, &stdout, &stderr,
		func(_ string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
			return fetch.New(config, notify)
		}); code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
	target := filepath.Join(base, "6300", "10.10.zip")
	resolvedTarget, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	resolvedTarget = filepath.Join(resolvedTarget, filepath.Base(target))
	if !strings.Contains(stdout.String(), "ZIP: "+resolvedTarget) {
		t.Fatalf("human output omitted ZIP path: %s", &stdout)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
}

func TestPartialMappingAndUnavailablePairAreDistinct(t *testing.T) {
	for _, partial := range []bool{true, false} {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/robots.txt":
				w.WriteHeader(404)
			case "/aoscx.html":
				io.WriteString(w, fixturePortal)
			case "/json/aoscx/fundamentals.json":
				io.WriteString(w, `{"10.16":{"6300":"fund"}}`)
			case "/json/aoscx/cli.json":
				if partial {
					w.WriteHeader(403)
				} else {
					io.WriteString(w, `{}`)
				}
			}
		}))
		var out, stderr bytes.Buffer
		args := []string{"--list", "--json", "--platform", "6300", "--version", "10.18.xxxx",
			"--portal-url", server.URL + "/aoscx.html", "--raw-cache", t.TempDir(), "--delay", "0", "--retries", "0"}
		code := runContract(context.Background(), args, &out, &stderr)
		server.Close()
		if partial {
			var listing Listing
			if err := json.Unmarshal(out.Bytes(), &listing); err != nil || code != 2 || listing.Complete ||
				len(listing.Catalog.Warnings) == 0 {
				t.Fatalf("partial mapping hidden: exit=%d output=%s stderr=%s err=%v", code, &out, &stderr, err)
			}
		} else if code != 1 || !strings.Contains(stderr.String(), "Available versions: 10.16") {
			t.Fatalf("unsupported pair not actionable: exit=%d stderr=%s", code, &stderr)
		}
	}
}

func TestCancelledRunReturns130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, stderr bytes.Buffer
	code := runContract(ctx, []string{"--list", "--raw-cache", t.TempDir()}, &out, &stderr)
	if code != 130 || !strings.Contains(stderr.String(), "Cancelled") {
		t.Fatalf("exit=%d stderr=%s", code, &stderr)
	}
}

func TestBoundedPlainDiagnosticsRetainJSONDetails(t *testing.T) {
	c := model.Catalog{Warnings: []string{"1", "2", "3", "4", "5", "6", "7"}}
	var out bytes.Buffer
	if err := printListing(&out, Listing{Catalog: c}, options{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Warning: 6") || !strings.Contains(out.String(), "2 additional diagnostics") ||
		!strings.Contains(out.String(), "INCOMPLETE") {
		t.Fatalf("diagnostic bounds: %s", &out)
	}
}

func TestPublisherControlsAreEscapedOnlyInHumanListing(t *testing.T) {
	title := "Guide \x1b[2J\x1b]0;owned\a\u009b31m 文档"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/aoscx.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">`+title+`</td>
</tr></table></div>`)
		case "/json/aoscx/guide.json":
			io.WriteString(w, `{"10.16":{"6300":"`+server.URL+`/guide/index.html"}}`)
		case "/guide/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			io.WriteString(w, `<main><h1>Guide</h1></main>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	baseArgs := []string{"--list", "--platform", "6300", "--version", "10.16",
		"--portal-url", server.URL + "/aoscx.html", "--raw-cache", t.TempDir(), "--delay", "0", "--retries", "0"}
	var human, humanErr bytes.Buffer
	if code := runContract(context.Background(), baseArgs, &human, &humanErr); code != 0 {
		t.Fatalf("human listing failed: code=%d stderr=%s", code, &humanErr)
	}
	if strings.ContainsAny(human.String()+humanErr.String(), "\x1b\a\u009b") ||
		!strings.Contains(human.String(), "文档") {
		t.Fatalf("human output retained terminal controls or lost Unicode: %q", human.String()+humanErr.String())
	}

	var jsonOut, jsonErr bytes.Buffer
	if code := runContract(context.Background(), append(baseArgs, "--json"), &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("JSON listing failed: code=%d stderr=%s", code, &jsonErr)
	}
	var listing Listing
	if err := json.Unmarshal(jsonOut.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if got := listing.Catalog.Guides[0].Title; got != title {
		t.Fatalf("JSON metadata was sanitized or changed: got %q want %q", got, title)
	}
}

func TestBuiltExecutableFixtureAndSIGINT(t *testing.T) {
	binary := os.Getenv("AOSCX_TEST_BINARY")
	if binary == "" {
		t.Skip("set AOSCX_TEST_BINARY to validate the built native executable")
	}
	requestStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/slow":
			requestStarted <- struct{}{}
			<-r.Context().Done()
		case "/html-aoscx.html":
			io.WriteString(w, `<select id="platform"><option>6300</option></select><select id="ver"><option>10.16</option></select>
<div id="menu1"><table><tr><td id="job" onclick="openFile('HTML','job','aoscx')">Job Scheduler</td></tr></table></div>`)
		case "/json/aoscx/job.json":
			io.WriteString(w, `{"10.16":{"6300":"http://`+r.Host+`/guide/Content/home.htm"}}`)
		case "/guide/Content/home.htm":
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul>
<main id="mc-main-content"><h1>Home</h1><p>Built executable fixture.</p><img src="../Resources/icon.png"></main></html>`)
		case "/guide/Data/Tocs/Guide.js":
			io.WriteString(w, `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0}]}});`)
		case "/guide/Data/Tocs/Chunk0.js":
			io.WriteString(w, `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']}});`)
		case "/guide/Resources/icon.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(tinyPNGFixture)
		case "/aoscx.html":
			io.WriteString(w, fixturePortal)
		case "/json/aoscx/fundamentals.json":
			io.WriteString(w, `{"10.16":{"6300":"fund"}}`)
		case "/json/aoscx/cli.json":
			io.WriteString(w, `{"10.16":{"6300":"cli"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	args := []string{"--list", "--json", "--platform", "6300", "--version", "10.16",
		"--portal-url", server.URL + "/aoscx.html", "--raw-cache", t.TempDir(), "--delay", "0", "--retries", "0"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	var listOut, listErr bytes.Buffer
	command.Stdout, command.Stderr = &listOut, &listErr
	err := command.Run()
	var listing Listing
	if err != nil || json.Unmarshal(listOut.Bytes(), &listing) != nil || !listing.Complete || len(listing.Documents) != 2 ||
		!strings.Contains(listErr.String(), "Refreshing Product Documentation catalogue") ||
		strings.Contains(listOut.String(), "Refreshing guide mappings") {
		t.Fatalf("built binary fixture failed: stdout=%s stderr=%s %v", &listOut, &listErr, err)
	}
	base := t.TempDir()
	command = exec.CommandContext(ctx, binary, "--json", "--platform", "6300", "--version", "10.16", "--guides", "job",
		"--destination", base, "--portal-url", server.URL+"/html-aoscx.html", "--raw-cache", filepath.Join(base, "raw"),
		"--delay", "0", "--retries", "0", "--max-resource-mb", "1", "--max-archive-mb", "4")
	var htmlOut, htmlErr bytes.Buffer
	command.Stdout, command.Stderr = &htmlOut, &htmlErr
	if err := command.Run(); err != nil {
		t.Fatalf("built HTML archive failed: %v stderr=%s stdout=%s", err, &htmlErr, &htmlOut)
	}
	var download DownloadOutput
	if json.Unmarshal(htmlOut.Bytes(), &download) != nil || len(download.Manifest.HTMLGuides) != 1 {
		t.Fatalf("built HTML output invalid: stdout=%s stderr=%s", &htmlOut, &htmlErr)
	}
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt subprocess delivery is unavailable on Windows")
	}
	command = exec.CommandContext(ctx, binary, "--list", "--portal-url", server.URL+"/slow",
		"--raw-cache", t.TempDir(), "--delay", "0", "--timeout", "30", "--attempt-timeout", "30")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-ctx.Done():
		command.Wait()
		t.Fatal("binary did not start the expected request")
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		command.Process.Kill()
		command.Wait()
		t.Fatal(err)
	}
	err = command.Wait()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 130 || !strings.Contains(stderr.String(), "Cancelled") {
		t.Fatalf("SIGINT exit=%v stderr=%s", err, &stderr)
	}
}
