package pdfgen

import "github.com/chromedp/chromedp"

// platformChromeOptions returns Linux-specific launch flags.
//
// Chrome's layer-one sandbox needs either the SUID helper or unprivileged user
// namespaces. Container runtimes deny both by default, and the headless shell
// then refuses to start at all ("No usable sandbox!"). The image already runs
// as an unprivileged user inside the container boundary, and the renderer is
// launched with scripts disabled, all network routed to a dead proxy, foreign
// file access refused at the CDP layer, and a private throwaway profile. Those
// controls, not the Chrome sandbox, are what this application relies on for
// isolation, so disabling the sandbox here does not weaken an invariant the
// design depends on. macOS keeps Chrome's sandbox enabled.
//
// chromedp's defaults already include disable-dev-shm-usage, which matters in
// containers where /dev/shm is 64 MiB.
func platformChromeOptions() []chromedp.ExecAllocatorOption {
	return []chromedp.ExecAllocatorOption{chromedp.NoSandbox}
}
