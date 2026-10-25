package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"aos-cx-docs-dldr/internal/cli"
)

// shutdownSignals all cancel the run the same way as Ctrl-C, so accepted work
// is published, sidecar process groups are killed and private temporary
// files are removed before the process exits with 130. An untrapped SIGTERM
// (kill, IDE stop) or SIGHUP (terminal closed, ssh dropped) would end the
// process at once, skipping that cleanup and leaving Chrome running in its own
// process group.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals...)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
