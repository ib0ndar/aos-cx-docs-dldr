//go:build reqexperiment

package cli

import (
	"context"
	"io"

	"aos-cx-docs-dldr/internal/fetch"
)

// RunObserved keeps the real CLI selection/cache/catalogue/output path while
// allowing a diagnostic harness to bound and observe its selected transport.
func RunObserved(ctx context.Context, args []string, out, stderr io.Writer,
	factory func(string, fetch.Config, func(string)) (*fetch.Client, error),
) int {
	return run(ctx, args, out, stderr, factory)
}
