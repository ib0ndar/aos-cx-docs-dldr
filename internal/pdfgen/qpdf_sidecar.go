package pdfgen

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	QPDFVersion                    = "12.4.1"
	QPDFPlatform                   = "mac-arm64"
	QPDFPathEnvironment            = "AOSCX_DOCS_QPDF_PATH"
	QPDFExecutableSHA256           = "0326859206213694229c4b0917cc0eedf11d023dc5b1aa517eeab7ba3e94aec4"
	QPDFLibrarySHA256              = "56ee3ac586187e129aaaacaa4e458cc8bedc98811bd961f73196d03be442fd8a"
	QPDFJPEGLibrarySHA256          = "cbdd66791e24c27c483587aed048a0c6270ff7ce9f6c8a83a010e90dd63af498"
	QPDFCryptoLibrarySHA256        = "f9563883fdbb4f2b367c708a6abd786119c935d3f9cd01fa8e11c34666775d1b"
	QPDFRuntimeBundleSHA256        = "751e59fcecb6d3cd2fce2d5379a477c799065dbf4343b07f049a1ed48f93a15c"
	QPDFBottleSHA256               = "7e3e764df933760c100b2bd5d7177ebd0511733685e647e57d9e90afa01427c5"
	QPDFJPEGBottleSHA256           = "02539b0736cfacdc6c4bb6a7d274d0c5c8b6e1faf9b5bab1e155168961d288aa"
	QPDFOpenSSLBottleSHA256        = "8c7dab98311d025fda379dba80eaf985af1ceca26c403c66318b3c7d43a028b9"
	QPDFBottleURL                  = "https://ghcr.io/v2/homebrew/core/qpdf/blobs/sha256:7e3e764df933760c100b2bd5d7177ebd0511733685e647e57d9e90afa01427c5"
	QPDFJPEGBottleURL              = "https://ghcr.io/v2/homebrew/core/jpeg-turbo/blobs/sha256:02539b0736cfacdc6c4bb6a7d274d0c5c8b6e1faf9b5bab1e155168961d288aa"
	QPDFOpenSSLBottleURL           = "https://ghcr.io/v2/homebrew/core/openssl/3/blobs/sha256:8c7dab98311d025fda379dba80eaf985af1ceca26c403c66318b3c7d43a028b9"
	QPDFPostprocessorName          = "qpdf"
	QPDFOutlinePlanSchemaVersion   = 2
	QPDFOutlineValidationMaxBytes  = int64(512 << 20)
	QPDFOutlineProcessMaxLogBytes  = 1 << 20
	QPDFOutlineProcessMaxRSSBytes  = int64(2 << 30)
	QPDFOutlineProcessMaxNodeCount = 100_003
	QPDFOutlineProcessMaxDepth     = 1_026
)

type QPDFSidecar struct {
	Path             string
	Version          string
	ExecutableSHA256 string
	BundleSHA256     string
}

type QPDFSidecarOptions struct {
	ExplicitPath   string
	ExecutablePath string
	LookupEnv      func(string) string
}

func ResolveQPDFSidecar(explicitPath string) (QPDFSidecar, error) {
	executable, err := os.Executable()
	if err != nil {
		return QPDFSidecar{}, fmt.Errorf("locate aos-cx-docs-dldr executable: %w", err)
	}
	return resolveQPDFSidecar(QPDFSidecarOptions{
		ExplicitPath: explicitPath, ExecutablePath: executable, LookupEnv: os.Getenv,
	})
}

func resolveQPDFSidecar(options QPDFSidecarOptions) (QPDFSidecar, error) {
	platform, err := currentSidecarPlatform()
	if err != nil {
		return QPDFSidecar{}, err
	}
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.Getenv
	}
	var candidates []string
	if strings.TrimSpace(options.ExplicitPath) != "" {
		candidates = append(candidates, options.ExplicitPath)
	} else if value := strings.TrimSpace(lookup(QPDFPathEnvironment)); value != "" {
		candidates = append(candidates, value)
	} else {
		executable, err := filepath.Abs(options.ExecutablePath)
		if err != nil {
			return QPDFSidecar{}, err
		}
		dir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(dir, "sidecars", platform.qpdfDirectory, "bin", "qpdf"),
			filepath.Clean(filepath.Join(dir, "..", "Resources", platform.qpdfDirectory, "bin", "qpdf")),
		)
	}
	var missing []string
	for _, candidate := range candidates {
		sidecar, err := verifyQPDFSidecar(platform, candidate)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, candidate)
			continue
		}
		if err != nil {
			return QPDFSidecar{}, err
		}
		return sidecar, nil
	}
	return QPDFSidecar{}, fmt.Errorf(
		"pinned qpdf sidecar %s (%s) was not found at %s; %s or provide --qpdf-path/%s; aos-cx-docs-dldr never downloads or searches PATH",
		QPDFVersion, platform.qpdfPlatform, strings.Join(missing, " or "), platform.qpdfAcquireAdvice, QPDFPathEnvironment,
	)
}

func verifyQPDFSidecar(platform sidecarPlatform, raw string) (QPDFSidecar, error) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return QPDFSidecar{}, err
	}
	executableSHA256 := ""
	for _, component := range platform.qpdfComponents {
		path := absolute
		if component.relative != "." {
			path = filepath.Clean(filepath.Join(filepath.Dir(absolute), component.relative))
		}
		if err := verifyQPDFComponent(platform, path, component.sha256, component.executable, component.imports); err != nil {
			return QPDFSidecar{}, err
		}
		if component.relative == "." {
			executableSHA256 = component.sha256
		}
	}
	if executableSHA256 == "" {
		return QPDFSidecar{}, errors.New("qpdf sidecar platform table does not pin the executable")
	}
	return QPDFSidecar{
		Path:             absolute,
		Version:          QPDFVersion,
		ExecutableSHA256: executableSHA256,
		BundleSHA256:     platform.qpdfBundleSHA256,
	}, nil
}

func verifyQPDFComponent(platform sidecarPlatform, path, expectedHash string, executable bool, expectedImports []string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("qpdf sidecar component must not be a symbolic link: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("qpdf sidecar component is not a regular file: %s", path)
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("qpdf sidecar executable is not executable: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("qpdf sidecar component identity changed while opening: %s", path)
	}
	imports, err := platform.verifyObject(file, path)
	if err != nil {
		return fmt.Errorf("qpdf sidecar: %w", err)
	}
	imports = slices.Clone(imports)
	expectedImports = slices.Clone(expectedImports)
	slices.Sort(imports)
	slices.Sort(expectedImports)
	if !slices.Equal(imports, expectedImports) {
		return fmt.Errorf("qpdf sidecar component dependency closure differs at %s: %v", path, imports)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != expectedHash {
		return fmt.Errorf("qpdf sidecar component SHA-256 is %s, expected %s: %s", digest, expectedHash, path)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return fmt.Errorf("qpdf sidecar component identity changed during verification: %s", path)
	}
	return nil
}
