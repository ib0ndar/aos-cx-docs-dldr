package pdfgen

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	ChromeVersion          = "153.0.8010.36"
	ChromeRevision         = "1681091"
	ChromePlatform         = "mac-arm64"
	ChromeArchiveSHA256    = "3b133378fe44a5f9c849df9049763577fbed296ee4d02d1ffb31b9fcabf79850"
	ChromeExecutableSHA256 = "ad3cf5ce958e43a6e3457d6f4d9df0a0c1483fc24ffb36a30530e88d726a82f8"
	ChromeOfficialURL      = "https://storage.googleapis.com/chrome-for-testing-public/153.0.8010.36/mac-arm64/chrome-headless-shell-mac-arm64.zip"
	ChromePathEnvironment  = "AOSCX_DOCS_CHROME_PATH"
	RendererName           = "chrome-for-testing-headless-shell"
	GeneratedProvenance    = "generated from verified archived HTML with pinned Chrome for Testing"
)

type Sidecar struct {
	Path             string
	Version          string
	Revision         string
	ExecutableSHA256 string
}

type SidecarOptions struct {
	ExplicitPath   string
	ExecutablePath string
	LookupEnv      func(string) string
}

func ResolveSidecar(explicitPath string) (Sidecar, error) {
	executable, err := os.Executable()
	if err != nil {
		return Sidecar{}, fmt.Errorf("locate aos-cx-docs-dldr executable: %w", err)
	}
	return resolveSidecar(SidecarOptions{
		ExplicitPath: explicitPath, ExecutablePath: executable, LookupEnv: os.Getenv,
	})
}

func resolveSidecar(options SidecarOptions) (Sidecar, error) {
	platform, err := currentSidecarPlatform()
	if err != nil {
		return Sidecar{}, err
	}
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	var candidates []string
	if strings.TrimSpace(options.ExplicitPath) != "" {
		candidates = append(candidates, options.ExplicitPath)
	} else if value := strings.TrimSpace(lookup(ChromePathEnvironment)); value != "" {
		candidates = append(candidates, value)
	} else {
		executable, err := filepath.Abs(options.ExecutablePath)
		if err != nil {
			return Sidecar{}, err
		}
		dir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(dir, "sidecars", platform.chromeDirectory, platform.chromeExecutable),
			filepath.Clean(filepath.Join(dir, "..", "Resources", platform.chromeDirectory, platform.chromeExecutable)),
		)
	}
	var missing []string
	for _, candidate := range candidates {
		sidecar, err := verifySidecar(platform, candidate)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, candidate)
			continue
		}
		if err != nil {
			return Sidecar{}, err
		}
		return sidecar, nil
	}
	return Sidecar{}, fmt.Errorf(
		"pinned Chrome for Testing sidecar %s (%s) was not found at %s; install the official archive from %s beside aos-cx-docs-dldr or provide --chrome-path/%s; aos-cx-docs-dldr never downloads or searches for a browser",
		ChromeVersion, platform.chromePlatform, strings.Join(missing, " or "), platform.chromeOfficialURL, ChromePathEnvironment,
	)
}

func verifySidecar(platform sidecarPlatform, raw string) (Sidecar, error) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return Sidecar{}, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return Sidecar{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Sidecar{}, fmt.Errorf("Chrome sidecar must not be a symbolic link: %s", absolute)
	}
	if !info.Mode().IsRegular() {
		return Sidecar{}, fmt.Errorf("Chrome sidecar is not a regular file: %s", absolute)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return Sidecar{}, fmt.Errorf("Chrome sidecar is not executable: %s", absolute)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return Sidecar{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return Sidecar{}, fmt.Errorf("Chrome sidecar identity changed while opening: %s", absolute)
	}
	if _, err := platform.verifyObject(file, absolute); err != nil {
		return Sidecar{}, fmt.Errorf("Chrome sidecar: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Sidecar{}, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return Sidecar{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != platform.chromeExecutableSHA256 {
		return Sidecar{}, fmt.Errorf(
			"Chrome sidecar SHA-256 is %s, expected %s for Chrome for Testing %s %s; refusing to launch",
			digest, platform.chromeExecutableSHA256, ChromeVersion, platform.chromePlatform,
		)
	}
	current, err := os.Lstat(absolute)
	if err != nil || !os.SameFile(info, current) {
		return Sidecar{}, fmt.Errorf("Chrome sidecar identity changed during verification: %s", absolute)
	}
	return Sidecar{Path: absolute, Version: ChromeVersion, Revision: ChromeRevision, ExecutableSHA256: digest}, nil
}
