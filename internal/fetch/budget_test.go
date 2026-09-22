package fetch

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

type traceRoundTripper struct {
	writes int
	err    error
}

func (t traceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(request.Context())
	for range t.writes {
		if trace != nil {
			if trace.GotConn != nil {
				trace.GotConn(httptrace.GotConnInfo{})
			}
			if trace.WroteHeaders != nil {
				trace.WroteHeaders()
			}
		}
	}
	if t.err != nil {
		return nil, t.err
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: request,
	}, nil
}

func TestAttemptBudgetCoversRobotsRedirectRetryHeadAndExhaustion(t *testing.T) {
	var hits atomic.Int32
	var finalHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/robots.txt":
			w.WriteHeader(http.StatusNotFound)
		case "/start":
			http.Redirect(w, r, "/final", http.StatusFound)
		case "/final":
			if finalHits.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, "ok")
		default:
			io.WriteString(w, "ok")
		}
	}))
	defer server.Close()
	budget, err := NewAttemptBudget(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{Timeout: 3 * time.Second, Retries: 1, MaxBytes: 1024, Attempts: budget}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Do(context.Background(), model.Request{URL: server.URL + "/start"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = client.Do(context.Background(), model.Request{URL: server.URL + "/head", Method: http.MethodHead})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	response, err = client.Do(context.Background(), model.Request{URL: server.URL + "/last"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	_, err = client.Do(context.Background(), model.Request{URL: server.URL + "/blocked"})
	var exhausted *AttemptBudgetError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected typed budget exhaustion, got %v", err)
	}
	stats := budget.Stats()
	if hits.Load() != 7 || stats.AttemptedTransmissions != 7 || stats.ObservedHeaderWrites != 7 ||
		stats.AdmittedRequests != 7 || stats.ApplicationRetrievals != 4 ||
		stats.ApplicationAttempts != 5 || stats.ApplicationRetries != 1 ||
		stats.ReservedAttempts != 0 || !stats.Exhausted || stats.AllowanceViolated {
		t.Fatalf("wrong shared budget accounting: hits=%d stats=%+v", hits.Load(), stats)
	}
	_, err = client.Do(context.Background(), model.Request{URL: server.URL + "/still-blocked"})
	if !errors.As(err, &exhausted) || hits.Load() != 7 {
		t.Fatalf("exhaustion did not stop later work: hits=%d err=%v", hits.Load(), err)
	}
}

func TestAttemptBudgetParallelCallersCannotExceedLimit(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		<-release
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	budget, _ := NewAttemptBudget(6, 2)
	client, err := New(Config{Timeout: 2 * time.Second, MaxBytes: 1024, Attempts: budget}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := range 3 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			response, err := client.Do(context.Background(), model.Request{URL: server.URL + "/topic?i=" + string(rune('a'+index))})
			if response.Body != nil {
				response.Body.Close()
			}
			errs <- err
		}(i)
	}
	deadline := time.Now().Add(time.Second)
	for hits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
	close(errs)
	exhaustions := 0
	for err := range errs {
		var exhausted *AttemptBudgetError
		if errors.As(err, &exhausted) {
			exhaustions++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 || exhaustions != 1 || budget.Stats().AttemptedTransmissions != 3 ||
		budget.Stats().ObservedHeaderWrites != 3 {
		t.Fatalf("parallel budget escaped: hits=%d exhaustion=%d stats=%+v", hits.Load(), exhaustions, budget.Stats())
	}
}

func TestOverallContextDeadlineIsNotResetPerResource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		time.Sleep(70 * time.Millisecond)
		io.WriteString(w, "ok")
	}))
	defer server.Close()
	budget, _ := NewAttemptBudget(6, 2)
	client, err := New(Config{Timeout: time.Second, MaxBytes: 1024, Attempts: budget}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	response, err := client.Do(ctx, model.Request{URL: server.URL + "/one"})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	start := time.Now()
	_, err = client.Do(ctx, model.Request{URL: server.URL + "/two"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 90*time.Millisecond {
		t.Fatalf("per-resource timeout reset overall deadline: elapsed=%s err=%v", time.Since(start), err)
	}
	if stats := budget.Stats(); stats.AttemptedTransmissions != 3 ||
		stats.ObservedHeaderWrites != 3 || stats.ReservedAttempts != 0 {
		t.Fatalf("deadline attempt/reservation accounting mismatch: %+v", stats)
	}
}

func TestAttemptReservationsReleaseExactlyOnceOnErrorAndCancellation(t *testing.T) {
	budget, _ := NewAttemptBudget(4, 2)
	schedule := func() context.Context {
		return context.WithValue(context.Background(), dispatchKey{}, func() error { return nil })
	}
	backend := scheduledBackend{backend: traceRoundTripper{err: errors.New("dial failed")}, attempts: budget}
	request, _ := http.NewRequestWithContext(schedule(), http.MethodGet, "https://example.test/error", nil)
	if _, err := backend.RoundTrip(request); err == nil {
		t.Fatal("backend error hidden")
	}
	if stats := budget.Stats(); stats.ReservedAttempts != 0 || stats.AttemptedTransmissions != 0 ||
		stats.ObservedHeaderWrites != 0 || stats.AdmittedRequests != 1 {
		t.Fatalf("error leaked reservation: %+v", stats)
	}
	backend.backend = traceRoundTripper{writes: 1}
	request, _ = http.NewRequestWithContext(schedule(), http.MethodGet, "https://example.test/success", nil)
	response, err := backend.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if stats := budget.Stats(); stats.ReservedAttempts != 1 || stats.AttemptedTransmissions != 1 ||
		stats.ObservedHeaderWrites != 1 {
		t.Fatalf("response body did not retain unused replay headroom: %+v", stats)
	}
	response.Body.Close()
	response.Body.Close()
	if stats := budget.Stats(); stats.ReservedAttempts != 0 || stats.AttemptedTransmissions != 1 ||
		stats.ObservedHeaderWrites != 1 || stats.AdmittedRequests != 2 {
		t.Fatalf("success double-released or leaked reservation: %+v", stats)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled = context.WithValue(cancelled, dispatchKey{}, func() error { return context.Canceled })
	request, _ = http.NewRequestWithContext(cancelled, http.MethodGet, "https://example.test/cancelled", nil)
	if _, err := backend.RoundTrip(request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was not preserved: %v", err)
	}
	if stats := budget.Stats(); stats.ReservedAttempts != 0 || stats.AttemptedTransmissions != 1 ||
		stats.ObservedHeaderWrites != 1 {
		t.Fatalf("cancellation leaked reservation: %+v", stats)
	}
}

func TestAttemptBudgetAccountsStandardHTTP1StaleConnectionRetry(t *testing.T) {
	var dials atomic.Int32
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		client, server := net.Pipe()
		switch dials.Add(1) {
		case 1:
			go func() {
				defer server.Close()
				reader := bufio.NewReader(server)
				first, err := http.ReadRequest(reader)
				if err != nil {
					return
				}

				first.Body.Close()
				io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK")
				second, err := http.ReadRequest(reader)
				if err == nil {
					second.Body.Close()
				}
				// Closing after accepting the second request creates the reused
				// connection read failure that net/http transparently retries.
			}()
		case 2:
			go func() {
				defer server.Close()
				request, err := http.ReadRequest(bufio.NewReader(server))
				if err != nil {
					return
				}
				request.Body.Close()
				io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK")
			}()
		default:
			server.Close()
		}
		return client, nil
	}
	budget, _ := NewAttemptBudget(3, 2)
	client := &http.Client{Transport: scheduledBackend{backend: transport, attempts: budget}}
	requestContext := func() context.Context {
		return context.WithValue(context.Background(), dispatchKey{}, func() error { return nil })
	}
	for _, path := range []string{"/first", "/stale"} {
		request, _ := http.NewRequestWithContext(requestContext(), http.MethodGet, "http://fixture.test"+path, nil)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil || string(body) != "OK" {
			t.Fatalf("bad response: %q %v %v", body, readErr, closeErr)
		}
	}
	stats := budget.Stats()
	if dials.Load() != 2 || stats.AttemptedTransmissions != 3 || stats.ObservedHeaderWrites != 3 ||
		stats.AdmittedRequests != 2 ||
		stats.ReservedAttempts != 0 || stats.AllowanceViolated {
		t.Fatalf("standard stale retry accounting failed: dials=%d stats=%+v", dials.Load(), stats)
	}
	request, _ := http.NewRequestWithContext(requestContext(), http.MethodGet, "http://fixture.test/blocked", nil)
	_, err := client.Do(request)
	var exhausted *AttemptBudgetError
	if !errors.As(err, &exhausted) || dials.Load() != 2 {
		t.Fatalf("stale retry headroom did not stop N+1: dials=%d err=%v", dials.Load(), err)
	}
	transport.CloseIdleConnections()
}

func TestAttemptAllowanceViolationIsReportedFailClosed(t *testing.T) {
	budget, _ := NewAttemptBudget(4, 2)
	backend := scheduledBackend{backend: traceRoundTripper{writes: 3}, attempts: budget}
	ctx := context.WithValue(context.Background(), dispatchKey{}, func() error { return nil })
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/overflow", nil)
	response, err := backend.RoundTrip(request)
	var exceeded *AttemptBudgetError
	if response != nil || !errors.As(err, &exceeded) {
		t.Fatalf("allowance violation did not cancel active request: response=%v err=%v", response, err)
	}
	stats := budget.Stats()
	if !stats.AllowanceViolated || !stats.Exhausted || stats.AttemptedTransmissions != 2 ||
		stats.ObservedHeaderWrites != 3 || stats.ReservedAttempts != 0 {
		t.Fatalf("allowance change was not fail-closed: %+v", stats)
	}
	request, _ = http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/blocked", nil)
	_, err = backend.RoundTrip(request)
	var exhausted *AttemptBudgetError
	if !errors.As(err, &exhausted) {
		t.Fatalf("allowance violation did not block later dispatch: %v", err)
	}
}

func TestBudgetedBackendCannotReturnSuccessWithoutAttemptTrace(t *testing.T) {
	budget, _ := NewAttemptBudget(2, 1)
	backend := scheduledBackend{backend: traceRoundTripper{}, attempts: budget}
	ctx := context.WithValue(context.Background(), dispatchKey{}, func() error { return nil })
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/untraced", nil)
	response, err := backend.RoundTrip(request)
	var exhausted *AttemptBudgetError
	if response != nil || !errors.As(err, &exhausted) {
		t.Fatalf("untraced backend success bypassed budget: response=%v err=%v", response, err)
	}
	stats := budget.Stats()
	if !stats.AllowanceViolated || !stats.Exhausted || stats.AttemptedTransmissions != 0 ||
		stats.ReservedAttempts != 0 {
		t.Fatalf("untraced backend did not fail closed: %+v", stats)
	}
}
