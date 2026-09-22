//go:build reqexperiment && !compatibletests

package cli

import (
	"context"
	"io"

	"aos-cx-docs-dldr/internal/fetch"
)

func runContract(ctx context.Context, args []string, out, stderr io.Writer) int {
	return run(ctx, args, out, stderr, func(name string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
		if name == "compatible" {
			return fetch.NewCompatible(config, notify)
		}
		return fetch.NewReqExperiment(config, notify)
	})
}
