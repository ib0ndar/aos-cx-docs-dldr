package fetch

import (
	"errors"
	"fmt"
	"sync"
)

// CompatibleWireAttemptAllowance reserves capacity for one reused-connection
// attempt in req's outer transport plus the initial attempt and seven retries
// bounded by req v3.61.0's pinned internal HTTP/2 transport.
const CompatibleWireAttemptAllowance = 9

// AttemptBudget bounds conservative connection/stream attempt starts while
// reserving enough headroom for transparent retries inside an admitted
// backend RoundTrip. Completed header writes are reported separately.
type AttemptBudget struct {
	mu          sync.Mutex
	max         int
	allowance   int
	attempted   int
	headers     int
	reserved    int
	admitted    int
	retrievals  int
	appAttempts int
	appRetries  int
	exhausted   bool
	violated    bool
}

type AttemptStats struct {
	Max                    int  `json:"max_wire_attempts"`
	Allowance              int  `json:"per_request_allowance"`
	AdmittedRequests       int  `json:"admitted_requests"`
	ApplicationRetrievals  int  `json:"application_retrievals"`
	ApplicationAttempts    int  `json:"application_attempts"`
	ApplicationRetries     int  `json:"application_retries"`
	AttemptedTransmissions int  `json:"attempted_transmissions"`
	ObservedHeaderWrites   int  `json:"observed_header_writes"`
	ReservedAttempts       int  `json:"reserved_attempts"`
	Exhausted              bool `json:"exhausted"`
	AllowanceViolated      bool `json:"allowance_violated"`
}

type AttemptBudgetError struct {
	Stats AttemptStats
}

func (e *AttemptBudgetError) Error() string {
	return fmt.Sprintf(
		"network attempt budget exhausted before dispatch: attempted=%d reserved=%d allowance=%d max=%d",
		e.Stats.AttemptedTransmissions, e.Stats.ReservedAttempts, e.Stats.Allowance, e.Stats.Max,
	)
}

func NewAttemptBudget(maxAttempts, perRequestAllowance int) (*AttemptBudget, error) {
	if maxAttempts < 1 || perRequestAllowance < 1 || perRequestAllowance > maxAttempts {
		return nil, errors.New("network attempt budget requires positive max and per-request allowance no larger than max")
	}
	return &AttemptBudget{max: maxAttempts, allowance: perRequestAllowance}, nil
}

func (b *AttemptBudget) Stats() AttemptStats {
	if b == nil {
		return AttemptStats{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.statsLocked()
}

func (b *AttemptBudget) statsLocked() AttemptStats {
	return AttemptStats{
		Max: b.max, Allowance: b.allowance, AdmittedRequests: b.admitted,
		ApplicationRetrievals: b.retrievals, ApplicationAttempts: b.appAttempts,
		ApplicationRetries:     b.appRetries,
		AttemptedTransmissions: b.attempted, ObservedHeaderWrites: b.headers,
		ReservedAttempts: b.reserved, Exhausted: b.exhausted,
		AllowanceViolated: b.violated,
	}
}

func (b *AttemptBudget) beginApplicationRetrieval() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.retrievals++
	b.mu.Unlock()
}

func (b *AttemptBudget) beginApplicationAttempt(retry bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.appAttempts++
	if retry {
		b.appRetries++
	}
	b.mu.Unlock()
}

func (b *AttemptBudget) begin() (*attemptPermit, error) {
	if b == nil {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exhausted || b.attempted+b.reserved+b.allowance > b.max {
		b.exhausted = true
		return nil, &AttemptBudgetError{Stats: b.statsLocked()}
	}
	b.reserved += b.allowance
	b.admitted++
	return &attemptPermit{budget: b, remaining: b.allowance}, nil
}

type attemptPermit struct {
	budget    *AttemptBudget
	remaining int
	started   int
	done      bool
}

func (p *attemptPermit) startAttempt() error {
	if p == nil {
		return nil
	}
	b := p.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.done || p.remaining == 0 {
		// The pinned backend allowance is verified by integration tests. Mark
		// violation fail-closed for all subsequent dispatches if it changes.
		b.exhausted = true
		b.violated = true
		return &AttemptBudgetError{Stats: b.statsLocked()}
	}
	p.remaining--
	p.started++
	b.reserved--
	b.attempted++
	if b.attempted > b.max {
		b.exhausted = true
	}
	return nil
}

func (p *attemptPermit) requireObservedAttempt() error {
	if p == nil {
		return nil
	}
	b := p.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.started > 0 {
		return nil
	}
	b.exhausted = true
	b.violated = true
	return &AttemptBudgetError{Stats: b.statsLocked()}
}

func (p *attemptPermit) wroteHeaders() {
	if p == nil {
		return
	}
	b := p.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	b.headers++
	if b.headers > b.attempted {
		b.exhausted = true
		b.violated = true
	}
}

func (p *attemptPermit) finish() {
	if p == nil {
		return
	}
	b := p.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.done {
		return
	}
	b.reserved -= p.remaining
	p.remaining = 0
	p.done = true
}
