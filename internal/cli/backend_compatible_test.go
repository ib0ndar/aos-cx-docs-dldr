//go:build compatibletests

package cli

import (
	"context"
	"io"
	"strings"
)

func runContract(ctx context.Context, args []string, out, stderr io.Writer) int {
	for _, arg := range args {
		if arg == "--transport" || strings.HasPrefix(arg, "--transport=") {
			return Run(ctx, args, out, stderr)
		}
	}
	args = append(append([]string{}, args...), "--transport", "compatible")
	return Run(ctx, args, out, stderr)
}
