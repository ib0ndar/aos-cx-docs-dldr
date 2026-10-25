package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

func TestStallDelayOutlastsAPublisherStall(t *testing.T) {
	c, err := New(DefaultConfig(), func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for used, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 2 * time.Minute} {
		if got := c.stallDelay(used); got != want {
			t.Fatalf("stall delay after %d retries = %s, want %s", used, got, want)
		}
	}
	// Two attempts and both waits fit the default overall deadline.
	if total := 3*c.config.AttemptTimeout + c.stallDelay(0) + c.stallDelay(1); total > c.config.Timeout {
		t.Fatalf("stall retries need %s but the deadline is %s", total, c.config.Timeout)
	}
}

type recordedWaits struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (r *recordedWaits) wait(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.waits = append(r.waits, d)
	return nil
}

func (r *recordedWaits) list() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration{}, r.waits...)
}

func TestStalledAttemptPausesTheSiteBeforeRetrying(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if hits.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "final")
	}))
	defer server.Close()
	var notices []string
	c, err := New(Config{Timeout: 5 * time.Second, AttemptTimeout: 60 * time.Millisecond, Retries: 2, MaxBytes: 1024,
		AllowLoopbackFixtures: true}, func(message string) { notices = append(notices, message) })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waits := &recordedWaits{}
	c.backoff = waits.wait
	var body strings.Builder
	_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/topic"}, func(resource model.Resource) error {
		body.Reset()
		_, err := io.Copy(&body, resource.Body)
		return err
	})
	if err != nil || body.String() != "final" || hits.Load() != 2 {
		t.Fatalf("stalled attempt was not retried: hits=%d body=%q err=%v", hits.Load(), body.String(), err)
	}
	if recorded := waits.list(); len(recorded) == 0 || recorded[0] != 40*time.Millisecond {
		t.Fatalf("stall retry waits %v, want 40ms first", recorded)
	}
	want := "requests to " + origin(server.URL) + " paused, retry 1/2 in 40ms: " + server.URL + "/topic"
	if len(notices) != 1 || !strings.Contains(notices[0], "per-attempt deadline exceeded at headers") || !strings.Contains(notices[0], want) {
		t.Fatalf("stall diagnostic %q does not contain %q", notices, want)
	}
}

func TestPausedSiteHoldsItsOtherRequests(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "ok")
	})
	paused, other := httptest.NewServer(handler), httptest.NewServer(handler)
	defer paused.Close()
	defer other.Close()
	c, err := New(Config{Timeout: 5 * time.Second, AttemptTimeout: time.Second, MaxBytes: 1024, AllowLoopbackFixtures: true}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waits := &recordedWaits{}
	c.backoff = waits.wait
	c.pause(paused.URL+"/stalled", time.Minute)
	get := func(raw string) {
		t.Helper()
		if _, err := c.Download(context.Background(), model.Request{URL: raw}, func(resource model.Resource) error {
			_, err := io.Copy(io.Discard, resource.Body)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	get(other.URL + "/topic")
	if recorded := waits.list(); len(recorded) != 0 {
		t.Fatalf("another site waited for the pause: %v", recorded)
	}
	// Both the site's robots.txt and the topic request wait for the pause.
	get(paused.URL + "/topic")
	recorded := waits.list()
	if len(recorded) != 2 {
		t.Fatalf("paused site did not hold its requests: %v", recorded)
	}
	for _, wait := range recorded {
		if wait < 59*time.Second || wait > time.Minute {
			t.Fatalf("paused site did not hold its requests: %v", recorded)
		}
	}
}
