package releasepkg

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"aos-cx-docs-dldr/internal/pdfgen"
)

const (
	QPDFVersion              = "12.4.1"
	JPEGVersion              = "3.2.0"
	OpenSSLVersion           = "3.6.4"
	QPDFLibraryInputSHA256   = "8b777a86a01396b76f4fe28525b15ca9c361542afc7632cbce6e203ce215cac0"
	JPEGLibraryInputSHA256   = "e7352232bc8b626daf5cc8d589cf894ced4c5c17f9298b8363cd98989afa5771"
	CryptoLibraryInputSHA256 = "cc4f0e5544506e09c5835d3a4151c4ed1685e64a61a0519ece1e1baf3aa29525"
)

var requiredSidecarLicenses = map[string][]string{
	"chrome": {
		"ABOUT",
		"LICENSE.headless_shell",
	},
	"qpdf": {
		"licenses/jpeg-turbo/LICENSE.md",
		"licenses/openssl/LICENSE.txt",
		"licenses/qpdf/LICENSE.txt",
		"licenses/qpdf/NOTICE.md",
	},
}

func StageTree(source, destination string) error {
	source, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("refusing to replace existing sidecar destination: %s", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(destination)
		}
	}()
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		clean, err := cleanArchivePath(relative)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("sidecar source contains symbolic link: %s", clean)
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if entry.IsDir() {
			return os.Mkdir(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("sidecar source contains special file: %s", clean)
		}
		return copyRegularFile(path, target, info)
	})
	if err != nil {
		return err
	}
	success = true
	return nil
}

func copyRegularFile(source, destination string, sourceInfo fs.FileInfo) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(sourceInfo, opened) {
		return fmt.Errorf("sidecar source identity changed while opening: %s", source)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sourceInfo.Mode().Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := firstError(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	current, err := os.Lstat(source)
	if err != nil || !os.SameFile(sourceInfo, current) {
		return fmt.Errorf("sidecar source identity changed while copying: %s", source)
	}
	return nil
}

func VerifySidecars(chromeDir, qpdfDir string) ([]SidecarIdentity, error) {
	for kind, root := range map[string]string{"chrome": chromeDir, "qpdf": qpdfDir} {
		for _, relative := range requiredSidecarLicenses[kind] {
			path := filepath.Join(root, filepath.FromSlash(relative))
			info, err := os.Lstat(path)
			if err != nil {
				return nil, fmt.Errorf("%s required license evidence %s: %w", kind, relative, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
				return nil, fmt.Errorf("%s required license evidence is not a nonempty regular file: %s", kind, relative)
			}
		}
	}
	chrome, err := pdfgen.ResolveSidecar(filepath.Join(chromeDir, "chrome-headless-shell"))
	if err != nil {
		return nil, err
	}
	qpdf, err := pdfgen.ResolveQPDFSidecar(filepath.Join(qpdfDir, "bin", "qpdf"))
	if err != nil {
		return nil, err
	}
	chromeComponent, err := fileIdentity(chromeDir, chrome.Path)
	if err != nil {
		return nil, err
	}
	qpdfComponents := make([]FileIdentity, 0, 4)
	for _, relative := range []string{
		"bin/qpdf",
		"lib/libcrypto.3.dylib",
		"lib/libjpeg.8.dylib",
		"lib/libqpdf.30.dylib",
	} {
		identity, err := fileIdentity(qpdfDir, filepath.Join(qpdfDir, filepath.FromSlash(relative)))
		if err != nil {
			return nil, err
		}
		qpdfComponents = append(qpdfComponents, identity)
	}
	sort.Slice(qpdfComponents, func(i, j int) bool { return qpdfComponents[i].Path < qpdfComponents[j].Path })
	return []SidecarIdentity{
		{
			Name:       pdfgen.RendererName,
			Version:    chrome.Version,
			Revision:   chrome.Revision,
			Platform:   pdfgen.ChromePlatform,
			Components: []FileIdentity{chromeComponent},
		},
		{
			Name:       pdfgen.QPDFPostprocessorName,
			Version:    qpdf.Version,
			Platform:   pdfgen.QPDFPlatform,
			Components: qpdfComponents,
		},
	}, nil
}

func Pin(name string) (string, error) {
	pins := map[string]string{
		"chrome-version":              pdfgen.ChromeVersion,
		"chrome-revision":             pdfgen.ChromeRevision,
		"chrome-platform":             pdfgen.ChromePlatform,
		"chrome-archive-sha256":       pdfgen.ChromeArchiveSHA256,
		"chrome-executable-sha256":    pdfgen.ChromeExecutableSHA256,
		"chrome-url":                  pdfgen.ChromeOfficialURL,
		"qpdf-version":                QPDFVersion,
		"jpeg-version":                JPEGVersion,
		"openssl-version":             OpenSSLVersion,
		"qpdf-bottle-sha256":          pdfgen.QPDFBottleSHA256,
		"jpeg-bottle-sha256":          pdfgen.QPDFJPEGBottleSHA256,
		"openssl-bottle-sha256":       pdfgen.QPDFOpenSSLBottleSHA256,
		"qpdf-url":                    pdfgen.QPDFBottleURL,
		"jpeg-url":                    pdfgen.QPDFJPEGBottleURL,
		"openssl-url":                 pdfgen.QPDFOpenSSLBottleURL,
		"qpdf-executable-sha256":      pdfgen.QPDFExecutableSHA256,
		"qpdf-library-input-sha256":   QPDFLibraryInputSHA256,
		"jpeg-library-input-sha256":   JPEGLibraryInputSHA256,
		"crypto-library-input-sha256": CryptoLibraryInputSHA256,
		"qpdf-library-sha256":         pdfgen.QPDFLibrarySHA256,
		"jpeg-library-sha256":         pdfgen.QPDFJPEGLibrarySHA256,
		"crypto-library-sha256":       pdfgen.QPDFCryptoLibrarySHA256,
	}
	value, ok := pins[name]
	if !ok {
		return "", fmt.Errorf("unknown sidecar pin %q", name)
	}
	return value, nil
}
