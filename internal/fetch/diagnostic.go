package fetch

import (
	"fmt"
	"sync"
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
	Cause               error
}

func (e *RetrievalError) Error() string {
	final := e.FinalURL
	if final == "" {
		final = e.RequestURL
	}
	return fmt.Sprintf(
		"retrieval failed: request=%s final=%s stage=%s elapsed=%s application_attempts=%d application_retries=%d timeout_scope=%s: %v",
		e.RequestURL, final, e.Stage, e.Elapsed.Round(time.Millisecond), e.ApplicationAttempts,
		e.ApplicationRetries, e.TimeoutScope, e.Cause,
	)
}

func (e *RetrievalError) Unwrap() error { return e.Cause }

type retrievalTrace struct {
	mu         sync.Mutex
	requestURL string
	finalURL   string
	stage      RetrievalStage
	started    time.Time
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
		TimeoutScope: timeoutScope, Cause: cause,
	}
}

type retrievalTraceKey struct{}

func traceFromContext(ctx interface{ Value(any) any }) *retrievalTrace {
	trace, _ := ctx.Value(retrievalTraceKey{}).(*retrievalTrace)
	return trace
}
