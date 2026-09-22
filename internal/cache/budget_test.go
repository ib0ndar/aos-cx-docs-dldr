package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
)

func TestVerifiedCacheHitConsumesNoAttemptBudget(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, "cached")
	}))
	defer server.Close()
	budget, _ := fetch.NewAttemptBudget(2, 1)
	client, err := fetch.New(fetch.Config{Timeout: time.Second, MaxBytes: 1024, Attempts: budget}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	store, err := Open(t.TempDir(), client, 1024, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for range 2 {
		resource, err := store.Get(context.Background(), server.URL+"/body", false)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resource.Body)
		resource.Body.Close()
	}
	if hits.Load() != 2 || budget.Stats().AttemptedTransmissions != 2 ||
		budget.Stats().ObservedHeaderWrites != 2 {
		t.Fatalf("cache hit consumed network budget: hits=%d stats=%+v", hits.Load(), budget.Stats())
	}
}
