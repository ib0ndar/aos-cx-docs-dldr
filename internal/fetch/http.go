package fetch

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"aos-cx-docs-dldr/internal/model"
	"github.com/temoto/robotstxt"
)

const userAgent = "aos-cx-docs-dldr/" + model.Version
const maxRobotsBytes = 512 * 1024

type Config struct {
	Delay          time.Duration
	Timeout        time.Duration
	AttemptTimeout time.Duration
	Retries        int
	MaxBytes       int64
	Attempts       *AttemptBudget
}

func DefaultConfig() Config {
	return Config{
		Delay: 100 * time.Millisecond, Timeout: 150 * time.Second,
		AttemptTimeout: 45 * time.Second, Retries: 2, MaxBytes: 256 << 20,
	}
}

func (c Config) Validate() error {
	if c.Delay < 0 || c.Delay > 300*time.Second || c.Timeout <= 0 || c.Timeout > 24*time.Hour ||
		c.AttemptTimeout <= 0 || c.AttemptTimeout > 24*time.Hour || c.AttemptTimeout > c.Timeout ||
		c.Retries < 0 || c.Retries > 10 || c.MaxBytes < 1 || c.MaxBytes > 16<<30 {
		return errors.New("invalid network limits: delay 0..300s, timeout >0..86400s, attempt timeout >0..timeout, retries 0..10, response size >0..16384 MiB")
	}
	return nil
}

func (c Config) normalized() Config {
	if c.AttemptTimeout == 0 && c.Timeout > 0 {
		c.AttemptTimeout = min(c.Timeout, 45*time.Second)
	}
	return c
}

type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string {
	if e.Status == 401 || e.Status == 403 {
		return fmt.Sprintf("HTTP %d access denied: %s; native transport will not launch a browser or bypass access restrictions", e.Status, e.URL)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.URL)
}

type robotsState struct {
	ready chan struct{}
	group *robotstxt.Group
	err   error
}

type Client struct {
	config  Config
	http    *http.Client
	notify  func(string)
	backoff func(context.Context, time.Duration) error
	mu      sync.Mutex
	last    map[string]time.Time
	robots  map[string]*robotsState
}

func New(c Config, notify func(string)) (*Client, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxResponseHeaderBytes = 1 << 20
	t.MaxIdleConnsPerHost = 4
	t.DisableCompression = true
	return NewWithRoundTripper(c, notify, t)
}

// NewWithRoundTripper keeps redirects, robots and complete-retrieval policy in
// this package. The backend must return original encoded bytes without cookies,
// authentication, high-level retries, redirects or text decoding of its own.
func NewWithRoundTripper(c Config, notify func(string), backend http.RoundTripper) (*Client, error) {
	c = c.normalized()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if notify == nil {
		return nil, errors.New("transport requires a diagnostic callback")
	}
	if backend == nil {
		return nil, errors.New("transport requires an HTTP backend")
	}
	return &Client{
		config: c, notify: notify, backoff: wait,
		last: make(map[string]time.Time), robots: make(map[string]*robotsState),
		http: &http.Client{Transport: scheduledBackend{backend: backend, attempts: c.Attempts}, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}, nil
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

// Do opens a response stream. Use Download with a replaceable sink when the
// complete body must be retried: already-consumed bytes cannot be replayed safely.
func (c *Client) Do(ctx context.Context, req model.Request) (model.Resource, error) {
	return c.execute(ctx, req, nil)
}

func (c *Client) Download(ctx context.Context, req model.Request, consume func(model.Resource) error) (model.Resource, error) {
	if consume == nil {
		return model.Resource{}, errors.New("download requires a replaceable response sink")
	}
	return c.execute(ctx, req, consume)
}

func (c *Client) execute(ctx context.Context, req model.Request, consume func(model.Resource) error) (model.Resource, error) {
	raw, err := CanonicalURL(req.URL)
	if err != nil {
		return model.Resource{}, err
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return model.Resource{}, errors.New("only public GET and HEAD requests are supported")
	}
	req.URL = raw
	trace := newRetrievalTrace(raw)
	callerCtx := ctx
	overallCtx, overallCancel := context.WithTimeout(ctx, c.config.Timeout)
	overallCtx = context.WithValue(overallCtx, retrievalTraceKey{}, trace)
	budget := &retryBudget{}
	c.config.Attempts.beginApplicationRetrieval()
	r, attemptCancel, attemptCtx, err := c.retrieve(callerCtx, overallCtx, req, false, budget, consume)
	if err != nil {
		timeoutScope := retrievalTimeoutScope(callerCtx, overallCtx, attemptCtx, err)
		overallCancel()
		return model.Resource{}, trace.failure(
			budget.attempts, budget.used, timeoutScope, err,
		)
	}
	if consume != nil {
		overallCancel()
		return r, nil
	}
	r.Body = &diagnosticBody{
		ReadCloser: &closingBody{ReadCloser: r.Body, cancel: func() {
			attemptCancel()
			overallCancel()
		}},
		trace: trace, budget: budget, caller: callerCtx, overall: overallCtx, attempt: attemptCtx,
	}
	return r, nil
}

type retryBudget struct {
	used     int
	attempts int
}

func (c *Client) retry(ctx context.Context, budget *retryBudget, cause error, header, raw string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget.used >= c.config.Retries {
		return cause
	}
	delay, err := retryDelay(header, budget.used, time.Now())
	if err != nil {
		if delay == 0 {
			return fmt.Errorf("%s: %w", raw, err)
		}
		c.notify(err.Error())
	}
	retry := budget.used + 1
	c.notify(fmt.Sprintf("%s; retry %d/%d in %s: %s", cause, retry, c.config.Retries, delay, raw))
	traceFromContext(ctx).update(StageBackoff, raw)
	if err := c.backoff(ctx, delay); err != nil {
		return err
	}
	budget.used = retry
	return nil
}

func (c *Client) retrieve(
	callerCtx, overallCtx context.Context,
	req model.Request,
	robots bool,
	budget *retryBudget,
	consume func(model.Resource) error,
) (model.Resource, context.CancelFunc, context.Context, error) {
	for {
		if err := overallCtx.Err(); err != nil {
			return model.Resource{}, nil, overallCtx, err
		}
		budget.attempts++
		c.config.Attempts.beginApplicationAttempt(budget.attempts > 1)
		attemptCtx, attemptCancel := context.WithTimeout(overallCtx, c.config.AttemptTimeout)
		r, err := c.request(attemptCtx, req, robots)
		if err == nil && !robots && r.Status != http.StatusNotModified && (r.Status < 200 || r.Status >= 300) {
			err = errors.Join(err, r.Body.Close())
			if err == nil {
				traceFromContext(attemptCtx).update(StageHeaders, r.URL)
				err = &StatusError{URL: r.URL, Status: r.Status}
			}
		}
		if err == nil && consume == nil {
			return r, attemptCancel, attemptCtx, nil
		}
		if err == nil {
			if robots {
				traceFromContext(attemptCtx).update(StageRobots, r.URL)
			} else {
				traceFromContext(attemptCtx).update(StageBody, r.URL)
			}
			err = consume(r)
			closeErr := r.Body.Close()
			if err == nil {
				err = closeErr
			} else if closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
		if err == nil {
			r.Body = http.NoBody
			attemptCancel()
			return r, nil, attemptCtx, nil
		}
		err = normalizeAttemptFailure(callerCtx, overallCtx, attemptCtx, err)
		attemptCancel()
		if !retryableAttemptFailure(err) {
			return model.Resource{}, nil, attemptCtx, err
		}
		retryAfter := ""
		var status *retryableStatusError
		if errors.As(err, &status) {
			retryAfter = status.retryAfter
		}
		if err := c.retry(overallCtx, budget, err, retryAfter, req.URL); err != nil {
			return model.Resource{}, nil, attemptCtx, err
		}
	}
}

type attemptTimeoutError struct {
	stage RetrievalStage
	cause error
}

func (e *attemptTimeoutError) Error() string {
	return fmt.Sprintf("per-attempt deadline exceeded at %s: %v", e.stage, e.cause)
}
func (e *attemptTimeoutError) Unwrap() error { return e.cause }

type retryableStatusError struct {
	status     *StatusError
	retryAfter string
}

func (e *retryableStatusError) Error() string { return e.status.Error() }
func (e *retryableStatusError) Unwrap() error { return e.status }

func normalizeAttemptFailure(caller, overall, attempt context.Context, err error) error {
	if caller.Err() != nil {
		return caller.Err()
	}
	if overall.Err() != nil {
		return overall.Err()
	}
	if attempt.Err() != nil {
		stage := StageHeaders
		if trace := traceFromContext(attempt); trace != nil {
			trace.mu.Lock()
			stage = trace.stage
			trace.mu.Unlock()
		}
		return &attemptTimeoutError{stage: stage, cause: attempt.Err()}
	}
	return err
}

func retryableAttemptFailure(err error) bool {
	var attemptTimeout *attemptTimeoutError
	if errors.As(err, &attemptTimeout) {
		return true
	}
	var status *retryableStatusError
	if errors.As(err, &status) {
		return true
	}
	var body *transientBodyError
	if errors.As(err, &body) {
		return true
	}
	var request *transientRequestError
	return errors.As(err, &request)
}

func retrievalTimeoutScope(caller, overall, attempt context.Context, err error) string {
	if !errors.Is(err, context.DeadlineExceeded) {
		return "none"
	}
	var attemptTimeout *attemptTimeoutError
	if errors.As(err, &attemptTimeout) {
		return "attempt"
	}
	if caller.Err() != nil {
		return "caller"
	}
	if overall.Err() != nil {
		return "overall"
	}
	if attempt != nil && attempt.Err() != nil {
		return "attempt"
	}
	return "unknown"
}

type transientBodyError struct{ err error }

func (e *transientBodyError) Error() string { return "transient response read: " + e.err.Error() }
func (e *transientBodyError) Unwrap() error { return e.err }

type transientRequestError struct{ err error }

func (e *transientRequestError) Error() string { return "transient HTTP request: " + e.err.Error() }
func (e *transientRequestError) Unwrap() error { return e.err }

func transientNetworkError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var networkError net.Error
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || (errors.As(err, &networkError) && networkError.Timeout())
}

type networkBody struct{ io.ReadCloser }

func (b *networkBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && transientNetworkError(err) {
		err = &transientBodyError{err}
	}
	return n, err
}

type closingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *closingBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

type limitedBody struct {
	io.ReadCloser
	left int64
}

type gzipBody struct {
	*gzip.Reader
	source  io.Closer
	readErr error
}

func (b *gzipBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err != nil && err != io.EOF {
		b.readErr = err
		err = &transientBodyError{err: err}
	}
	return n, err
}

func (b *gzipBody) Close() error {
	decodeErr := b.Reader.Close()
	// flate.Close can repeat the preceding read error, not a cleanup failure.
	if b.readErr != nil && errors.Is(decodeErr, b.readErr) {
		decodeErr = nil
	}
	return errors.Join(decodeErr, b.source.Close())
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if int64(len(p)) > b.left+1 {
		p = p[:b.left+1]
	}
	n, err := b.ReadCloser.Read(p)
	if int64(n) > b.left {
		allowed := int(b.left)
		b.left = 0
		return allowed, errors.New("response exceeds configured byte limit")
	}
	b.left -= int64(n)
	return n, err
}

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) throttle(ctx context.Context, raw string, gap time.Duration) error {
	key := origin(raw)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c.mu.Lock()
		delay := time.Until(c.last[key].Add(gap))
		if delay <= 0 {
			c.last[key] = time.Now()
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		if err := wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) checkRobots(ctx context.Context, raw string) (time.Duration, error) {
	traceFromContext(ctx).update(StageRobots, raw)
	key := origin(raw)
	c.mu.Lock()
	state, exists := c.robots[key]
	if !exists {
		state = &robotsState{ready: make(chan struct{})}
		c.robots[key] = state
	}
	c.mu.Unlock()
	if !exists {
		state.group, state.err = c.loadRobots(ctx, key+"/robots.txt")
		close(state.ready)
		if state.err != nil && (retryableAttemptFailure(state.err) ||
			errors.Is(state.err, context.Canceled) ||
			errors.Is(state.err, context.DeadlineExceeded)) {
			c.mu.Lock()
			if c.robots[key] == state {
				delete(c.robots, key)
			}
			c.mu.Unlock()
		}
	}
	select {
	case <-ctx.Done():
		traceFromContext(ctx).update(StageRobots, "")
		return 0, ctx.Err()
	case <-state.ready:
	}
	if state.err != nil {
		traceFromContext(ctx).update(StageRobots, "")
		return 0, state.err
	}
	u, _ := url.Parse(raw)
	if !state.group.Test(u.RequestURI()) {
		traceFromContext(ctx).update(StageRobots, raw)
		return 0, fmt.Errorf("publisher robots.txt disallows %s", raw)
	}
	return max(c.config.Delay, state.group.CrawlDelay), nil
}

func retryable(status int) bool {
	return status == 408 || status == 425 || status == 429 ||
		status == 500 || status == 502 || status == 503 || status == 504
}

func retryDelay(value string, attempt int, now time.Time) (time.Duration, error) {
	delay := time.Second << attempt
	if value == "" {
		return delay, nil
	}
	var requested time.Duration
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > 300 {
			return 0, errors.New("server requested a wait longer than 300s; retry this job later")
		}
		requested = time.Duration(seconds) * time.Second
	} else if date, err := http.ParseTime(value); err == nil {
		requested = date.Sub(now)
	} else {
		return delay, fmt.Errorf("invalid Retry-After header %q", value)
	}
	if requested > 300*time.Second {
		return 0, errors.New("server requested a wait longer than 300s; retry this job later")
	}
	return max(delay, requested), nil
}

func (c *Client) request(
	ctx context.Context,
	req model.Request,
	robots bool,
) (model.Resource, error) {
	for hop := 0; hop <= 10; hop++ {
		if robots {
			traceFromContext(ctx).update(StageRobots, req.URL)
		} else {
			traceFromContext(ctx).update(StageScope, req.URL)
		}
		gap := c.config.Delay
		if !robots {
			if scope, ok := ctx.Value(requestScopeKey{}).(func(string) error); ok {
				if err := scope(req.URL); err != nil {
					return model.Resource{}, err
				}
			}
			var err error
			gap, err = c.checkRobots(ctx, req.URL)
			if err != nil {
				return model.Resource{}, err
			}
		}
		target := req.URL
		traceFromContext(ctx).update(StageLease, target)
		requestContext := context.WithValue(ctx, dispatchKey{}, func() error { return c.throttle(ctx, target, gap) })
		r, err := http.NewRequestWithContext(requestContext, req.Method, req.URL, nil)
		if err != nil {
			return model.Resource{}, err
		}
		r.Header.Set("User-Agent", userAgent)
		r.Header.Set("Accept-Encoding", "gzip")
		if req.Refresh {
			r.Header.Set("Cache-Control", "no-cache")
		}
		if req.URL == req.ValidatorURL {
			if req.ETag != "" {
				r.Header.Set("If-None-Match", req.ETag)
			}
			if req.Modified != "" {
				r.Header.Set("If-Modified-Since", req.Modified)
			}
		}
		response, err := c.http.Do(r)
		if err != nil {
			if ctx.Err() != nil {
				return model.Resource{}, ctx.Err()
			}
			if !transientNetworkError(err) {
				return model.Resource{}, fmt.Errorf("HTTP request failed: %w", err)
			}
			return model.Resource{}, &transientRequestError{err: err}
		}
		if retryable(response.StatusCode) {
			response.Body.Close()
			return model.Resource{}, &retryableStatusError{
				status:     &StatusError{URL: req.URL, Status: response.StatusCode},
				retryAfter: response.Header.Get("Retry-After"),
			}
		}
		status := response.StatusCode
		if status == 301 || status == 302 || status == 303 || status == 307 || status == 308 {
			response.Body.Close()
			if hop == 10 {
				return model.Resource{}, fmt.Errorf("too many redirects: %s", req.URL)
			}
			location := response.Header.Get("Location")
			if location == "" {
				return model.Resource{}, fmt.Errorf("redirect without Location: %s", req.URL)
			}
			// Validate raw Location before net/url can discard leading controls.
			if strings.Contains(location, "\\") || strings.IndexFunc(location, unicode.IsControl) >= 0 {
				return model.Resource{}, fmt.Errorf("invalid redirect Location %q", location)
			}
			u, err := response.Location()
			if err != nil {
				return model.Resource{}, fmt.Errorf("invalid redirect: %w", err)
			}
			next, err := CanonicalURL(u.String())
			if err != nil {
				return model.Resource{}, err
			}
			if strings.HasPrefix(req.URL, "https:") && !strings.HasPrefix(next, "https:") {
				return model.Resource{}, errors.New("refusing HTTPS to HTTP redirect downgrade")
			}
			req.URL = next
			continue
		}
		limit := c.config.MaxBytes
		if robots {
			limit = min(limit, maxRobotsBytes)
		}
		hasContent := req.Method != http.MethodHead && status >= 200 && status < 300 && status != http.StatusNoContent
		if hasContent && response.ContentLength > limit {
			response.Body.Close()
			return model.Resource{}, fmt.Errorf("response exceeds configured byte limit: %s", req.URL)
		}
		if robots {
			traceFromContext(ctx).update(StageRobots, req.URL)
		} else {
			traceFromContext(ctx).update(StageBody, req.URL)
		}
		var body io.ReadCloser = &limitedBody{ReadCloser: &networkBody{response.Body}, left: limit}
		if hasContent {
			switch strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding"))) {
			case "gzip":
				reader, err := gzip.NewReader(body)
				if err != nil {
					body.Close()
					return model.Resource{}, &transientBodyError{
						err: fmt.Errorf("invalid gzip response at %s: %w", req.URL, err),
					}
				}
				body = &limitedBody{ReadCloser: &gzipBody{Reader: reader, source: body}, left: limit}
			case "", "identity":
			default:
				body.Close()
				return model.Resource{}, fmt.Errorf("unsupported Content-Encoding %q: %s", response.Header.Get("Content-Encoding"), req.URL)
			}
		}
		return model.Resource{URL: req.URL, Status: status, Headers: response.Header.Clone(), Body: body}, nil
	}
	return model.Resource{}, errors.New("redirect limit exceeded")
}

type diagnosticBody struct {
	io.ReadCloser
	trace   *retrievalTrace
	budget  *retryBudget
	caller  context.Context
	overall context.Context
	attempt context.Context
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		err = normalizeAttemptFailure(b.caller, b.overall, b.attempt, err)
		err = b.trace.failure(
			b.budget.attempts, b.budget.used,
			retrievalTimeoutScope(b.caller, b.overall, b.attempt, err), err,
		)
	}
	return n, err
}

func (b *diagnosticBody) Close() error {
	err := b.ReadCloser.Close()
	if err != nil {
		err = normalizeAttemptFailure(b.caller, b.overall, b.attempt, err)
		return b.trace.failure(
			b.budget.attempts, b.budget.used,
			retrievalTimeoutScope(b.caller, b.overall, b.attempt, err), err,
		)
	}
	return nil
}
