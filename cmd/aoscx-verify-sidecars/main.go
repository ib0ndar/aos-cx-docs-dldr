// Command aoscx-verify-sidecars resolves a Chrome and a qpdf sidecar through
// the application's own platform-aware verification and reports their pinned
// identities. It performs exactly the checks aos-cx-docs-dldr performs before every
// launch: regular nonsymlink file, expected object format and architecture,
// SHA-256, and exact shared-library closure.
//
// It is build tooling: the container image runs it once to prove the staged
// sidecars match the pins, and it is useful for checking any bundle by hand.
// It reads and hashes files only; nothing is executed.
package main

import (
	"fmt"
	"os"

	"aos-cx-docs-dldr/internal/pdfgen"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: aoscx-verify-sidecars CHROME_EXECUTABLE QPDF_EXECUTABLE")
		os.Exit(2)
	}
	chrome, err := pdfgen.ResolveSidecar(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "chrome:", err)
		os.Exit(1)
	}
	qpdf, err := pdfgen.ResolveQPDFSidecar(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "qpdf:", err)
		os.Exit(1)
	}
	fmt.Printf("chrome-headless-shell %s r%s sha256=%s\n", chrome.Version, chrome.Revision, chrome.ExecutableSHA256)
	fmt.Printf("qpdf %s sha256=%s bundle=%s\n", qpdf.Version, qpdf.ExecutableSHA256, qpdf.BundleSHA256)
}
