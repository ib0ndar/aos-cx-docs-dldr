package pdfgen

import (
	"errors"
	"runtime"
	"testing"
)

func TestShellQuoteKeepsEveryPathOneWord(t *testing.T) {
	for value, want := range map[string]string{
		"/Users/me/Applications/aos-cx-docs-dldr/1.0.0": "'/Users/me/Applications/aos-cx-docs-dldr/1.0.0'",
		"/Users/me/My Apps/tool":                        "'/Users/me/My Apps/tool'",
		"/Users/me/it's/$HOME/`x`":                      `'/Users/me/it'\''s/$HOME/` + "`x`'",
	} {
		if got := shellQuote(value); got != want {
			t.Fatalf("shellQuote(%q) = %s, want %s", value, got, want)
		}
	}
}

func TestQuarantineErrorUnwrapsItsCause(t *testing.T) {
	cause := errors.New("permission denied")
	err := error(&QuarantineError{Files: []string{"/a"}, Command: "xattr -dr com.apple.quarantine '/a'", Cause: cause})
	if !errors.Is(err, cause) {
		t.Fatal("the removal failure is not reachable through the quarantine error")
	}
}

func TestClearSidecarQuarantineDoesNothingWithoutQuarantine(t *testing.T) {
	if runtime.GOOS == "darwin" {
		if !quarantineSupported {
			t.Fatal("macOS must clear the download quarantine")
		}
		return
	}
	clearance, err := ClearSidecarQuarantine("", "")
	if quarantineSupported || err != nil || len(clearance.Files) != 0 {
		t.Fatalf("%s acted on a quarantine it does not have: %+v %v", runtime.GOOS, clearance, err)
	}
}
