//go:build !darwin

package main

import (
	"fmt"
	"os"
	"runtime"
)

// The generated-PDF acceptance command is maintainer-only tooling. It depends
// on the explicitly manifest-bound macOS arm64 MuPDF build and on the pinned
// Chrome/qpdf sidecar closure, none of which are shipped or reproduced on other
// platforms. Keeping a buildable stub here lets `go build ./...` and
// `go vet ./...` cover the whole module on every supported build host instead
// of failing with an opaque "build constraints exclude all Go files" error.
func main() {
	fmt.Fprintf(
		os.Stderr,
		"aoscx-pdf-acceptance is maintainer-only tooling supported on macOS arm64, not %s/%s\n",
		runtime.GOOS, runtime.GOARCH,
	)
	os.Exit(2)
}
