package fetch

import (
	"bytes"
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

func TestPerAttemptTimeoutRetriesHeadersAndBody(t *testing.T) {
	for _, mode := range []string{"headers", "body"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				attempt := hits.Add(1)
				if attempt == 1 {
					if mode == "body" {
						w.Header().Set("Content-Length", "12")
						io.WriteString(w, "partial")
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return
				}
				io.WriteString(w, "final")
			}))
			defer server.Close()
			budget, _ := NewAttemptBudget(8, 1)
			var notices []string
			c, err := New(Config{
				Timeout: 500 * time.Millisecond, AttemptTimeout: 30 * time.Millisecond,
				Retries: 1, MaxBytes: 1024, Attempts: budget,
			}, func(message string) { notices = append(notices, message) })
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.backoff = func(context.Context, time.Duration) error { return nil }
			var sink bytes.Buffer
			_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/topic"}, func(resource model.Resource) error {
				sink.Reset()
				_, err := io.Copy(&sink, resource.Body)
				return err
			})
			if err != nil || sink.String() != "final" || hits.Load() != 2 {
				t.Fatalf("retry result mode=%s hits=%d sink=%q err=%v", mode, hits.Load(), sink.String(), err)
			}
			stats := budget.Stats()
			if stats.ApplicationRetrievals != 1 || stats.ApplicationAttempts != 2 ||
				stats.ApplicationRetries != 1 || stats.AttemptedTransmissions != 3 ||
				stats.ObservedHeaderWrites != 3 || stats.ReservedAttempts != 0 {
				t.Fatalf("retry accounting mode=%s stats=%+v", mode, stats)
			}
			if len(notices) != 1 || !strings.Contains(notices[0], "per-attempt deadline exceeded at "+mode) {
				t.Fatalf("retry diagnostic mode=%s notices=%v", mode, notices)
			}
		})
	}
}

func TestPerAttemptTimeoutRetriesLeaseAndPacing(t *testing.T) {
	t.Run("lease", func(t *testing.T) {
		backend := &retryLeaseBackend{}
		var notices []string
		c, err := NewWithRoundTripper(Config{
			Timeout: 500 * time.Millisecond, AttemptTimeout: 30 * time.Millisecond,
			Retries: 1, MaxBytes: 1024,
		}, func(message string) { notices = append(notices, message) }, backend)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.backoff = func(context.Context, time.Duration) error { return nil }
		response, err := c.Do(context.Background(), model.Request{URL: "https://publisher.example/topic"})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || string(body) != "final" || backend.topicCalls.Load() != 2 {
			t.Fatalf("lease retry body=%q calls=%d read=%v close=%v", body, backend.topicCalls.Load(), readErr, closeErr)
		}
		if len(notices) != 1 || !strings.Contains(notices[0], "per-attempt deadline exceeded at lease") {
			t.Fatalf("lease retry diagnostics=%v", notices)
		}
	})

	t.Run("pacing", func(t *testing.T) {
		var topicHits atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/robots.txt" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			topicHits.Add(1)
			io.WriteString(w, "final")
		}))
		defer server.Close()
		var notices []string
		c, err := New(Config{
			Delay: 60 * time.Millisecond, Timeout: 500 * time.Millisecond,
			AttemptTimeout: 30 * time.Millisecond, Retries: 1, MaxBytes: 1024,
		}, func(message string) { notices = append(notices, message) })
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.backoff = func(context.Context, time.Duration) error {
			c.mu.Lock()
			c.last[origin(server.URL)] = time.Time{}
			c.mu.Unlock()
			return nil
		}
		response, err := c.Do(context.Background(), model.Request{URL: server.URL + "/topic"})
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || string(body) != "final" || topicHits.Load() != 1 {
			t.Fatalf("pacing retry body=%q hits=%d read=%v close=%v", body, topicHits.Load(), readErr, closeErr)
		}
		if len(notices) != 1 || !strings.Contains(notices[0], "per-attempt deadline exceeded at pacing") {
			t.Fatalf("pacing retry diagnostics=%v", notices)
		}
	})
}

func TestThreeAttemptsReplacePartialSinkAndFinalFailureIsTyped(t *testing.T) {
	for _, finalSuccess := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "exhausted"}[finalSuccess], func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				attempt := hits.Add(1)
				if finalSuccess && attempt == 3 {
					io.WriteString(w, "exact-final")
					return
				}
				w.Header().Set("Content-Length", "20")
				io.WriteString(w, "partial-"+string(rune('0'+attempt)))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			budget, _ := NewAttemptBudget(12, 1)
			c, err := New(Config{
				Timeout: 500 * time.Millisecond, AttemptTimeout: 25 * time.Millisecond,
				Retries: 2, MaxBytes: 1024, Attempts: budget,
			}, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.backoff = func(context.Context, time.Duration) error { return nil }
			var sink bytes.Buffer
			_, err = c.Download(context.Background(), model.Request{URL: server.URL + "/topic"}, func(resource model.Resource) error {
				sink.Reset()
				_, err := io.Copy(&sink, resource.Body)
				return err
			})
			stats := budget.Stats()
			if hits.Load() != 3 || stats.ApplicationAttempts != 3 || stats.ApplicationRetries != 2 {
				t.Fatalf("three-attempt accounting success=%v hits=%d stats=%+v", finalSuccess, hits.Load(), stats)
			}
			if stats.ApplicationRetrievals != 1 || stats.AdmittedRequests != 4 ||
				stats.AttemptedTransmissions != 4 || stats.ObservedHeaderWrites != 4 ||
				stats.ReservedAttempts != 0 || stats.AllowanceViolated {
				t.Fatalf("standard physical-attempt accounting=%+v", stats)
			}
			if finalSuccess {
				if err != nil || sink.String() != "exact-final" {
					t.Fatalf("partial sink was not replaced: sink=%q err=%v", sink.String(), err)
				}
				return
			}
			diagnostic := requireRetrievalError(t, err, StageBody, 2)
			if !errors.Is(err, context.DeadlineExceeded) || diagnostic.ApplicationAttempts != 3 ||
				diagnostic.TimeoutScope != "attempt" {
				t.Fatalf("exhausted attempt diagnostic=%+v err=%v", diagnostic, err)
			}
		})
	}
}

func TestFinalAttemptUsesRemainingOverallWindow(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	c, err := New(Config{
		Timeout: 75 * time.Millisecond, AttemptTimeout: 50 * time.Millisecond,
		Retries: 1, MaxBytes: 1024,
	}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.backoff = func(context.Context, time.Duration) error { return nil }
	start := time.Now()
	_, err = c.Do(context.Background(), model.Request{URL: server.URL + "/topic"})
	elapsed := time.Since(start)
	diagnostic := requireRetrievalError(t, err, StageHeaders, 1)
	if !errors.Is(err, context.DeadlineExceeded) || diagnostic.ApplicationAttempts != 2 ||
		diagnostic.TimeoutScope != "overall" || hits.Load() != 2 ||
		elapsed < 65*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("remaining-window retry elapsed=%s hits=%d diagnostic=%+v err=%v",
			elapsed, hits.Load(), diagnostic, err)
	}
}

func TestRetryableAndPermanentStatuses(t *testing.T) {
	for _, status := range []int{408, 425, 429, 500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if hits.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, "ok")
			}))
			defer server.Close()
			c := client(t, func(config *Config) {
				config.Timeout = time.Second
				config.AttemptTimeout = 500 * time.Millisecond
				config.Retries = 1
			})
			c.backoff = func(context.Context, time.Duration) error { return nil }
			response, err := c.Do(context.Background(), model.Request{URL: server.URL + "/topic"})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if hits.Load() != 2 {
				t.Fatalf("retryable status %d hits=%d", status, hits.Load())
			}
		})
	}
	for _, status := range []int{400, 401, 403, 404, 405, 410} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/robots.txt" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				hits.Add(1)
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := client(t, func(config *Config) { config.Retries = 2 }).Do(
				context.Background(), model.Request{URL: server.URL + "/topic"},
			)
			if err == nil || hits.Load() != 1 {
				t.Fatalf("permanent status %d hits=%d err=%v", status, hits.Load(), err)
			}
		})
	}
}

type retryLeaseBackend struct {
	topicCalls atomic.Int32
}

func (b *retryLeaseBackend) RoundTrip(request *http.Request) (*http.Response, error) {
	return b.RoundTripWithDispatch(request, func() error { return nil })
}

func (b *retryLeaseBackend) RoundTripWithDispatch(
	request *http.Request,
	start func() error,
) (*http.Response, error) {
	if request.URL.Path == "/robots.txt" {
		if err := start(); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusNotFound, Header: make(http.Header),
			Body: http.NoBody, Request: request,
		}, nil
	}
	if b.topicCalls.Add(1) == 1 {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	if err := start(); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader("final")), Request: request,
	}, nil
}

func TestConfigAttemptTimeoutValidationAndNormalization(t *testing.T) {
	config := DefaultConfig()
	if config.Timeout != 150*time.Second || config.AttemptTimeout != 45*time.Second || config.Retries != 2 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	config.AttemptTimeout = config.Timeout + time.Second
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "attempt timeout") {
		t.Fatalf("attempt timeout above overall accepted: %v", err)
	}
	normalized := (Config{Timeout: 30 * time.Millisecond}).normalized()
	if normalized.AttemptTimeout != 30*time.Millisecond {
		t.Fatalf("internal zero-value attempt timeout not normalized: %+v", normalized)
	}
	c, err := New(Config{Timeout: 30 * time.Millisecond, MaxBytes: 1024}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.config.AttemptTimeout != 30*time.Millisecond {
		t.Fatalf("zero-value internal attempt timeout not normalized: %+v", c.config)
	}
}

func TestCallerCancellationWinsOverAttemptAndOverallDeadlines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	c := client(t, func(config *Config) {
		config.Timeout = time.Second
		config.AttemptTimeout = 500 * time.Millisecond
		config.Retries = 2
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err := c.Do(ctx, model.Request{URL: server.URL + "/topic"})
	diagnostic := requireRetrievalError(t, err, StageHeaders, 0)
	if !errors.Is(err, context.Canceled) || diagnostic.TimeoutScope != "none" ||
		diagnostic.ApplicationAttempts != 1 {
		t.Fatalf("caller cancellation lost: %+v err=%v", diagnostic, err)
	}
}

var _ interface {
	RoundTripWithDispatch(*http.Request, func() error) (*http.Response, error)
} = (*retryLeaseBackend)(nil)
