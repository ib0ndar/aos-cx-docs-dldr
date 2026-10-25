package pdfgen

import (
	"os"
	"strings"
)

// sidecarPath is the only PATH a sidecar sees. Both sidecars are launched by
// absolute verified path and never search PATH for their own components.
const sidecarPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// sidecarInheritedVariables are the only variables copied from this process.
// They select locale and time zone; nothing in this list names a library,
// plugin, configuration file or search path.
var sidecarInheritedVariables = []string{
	"LANG", "LANGUAGE", "LC_ALL", "LC_ADDRESS", "LC_COLLATE", "LC_CTYPE",
	"LC_IDENTIFICATION", "LC_MEASUREMENT", "LC_MESSAGES", "LC_MONETARY",
	"LC_NAME", "LC_NUMERIC", "LC_PAPER", "LC_TELEPHONE", "LC_TIME", "TZ",
}

// sidecarEnvironment returns the complete environment for a pinned Chrome or
// qpdf process. Both are ad-hoc or linker signed without the hardened runtime,
// so the dynamic loader would honour DYLD_* on macOS and LD_* on Linux and
// load libraries outside the verified closure. The environment is therefore
// an allowlist, never a filtered copy: loader, malloc-debugging, OpenSSL,
// gconv, proxy and every other inherited variable is dropped. A non-empty home
// or temp becomes HOME or TMPDIR, so the sidecar writes nothing into the
// user's home or shared temporary directory.
func sidecarEnvironment(home, temp string) []string {
	environment := []string{"PATH=" + sidecarPath}
	if home != "" {
		environment = append(environment, "HOME="+home)
	}
	if temp != "" {
		environment = append(environment, "TMPDIR="+temp)
	}
	for _, name := range sidecarInheritedVariables {
		if value, ok := os.LookupEnv(name); ok && inheritableSidecarValue(name, value) {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

// inheritableSidecarValue accepts ordinary locale names ("en_US.UTF-8",
// "de_DE@euro", "en_GB:en") and time-zone names or POSIX rules
// ("Europe/Berlin", ":UTC", "<+03>-3"). It rejects paths, so a value cannot
// point the C library or ICU at a foreign locale or zoneinfo file.
func inheritableSidecarValue(name, value string) bool {
	if value == "" || len(value) > 128 || strings.Contains(value, "..") {
		return false
	}
	allowed := "_.@-+:,"
	if name == "TZ" {
		if strings.HasPrefix(strings.TrimPrefix(value, ":"), "/") {
			return false
		}
		allowed += "/<>"
	}
	for _, r := range value {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
			strings.ContainsRune(allowed, r)) {
			return false
		}
	}
	return true
}
