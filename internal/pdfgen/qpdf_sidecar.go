package pdfgen

import (
	"errors"
	"fmt"
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
	if err := verifyQPDFSearchDirectories(platform, absolute); err != nil {
		return QPDFSidecar{}, err
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
	if pinnedContentCurrent(path, info, expectedHash) {
		return nil
	}
	digest, stable, err := hashPinnedFile("qpdf sidecar component", path, info, func(file *os.File) error {
		imports, err := platform.verifyObject(file, path)
		if err != nil {
			return fmt.Errorf("qpdf sidecar: %w", err)
		}
		imports = slices.Clone(imports)
		expected := slices.Clone(expectedImports)
		slices.Sort(imports)
		slices.Sort(expected)
		if !slices.Equal(imports, expected) {
			return fmt.Errorf("qpdf sidecar component dependency closure differs at %s: %v", path, imports)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if digest != expectedHash {
		return fmt.Errorf("qpdf sidecar component SHA-256 is %s, expected %s: %s", digest, expectedHash, path)
	}
	rememberPinnedContent(path, stable, digest)
	return nil
}

// verifyQPDFSearchDirectories refuses any entry beside the pinned components
// in the directories that hold them. Those directories are the loader's search
// path: the Linux libraries carry RUNPATH $ORIGIN, so a stray libz.so.1 or
// libgmp.so.10 there would be loaded in place of the system copy. A bounded
// Finder .DS_Store is tolerated where the platform has Finder.
func verifyQPDFSearchDirectories(platform sidecarPlatform, executable string) error {
	pinned := map[string]map[string]bool{}
	for _, component := range platform.qpdfComponents {
		path := executable
		if component.relative != "." {
			path = filepath.Clean(filepath.Join(filepath.Dir(executable), component.relative))
		}
		dir := filepath.Dir(path)
		if pinned[dir] == nil {
			pinned[dir] = map[string]bool{}
		}
		pinned[dir][filepath.Base(path)] = true
	}
	directories := make([]string, 0, len(pinned))
	for dir := range pinned {
		directories = append(directories, dir)
	}
	slices.Sort(directories)
	for _, dir := range directories {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("qpdf sidecar directory cannot be listed: %s: %v", dir, err)
		}
		for _, entry := range entries {
			if pinned[dir][entry.Name()] {
				continue
			}
			if platform.finderMetadata && entry.Name() == ".DS_Store" && entry.Type().IsRegular() {
				if info, err := entry.Info(); err == nil && info.Mode().IsRegular() &&
					info.Size() <= finderMetadataMaxBytes {
					continue
				}
			}
			return fmt.Errorf(
				"qpdf sidecar directory contains an unpinned entry on the library search path: %s",
				filepath.Join(dir, entry.Name()),
			)
		}
	}
	return nil
}
