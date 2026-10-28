package pdfgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// macOS gives every file extracted from a browser download the
// com.apple.quarantine attribute. Gatekeeper then stops each quarantined
// executable and library of the unsigned bundle separately when it is first
// run or loaded, and Privacy & Security approves only that one file: allowing
// aos-cx-docs-dldr still leaves a dialog for every executable and library of
// the bundled Chrome and qpdf, each in the middle of a conversion that then
// fails. The approved executable pins the SHA-256 of every sidecar file, so
// before a run converts anything it removes the attribute itself from exactly
// the pinned code files it has just verified.

// quarantineAttribute is the extended attribute macOS adds to downloaded files.
const quarantineAttribute = "com.apple.quarantine"

// QuarantineClearance lists the pinned sidecar files whose download quarantine
// ClearSidecarQuarantine removed.
type QuarantineClearance struct {
	Files []string
}

// QuarantineError reports pinned sidecar files that still carry the download
// quarantine, which Gatekeeper would stop one by one during conversion, and
// the command that removes it from their directories.
type QuarantineError struct {
	// Files still carry the quarantine.
	Files []string
	// Command removes the quarantine from every directory holding Files.
	Command string
	// Overridden is set when some of Files belong to a sidecar named by
	// --chrome-path, --qpdf-path or their environment variables, which the
	// application never changes.
	Overridden bool
	// Cause is the first failure to remove the quarantine, if any.
	Cause error
}

func (e *QuarantineError) Error() string {
	var message strings.Builder
	if len(e.Files) == 1 {
		message.WriteString("macOS keeps the download quarantine on 1 PDF tool file")
	} else {
		fmt.Fprintf(&message, "macOS keeps the download quarantine on %d PDF tool files", len(e.Files))
	}
	if e.Cause != nil {
		fmt.Fprintf(&message, "; removing it failed: %v", e.Cause)
	}
	if e.Overridden {
		fmt.Fprintf(&message,
			"; PDF tools named by --chrome-path, --qpdf-path, %s or %s are never changed",
			ChromePathEnvironment, QPDFPathEnvironment)
	}
	// The CLI prints errors on one line, so the command comes last, whole.
	message.WriteString(". Gatekeeper would stop the PDF tools during conversion, so nothing has been downloaded. To allow the PDF tools, run this command, then start the download again: ")
	message.WriteString(e.Command)
	return message.String()
}

func (e *QuarantineError) Unwrap() error { return e.Cause }

// ClearSidecarQuarantine prepares the pinned PDF sidecars for a run that will
// convert HTML, before anything is downloaded. On macOS it resolves and
// verifies each sidecar exactly as conversion does and, for a sidecar bundled
// beside the running executable, removes the quarantine attribute from its
// pinned code files. A sidecar named by flag or environment is only checked:
// it is never changed, and if it is quarantined the error names the command
// to run. A sidecar that is missing or fails verification is left alone for
// conversion to report. Elsewhere it does nothing.
func ClearSidecarQuarantine(chromePath, qpdfPath string) (QuarantineClearance, error) {
	if !quarantineSupported {
		return QuarantineClearance{}, nil
	}
	platform, err := currentSidecarPlatform()
	if err != nil {
		return QuarantineClearance{}, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return QuarantineClearance{}, nil
	}
	return clearSidecarQuarantine(platform,
		SidecarOptions{ExplicitPath: chromePath, ExecutablePath: executable, LookupEnv: os.Getenv},
		QPDFSidecarOptions{ExplicitPath: qpdfPath, ExecutablePath: executable, LookupEnv: os.Getenv},
	)
}

// quarantineTool is the pinned code of one resolved sidecar.
type quarantineTool struct {
	// directory is the sidecar's own directory, the target of the fallback command.
	directory  string
	overridden bool
	files      []pinnedCode
}

// pinnedCode is one executable or library of a resolved sidecar.
type pinnedCode struct {
	path   string
	sha256 string
}

func clearSidecarQuarantine(platform sidecarPlatform, chrome SidecarOptions, qpdf QPDFSidecarOptions) (QuarantineClearance, error) {
	var tools []quarantineTool
	if sidecar, err := resolveSidecarFor(platform, chrome); err == nil {
		tool := quarantineTool{
			directory:  filepath.Dir(sidecar.Path),
			overridden: sidecarOverridden(chrome.ExplicitPath, chrome.LookupEnv, ChromePathEnvironment),
		}
		for _, file := range platform.chromeFiles {
			if file.kind == pinnedExecutable {
				tool.files = append(tool.files, pinnedCode{
					filepath.Join(tool.directory, filepath.FromSlash(file.relative)), file.sha256,
				})
			}
		}
		tools = append(tools, tool)
	}
	if sidecar, err := resolveQPDFSidecarFor(platform, qpdf); err == nil {
		tool := quarantineTool{
			directory:  filepath.Dir(filepath.Dir(sidecar.Path)),
			overridden: sidecarOverridden(qpdf.ExplicitPath, qpdf.LookupEnv, QPDFPathEnvironment),
		}
		for _, component := range platform.qpdfComponents {
			path := sidecar.Path
			if component.relative != "." {
				path = filepath.Clean(filepath.Join(filepath.Dir(sidecar.Path), component.relative))
			}
			tool.files = append(tool.files, pinnedCode{path, component.sha256})
		}
		tools = append(tools, tool)
	}

	var clearance QuarantineClearance
	failure := QuarantineError{}
	var directories []string
	privileged := false
	for _, tool := range tools {
		kept := false
		for _, file := range tool.files {
			cleared, still, needsPrivilege, err := clearPinnedQuarantine(file.path, file.sha256, !tool.overridden)
			if cleared {
				clearance.Files = append(clearance.Files, file.path)
			}
			if !still {
				continue
			}
			kept = true
			privileged = privileged || needsPrivilege
			failure.Files = append(failure.Files, file.path)
			if err != nil && failure.Cause == nil {
				failure.Cause = err
			}
		}
		if kept {
			directories = append(directories, tool.directory)
			failure.Overridden = failure.Overridden || tool.overridden
		}
	}
	if len(failure.Files) == 0 {
		return clearance, nil
	}
	command := "xattr -dr " + quarantineAttribute
	if privileged {
		command = "sudo " + command
	}
	for _, directory := range directories {
		command += " " + shellQuote(directory)
	}
	failure.Command = command
	return clearance, &failure
}

// clearPinnedQuarantine removes the quarantine from the pinned file at path
// when change is set. It acts only on the file it opened, and only when that
// file is still exactly the one verification hashed to expectedSHA256, so a
// file replaced after verification is never released. still reports that the
// file keeps the quarantine; needsPrivilege that the current user cannot
// change its attributes.
func clearPinnedQuarantine(path, expectedSHA256 string, change bool) (cleared, still, needsPrivilege bool, err error) {
	file, err := openPinnedNoFollow(path)
	if err != nil {
		return false, true, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false, true, false, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || !pinnedContentCurrent(path, info, expectedSHA256) {
		return false, true, false, fmt.Errorf("%s changed after verification", path)
	}
	quarantined, err := hasQuarantine(file)
	if err != nil {
		return false, true, attributesNeedPrivilege(info), fmt.Errorf("read the quarantine of %s: %w", path, err)
	}
	if !quarantined {
		return false, false, false, nil
	}
	if !change {
		return false, true, attributesNeedPrivilege(info), nil
	}
	if err := removeQuarantine(file, info); err != nil {
		return false, true, attributesNeedPrivilege(info), fmt.Errorf("%s: %w", path, err)
	}
	return true, false, false, nil
}

// sidecarOverridden reports whether a sidecar is named by flag or environment
// rather than found beside the executable, mirroring the resolvers.
func sidecarOverridden(explicitPath string, lookup func(string) string, variable string) bool {
	if lookup == nil {
		lookup = os.Getenv
	}
	return strings.TrimSpace(explicitPath) != "" || strings.TrimSpace(lookup(variable)) != ""
}

// shellQuote quotes value as one POSIX shell word.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
