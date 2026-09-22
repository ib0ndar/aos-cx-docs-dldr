package cache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

type controlledTransport struct {
	mu       sync.Mutex
	calls    map[string]int
	active   int
	max      int
	started  chan string
	release  <-chan struct{}
	response string
	err      error
}

type retiringTransport struct {
	mu            sync.Mutex
	calls         int
	firstStarted  chan struct{}
	firstCanceled chan struct{}
	releaseFirst  chan struct{}
}

func (t *retiringTransport) Download(ctx context.Context, request model.Request, consume func(model.Resource) error) (model.Resource, error) {
	t.mu.Lock()
	t.calls++
	call := t.calls
	t.mu.Unlock()
	if call == 1 {
		close(t.firstStarted)
		<-ctx.Done()
		close(t.firstCanceled)
		<-t.releaseFirst
		return model.Resource{}, ctx.Err()
	}
	resource := model.Resource{
		URL: request.URL, Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/octet-stream"}},
		Body:    io.NopCloser(strings.NewReader("healthy")),
	}
	err := consume(resource)
	resource.Body.Close()
	resource.Body = http.NoBody
	return resource, err
}

func (t *retiringTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func (t *controlledTransport) Download(ctx context.Context, request model.Request, consume func(model.Resource) error) (model.Resource, error) {
	t.mu.Lock()
	if t.calls == nil {
		t.calls = map[string]int{}
	}
	t.calls[request.URL]++
	t.active++
	if t.active > t.max {
		t.max = t.active
	}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.active--
		t.mu.Unlock()
	}()
	if t.started != nil {
		select {
		case t.started <- request.URL:
		case <-ctx.Done():
			return model.Resource{}, ctx.Err()
		}
	}
	if t.release != nil {
		select {
		case <-t.release:
		case <-ctx.Done():
			return model.Resource{}, ctx.Err()
		}
	}
	if t.err != nil {
		return model.Resource{}, t.err
	}
	resource := model.Resource{
		URL: request.URL, Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"application/octet-stream"}},
		Body:    io.NopCloser(strings.NewReader(t.response)),
	}
	err := consume(resource)
	resource.Body.Close()
	resource.Body = http.NoBody
	return resource, err
}

func (t *controlledTransport) snapshot() (map[string]int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	calls := make(map[string]int, len(t.calls))
	for key, value := range t.calls {
		calls[key] = value
	}
	return calls, t.max
}

func openControlledStore(t *testing.T, transport model.Transport) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), transport, 1<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil && !errors.Is(err, errClosed) {
			t.Error(err)
		}
	})
	return store
}

type getResult struct {
	resource model.Resource
	err      error
}

func asyncGet(store *Store, ctx context.Context, url string, refresh bool) <-chan getResult {
	result := make(chan getResult, 1)
	go func() {
		resource, err := store.Get(ctx, url, refresh)
		result <- getResult{resource: resource, err: err}
	}()
	return result
}

func asyncPrefetch(store *Store, ctx context.Context, url string, refresh bool) <-chan error {
	result := make(chan error, 1)
	go func() { result <- store.Prefetch(ctx, url, refresh) }()
	return result
}

func awaitStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case url := <-started:
		return url
	case <-time.After(2 * time.Second):
		t.Fatal("network request did not start")
		return ""
	}
}

func awaitResult(t *testing.T, result <-chan getResult) getResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("cache request did not finish")
		return getResult{}
	}
}

func closeAndRead(t *testing.T, resource model.Resource) string {
	t.Helper()
	if _, ok := resource.Body.(*os.File); !ok {
		t.Fatal("cache response body is not an independent disk handle")
	}
	data, err := io.ReadAll(resource.Body)
	closeErr := resource.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read cached body: read=%v close=%v", err, closeErr)
	}
	return string(data)
}

func TestDifferentURLsDownloadConcurrently(t *testing.T) {
	release := make(chan struct{})
	transport := &controlledTransport{started: make(chan string, 2), release: release, response: "same bytes"}
	store := openControlledStore(t, transport)
	first := asyncGet(store, context.Background(), "https://docs.example/one", false)
	second := asyncGet(store, context.Background(), "https://docs.example/two?part=2", false)
	awaitStart(t, transport.started)
	awaitStart(t, transport.started)
	close(release)
	for _, result := range []<-chan getResult{first, second} {
		got := awaitResult(t, result)
		if got.err != nil || closeAndRead(t, got.resource) != "same bytes" {
			t.Fatalf("concurrent fetch failed: %v", got.err)
		}
	}
	calls, maximum := transport.snapshot()
	if len(calls) != 2 || maximum != 2 {
		t.Fatalf("different URLs were serialized: calls=%v max=%d", calls, maximum)
	}
	root, err := store.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, err := root.ReadDir(-1)
	bodies := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bin") {
			bodies++
		}
	}
	if err != nil || bodies != 1 {
		t.Fatalf("identical bodies were not safely shared: bodies=%d err=%v", bodies, err)
	}
}

func waitForWaiters(t *testing.T, store *Store, url string, count int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		active := store.flights[url]
		got := 0
		if active != nil {
			got = active.waiters
		}
		store.mu.Unlock()
		if got == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("flight waiter count=%d, want %d", got, count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSameURLDeduplicatesAndReturnsIndependentHandles(t *testing.T) {
	const url = "https://docs.example/topic?page=one"
	release := make(chan struct{})
	transport := &controlledTransport{started: make(chan string, 1), release: release, response: "topic"}
	store := openControlledStore(t, transport)
	results := []<-chan getResult{
		asyncGet(store, context.Background(), url, false),
		asyncGet(store, context.Background(), url+"#first", false),
		asyncGet(store, context.Background(), url+"#second", true),
		asyncGet(store, context.Background(), url, false),
	}
	awaitStart(t, transport.started)
	waitForWaiters(t, store, url, len(results))
	close(release)
	handles := map[*os.File]bool{}
	for _, result := range results {
		got := awaitResult(t, result)
		if got.err != nil {
			t.Fatal(got.err)
		}
		handle, ok := got.resource.Body.(*os.File)
		if !ok || handles[handle] {
			t.Fatal("same-URL callers did not receive distinct disk handles")
		}
		handles[handle] = true
		if closeAndRead(t, got.resource) != "topic" {
			t.Fatal("cached bytes changed")
		}
	}
	calls, _ := transport.snapshot()
	if calls[url] != 1 {
		t.Fatalf("same URL was retrieved %d times", calls[url])
	}
}

func TestConcurrentRefreshDeduplicatesAndFailureCanRetry(t *testing.T) {
	const url = "https://docs.example/topic"
	seed := &controlledTransport{response: "original"}
	store := openControlledStore(t, seed)
	original, err := store.Get(context.Background(), url, false)
	if err != nil || closeAndRead(t, original) != "original" {
		t.Fatal(err)
	}
	release := make(chan struct{})
	refresh := &controlledTransport{
		started: make(chan string, 1), release: release,
		err: errors.New("publisher unavailable"),
	}
	store.transport = refresh
	first := asyncGet(store, context.Background(), url, true)
	awaitStart(t, refresh.started)
	second := asyncPrefetch(store, context.Background(), url, true)
	waitForWaiters(t, store, url, 2)
	close(release)
	if got := awaitResult(t, first); !errors.Is(got.err, refresh.err) {
		t.Fatalf("refresh failure was hidden from Get: %v", got.err)
	}
	if err := <-second; !errors.Is(err, refresh.err) {
		t.Fatalf("in-flight refresh failure was hidden from Prefetch: %v", err)
	}
	if calls, _ := refresh.snapshot(); calls[url] != 1 {
		t.Fatalf("concurrent refresh made %d requests", calls[url])
	}
	refresh.err = nil
	refresh.response = "updated"
	if err := store.Prefetch(context.Background(), url, true); err != nil {
		t.Fatalf("failed refresh was incorrectly memoized as fresh: %v", err)
	}
	updated, err := store.Get(context.Background(), url, false)
	if err != nil || closeAndRead(t, updated) != "updated" {
		t.Fatalf("successful prefetch did not replace the older body: %v", err)
	}
	if calls, _ := refresh.snapshot(); calls[url] != 2 {
		t.Fatalf("prefetch after failed validation did not retry: %v", calls)
	}
}

func TestHealthyCallerWaitsForAbandonedFlightRetirement(t *testing.T) {
	const url = "https://docs.example/topic"
	for _, method := range []string{"get", "prefetch"} {
		t.Run(method, func(t *testing.T) {
			transport := &retiringTransport{
				firstStarted: make(chan struct{}), firstCanceled: make(chan struct{}),
				releaseFirst: make(chan struct{}),
			}
			store := openControlledStore(t, transport)
			ctx, cancel := context.WithCancel(context.Background())
			var first <-chan getResult
			prefetchResult := make(chan error, 1)
			if method == "get" {
				first = asyncGet(store, ctx, url, false)
			} else {
				go func() { prefetchResult <- store.Prefetch(ctx, url, false) }()
			}
			<-transport.firstStarted
			cancel()
			if method == "get" {
				if got := awaitResult(t, first); !errors.Is(got.err, context.Canceled) {
					t.Fatalf("first caller returned %v", got.err)
				}
			} else if err := <-prefetchResult; !errors.Is(err, context.Canceled) {
				t.Fatalf("first prefetch returned %v", err)
			}
			<-transport.firstCanceled

			var second <-chan getResult
			secondPrefetch := make(chan error, 1)
			if method == "get" {
				second = asyncGet(store, context.Background(), url, false)
			} else {
				go func() { secondPrefetch <- store.Prefetch(context.Background(), url, false) }()
			}
			select {
			case got := <-second:
				t.Fatalf("healthy caller joined abandoned flight: %v", got.err)
			case err := <-secondPrefetch:
				t.Fatalf("healthy prefetch joined abandoned flight: %v", err)
			case <-time.After(30 * time.Millisecond):
			}
			if transport.callCount() != 1 {
				t.Fatal("replacement request started before abandoned same-URL writer retired")
			}
			close(transport.releaseFirst)
			if method == "get" {
				got := awaitResult(t, second)
				if got.err != nil || closeAndRead(t, got.resource) != "healthy" {
					t.Fatalf("healthy caller inherited abandoned error: %v", got.err)
				}
			} else {
				select {
				case err := <-secondPrefetch:
					if err != nil {
						t.Fatalf("healthy prefetch inherited abandoned error: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("healthy prefetch did not retry after retirement")
				}
				got, err := store.Get(context.Background(), url, false)
				if err != nil || closeAndRead(t, got) != "healthy" {
					t.Fatalf("prefetched body unavailable: %v", err)
				}
			}
			if transport.callCount() != 2 {
				t.Fatalf("replacement network calls=%d, want 2", transport.callCount())
			}
		})
	}
}

func TestCancelledWaiterDoesNotCancelSharedWork(t *testing.T) {
	const url = "https://docs.example/topic"
	release := make(chan struct{})
	transport := &controlledTransport{started: make(chan string, 1), release: release, response: "complete"}
	store := openControlledStore(t, transport)
	owner := asyncGet(store, context.Background(), url, false)
	awaitStart(t, transport.started)
	waitCtx, cancel := context.WithCancel(context.Background())
	waiter := asyncGet(store, waitCtx, url, false)
	waitForWaiters(t, store, url, 2)
	cancel()
	if got := awaitResult(t, waiter); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("cancelled waiter returned %v", got.err)
	}
	close(release)
	got := awaitResult(t, owner)
	if got.err != nil || closeAndRead(t, got.resource) != "complete" {
		t.Fatalf("shared work was cancelled: %v", got.err)
	}
	if calls, _ := transport.snapshot(); calls[url] != 1 {
		t.Fatalf("shared request count=%d", calls[url])
	}
}

func TestCloseCancelsActiveWorkAndOpenHandlesRemainReadable(t *testing.T) {
	const url = "https://docs.example/topic"
	transport := &controlledTransport{started: make(chan string, 1), response: "kept"}
	store := openControlledStore(t, transport)
	resource, err := store.Get(context.Background(), url, false)
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- store.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("store close waited on an already returned body handle")
	}
	if closeAndRead(t, resource) != "kept" {
		t.Fatal("store close invalidated an independent body handle")
	}
	if _, err := store.Get(context.Background(), url, false); !errors.Is(err, errClosed) {
		t.Fatalf("closed store accepted a request: %v", err)
	}

	blocked := &controlledTransport{started: make(chan string, 1), release: make(chan struct{})}
	active := openControlledStore(t, blocked)
	result := asyncGet(active, context.Background(), url, false)
	awaitStart(t, blocked.started)
	if err := active.Close(); err != nil {
		t.Fatal(err)
	}
	if got := awaitResult(t, result); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("close did not cancel active work: %v", got.err)
	}
}
