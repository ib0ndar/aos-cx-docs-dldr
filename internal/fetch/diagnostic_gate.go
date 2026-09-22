//go:build reqexperiment

package fetch

import "context"

// CheckRobotsForDiagnostic retrieves and validates robots without fetching the
// target. The policy identity is always the application, regardless of wire UA.
func (c *Client) CheckRobotsForDiagnostic(ctx context.Context, target string) error {
	target, err := CanonicalURL(target)
	if err != nil {
		return err
	}
	callerCtx := ctx
	trace := newRetrievalTrace(target)
	overallCtx, overallCancel := context.WithTimeout(ctx, c.config.Timeout)
	defer overallCancel()
	overallCtx = context.WithValue(overallCtx, retrievalTraceKey{}, trace)
	budget := &retryBudget{}
	c.config.Attempts.beginApplicationRetrieval()
	for {
		if err := overallCtx.Err(); err != nil {
			return trace.failure(
				budget.attempts, budget.used,
				retrievalTimeoutScope(callerCtx, overallCtx, overallCtx, err), err,
			)
		}
		budget.attempts++
		c.config.Attempts.beginApplicationAttempt(budget.attempts > 1)
		attemptCtx, attemptCancel := context.WithTimeout(overallCtx, c.config.AttemptTimeout)
		_, err = c.checkRobots(attemptCtx, target)
		if err == nil {
			attemptCancel()
			return nil
		}
		err = normalizeAttemptFailure(callerCtx, overallCtx, attemptCtx, err)
		attemptCancel()
		if !retryableAttemptFailure(err) {
			return trace.failure(
				budget.attempts, budget.used,
				retrievalTimeoutScope(callerCtx, overallCtx, attemptCtx, err), err,
			)
		}
		if retryErr := c.retry(overallCtx, budget, err, "", target); retryErr != nil {
			return trace.failure(
				budget.attempts, budget.used,
				retrievalTimeoutScope(callerCtx, overallCtx, attemptCtx, retryErr), retryErr,
			)
		}
	}
}
