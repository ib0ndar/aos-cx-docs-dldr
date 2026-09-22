package pdfgen

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLinuxAMD64PinsMatchOfficialArchives verifies the linux/amd64 platform
// table against extracted official archives. ELF parsing is host independent,
// so this runs on any platform when AOSCX_LINUX_SIDECAR_ROOT points at a
// directory containing chrome-headless-shell-linux64/ and qpdf-linux-x86_64/.
func TestLinuxAMD64PinsMatchOfficialArchives(t *testing.T) {
	root := os.Getenv("AOSCX_LINUX_SIDECAR_ROOT")
	if root == "" {
		t.Skip("AOSCX_LINUX_SIDECAR_ROOT not set")
	}
	platform := linuxAMD64Sidecars
	chrome := filepath.Join(root, platform.chromeDirectory, platform.chromeExecutable)
	sidecar, err := verifySidecar(platform, chrome)
	if err != nil {
		t.Fatalf("Chrome linux64 does not match its pins: %v", err)
	}
	if sidecar.Version != ChromeVersion || sidecar.Revision != ChromeRevision {
		t.Fatalf("unexpected Chrome identity: %+v", sidecar)
	}
	qpdf := filepath.Join(root, platform.qpdfDirectory, "bin", "qpdf")
	q, err := verifyQPDFSidecar(platform, qpdf)
	if err != nil {
		t.Fatalf("qpdf linux-x86_64 does not match its pins or closure: %v", err)
	}
	if q.Version != QPDFVersion || q.BundleSHA256 != platform.qpdfBundleSHA256 {
		t.Fatalf("unexpected qpdf identity: %+v", q)
	}
}

func TestDarwinARM64TableUsesExportedConstants(t *testing.T) {
	p := darwinARM64Sidecars
	if p.chromeExecutableSHA256 != ChromeExecutableSHA256 || p.chromePlatform != ChromePlatform ||
		p.qpdfBundleSHA256 != QPDFRuntimeBundleSHA256 || p.qpdfPlatform != QPDFPlatform {
		t.Fatal("darwin/arm64 table drifted from the exported release constants")
	}
	if len(p.qpdfComponents) == 0 || p.qpdfComponents[0].relative != "." || !p.qpdfComponents[0].executable {
		t.Fatal("darwin/arm64 table must pin the qpdf executable first")
	}
}

func TestEveryPlatformPinsTheQPDFExecutable(t *testing.T) {
	for _, p := range []sidecarPlatform{darwinARM64Sidecars, linuxAMD64Sidecars} {
		found := false
		for _, c := range p.qpdfComponents {
			if c.relative == "." && c.executable && c.sha256 != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s does not pin the qpdf executable", p.name)
		}
		if p.verifyObject == nil || p.chromeExecutableSHA256 == "" || p.chromeArchiveSHA256 == "" {
			t.Fatalf("%s table is incomplete", p.name)
		}
	}
}

// minimalELF64 returns a 64-byte ELF64 little-endian executable header for the
// given e_machine. It has no sections or segments; debug/elf parses it far
// enough to report Class, Data and Machine, which is all the architecture
// check consults.
func minimalELF64(machine elf.Machine) []byte {
	header := make([]byte, 64)
	copy(header, []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)})
	binary.LittleEndian.PutUint16(header[16:], uint16(elf.ET_EXEC))
	binary.LittleEndian.PutUint16(header[18:], uint16(machine))
	binary.LittleEndian.PutUint32(header[20:], uint32(elf.EV_CURRENT))
	binary.LittleEndian.PutUint16(header[52:], 64) // e_ehsize
	return header
}

func writeExecutable(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestELFVerifierEnforcesFormatAndArchitecture runs on every platform: the
// Linux table's object verifier is pure debug/elf parsing and needs no Linux
// host. It proves that the x86-64 gate rejects other architectures and
// non-ELF input, and that a correct-architecture object then fails on its hash
// rather than being accepted.
func TestELFVerifierEnforcesFormatAndArchitecture(t *testing.T) {
	platform := linuxAMD64Sidecars

	aarch64 := writeExecutable(t, "aarch64", minimalELF64(elf.EM_AARCH64))
	if _, err := verifySidecar(platform, aarch64); err == nil || !strings.Contains(err.Error(), "expected x86-64") {
		t.Fatalf("aarch64 ELF was not rejected by the Linux table: %v", err)
	}

	notELF := writeExecutable(t, "not-elf", []byte("#!/bin/sh\necho not chrome\n"))
	if _, err := verifySidecar(platform, notELF); err == nil || !strings.Contains(err.Error(), "not a supported ELF object") {
		t.Fatalf("non-ELF file was not rejected by the Linux table: %v", err)
	}

	x8664 := writeExecutable(t, "x86_64", minimalELF64(elf.EM_X86_64))
	if _, err := verifySidecar(platform, x8664); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("x86-64 ELF with wrong content was not rejected on hash: %v", err)
	}

	// And the two tables do not accept each other's object format.
	if _, err := verifySidecar(darwinARM64Sidecars, x8664); err == nil || !strings.Contains(err.Error(), "Mach-O") {
		t.Fatalf("darwin table accepted an ELF object: %v", err)
	}
}

// TestELFQPDFClosureMismatchIsRejected proves the DT_NEEDED closure check is
// live for the Linux table: an x86-64 object with no imports must be rejected
// for its closure before its hash is even consulted.
func TestELFQPDFClosureMismatchIsRejected(t *testing.T) {
	platform := linuxAMD64Sidecars
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	qpdf := filepath.Join(bin, "qpdf")
	if err := os.WriteFile(qpdf, minimalELF64(elf.EM_X86_64), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := verifyQPDFSidecar(platform, qpdf)
	if err == nil || !strings.Contains(err.Error(), "dependency closure differs") {
		t.Fatalf("qpdf object with empty DT_NEEDED was not rejected on closure: %v", err)
	}
}
