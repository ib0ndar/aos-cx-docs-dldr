package source

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

func TestFlarePlanningUsesRealTransportCacheAndCompleteInputs(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/guide/Content/home.htm":
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><a href="contents.htm">Table of Contents</a></html>`)
		case "/guide/Content/contents.htm":
			io.WriteString(w, `<html><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul></html>`)
		case "/guide/Data/Tocs/Guide.js":
			io.WriteString(w, `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0}]}});`)
		case "/guide/Data/Tocs/Chunk0.js":
			io.WriteString(w, `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']}});`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := fetch.New(fetch.Config{Timeout: time.Second, MaxBytes: 1 << 20}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache"), client, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	doc := model.Document{ID: "guide", Title: "Guide", Platform: "6300", Version: "10.16", Kind: "flare", URL: server.URL + "/guide/Content/home.htm"}
	first, err := LoadPlan(context.Background(), store, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Inputs) != 4 || len(first.Topics) != 2 || !first.Inventory.Complete {
		t.Fatalf("incomplete transport plan: %+v", first)
	}
	mu.Lock()
	before := map[string]int{}
	for k, v := range hits {
		before[k] = v
	}
	mu.Unlock()
	second, err := LoadPlan(context.Background(), store, doc)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(second.Inputs) != 4 || len(hits) != len(before) {
		t.Fatal("cached plan changed shape")
	}
	for k, v := range before {
		if hits[k] != v {
			t.Fatalf("planning refetched cached resource %s: %d -> %d", k, v, hits[k])
		}
	}
}

func TestPlanningRedirectScopeStopsBeforeUnsafeTarget(t *testing.T) {
	var outsideHits int
	outside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { outsideHits++; io.WriteString(w, "outside") }))
	defer outside.Close()
	inside := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		http.Redirect(w, r, outside.URL+"/private", http.StatusFound)
	}))
	defer inside.Close()
	client, err := fetch.New(fetch.Config{Timeout: time.Second, MaxBytes: 1 << 20}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache"), client, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	doc := model.Document{ID: "guide", Title: "Guide", Platform: "6300", Version: "10.16", Kind: "flare", URL: inside.URL + "/guide/Content/home.htm"}
	if _, err := LoadPlan(context.Background(), store, doc); err == nil {
		t.Fatal("out-of-scope redirect accepted")
	}
	if outsideHits != 0 {
		t.Fatal("unsafe redirect target was requested")
	}
}

func TestFlareRedirectWithinSelectedReleasePreservesMappedPublicIdentity(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}

		switch r.URL.Path {
		case "/AOS-CX/10.16/HTML/alias/Content/home.htm":
			http.Redirect(w, r, "/AOS-CX/10.16/HTML/shared/Content/home.htm", http.StatusFound)
		case "/AOS-CX/10.16/HTML/shared/Content/home.htm":
			io.WriteString(w, `<html data-mc-path-to-help-system="../"><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul></html>`)
		case "/AOS-CX/10.16/HTML/shared/Data/Tocs/Guide.js":
			io.WriteString(w, `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0}]}});`)
		case "/AOS-CX/10.16/HTML/shared/Data/Tocs/Chunk0.js":
			io.WriteString(w, `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']}});`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := fetch.New(fetch.Config{Timeout: time.Second, MaxBytes: 1 << 20}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache"), client, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	public := server.URL + "/AOS-CX/10.16/HTML/alias/Content/home.htm"
	plan, err := LoadPlan(context.Background(), store, model.Document{
		ID: "shared", Title: "Shared", Platform: "6300", Version: "10.16", Kind: "flare", URL: public,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Topics[0].URL != public ||
		plan.Topics[0].FetchURL != server.URL+"/AOS-CX/10.16/HTML/shared/Content/home.htm" ||
		plan.Inventory.RootURL != server.URL+"/AOS-CX/10.16/HTML/shared/" {
		t.Fatalf("redirect/public/fetch identities lost: %+v", plan)
	}
}

type hpeRoundTripper struct{ requests []string }

func (t *hpeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, r.URL.String())
	status, body, content := 200, "", "text/plain"
	switch {
	case r.URL.Path == "/robots.txt":
		body = "User-agent: *\nAllow: /\n"
	case r.URL.Path == "/hpesc/public/api/document/doc-A" && r.URL.Query().Get("ignorePayload") == "true":
		body = `<main role="main" class="ditasrc"><h1>Guide</h1></main>`
		content = "multiPage;charset=UTF-8"
	case r.URL.Path == "/hpesc/public/api/document/doc-A" && r.URL.Query().Get("page") == "content.json":
		body = `[{"topicName":"Topic","topicLink":"GUID-topic.html"},{"topicName":"Other","topicLink":"GUID-other.html"}]`
		content = "application/json"
	default:
		status = 404
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {content}},
		Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func TestHPEPlanningUsesRealTransportCacheAndSerializablePlan(t *testing.T) {
	backend := &hpeRoundTripper{}
	client, err := fetch.NewWithRoundTripper(fetch.Config{Timeout: time.Second, MaxBytes: 1 << 20}, func(string) {}, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := cache.Open(filepath.Join(t.TempDir(), "cache"), client, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	doc := model.Document{ID: "guide", Title: "Guide", Platform: "6300", Version: "10.18.xxxx", Kind: "hpe",
		URL: "https://support.hpe.com/hpesc/public/docDisplay?docId=doc-A"}
	plan, err := LoadPlan(context.Background(), store, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Topics) != 3 || len(plan.Inputs) != 2 || len(backend.requests) != 3 || !plan.Inventory.Complete {
		t.Fatalf("wrong real-adapter plan: %+v requests=%v", plan, backend.requests)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded model.DocumentPlan
	if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.TOC) != 3 || decoded.Topics[1].FetchURL == "" {
		t.Fatalf("plan is not usable after serialization: %+v %v", decoded, err)
	}
	before := len(backend.requests)
	if _, err := LoadPlan(context.Background(), store, doc); err != nil {
		t.Fatal(err)
	}
	if len(backend.requests) != before {
		t.Fatal("cached HPE inventory refetched")
	}
}
