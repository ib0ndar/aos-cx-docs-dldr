package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var unsafeComponent = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
var pathSeparators = regexp.MustCompile(`[/\\]+`)
var reserved = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$`)

func SafeComponent(value string) (string, error) {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return "", errors.New("empty or invalid path label")
	}
	clean := strings.Trim(unsafeComponent.ReplaceAllString(value, "-"), ".-")
	if clean == "" {
		return "", fmt.Errorf("cannot use %q as a directory name", value)
	}
	if reserved.MatchString(clean) {
		clean = "_" + clean
	}
	if clean != value || len(clean) > 100 {
		sum := sha256.Sum256([]byte(value))
		if len(clean) > 80 {
			clean = clean[:80]
		}
		clean += "-" + hex.EncodeToString(sum[:])[:10]
	}
	return clean, nil
}

// PDFFilename names a PDF readably from its labels. Control characters and
// Unicode format characters (category Cf, which holds every bidirectional
// control and the zero-width characters) are removed, so a publisher title
// cannot reorder or hide part of the name a file manager shows.
func PDFFilename(platform, version, title string) (string, error) {
	return pdfFilename(platform, version, title, true)
}

// PDFFilenameAccepted reports whether name is the PDF filename for these
// labels, or the name 0.8 produced before format characters were removed.
// PDFs that 0.8 libraries already hold keep their names; nothing is renamed.
func PDFFilenameAccepted(platform, version, title, name string) bool {
	for _, stripFormat := range []bool{true, false} {
		expected, err := pdfFilename(platform, version, title, stripFormat)
		if err == nil && expected == name {
			return true
		}
	}
	return false
}

func pdfFilename(platform, version, title string, stripFormat bool) (string, error) {
	var parts []string
	for _, value := range []string{platform, version, title} {
		if !utf8.ValidString(value) {
			return "", errors.New("invalid PDF filename label")
		}
		value = norm.NFC.String(value)
		value = pathSeparators.ReplaceAllString(value, " - ")
		value = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || (stripFormat && unicode.In(r, unicode.Cf)) || strings.ContainsRune(`<>:"|?*`, r) {
				return -1
			}
			return r
		}, value)
		value = strings.Trim(strings.Join(strings.Fields(value), " "), " .")
		if value == "" {
			return "", errors.New("cannot create PDF filename from empty label")
		}
		parts = append(parts, value)
	}
	stem := strings.Join(parts, " - ")
	if len(stem)+4 > 240 {
		sum := sha256.Sum256([]byte(stem))
		stem = stem[:225]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		stem = strings.TrimRight(stem, " .-") + "-" + hex.EncodeToString(sum[:])[:10]
	}
	return stem + ".pdf", nil
}

// ResolveBase follows user-selected shortcuts, including missing descendants,
// before opening the app-managed namespace where links are forbidden.
func ResolveBase(value string) (string, error) {
	if value == "" {
		return "", errors.New("base directory is required")
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	existing := absolute
	var missing []string
	for {
		_, err := os.Lstat(existing)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("cannot resolve base directory %s", absolute)
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("cannot resolve directory shortcut: %w", err)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	return resolved, nil
}
