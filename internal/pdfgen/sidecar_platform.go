package pdfgen

import (
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"os"
	"runtime"
)

// Generated-PDF conversion runs on exactly two platforms, each with its own
// pinned Chrome for Testing headless shell and qpdf runtime closure:
//
//   - darwin/arm64: the native macOS bundle
//   - linux/amd64:  the container image
//
// Both use the same Chrome version and revision and the same qpdf version.
// The archives, executables and shared-library closures differ per platform
// and are pinned separately. Nothing is downloaded at runtime, PATH is never
// searched, and there is no fallback between platforms.

// sidecarComponent is one file of a sidecar bundle with its pinned identity.
type sidecarComponent struct {
	// relative is the path relative to the directory holding the qpdf
	// executable, using "." for the executable itself.
	relative   string
	sha256     string
	executable bool
	// imports is the exact dynamic-library closure the object must declare.
	// Mach-O reports install names; ELF reports DT_NEEDED sonames.
	imports []string
}

// sidecarPlatform holds every platform-specific pin and layout fact.
type sidecarPlatform struct {
	name string

	chromeDirectory        string
	chromeExecutable       string
	chromePlatform         string
	chromeArchiveSHA256    string
	chromeExecutableSHA256 string
	chromeOfficialURL      string

	qpdfDirectory     string
	qpdfPlatform      string
	qpdfBundleSHA256  string
	qpdfComponents    []sidecarComponent
	qpdfAcquireAdvice string

	// verifyObject confirms the file at path is an executable object of this
	// platform's format and architecture and returns its declared imports.
	verifyObject func(file *os.File, path string) ([]string, error)
}

var darwinARM64Sidecars = sidecarPlatform{
	name: "macOS arm64",

	chromeDirectory:        "chrome-headless-shell-mac-arm64",
	chromeExecutable:       "chrome-headless-shell",
	chromePlatform:         ChromePlatform,
	chromeArchiveSHA256:    ChromeArchiveSHA256,
	chromeExecutableSHA256: ChromeExecutableSHA256,
	chromeOfficialURL:      ChromeOfficialURL,

	qpdfDirectory:     "qpdf-mac-arm64",
	qpdfPlatform:      QPDFPlatform,
	qpdfBundleSHA256:  QPDFRuntimeBundleSHA256,
	qpdfAcquireAdvice: "stage the checksum-recorded Homebrew bottle bundle beside aos-cx-docs-dldr",
	qpdfComponents: []sidecarComponent{
		{".", QPDFExecutableSHA256, true, []string{
			"@rpath/libqpdf.30.dylib", "/usr/lib/libc++.1.dylib", "/usr/lib/libSystem.B.dylib",
		}},
		{"../lib/libqpdf.30.dylib", QPDFLibrarySHA256, false, []string{
			"/usr/lib/libz.1.dylib", "@loader_path/libjpeg.8.dylib",
			"@loader_path/libcrypto.3.dylib", "/usr/lib/libc++.1.dylib", "/usr/lib/libSystem.B.dylib",
		}},
		{"../lib/libjpeg.8.dylib", QPDFJPEGLibrarySHA256, false, []string{"/usr/lib/libSystem.B.dylib"}},
		{"../lib/libcrypto.3.dylib", QPDFCryptoLibrarySHA256, false, []string{"/usr/lib/libSystem.B.dylib"}},
	},

	verifyObject: verifyMachOARM64,
}

// Linux pins were recorded on 2026-09-19 from the official archives:
//
//	https://storage.googleapis.com/chrome-for-testing-public/153.0.8010.36/linux64/chrome-headless-shell-linux64.zip
//	https://github.com/qpdf/qpdf/releases/download/v12.4.1/qpdf-12.4.1-bin-linux-x86_64.zip
//
// The qpdf archive is self-contained apart from libc, libstdc++, libgcc_s,
// libz.so.1 and libgmp.so.10, which the container image supplies from Debian,
// exactly as macOS supplies libSystem and /usr/lib/libz.1.dylib.
var linuxAMD64Sidecars = sidecarPlatform{
	name: "Linux amd64",

	chromeDirectory:        "chrome-headless-shell-linux64",
	chromeExecutable:       "chrome-headless-shell",
	chromePlatform:         "linux64",
	chromeArchiveSHA256:    "a0079df5617da34bcd1debad18196568b072ec3d8ec57944008972a4fc970580",
	chromeExecutableSHA256: "dabfdd70006e411b521882d3b11d667f2c1323ff4f4ec8da7d043dbc8672bf39",
	chromeOfficialURL:      "https://storage.googleapis.com/chrome-for-testing-public/153.0.8010.36/linux64/chrome-headless-shell-linux64.zip",

	qpdfDirectory:     "qpdf-linux-x86_64",
	qpdfPlatform:      "linux-x86_64",
	qpdfBundleSHA256:  "db9122e88ec00c76ac6a14e09ffb92406db1773d47b968911ff6e69f28c09bf9",
	qpdfAcquireAdvice: "stage the checksum-recorded upstream qpdf linux-x86_64 bundle beside aos-cx-docs-dldr",
	qpdfComponents: []sidecarComponent{
		{".", "9ac787a28597e8428289a12ba3fedafd74bdfb4b4da1be814722faf76f14f21b", true, []string{
			"libc.so.6", "libgcc_s.so.1", "libqpdf.so.30", "libstdc++.so.6",
		}},
		{"../lib/libqpdf.so.30", "520c101f3aff149014d552381fb148967b2fb723e885d9050e83bfe4c023f487", false, []string{
			"libc.so.6", "libgcc_s.so.1", "libgnutls.so.30", "libjpeg.so.8", "libstdc++.so.6", "libz.so.1",
		}},
		{"../lib/libgnutls.so.30", "1333e5627c3e0c9c67079abf8f46df1e9369e4d6aed800723e852b657467fbb9", false, []string{
			"ld-linux-x86-64.so.2", "libc.so.6", "libgmp.so.10", "libhogweed.so.6", "libidn2.so.0",
			"libnettle.so.8", "libp11-kit.so.0", "libtasn1.so.6", "libunistring.so.2",
		}},
		{"../lib/libhogweed.so.6", "5cf6f6da565d6f8132918f7c9b3558239e4193ea2299eb53082d92458f56b42e", false, []string{
			"libc.so.6", "libgmp.so.10", "libnettle.so.8",
		}},
		{"../lib/libnettle.so.8", "250d19ee04109927927fbbe8a3be63572cd242481dc6cb7106279461244ccf33", false, []string{"libc.so.6"}},
		{"../lib/libidn2.so.0", "a8e6f7c0d5770294830db63b7dc8fd005362e20b109c74ee600689d5f8317324", false, []string{
			"libc.so.6", "libunistring.so.2",
		}},
		{"../lib/libunistring.so.2", "a821050079d149d6f0a0e58fd90c27eba52f259da80075c6b96c965af0b4a224", false, []string{"libc.so.6"}},
		{"../lib/libp11-kit.so.0", "6f908a6c1cc40d0da1df4f921acedc39cab2a84e92146cbbecfeb58354f0ddc3", false, []string{
			"ld-linux-x86-64.so.2", "libc.so.6", "libffi.so.8",
		}},
		{"../lib/libffi.so.8", "1ae3d582f75ac136e9ff223acf7e1a1c6fcea7dfd792f4b3b401888082e47ec8", false, []string{"libc.so.6"}},
		{"../lib/libtasn1.so.6", "28a1d44689c3e88c0b8004d97387b75880e8d75e9d7ea8c968b29a3653caec52", false, []string{"libc.so.6"}},
		{"../lib/libjpeg.so.8", "21661bf728676700a61be235403cedc0c6f61247b45baa685904414a5ecd8f69", false, []string{"libc.so.6"}},
	},

	verifyObject: verifyELFX8664,
}

// currentSidecarPlatform returns the pins for the running platform, or an
// error naming the platforms that do have pinned sidecars.
func currentSidecarPlatform() (sidecarPlatform, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return darwinARM64Sidecars, nil
	case "linux/amd64":
		return linuxAMD64Sidecars, nil
	}
	return sidecarPlatform{}, fmt.Errorf(
		"generated PDF conversion is supported only on macOS arm64 and Linux amd64, not %s/%s",
		runtime.GOOS, runtime.GOARCH,
	)
}

func verifyMachOARM64(file *os.File, path string) ([]string, error) {
	object, err := macho.NewFile(file)
	if err != nil {
		return nil, fmt.Errorf("sidecar component is not a supported Mach-O object: %s: %w", path, err)
	}
	if object.Cpu != macho.CpuArm64 {
		_ = object.Close()
		return nil, fmt.Errorf("sidecar component architecture is %s, expected arm64: %s", object.Cpu, path)
	}
	imports, err := object.ImportedLibraries()
	if closeErr := object.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return imports, err
}

func verifyELFX8664(file *os.File, path string) ([]string, error) {
	object, err := elf.NewFile(file)
	if err != nil {
		return nil, fmt.Errorf("sidecar component is not a supported ELF object: %s: %w", path, err)
	}
	if object.Machine != elf.EM_X86_64 {
		_ = object.Close()
		return nil, fmt.Errorf("sidecar component architecture is %s, expected x86-64: %s", object.Machine, path)
	}
	imports, err := object.DynString(elf.DT_NEEDED)
	if closeErr := object.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	return imports, err
}

// reverifySidecar re-checks an already resolved Chrome sidecar immediately
// before launch, so a file swapped between resolution and execution is
// refused. It resolves the platform table itself for the running host.
func reverifySidecar(path string) (Sidecar, error) {
	platform, err := currentSidecarPlatform()
	if err != nil {
		return Sidecar{}, err
	}
	return verifySidecar(platform, path)
}

// reverifyQPDFSidecar is the qpdf counterpart of reverifySidecar.
func reverifyQPDFSidecar(path string) (QPDFSidecar, error) {
	platform, err := currentSidecarPlatform()
	if err != nil {
		return QPDFSidecar{}, err
	}
	return verifyQPDFSidecar(platform, path)
}

// PlatformPins are the sidecar identities a complete generated-PDF record must
// carry on the running platform. Libraries record the pins of the platform that
// produced them, so a library is validated only by the platform that made it,
// which matches the existing rule that libraries are not portable across
// operating systems.
type PlatformPins struct {
	ChromeExecutableSHA256 string
	QPDFExecutableSHA256   string
	QPDFBundleSHA256       string
}

// CurrentPlatformPins returns the pins for the running platform, or an error
// on platforms without pinned sidecars.
func CurrentPlatformPins() (PlatformPins, error) {
	platform, err := currentSidecarPlatform()
	if err != nil {
		return PlatformPins{}, err
	}
	pins := PlatformPins{
		ChromeExecutableSHA256: platform.chromeExecutableSHA256,
		QPDFBundleSHA256:       platform.qpdfBundleSHA256,
	}
	for _, component := range platform.qpdfComponents {
		if component.relative == "." {
			pins.QPDFExecutableSHA256 = component.sha256
		}
	}
	return pins, nil
}
