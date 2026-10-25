//go:build unix

package pdfgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

const (
	// fakeChromeRecordFlag turns the test binary into a fake Chrome that
	// records its launch and answers DevTools commands on fds 3 and 4.
	fakeChromeRecordFlag = "aoscx-test-fake-chrome-record"
	// fakeSidecarEnvironmentArgument turns the test binary into a fake qpdf
	// that prints its environment.
	fakeSidecarEnvironmentArgument = "--aoscx-test-print-environment"
	// chromeParentHelperEnvironment turns the test binary into a parent that
	// launches the pinned Chrome at the given path and waits to be killed.
	chromeParentHelperEnvironment = "AOSCX_TEST_CHROME_PARENT_HELPER"
	chromeParentRootEnvironment   = "AOSCX_TEST_CHROME_PARENT_ROOT"
)

func TestMain(m *testing.M) {
	for _, argument := range os.Args[1:] {
		if record, ok := strings.CutPrefix(argument, "--"+fakeChromeRecordFlag+"="); ok {
			os.Exit(runFakeChrome(record))
		}
	}
	if len(os.Args) == 2 && os.Args[1] == fakeSidecarEnvironmentArgument {
		for _, variable := range os.Environ() {
			fmt.Println(variable)
		}
		os.Exit(0)
	}
	if chrome := os.Getenv(chromeParentHelperEnvironment); chrome != "" {
		os.Exit(runChromeParentHelper(chrome, os.Getenv(chromeParentRootEnvironment)))
	}
	os.Exit(m.Run())
}

type fakeChromeLaunch struct {
	Args    []string `json:"args"`
	Env     []string `json:"env"`
	PID     int      `json:"pid"`
	PGID    int      `json:"pgid"`
	PipeFDs bool     `json:"pipe_fds"`
}

func runFakeChrome(record string) int {
	commands := os.NewFile(3, "devtools-commands")
	responses := os.NewFile(4, "devtools-responses")
	launch := fakeChromeLaunch{Args: os.Args[1:], Env: os.Environ(), PID: os.Getpid()}
	launch.PGID, _ = syscall.Getpgid(0)
	_, commandErr := commands.Stat()
	_, responseErr := responses.Stat()
	launch.PipeFDs = commandErr == nil && responseErr == nil
	data, err := json.Marshal(launch)
	if err != nil || os.WriteFile(record, data, 0o600) != nil {
		return 2
	}
	fmt.Fprintln(os.Stderr, "fake Chrome diagnostics")
	if !launch.PipeFDs {
		return 3
	}
	reader := bufio.NewReader(commands)
	for {
		message, err := reader.ReadBytes(0)
		if err != nil {
			return 0 // the parent closed the DevTools pipe
		}
		var command struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(message[:len(message)-1], &command) != nil {
			return 4
		}
		reply, _ := json.Marshal(map[string]any{"id": command.ID, "result": map[string]any{}})
		if _, err := responses.Write(append(reply, 0)); err != nil {
			return 0
		}
	}
}

// runChromeParentHelper launches the real pinned Chrome exactly as a render
// does, reports its process ID and profile, and then waits to be killed.
func runChromeParentHelper(chromePath, root string) int {
	tempRoot, err := os.MkdirTemp(root, "chrome-parent-")
	if err == nil {
		err = createChromeLaunchDirectories(tempRoot)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logs := &boundedLog{limit: maxChromeLogBytes}
	allocatorCtx, _ := chromedp.NewExecAllocator(context.Background(), chromeAllocatorOptions(chromePath, tempRoot, logs)...)
	browserCtx, _ := chromedp.NewContext(allocatorCtx)
	if err := chromedp.Run(browserCtx, chromedp.Navigate("about:blank")); err != nil {
		fmt.Fprintln(os.Stderr, chromeError("start pinned Chrome", err, logs))
		return 1
	}
	fmt.Printf("%d\t%s\n", chromedp.FromContext(browserCtx).Browser.Process().Pid, filepath.Join(tempRoot, "profile"))
	time.Sleep(10 * time.Minute)
	return 1
}

// clearInheritedSidecarVariables empties the developer's own locale and time
// zone settings for one test; empty values are never inherited.
func clearInheritedSidecarVariables(t *testing.T) {
	t.Helper()
	for _, name := range sidecarInheritedVariables {
		t.Setenv(name, "")
	}
}

func TestSidecarEnvironmentIsAllowlisted(t *testing.T) {
	clearInheritedSidecarVariables(t)
	for name, value := range map[string]string{
		"DYLD_INSERT_LIBRARIES": "/nonexistent/aoscx-probe.dylib",
		"DYLD_LIBRARY_PATH":     "/nonexistent/aoscx-probe",
		"LD_PRELOAD":            "/nonexistent/aoscx-probe.so",
		"LD_LIBRARY_PATH":       "/nonexistent/aoscx-probe",
		"MallocStackLogging":    "1",
		"MallocLogFile":         "/nonexistent/aoscx-probe.log",
		"OPENSSL_CONF":          "/nonexistent/openssl.cnf",
		"GCONV_PATH":            "/nonexistent/aoscx-probe",
		"LOCPATH":               "/nonexistent/aoscx-probe",
		"HTTPS_PROXY":           "http://127.0.0.1:1",
		"PATH":                  "/nonexistent/aoscx-probe:/usr/bin",
		"HOME":                  "/nonexistent/home",
		"TMPDIR":                "/nonexistent/tmp",
		"LANG":                  "en_US.UTF-8",
		"LC_ALL":                "../../nonexistent/locale",
		"LC_CTYPE":              "de_DE@euro",
		"LANGUAGE":              "en_GB:en",
		"TZ":                    ":/nonexistent/zoneinfo",
	} {
		t.Setenv(name, value)
	}
	got := sidecarEnvironment("/private/home", "/private/tmp")
	want := []string{
		"PATH=" + sidecarPath, "HOME=/private/home", "TMPDIR=/private/tmp",
		"LANG=en_US.UTF-8", "LANGUAGE=en_GB:en", "LC_CTYPE=de_DE@euro",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sidecar environment = %q, want %q", got, want)
	}
	t.Setenv("TZ", "Europe/Berlin")
	if got := sidecarEnvironment("", ""); !slices.Contains(got, "TZ=Europe/Berlin") ||
		slices.ContainsFunc(got, func(variable string) bool {
			return strings.HasPrefix(variable, "HOME=") || strings.HasPrefix(variable, "TMPDIR=")
		}) {
		t.Fatalf("sidecar environment without private directories = %q", got)
	}
	for _, value := range []string{"UTC", ":Europe/Berlin", "<+03>-3", "CET-1CEST,M3.5.0,M10.5.0/3"} {
		if !inheritableSidecarValue("TZ", value) {
			t.Errorf("valid time zone %q was dropped", value)
		}
	}
	for _, value := range []string{"/etc/localtime", "Europe/../../tmp/x", "en US", "C\x00", strings.Repeat("a", 129)} {
		if inheritableSidecarValue("TZ", value) || inheritableSidecarValue("LANG", value) {
			t.Errorf("unsafe value %q was inherited", value)
		}
	}
}

// TestChromeLaunchUsesPrivatePipeAndAllowlistedEnvironment launches the real
// render configuration against a fake Chrome. It pins the exact argument set:
// DevTools over --remote-debugging-pipe with no TCP port, the platform's
// explicit sandbox decision, and the private profile, plus the allowlisted
// environment, the private process group and the bounded combined log.
func TestChromeLaunchUsesPrivatePipeAndAllowlistedEnvironment(t *testing.T) {
	for name, value := range map[string]string{
		"DYLD_LIBRARY_PATH":               "/nonexistent/aoscx-probe",
		"DYLD_FRAMEWORK_PATH":             "/nonexistent/aoscx-probe",
		"LD_PRELOAD":                      "/nonexistent/aoscx-probe.so",
		"LD_LIBRARY_PATH":                 "/nonexistent/aoscx-probe",
		"OPENSSL_CONF":                    "/nonexistent/openssl.cnf",
		"AOSCX_SIDECAR_ENVIRONMENT_PROBE": "leaked",
		"LANG":                            "en_US.UTF-8",
		"TZ":                              "Europe/Berlin",
	} {
		t.Setenv(name, value)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tempRoot := t.TempDir()
	if err := createChromeLaunchDirectories(tempRoot); err != nil {
		t.Fatal(err)
	}
	for _, name := range chromeLaunchDirectories {
		if info, err := os.Stat(filepath.Join(tempRoot, name)); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("launch directory %s is not private: %v %v", name, info, err)
		}
	}
	record := filepath.Join(t.TempDir(), "launch.json")
	logs := &boundedLog{limit: maxChromeLogBytes}
	options := append(chromeAllocatorOptions(executable, tempRoot, logs), chromedp.Flag(fakeChromeRecordFlag, record))
	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(t.Context(), options...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()
	browser, err := chromedp.FromContext(browserCtx).Allocator.Allocate(browserCtx)
	if err != nil {
		t.Fatalf("pipe launch failed: %v; diagnostics: %s", err, logs.String())
	}
	if browser.Process() == nil {
		t.Fatal("launched browser has no process identity")
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var launch fakeChromeLaunch
	if err := json.Unmarshal(data, &launch); err != nil {
		t.Fatal(err)
	}

	wantArgs := []string{
		"--crash-dumps-dir=" + filepath.Join(tempRoot, "crashes"),
		"--disable-background-networking",
		"--disable-background-timer-throttling",
		"--disable-backgrounding-occluded-windows",
		"--disable-breakpad",
		"--disable-client-side-phishing-detection",
		"--disable-component-update",
		"--disable-crash-reporter",
		"--disable-default-apps",
		"--disable-dev-shm-usage",
		"--disable-domain-reliability",
		"--disable-extensions",
		"--disable-features=Translate,MediaRouter,OptimizationHints,AutofillServerCommunication",
		"--disable-hang-monitor",
		"--disable-ipc-flooding-protection",
		"--disable-popup-blocking",
		"--disable-prompt-on-repost",
		"--disable-quic",
		"--disable-renderer-backgrounding",
		"--disable-sync",
		"--disk-cache-dir=" + filepath.Join(tempRoot, "cache"),
		"--enable-automation",
		"--enable-features=NetworkService,NetworkServiceInProcess",
		"--force-color-profile=srgb",
		"--headless",
		"--hide-scrollbars",
		"--host-resolver-rules=MAP * 0.0.0.0",
		"--metrics-recording-only",
		"--mute-audio",
		"--no-default-browser-check",
		"--no-first-run",
		"--password-store=basic",
		"--proxy-bypass-list=<-loopback>",
		"--proxy-server=http://127.0.0.1:9",
		"--remote-debugging-pipe",
		"--safebrowsing-disable-auto-update",
		"--use-mock-keychain",
		"--user-data-dir=" + filepath.Join(tempRoot, "profile"),
		"--" + fakeChromeRecordFlag + "=" + record,
		"about:blank",
	}
	if runtime.GOOS == "linux" {
		wantArgs = append(wantArgs, "--no-sandbox")
	} else if chromeNoSandbox {
		t.Fatal("Chrome's sandbox is disabled outside Linux")
	}
	gotArgs := slices.Clone(launch.Args)
	slices.Sort(gotArgs)
	slices.Sort(wantArgs)
	if !slices.Equal(gotArgs, wantArgs) {
		t.Fatalf("Chrome launch arguments changed:\n got %q\nwant %q", gotArgs, wantArgs)
	}
	if slices.ContainsFunc(launch.Args, func(argument string) bool {
		return strings.HasPrefix(argument, "--remote-debugging-port")
	}) {
		t.Fatalf("Chrome was given a TCP DevTools port: %q", launch.Args)
	}
	if !launch.PipeFDs {
		t.Fatal("Chrome did not receive DevTools pipe file descriptors 3 and 4")
	}
	if launch.PGID != launch.PID {
		t.Fatalf("Chrome is not in its own process group: pid=%d pgid=%d", launch.PID, launch.PGID)
	}

	gotEnv := slices.Clone(launch.Env)
	wantEnv := sidecarEnvironment(filepath.Join(tempRoot, "home"), filepath.Join(tempRoot, "tmp"))
	slices.Sort(gotEnv)
	slices.Sort(wantEnv)
	if !slices.Equal(gotEnv, wantEnv) ||
		!slices.Contains(gotEnv, "HOME="+filepath.Join(tempRoot, "home")) ||
		!slices.Contains(gotEnv, "TMPDIR="+filepath.Join(tempRoot, "tmp")) ||
		!slices.Contains(gotEnv, "LANG=en_US.UTF-8") || !slices.Contains(gotEnv, "TZ=Europe/Berlin") {
		t.Fatalf("Chrome environment is not the sidecar allowlist:\n got %q\nwant %q", gotEnv, wantEnv)
	}

	cancelBrowser()
	cancelAllocator()
	if err := syscall.Kill(-launch.PGID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("Chrome process group survived cancellation: %v", err)
	}
	if !strings.Contains(logs.String(), "fake Chrome diagnostics") {
		t.Fatalf("bounded Chrome log lost process output: %q", logs.String())
	}
}

func TestChromeLaunchRejectsPortWithPipe(t *testing.T) {
	logs := &boundedLog{limit: maxChromeLogBytes}
	options := append(chromeAllocatorOptions("/nonexistent/chrome", t.TempDir(), logs),
		chromedp.Flag("remote-debugging-port", "0"))
	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(t.Context(), options...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()
	if err := chromedp.Run(browserCtx); err == nil || !strings.Contains(err.Error(), "RemoteDebuggingPipe") {
		t.Fatalf("a TCP DevTools port was accepted with the pipe transport: %v", err)
	}
}

func TestQPDFLaunchUsesAllowlistedEnvironment(t *testing.T) {
	allowStandInQPDF(t)
	for name, value := range map[string]string{
		"DYLD_LIBRARY_PATH":               "/nonexistent/aoscx-probe",
		"LD_PRELOAD":                      "/nonexistent/aoscx-probe.so",
		"OPENSSL_CONF":                    "/nonexistent/openssl.cnf",
		"AOSCX_SIDECAR_ENVIRONMENT_PROBE": "leaked",
		"LANG":                            "en_US.UTF-8",
	} {
		t.Setenv(name, value)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if _, err := runQPDF(t.Context(), QPDFSidecar{Path: executable}, &stdout, 1<<30, fakeSidecarEnvironmentArgument); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	want := sidecarEnvironment("", "")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) || !slices.Contains(got, "LANG=en_US.UTF-8") {
		t.Fatalf("qpdf environment is not the sidecar allowlist:\n got %q\nwant %q", got, want)
	}
}
