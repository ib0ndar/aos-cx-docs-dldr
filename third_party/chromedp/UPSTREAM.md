# Local chromedp fork

Upstream: https://github.com/chromedp/chromedp, tag `v0.16.0`, commit
`7963c203ed5458147d27dc39a5c06d2b12e81664`.
Module checksum: `h1:rOO4deOm4CbZgBCa8mD9g2rDyIoNs0BkgvNrlbp5ouk=`.

The upstream MIT `LICENSE` is retained unchanged. No module-cache files were
modified. The root module uses an explicit local replacement; this is not a
root vendor tree and does not affect unrelated dependencies. The module path,
package API and version requirement (`v0.16.0`) are unchanged, so the release
SBOM and notices list it as a modified local replacement of that version.

Copied: every non-test Go source file of the root package, the embedded
`js/*.js` files, and the generated `kb/kb.go` and `device/device.go`. Not
copied: upstream tests and `testdata`, the network-fetching `//go:build ignore`
generators `kb/gen.go` and `device/gen.go`, CI files, `contrib/`, and
`README.md`. `go.mod`/`go.sum` drop the two test-only requirements
(`github.com/ledongthuc/pdf`, `github.com/orisano/pixelmatch`); the remaining
requirement versions are upstream's, and the application's own `go.sum`
governs the versions actually linked.

## Why

chromedp's `ExecAllocator` always starts Chrome with
`--remote-debugging-port=0` unless a port is set, and `NewBrowser` can only
dial a websocket URL. That exposes an unauthenticated DevTools endpoint on
`127.0.0.1` for the whole render: any local process or user can open its own
target, where neither request interception nor script disablement applies,
and read local files. Chrome's `--remote-debugging-pipe` has no listener, but
upstream offers no hook to use it: `Browser` fields are unexported and there
is no way to inject a `Transport`.

## Local patches

Every patch is marked `Local aos-cx-docs-dldr patch` in the source.

- `pipe.go` (new): `pipeConn`, a `Transport` over Chrome's DevTools pipe
  (commands to Chrome's fd 3, responses and events from fd 4, one JSON
  message per NUL-terminated frame); `startWithPipe`, which passes the child
  ends as fds 3 and 4, closes them in this process after start, and refuses
  foreign `ExtraFiles`; and `allocatePipeBrowser`, the pipe-mode tail of
  `Allocate`. It mirrors the websocket path (lost connection cancels the
  browser, `Process()` and user data dir are set) and bounds startup with the
  existing `WSURLReadTimeout` by requiring an answer to `Browser.getVersion`.
- `allocate.go`: new `RemoteDebuggingPipe` option. It adds
  `--remote-debugging-pipe` instead of `--remote-debugging-port=0`, rejects a
  `remote-debugging-port` flag, sends combined output straight to
  `CombinedOutput`'s writer through `exec.Cmd` (no URL is scraped), and starts
  Chrome through `startWithPipe`. New `ExactEnv` option: the given variables
  become the complete process environment instead of being appended to
  `os.Environ()`. Without either option, behaviour is upstream's.
- `browser.go`: `NewBrowser` dials only when no transport was supplied by the
  unexported `withTransport` option.
- `pipe_test.go` (new): framing, truncated-frame, close and descriptor
  conflict tests. Run them from the application root with
  `go test github.com/chromedp/chromedp`, which resolves this replacement
  with the application's dependency versions, or with `go test ./...` in this
  directory once its own pinned versions are in the module cache.

Unchanged upstream behaviour the application relies on: an explicit
`Flag("no-sandbox", false)` suppresses chromedp's automatic `--no-sandbox`
for root, and `ModifyCmdFunc` replaces the default Linux parent-death setup.

When upgrading chromedp, re-apply these patches to the new release or drop
the fork if upstream gains pipe transport and exact-environment support.
