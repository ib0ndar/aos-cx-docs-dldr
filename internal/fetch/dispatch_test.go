package fetch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
	"github.com/imroc/req/v3"
)

type queueProbe struct {
	backend http.RoundTripper
	entered chan struct{}
}

func (p *queueProbe) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/queued" {
		p.entered <- struct{}{}
	}
	return p.backend.RoundTrip(r)
}

func (p *queueProbe) RoundTripWithDispatch(r *http.Request, start func() error) (*http.Response, error) {
	if r.URL.Path == "/queued" {
		p.entered <- struct{}{}
	}
	if backend, ok := p.backend.(interface {
		RoundTripWithDispatch(*http.Request, func() error) (*http.Response, error)
	}); ok {
		return backend.RoundTripWithDispatch(r, start)
	}
	if err := start(); err != nil {
		return nil, err
	}
	return p.backend.RoundTrip(r)
}

func TestCompatibleQueuedLeasesPreserveDispatchStartGap(t *testing.T) {
	const gap = 60 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(404)
		case "/held":
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			io.WriteString(w, "body")
		}
	}))
	defer server.Close()
	backend := newCompatibilityBackend(true, true)
	defer backend.CloseIdleConnections()
	var mu sync.Mutex
	var dispatches []time.Time
	backend.transport.WrapRoundTripFunc(func(next http.RoundTripper) req.HttpRoundTripFunc {
		return func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/queued" {
				mu.Lock()
				dispatches = append(dispatches, time.Now())
				mu.Unlock()
			}
			return next.RoundTrip(r)
		}
	})
	probe := &queueProbe{backend: backend, entered: make(chan struct{}, 3)}
	budget, err := NewAttemptBudget(8*CompatibleWireAttemptAllowance, CompatibleWireAttemptAllowance)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewWithRoundTripper(
		Config{Delay: gap, Timeout: 5 * time.Second, MaxBytes: 1024, Attempts: budget},
		func(string) {}, probe,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var held []model.Resource
	for range 4 {
		r, err := client.Do(context.Background(), model.Request{URL: server.URL + "/held"})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			_, err := client.Download(context.Background(), model.Request{URL: server.URL + "/queued"}, func(r model.Resource) error {
				_, err := io.Copy(io.Discard, r.Body)
				return err
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	for range 3 {
		select {
		case <-probe.entered:
		case <-time.After(time.Second):
			t.Fatal("requests did not reach the occupied lease boundary")
		}
	}
	if stats := budget.Stats(); stats.AdmittedRequests != 5 || stats.AttemptedTransmissions != 5 ||
		stats.ObservedHeaderWrites != 5 || stats.ReservedAttempts != 4*(CompatibleWireAttemptAllowance-1) {
		t.Fatalf("queued requests reserved budget before lease admission: %+v", stats)
	}
	time.Sleep(2 * gap)
	for _, r := range held {
		r.Body.Close()
	}
	workers.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(dispatches) != 3 {
		t.Fatalf("expected three backend dispatches, got %d", len(dispatches))
	}
	if stats := budget.Stats(); stats.AdmittedRequests != 8 || stats.AttemptedTransmissions != 8 ||
		stats.ObservedHeaderWrites != 8 || stats.ReservedAttempts != 0 {
		t.Fatalf("leased budget lifecycle mismatch: %+v", stats)
	}
	for i := 1; i < len(dispatches); i++ {
		observed := dispatches[i].Sub(dispatches[i-1])
		if observed < gap-5*time.Millisecond {
			t.Fatalf("occupied leases compressed request-start pacing: actual=%s required=%s", observed, gap)
		}
	}
}
