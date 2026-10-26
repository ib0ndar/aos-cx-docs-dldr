package fetch

import (
	"context"
	"sync"
	"time"
)

// minBodyRate is the average rate, in bytes per second, at which a response
// body must keep arriving to postpone its retrieval's overall deadline. The
// publisher serves a file it first fetches from its origin at about 10 KB/s;
// a body that only trickles in still ends at the overall deadline.
const minBodyRate = 1024

// deadlineContext ends with context.DeadlineExceeded when its timer fires, as
// context.WithTimeout does, except that the deadline can be moved.
type deadlineContext struct {
	context.Context
	cancel   context.CancelCauseFunc
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
}

func newDeadlineContext(parent context.Context, d time.Duration) *deadlineContext {
	ctx, cancel := context.WithCancelCause(parent)
	c := &deadlineContext{Context: ctx, cancel: cancel, deadline: time.Now().Add(d)}
	c.timer = time.AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	return c
}

// Err reports context.DeadlineExceeded once the deadline has passed.
func (c *deadlineContext) Err() error {
	err := c.Context.Err()
	if err != nil && context.Cause(c.Context) == context.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return err
}

func (c *deadlineContext) Deadline() (time.Time, bool) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if parent, ok := c.Context.Deadline(); ok && parent.Before(deadline) {
		return parent, true
	}
	return deadline, true
}

// restart moves the deadline to d from now.
func (c *deadlineContext) restart(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Context.Err() == nil {
		c.deadline = time.Now().Add(d)
		c.timer.Reset(d)
	}
}

// extend moves the deadline to d from now if that is later.
func (c *deadlineContext) extend(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if target := time.Now().Add(d); c.Context.Err() == nil && target.After(c.deadline) {
		c.deadline = target
		c.timer.Reset(d)
	}
}

func (c *deadlineContext) stop() {
	c.mu.Lock()
	c.timer.Stop()
	c.mu.Unlock()
	c.cancel(context.Canceled)
}
