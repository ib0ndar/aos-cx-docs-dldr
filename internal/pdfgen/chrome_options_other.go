//go:build !linux

package pdfgen

import "github.com/chromedp/chromedp"

// platformChromeOptions returns no extra launch flags outside Linux. macOS
// keeps Chrome's own sandbox enabled.
func platformChromeOptions() []chromedp.ExecAllocatorOption { return nil }
