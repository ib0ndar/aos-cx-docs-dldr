package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

func requireRetrievalError(t *testing.T, err error, stage RetrievalStage, retries int) *RetrievalError {
	t.Helper()
	var diagnostic *RetrievalError
	if !errors.As(err, &diagnostic) {
		t.Fatalf("missing typed retrieval diagnostic: %T %v", err, err)
	}
	if diagnostic.Stage != stage || diagnostic.ApplicationRetries != retries ||
		diagnostic.ApplicationAttempts < 1 ||
		diagnostic.RequestURL == "" || diagnostic.FinalURL == "" || diagnostic.Elapsed <= 0 ||
		!strings.Contains(err.Error(), "application_attempts=") ||
		!strings.Contains(err.Error(), "application_retries=") ||
		!strings.Contains(err.Error(), "timeout_scope=") {
		t.Fatalf("incomplete retrieval diagnostic: %+v", diagnostic)
	}
	return diagnostic
}

func TestRetrievalDiagnosticsIdentifyObservableFailureStage(t *testing.T) {
	t.Run("robots", func(t *testing.T) {
		var topicHits atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, "<html>not robots</html>")
				return
			}
			topicHits.Add(1)
		}))
		defer server.Close()
		_, err := client(t, nil).Do(context.Background(), model.Request{URL: server.URL + "/topic"})
		diagnostic := requireRetrievalError(t, err, StageRobots, 0)
		if topicHits.Load() != 0 || diagnostic.FinalURL != server.URL+"/robots.txt" {
			t.Fatalf("robots failure identity changed: hits=%d diagnostic=%+v", topicHits.Load(), diagnostic)
		}
	})

	t.Run("lease", func(t *testing.T) {
		backend := &leaseBlockingBackend{}
		c, err := NewWithRoundTripper(
			Config{Timeout: 50 * time.Millisecond, MaxBytes: 1024},
			func(string) {},
			backend,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, err = c.Do(context.Background(), model.Request{URL: "https://publisher.example/topic"})
		requireRetrievalError(t, err, StageLease, 0)
		if !errors.Is(err, context.DeadlineExceeded) || backend.topicStarts.Load() != 0 {
			t.Fatalf("lease timeout was hidden or dispatched: starts=%d err=%v", backend.topicStarts.Load(), err)
		}
	})

	t.Run("pacing", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, "ok")
		}))
		defer server.Close()
		c := client(t, func(config *Config) {
			config.Delay = 100 * time.Millisecond
			config.Timeout = time.Second
		})
		first, err := c.Do(context.Background(), model.Request{URL: server.URL + "/first"})
		if err != nil {
			t.Fatal(err)
		}
		first.Body.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err = c.Do(ctx, model.Request{URL: server.URL + "/second"})
		requireRetrievalError(t, err, StagePacing, 0)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pacing deadline hidden: %v", err)
		}
	})

	t.Run("headers", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			<-r.Context().Done()
		}))
		defer server.Close()
		_, err := client(t, func(config *Config) {
			config.Timeout = 50 * time.Millisecond
		}).Do(context.Background(), model.Request{URL: server.URL + "/topic"})
		requireRetrievalError(t, err, StageHeaders, 0)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("header deadline hidden: %v", err)
		}
	})

	t.Run("body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer server.Close()
		_, err := client(t, func(config *Config) {
			config.Timeout = 50 * time.Millisecond
		}).Download(context.Background(), model.Request{URL: server.URL + "/topic"}, func(resource model.Resource) error {
			_, err := io.ReadAll(resource.Body)
			return err
		})
		requireRetrievalError(t, err, StageBody, 0)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("body deadline hidden: %v", err)
		}
	})

	t.Run("backoff", func(t *testing.T) {
		var topicHits atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			topicHits.Add(1)
			w.Header().Set("Set-Cookie", "private-session=must-not-appear")
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer server.Close()
		_, err := client(t, func(config *Config) {
			config.Timeout = 50 * time.Millisecond
			config.Retries = 2
		}).Do(context.Background(), model.Request{URL: server.URL + "/topic"})
		diagnostic := requireRetrievalError(t, err, StageBackoff, 0)
		if !errors.Is(err, context.DeadlineExceeded) || topicHits.Load() != 1 ||
			diagnostic.ApplicationAttempts != 1 || diagnostic.TimeoutScope != "overall" ||
			strings.Contains(err.Error(), "private-session") {
			t.Fatalf("backoff diagnostics changed policy or exposed headers: hits=%d err=%v", topicHits.Load(), err)
		}
	})
}

func TestRetrievalDiagnosticPreservesStatusAndCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/denied", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	requestURL := server.URL + "/start"
	_, err := client(t, nil).Do(context.Background(), model.Request{URL: requestURL})
	diagnostic := requireRetrievalError(t, err, StageHeaders, 0)
	if diagnostic.RequestURL != requestURL || diagnostic.FinalURL != server.URL+"/denied" {
		t.Fatalf("canonical request/final identity missing: %+v", diagnostic)
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Status != http.StatusForbidden || status.URL != diagnostic.FinalURL {
		t.Fatalf("status error hidden: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client(t, nil).Do(ctx, model.Request{URL: server.URL + "/cancelled"})
	var cancelled *RetrievalError
	if !errors.As(err, &cancelled) || cancelled.RequestURL != server.URL+"/cancelled" ||
		cancelled.ApplicationRetries != 0 || cancelled.Stage != StageRobots ||
		!errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation hidden: %v", err)
	}
}

func TestCompatibleRetrievalDiagnosticCoversLeaseWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	backend := newCompatibilityBackend(true, true)
	defer backend.CloseIdleConnections()
	c, err := NewWithRoundTripper(
		Config{Timeout: time.Second, MaxBytes: 1024},
		func(string) {},
		backend,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	held := make([]model.Resource, 0, 4)
	for index := range 4 {
		resource, err := c.Do(context.Background(), model.Request{
			URL: server.URL + "/held?slot=" + string(rune('0'+index)),
		})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, resource)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.Do(ctx, model.Request{URL: server.URL + "/queued"})
	requireRetrievalError(t, err, StageLease, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("compatible lease deadline hidden: %v", err)
	}
	for _, resource := range held {
		resource.Body.Close()
	}
}

type leaseBlockingBackend struct {
	topicStarts atomic.Int32
}

func (b *leaseBlockingBackend) RoundTrip(request *http.Request) (*http.Response, error) {
	return b.RoundTripWithDispatch(request, func() error { return nil })
}

func (b *leaseBlockingBackend) RoundTripWithDispatch(
	request *http.Request,
	start func() error,
) (*http.Response, error) {
	if request.URL.Path == "/robots.txt" {
		if err := start(); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	}
	<-request.Context().Done()
	return nil, request.Context().Err()
}
