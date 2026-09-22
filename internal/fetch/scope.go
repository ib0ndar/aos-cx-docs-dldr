package fetch

import "context"

type requestScopeKey struct{}

// WithRequestScope restricts document retrieval, including redirect targets.
// Robots bootstrap remains under the transport's independent robots policy.
func WithRequestScope(ctx context.Context, check func(string) error) context.Context {
	if previous, ok := ctx.Value(requestScopeKey{}).(func(string) error); ok {
		return context.WithValue(ctx, requestScopeKey{}, func(raw string) error {
			if err := previous(raw); err != nil {
				return err
			}
			return check(raw)
		})
	}
	return context.WithValue(ctx, requestScopeKey{}, check)
}
