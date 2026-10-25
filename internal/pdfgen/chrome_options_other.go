//go:build !linux

package pdfgen

// chromeNoSandbox keeps Chrome's own sandbox enabled outside Linux. The flag
// is passed explicitly as false because chromedp otherwise adds --no-sandbox
// by itself whenever the process runs as root.
const chromeNoSandbox = false
