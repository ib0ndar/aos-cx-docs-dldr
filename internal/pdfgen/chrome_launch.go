package pdfgen

import (
	"io"
	"os"
	"path/filepath"

	"github.com/chromedp/chromedp"
)

// chromeLaunchDirectories are created mode 0700 inside the private renderer
// temp root before launch. Chrome's profile, HOME and TMPDIR live there, so
// it writes nothing into the user's home or shared temporary directory, and
// removing the temp root after Chrome has exited removes all of it.
var chromeLaunchDirectories = []string{"profile", "home", "tmp"}

func createChromeLaunchDirectories(tempRoot string) error {
	for _, name := range chromeLaunchDirectories {
		if err := os.Mkdir(filepath.Join(tempRoot, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// chromeAllocatorOptions returns the complete launch configuration for one
// pinned Chrome render rooted at tempRoot.
//
// DevTools runs over --remote-debugging-pipe (Chrome's file descriptors 3 and
// 4), never a loopback TCP port: no other local process or user can attach to
// the renderer, open its own unrestricted target, or read local files through
// it. Chrome also shuts down when the pipe closes because this process died.
// The environment is the sidecar allowlist, and the sandbox flag is always
// explicit, so chromedp never adds --no-sandbox by itself for root.
func chromeAllocatorOptions(chromePath, tempRoot string, logs io.Writer) []chromedp.ExecAllocatorOption {
	return append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.UserDataDir(filepath.Join(tempRoot, "profile")),
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-crash-reporter", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-features", "Translate,MediaRouter,OptimizationHints,AutofillServerCommunication"),
		chromedp.Flag("disable-quic", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("proxy-server", "http://127.0.0.1:9"),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.Flag("host-resolver-rules", "MAP * 0.0.0.0"),
		chromedp.Flag("disk-cache-dir", filepath.Join(tempRoot, "cache")),
		chromedp.Flag("crash-dumps-dir", filepath.Join(tempRoot, "crashes")),
		chromedp.Flag("no-sandbox", chromeNoSandbox),
		chromedp.RemoteDebuggingPipe,
		chromedp.ExactEnv(sidecarEnvironment(filepath.Join(tempRoot, "home"), filepath.Join(tempRoot, "tmp"))...),
		chromedp.CombinedOutput(logs),
		chromedp.ModifyCmdFunc(configureProcessGroup),
	)
}
