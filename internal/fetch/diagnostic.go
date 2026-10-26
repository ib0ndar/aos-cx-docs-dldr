package fetch

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type RetrievalStage string

const (
	StageScope   RetrievalStage = "scope"
	StageRobots  RetrievalStage = "robots"
	StageLease   RetrievalStage = "lease"
	StagePacing  RetrievalStage = "pacing"
	StageHeaders RetrievalStage = "headers"
	StageBody    RetrievalStage = "body"
	StageBackoff RetrievalStage = "backoff"
)

type RetrievalError struct {
	RequestURL          string
	FinalURL            string
	Stage               RetrievalStage
	Elapsed             time.Duration
	ApplicationAttempts int
	ApplicationRetries  int
	TimeoutScope        string
	// BodyBytes is how much of the response body the last attempt received
	// when it failed in the body stage, so that a transfer that stopped can
	// be told from a slow one.
	BodyBytes int64
	Cause     error
}

func (e *RetrievalError) Error() string {
	final := e.FinalURL
	if final == "" {
		final = e.RequestURL
	}
	body := ""
	if e.Stage == StageBody {
		body = fmt.Sprintf(" body_bytes=%d", e.BodyBytes)
	}
	return fmt.Sprintf(
		"retrieval failed: request=%s final=%s stage=%s%s elapsed=%s application_attempts=%d application_retries=%d timeout_scope=%s: %v",
		e.RequestURL, final, e.Stage, body, e.Elapsed.Round(time.Millisecond), e.ApplicationAttempts,
		e.ApplicationRetries, e.TimeoutScope, e.Cause,
	)
}

// Stalled reports whether err is a retrieval that failed because the site
// stopped answering: the last attempt ran out of time, its body broke off or
// its connection failed, or the request's overall deadline passed. Such a
// failure can succeed when tried again minutes later.
func Stalled(err error) bool {
	var retrieval *RetrievalError
	if !errors.As(err, &retrieval) {
		return false
	}
	return networkStall(retrieval.Cause) || retrieval.TimeoutScope == "overall"
}

func (e *RetrievalError) Unwrap() error { return e.Cause }

type retrievalTrace struct {
	mu         sync.Mutex
	requestURL string
	finalURL   string
	stage      RetrievalStage
	started    time.Time
	bodyBytes  atomic.Int64
}

// countBody counts the bytes read from one attempt's response body and calls
// progress with the count whenever some arrive.
func (t *retrievalTrace) countBody(body io.ReadCloser, progress func(int64)) io.ReadCloser {
	if t != nil {
		t.bodyBytes.Store(0)
	}
	return &countingBody{ReadCloser: body, trace: t, progress: progress}
}

type countingBody struct {
	io.ReadCloser
	trace    *retrievalTrace
	progress func(int64)
	total    int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.total += int64(n)
		if b.trace != nil {
			b.trace.bodyBytes.Add(int64(n))
		}
		b.progress(b.total)
	}
	return n, err
}

func newRetrievalTrace(requestURL string) *retrievalTrace {
	return &retrievalTrace{
		requestURL: requestURL,
		finalURL:   requestURL,
		stage:      StageRobots,
		started:    time.Now(),
	}
}

func (t *retrievalTrace) update(stage RetrievalStage, finalURL string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stage = stage
	if finalURL != "" {
		t.finalURL = finalURL
	}
	t.mu.Unlock()
}

func (t *retrievalTrace) failure(attempts, retries int, timeoutScope string, cause error) error {
	if cause == nil {
		return nil
	}
	if t == nil {
		return cause
	}
	if existing, ok := cause.(*RetrievalError); ok {
		cause = existing.Cause
	}
	t.mu.Lock()
	requestURL, finalURL, stage, elapsed := t.requestURL, t.finalURL, t.stage, time.Since(t.started)
	t.mu.Unlock()
	return &RetrievalError{
		RequestURL: requestURL, FinalURL: finalURL, Stage: stage,
		Elapsed: elapsed, ApplicationAttempts: attempts, ApplicationRetries: retries,
		TimeoutScope: timeoutScope, BodyBytes: t.bodyBytes.Load(), Cause: cause,
	}
}

type retrievalTraceKey struct{}

func traceFromContext(ctx interface{ Value(any) any }) *retrievalTrace {
	trace, _ := ctx.Value(retrievalTraceKey{}).(*retrievalTrace)
	return trace
}
