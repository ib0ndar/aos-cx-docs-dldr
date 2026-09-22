//go:build compatibletests

package fetch

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

func TestCompatibleAllowanceCoversPinnedMaximumRefusedStreamReplay(t *testing.T) {
	server, attempts := scriptedHTTP2Server(t, func(attempt int32) bool { return attempt > 1 })
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	backend := newCompatibilityBackend(true, true)
	backend.transport.TLSClientConfig.RootCAs = roots
	defer backend.CloseIdleConnections()
	warm, err := http.NewRequest(http.MethodGet, server.URL+"/warm", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := backend.RoundTrip(warm)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if attempts.Load() != 1 {
		t.Fatalf("backend did not establish exactly one warm request: %d", attempts.Load())
	}
	budget, err := NewAttemptBudget(CompatibleWireAttemptAllowance, CompatibleWireAttemptAllowance)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: scheduledBackend{backend: backend, attempts: budget}}
	request, err := http.NewRequestWithContext(
		context.WithValue(context.Background(), dispatchKey{}, func() error { return nil }),
		http.MethodGet, server.URL+"/refused", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); err == nil {
		t.Fatal("pinned req backend exceeded its expected retry limit")
	}
	stats := budget.Stats()
	if attempts.Load() != 1+CompatibleWireAttemptAllowance ||
		stats.AttemptedTransmissions != CompatibleWireAttemptAllowance ||
		stats.ObservedHeaderWrites != CompatibleWireAttemptAllowance ||
		stats.AdmittedRequests != 1 || stats.ReservedAttempts != 0 ||
		stats.AllowanceViolated {
		t.Fatalf("pinned warm maximum replay differs: server=%d stats=%+v", attempts.Load(), stats)
	}
	request, _ = http.NewRequestWithContext(
		context.WithValue(context.Background(), dispatchKey{}, func() error { return nil }),
		http.MethodGet, server.URL+"/blocked", nil,
	)
	_, err = client.Do(request)
	var exhausted *AttemptBudgetError
	if !errors.As(err, &exhausted) || attempts.Load() != 1+CompatibleWireAttemptAllowance {
		t.Fatalf("post-replay N+1 dispatch was not blocked: server=%d err=%v", attempts.Load(), err)
	}
}

type maximumReplayPerApplicationAttempt struct {
	topics atomic.Int32
}

func (b *maximumReplayPerApplicationAttempt) RoundTrip(request *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(request.Context())
	writes := 1
	status := http.StatusNotFound
	body := ""
	if request.URL.Path != "/robots.txt" {
		writes = CompatibleWireAttemptAllowance
		if b.topics.Add(1) < 3 {
			status = http.StatusServiceUnavailable
		} else {
			status = http.StatusOK
			body = "ok"
		}
	}
	for range writes {
		trace.GotConn(httptrace.GotConnInfo{})
		trace.WroteHeaders()
	}
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}, nil
}

func TestCompatibleBudgetReservesEveryApplicationAttempt(t *testing.T) {
	budget, err := NewAttemptBudget(1+3*CompatibleWireAttemptAllowance, CompatibleWireAttemptAllowance)
	if err != nil {
		t.Fatal(err)
	}
	backend := &maximumReplayPerApplicationAttempt{}
	c, err := NewWithRoundTripper(Config{
		Timeout: time.Second, AttemptTimeout: 500 * time.Millisecond,
		Retries: 2, MaxBytes: 1024, Attempts: budget,
	}, func(string) {}, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.backoff = func(context.Context, time.Duration) error { return nil }
	response, err := c.Do(context.Background(), model.Request{URL: "https://publisher.example/topic"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	stats := budget.Stats()
	if backend.topics.Load() != 3 || stats.ApplicationRetrievals != 1 ||
		stats.ApplicationAttempts != 3 || stats.ApplicationRetries != 2 ||
		stats.AdmittedRequests != 4 ||
		stats.AttemptedTransmissions != 1+3*CompatibleWireAttemptAllowance ||
		stats.ObservedHeaderWrites != stats.AttemptedTransmissions ||
		stats.ReservedAttempts != 0 || stats.AllowanceViolated {
		t.Fatalf("compatible application-attempt accounting=%+v topics=%d", stats, backend.topics.Load())
	}
}
