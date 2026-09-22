package releasepkg

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testArchiveMember struct {
	name     string
	body     string
	mode     os.FileMode
	isDir    bool
	tarType  byte
	format   tar.Format
	linkname string
}

func TestExtractArchiveAcceptsBoundedZIPAndTarGZ(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input."+strings.ReplaceAll(format, ".", ""))
			members := []testArchiveMember{
				{name: "tool/", mode: 0o755, isDir: true},
				{name: "tool/bin/", mode: 0o755, isDir: true},
				{name: "tool/bin/run", body: "payload", mode: 0o755},
				{name: "tool/LICENSE", body: "license", mode: 0o644},
			}
			writeTestArchive(t, archivePath, format, members)
			destination := filepath.Join(root, "extracted")
			if err := ExtractArchive(context.Background(), extractTestOptions(t, archivePath, destination, format)); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(destination, "tool", "bin", "run"))
			if err != nil || string(data) != "payload" {
				t.Fatalf("unexpected extracted payload %q: %v", data, err)
			}
			info, err := os.Stat(filepath.Join(destination, "tool", "bin", "run"))
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Fatalf("unexpected extracted mode %v: %v", info.Mode(), err)
			}
			rootInfo, err := os.Stat(destination)
			if err != nil || rootInfo.Mode().Perm() != 0o700 {
				t.Fatalf("extraction root mode=%v err=%v", rootInfo.Mode(), err)
			}
		})
	}
}

func TestSelectiveExtractionAcceptsInertTarLinks(t *testing.T) {
	for _, linkType := range []byte{tar.TypeSymlink, tar.TypeLink} {
		t.Run(string([]byte{linkType}), func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
				{name: "tool/payload", body: "selected", mode: 0o755},
				{name: "tool/inert", mode: 0o777, tarType: linkType, linkname: "tool/payload"},
			})
			options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
			options.Selections = []ExtractSelection{{
				ArchivePath: "tool/payload",
				OutputPath:  "bin/renamed",
			}}
			if err := ExtractArchive(context.Background(), options); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(options.Destination, "bin", "renamed"))
			if err != nil || string(data) != "selected" {
				t.Fatalf("selected data=%q err=%v", data, err)
			}
			assertPathAbsent(t, filepath.Join(options.Destination, "tool"))
		})
	}
}

func TestSelectiveExtractionAcceptsInertZIPSymlink(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.zip")
	writeTestArchive(t, archivePath, "zip", []testArchiveMember{
		{name: "tool/payload", body: "selected", mode: 0o644},
		{name: "tool/inert", body: "tool/payload", mode: os.ModeSymlink | 0o777},
	})
	options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "zip")
	options.Selections = []ExtractSelection{{
		ArchivePath: "tool/payload",
		OutputPath:  "selected",
	}}
	if err := ExtractArchive(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	assertPathAbsent(t, filepath.Join(options.Destination, "tool"))
}

func TestSelectiveExtractionRejectsSelectedLinksAndUnsafeInertLinks(t *testing.T) {
	tests := []struct {
		name       string
		memberName string
		linkname   string
		selected   string
		want       string
	}{
		{name: "selected-link", memberName: "tool/link", linkname: "tool/file", selected: "tool/link", want: "not a regular file"},
		{name: "unsafe-name", memberName: "tool/../link", linkname: "tool/file", selected: "tool/file", want: "noncanonical"},
		{name: "absolute-target", memberName: "tool/link", linkname: "/tool/file", selected: "tool/file", want: "link metadata"},
		{name: "traversing-target", memberName: "tool/link", linkname: "../file", selected: "tool/file", want: "link metadata"},
		{name: "backslash-target", memberName: "tool/link", linkname: `tool\file`, selected: "tool/file", want: "link metadata"},
		{name: "control-target", memberName: "tool/link", linkname: "tool/\nfile", selected: "tool/file", want: "link metadata"},
		{name: "format-target", memberName: "tool/link", linkname: "tool/\u202Efile", selected: "tool/file", want: "unsafe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
				{name: "tool/file", body: "selected", mode: 0o644},
				{
					name: test.memberName, mode: 0o777, tarType: tar.TypeSymlink,
					linkname: test.linkname, format: tar.FormatPAX,
				},
			})
			options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
			options.Selections = []ExtractSelection{{ArchivePath: test.selected, OutputPath: "selected"}}
			err := ExtractArchive(context.Background(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe selective archive accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, options.Destination)
		})
	}
}

func TestSelectiveExtractionRejectsLinkCollisionsWithSelectedMembers(t *testing.T) {
	for _, members := range [][]testArchiveMember{
		{
			{name: "tool/link", mode: 0o777, tarType: tar.TypeSymlink, linkname: "tool/target"},
			{name: "tool/link/child", body: "selected", mode: 0o644},
		},
		{
			{name: "tool/BIN", mode: 0o777, tarType: tar.TypeSymlink, linkname: "tool/target"},
			{name: "tool/bin/child", body: "selected", mode: 0o644},
		},
	} {
		root := t.TempDir()
		archivePath := filepath.Join(root, "input.tar.gz")
		writeTestArchive(t, archivePath, "tar.gz", members)
		options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
		options.Selections = []ExtractSelection{{ArchivePath: members[1].name, OutputPath: "selected"}}
		if err := ExtractArchive(context.Background(), options); err == nil ||
			(!strings.Contains(err.Error(), "collision") && !strings.Contains(err.Error(), "case-fold")) {
			t.Fatalf("selected/link collision accepted or wrong error: %v", err)
		}
		assertPathAbsent(t, options.Destination)
	}
}

func TestSelectiveExtractionRequiresUniqueRegularSelections(t *testing.T) {
	tests := []struct {
		name       string
		selections []ExtractSelection
		want       string
	}{
		{
			name: "missing",
			selections: []ExtractSelection{
				{ArchivePath: "tool/missing", OutputPath: "missing"},
			},
			want: "missing",
		},
		{
			name: "duplicate",
			selections: []ExtractSelection{
				{ArchivePath: "tool/file", OutputPath: "one"},
				{ArchivePath: "tool/file", OutputPath: "two"},
			},
			want: "duplicate selected archive",
		},
		{
			name: "directory",
			selections: []ExtractSelection{
				{ArchivePath: "tool/dir", OutputPath: "directory"},
			},
			want: "not a regular file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
				{name: "tool/dir/", mode: 0o755, isDir: true},
				{name: "tool/file", body: "selected", mode: 0o644},
			})
			options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
			options.Selections = test.selections
			err := ExtractArchive(context.Background(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid selection accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, options.Destination)
		})
	}
}

func TestSelectiveExtractionRejectsDuplicateSelectedArchiveMember(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.tar.gz")
	writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
		{name: "tool/file", body: "first", mode: 0o644},
		{name: "tool/file", body: "second", mode: 0o644},
	})
	options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
	options.Selections = []ExtractSelection{{ArchivePath: "tool/file", OutputPath: "selected"}}
	if err := ExtractArchive(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "duplicate archive member") {
		t.Fatalf("duplicate selected archive member accepted or wrong error: %v", err)
	}
	assertPathAbsent(t, options.Destination)
}

func TestSelectiveExtractionRejectsUnsafeAndCollidingOutputMappings(t *testing.T) {
	tests := []struct {
		name    string
		outputs []string
		want    string
	}{
		{name: "absolute", outputs: []string{"/output", "other"}, want: "invalid selected output"},
		{name: "traversal", outputs: []string{"../output", "other"}, want: "invalid selected output"},
		{name: "backslash", outputs: []string{`bad\output`, "other"}, want: "invalid selected output"},
		{name: "duplicate", outputs: []string{"same", "same"}, want: "duplicate"},
		{name: "prefix", outputs: []string{"path", "path/child"}, want: "prefix collision"},
		{name: "case-fold", outputs: []string{"Readme", "README"}, want: "case-fold"},
		{name: "nfc", outputs: []string{"\u00E9", "e\u0301"}, want: "normalization"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
				{name: "tool/one", body: "1", mode: 0o644},
				{name: "tool/two", body: "2", mode: 0o644},
			})
			options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
			options.Selections = []ExtractSelection{
				{ArchivePath: "tool/one", OutputPath: test.outputs[0]},
				{ArchivePath: "tool/two", OutputPath: test.outputs[1]},
			}
			err := ExtractArchive(context.Background(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe output mapping accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, options.Destination)
		})
	}
}

func TestSelectiveExtractionAccountsUnselectedRegularBytes(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.tar.gz")
	writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
		{name: "tool/selected", body: "x", mode: 0o644},
		{name: "tool/unselected", body: "12345", mode: 0o644},
	})
	options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "tar.gz")
	options.Selections = []ExtractSelection{{ArchivePath: "tool/selected", OutputPath: "selected"}}
	options.Limits = DefaultExtractionLimits
	options.Limits.MaxTotalBytes = 5
	if err := ExtractArchive(context.Background(), options); err == nil ||
		!strings.Contains(err.Error(), "aggregate") {
		t.Fatalf("unselected regular bytes were not accounted: %v", err)
	}
	assertPathAbsent(t, options.Destination)
}

func TestExtractArchiveRejectsUnsafeZIPMembers(t *testing.T) {
	tests := []struct {
		name    string
		members []testArchiveMember
		want    string
	}{
		{name: "absolute", members: []testArchiveMember{{name: "/tool/file", body: "x", mode: 0o644}}, want: "unsafe"},
		{name: "traversal", members: []testArchiveMember{{name: "tool/../file", body: "x", mode: 0o644}}, want: "noncanonical"},
		{name: "backslash", members: []testArchiveMember{{name: `tool\file`, body: "x", mode: 0o644}}, want: "unsafe"},
		{name: "control", members: []testArchiveMember{{name: "tool/a\nb", body: "x", mode: 0o644}}, want: "unsafe"},
		{name: "format-control", members: []testArchiveMember{{name: "tool/a\u202Eb", body: "x", mode: 0o644}}, want: "unsafe"},
		{name: "wrong-root", members: []testArchiveMember{{name: "other/file", body: "x", mode: 0o644}}, want: "allowlisted"},
		{name: "root-file", members: []testArchiveMember{{name: "tool", body: "x", mode: 0o644}}, want: "must be a directory"},
		{name: "duplicate", members: []testArchiveMember{
			{name: "tool/file", body: "x", mode: 0o644},
			{name: "tool/file", body: "y", mode: 0o644},
		}, want: "duplicate"},
		{name: "prefix", members: []testArchiveMember{
			{name: "tool/file", body: "x", mode: 0o644},
			{name: "tool/file/child", body: "y", mode: 0o644},
		}, want: "prefix collision"},
		{name: "case-fold", members: []testArchiveMember{
			{name: "tool/Readme", body: "x", mode: 0o644},
			{name: "tool/README", body: "y", mode: 0o644},
		}, want: "case-fold"},
		{name: "nfc", members: []testArchiveMember{
			{name: "tool/\u00E9", body: "x", mode: 0o644},
			{name: "tool/e\u0301", body: "y", mode: 0o644},
		}, want: "normalization"},
		{name: "symlink", members: []testArchiveMember{
			{name: "tool/link", body: "target", mode: os.ModeSymlink | 0o777},
		}, want: "symbolic link"},
		{name: "setuid", members: []testArchiveMember{
			{name: "tool/run", body: "x", mode: os.ModeSetuid | 0o755},
		}, want: "forbidden permission"},
		{name: "special", members: []testArchiveMember{
			{name: "tool/device", body: "x", mode: os.ModeDevice | 0o600},
		}, want: "not a directory or regular file"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.zip")
			writeTestArchive(t, archivePath, "zip", test.members)
			destination := filepath.Join(root, "output")
			err := ExtractArchive(context.Background(), extractTestOptions(t, archivePath, destination, "zip"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe ZIP accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, destination)
			assertNoExtractStages(t, root)
		})
	}
}

func TestExtractArchiveRejectsEncryptedAndUnsupportedZIPShapes(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte)
		want   string
	}{
		{name: "encrypted", mutate: func(data []byte) { mutateZIPUint16(t, data, 6, 8, 1) }, want: "encrypted"},
		{name: "method", mutate: func(data []byte) { mutateZIPUint16(t, data, 8, 10, 99) }, want: "compression method"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.zip")
			writeTestArchive(t, archivePath, "zip", oneTestMember())
			data, err := os.ReadFile(archivePath)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(data)
			if err := os.WriteFile(archivePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(root, "output")
			err = ExtractArchive(context.Background(), extractTestOptions(t, archivePath, destination, "zip"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsupported ZIP shape accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, destination)
		})
	}
}

func TestZIPRejectsDeclaredFileLimitBeforeInflation(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.zip")
	writeTestArchive(t, archivePath, "zip", oneTestMember())
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	mutateZIPUint32(t, data, 22, 24, 1024)
	if err := os.WriteFile(archivePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "zip")
	options.Limits = DefaultExtractionLimits
	options.Limits.MaxFileBytes = 8
	err = ExtractArchive(context.Background(), options)
	if err == nil || !strings.Contains(err.Error(), "per-file limit") {
		t.Fatalf("ZIP member was inflated before declared-size rejection: %v", err)
	}
	assertPathAbsent(t, options.Destination)
}

func TestCanonicalArchiveMemberRejectsNULAndInvalidUTF8(t *testing.T) {
	for _, value := range []string{"tool/a\x00b", "tool/\xff"} {
		if _, err := canonicalMemberName(value, false, 100); err == nil {
			t.Fatalf("unsafe member name was accepted: %q", value)
		}
	}
}

func TestExtractArchiveRejectsUnsafeTarTypesAndHeaders(t *testing.T) {
	tests := []struct {
		name   string
		member testArchiveMember
		want   string
	}{
		{name: "symlink", member: testArchiveMember{name: "tool/link", mode: 0o777, tarType: tar.TypeSymlink}, want: "link"},
		{name: "hardlink", member: testArchiveMember{name: "tool/link", mode: 0o777, tarType: tar.TypeLink}, want: "link"},
		{name: "device", member: testArchiveMember{name: "tool/device", mode: 0o600, tarType: tar.TypeChar}, want: "type"},
		{name: "fifo", member: testArchiveMember{name: "tool/fifo", mode: 0o600, tarType: tar.TypeFifo}, want: "type"},
		{name: "pax-unsafe-path", member: testArchiveMember{
			name: "../" + strings.Repeat("nested/", 40) + "file",
			body: "x", mode: 0o644, format: tar.FormatPAX,
		}, want: "noncanonical"},
		{name: "gnu-link", member: testArchiveMember{
			name: "tool/link", mode: 0o777, tarType: tar.TypeSymlink, format: tar.FormatGNU,
		}, want: "link"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{test.member})
			destination := filepath.Join(root, "output")
			err := ExtractArchive(context.Background(), extractTestOptions(t, archivePath, destination, "tar.gz"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe tar accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, destination)
		})
	}
}

func TestExtractArchiveAcceptsResolvedPAXAndGNUPaths(t *testing.T) {
	longName := "tool/" + strings.Repeat("nested/", 40) + "file"
	for _, format := range []tar.Format{tar.FormatPAX, tar.FormatGNU} {
		t.Run(format.String(), func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{{
				name: longName, body: "metadata-safe", mode: 0o644, format: format,
			}})
			assertTestTarFormat(t, archivePath, format, longName)
			destination := filepath.Join(root, "output")
			if err := ExtractArchive(context.Background(), extractTestOptions(t, archivePath, destination, "tar.gz")); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(longName)))
			if err != nil || string(data) != "metadata-safe" {
				t.Fatalf("resolved %s member was not extracted: %q err=%v", format, data, err)
			}
		})
	}
}

func TestValidateTarMetadataAcceptsResolvedBenignPAXMetadata(t *testing.T) {
	header := &tar.Header{
		Name: "tool/file", Typeflag: tar.TypeReg, Size: 7, Format: tar.FormatPAX,
		Uid: 501, Gid: 20, Uname: "builder", Gname: "staff",
		PAXRecords: map[string]string{
			"path": "tool/file", "size": "7",
			"uid": "501", "gid": "20", "uname": "builder", "gname": "staff",
			"mtime": "1.25", "atime": "2", "ctime": "3.5",
		},
	}
	if err := validateTarMetadata(header, 4096); err != nil {
		t.Fatalf("resolved benign PAX metadata was rejected: %v", err)
	}
}

func TestExtractArchiveBoundsAndValidatesTarTrailingData(t *testing.T) {
	tests := []struct {
		name     string
		trailing []byte
		want     string
	}{
		{name: "bounded-zero-padding", trailing: make([]byte, 8)},
		{name: "excessive-zero-padding", trailing: make([]byte, 9), want: "trailing-zero limit"},
		{name: "nonzero-padding", trailing: []byte{0, 0, 1}, want: "nonzero data"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.tar.gz")
			writeTestArchive(t, archivePath, "tar.gz", oneTestMember())
			addTarGZTrailing(t, archivePath, test.trailing)
			destination := filepath.Join(root, "output")
			options := extractTestOptions(t, archivePath, destination, "tar.gz")
			options.Limits = DefaultExtractionLimits
			options.Limits.MaxTarTrailingBytes = 8
			err := ExtractArchive(context.Background(), options)
			if test.want == "" {
				if err != nil {
					t.Fatalf("bounded zero padding was rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe trailing data accepted or wrong error: %v", err)
			}
			assertPathAbsent(t, destination)
		})
	}
}

func TestValidateTarMetadataRejectsSparseAndAmbiguousOverrides(t *testing.T) {
	tests := []tar.Header{
		{Name: "tool/sparse", Typeflag: tar.TypeGNUSparse, Format: tar.FormatGNU},
		{
			Name: "tool/file", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"GNU.sparse.vendor": "1"},
		},
		{
			Name: "tool/file", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"comment": "unsupported"},
		},
		{
			Name: "tool/file", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"linkpath": "tool/other"},
		},
		{
			Name: "tool/link", Linkname: "tool/resolved", Typeflag: tar.TypeSymlink,
			Format: tar.FormatPAX, PAXRecords: map[string]string{"linkpath": "tool/different"},
		},
		{
			Name: "tool/file", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"path": ""},
		},
		{
			Name: "tool/resolved", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"path": "tool/different"},
		},
		{
			Name: "tool/file", Typeflag: tar.TypeReg, Size: 1, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"size": "01"},
		},
		{
			Name: "tool/file", Typeflag: tar.TypeReg, Format: tar.FormatPAX,
			PAXRecords: map[string]string{"mtime": "1\n2"},
		},
	}
	for index := range tests {
		if err := validateTarMetadata(&tests[index], 4096); err == nil {
			t.Fatalf("unsafe tar metadata case %d was accepted", index)
		}
	}
}

func TestExtractArchiveRejectsHashInputDestinationAndCancellation(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.zip")
	writeTestArchive(t, archivePath, "zip", []testArchiveMember{
		{name: "tool/file", body: "payload", mode: 0o644},
	})
	options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "zip")
	options.SHA256 = strings.Repeat("0", 64)
	if err := ExtractArchive(context.Background(), options); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("wrong hash accepted: %v", err)
	}
	assertPathAbsent(t, options.Destination)

	options = extractTestOptions(t, archivePath, filepath.Join(root, "dangling"), "zip")
	if err := os.Symlink(filepath.Join(root, "missing"), options.Destination); err != nil {
		t.Fatal(err)
	}
	if err := ExtractArchive(context.Background(), options); err == nil || !strings.Contains(err.Error(), "existing") {
		t.Fatalf("dangling destination accepted: %v", err)
	}

	linkedParent := filepath.Join(root, "linked-parent")
	if err := os.Symlink(root, linkedParent); err != nil {
		t.Fatal(err)
	}
	options = extractTestOptions(t, archivePath, filepath.Join(linkedParent, "through-link"), "zip")
	if err := ExtractArchive(context.Background(), options); err == nil || !strings.Contains(err.Error(), "nonsymlink directory") {
		t.Fatalf("symlink destination parent accepted: %v", err)
	}

	symlinkArchive := filepath.Join(root, "archive-link")
	if err := os.Symlink(archivePath, symlinkArchive); err != nil {
		t.Fatal(err)
	}
	options = extractTestOptions(t, archivePath, filepath.Join(root, "symlink-output"), "zip")
	options.ArchivePath = symlinkArchive
	if err := ExtractArchive(context.Background(), options); err == nil || !strings.Contains(err.Error(), "nonsymlink") {
		t.Fatalf("archive symlink accepted: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	options = extractTestOptions(t, archivePath, filepath.Join(root, "cancelled"), "zip")
	if err := ExtractArchive(ctx, options); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancelled extraction accepted: %v", err)
	}
	assertPathAbsent(t, options.Destination)
}

func TestPrivateStageOwnershipDetectsReplacement(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "output")
	stage, ownership, err := createPrivateStage(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyStageOwnership(stage, root, ownership); err != nil {
		t.Fatalf("fresh stage ownership was rejected: %v", err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyStageOwnership(stage, root, ownership); err == nil ||
		!strings.Contains(err.Error(), "staging identity") {
		t.Fatalf("replacement stage was accepted: %v", err)
	}
	removeOwnedStage(stage, ownership.stage)
	if _, err := os.Lstat(stage); err != nil {
		t.Fatalf("replacement stage was removed by stale cleanup: %v", err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveOwnedStageHandlesNonWritableExtractedDirectories(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, ".output.extract-owned")
	nested := filepath.Join(stage, "locked")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	owned, err := os.Lstat(stage)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0); err != nil {
		t.Fatal(err)
	}
	removeOwnedStage(stage, owned)
	assertPathAbsent(t, stage)
}

func TestImplicitParentThenExplicitDirectoryPreservesArchiveMode(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.tar.gz")
	writeTestArchive(t, archivePath, "tar.gz", []testArchiveMember{
		{name: "tool/bin/run", body: "payload", mode: 0o755},
		{name: "tool/bin/", mode: 0o755, isDir: true},
	})
	destination := filepath.Join(root, "output")
	if err := ExtractArchive(context.Background(), extractTestOptions(t, archivePath, destination, "tar.gz")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "tool", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("explicit directory mode=%v", info.Mode())
	}
}

func TestVerifyArchiveIdentityRejectsPathReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "archive")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	replacement := filepath.Join(root, "replacement")
	if err := os.WriteFile(replacement, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := verifyArchiveIdentity(path, opened, info); err == nil ||
		!strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("archive path replacement was accepted: %v", err)
	}
}

func TestExtractArchiveCancellationCleansExactOwnedStage(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "input.zip")
	writeTestArchive(t, archivePath, "zip", oneTestMember())
	destination := filepath.Join(root, "output")
	ctx := stageAwareCancellation{
		parent: root,
		prefix: "." + filepath.Base(destination) + ".extract-",
	}
	err := ExtractArchive(ctx, extractTestOptions(t, archivePath, destination, "zip"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("post-staging cancellation returned %v", err)
	}
	assertPathAbsent(t, destination)
	assertNoExtractStages(t, root)
}

func TestExtractArchiveEnforcesEveryLimit(t *testing.T) {
	tests := []struct {
		name    string
		members []testArchiveMember
		change  func(*ExtractionLimits)
		want    string
	}{
		{name: "archive", members: oneTestMember(), change: func(value *ExtractionLimits) { value.MaxArchiveBytes = 1 }, want: "archive exceeds"},
		{name: "members", members: []testArchiveMember{
			{name: "tool/a", body: "a", mode: 0o644},
			{name: "tool/b", body: "b", mode: 0o644},
		}, change: func(value *ExtractionLimits) { value.MaxMembers = 1 }, want: "member limit"},
		{name: "path", members: oneTestMember(), change: func(value *ExtractionLimits) { value.MaxPathBytes = 4 }, want: "unsafe"},
		{name: "file", members: oneTestMember(), change: func(value *ExtractionLimits) { value.MaxFileBytes = 3 }, want: "per-file"},
		{name: "aggregate", members: []testArchiveMember{
			{name: "tool/a", body: "abc", mode: 0o644},
			{name: "tool/b", body: "def", mode: 0o644},
		}, change: func(value *ExtractionLimits) { value.MaxTotalBytes = 5 }, want: "aggregate"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archivePath := filepath.Join(root, "input.zip")
			writeTestArchive(t, archivePath, "zip", test.members)
			options := extractTestOptions(t, archivePath, filepath.Join(root, "output"), "zip")
			options.Limits = DefaultExtractionLimits
			test.change(&options.Limits)
			err := ExtractArchive(context.Background(), options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("limit was not enforced: %v", err)
			}
			assertPathAbsent(t, options.Destination)
		})
	}
}

func TestWriteBearerHeaderValidatesJSONTokenAndPrivateOutput(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "header")
	token := "abc.DEF_123-~/+="
	if err := WriteBearerHeader(strings.NewReader(`{"metadata":{"source":"ghcr"},"token":"`+token+`"}`), output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "Authorization: Bearer "+token+"\n" {
		t.Fatalf("unexpected header file %q: %v", data, err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("header mode=%v err=%v", info.Mode(), err)
	}
	if err := WriteBearerHeader(strings.NewReader(`{"token":"other"}`), output); err == nil {
		t.Fatal("existing header path was replaced")
	}
}

func TestWriteBearerHeaderRejectsInvalidInputWithoutDisclosure(t *testing.T) {
	secret := "DO_NOT_DISCLOSE_TOKEN_123"
	tests := []struct {
		name    string
		payload string
	}{
		{name: "missing", payload: `{"metadata":1}`},
		{name: "duplicate", payload: `{"token":"` + secret + `","token":"other"}`},
		{name: "wrong-type", payload: `{"token":123}`},
		{name: "empty", payload: `{"token":""}`},
		{name: "whitespace", payload: `{"token":"` + secret + ` space"}`},
		{name: "control", payload: `{"token":"` + secret + `\n"}`},
		{name: "padding-middle", payload: `{"token":"abc=def"}`},
		{name: "trailing", payload: `{"token":"` + secret + `"} {"more":true}`},
		{name: "malformed", payload: `{"token":"` + secret},
		{name: "too-long-token", payload: `{"token":"` + strings.Repeat("a", MaxBearerTokenBytes+1) + `"}`},
		{name: "too-large-json", payload: `{"metadata":"` + strings.Repeat("a", MaxAuthJSONBytes) + `","token":"x"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "header")
			err := WriteBearerHeader(strings.NewReader(test.payload), output)
			if err == nil {
				t.Fatal("invalid authorization JSON was accepted")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), test.payload) {
				t.Fatalf("authorization error disclosed input: %v", err)
			}
			assertPathAbsent(t, output)
		})
	}
}

func oneTestMember() []testArchiveMember {
	return []testArchiveMember{{name: "tool/file", body: "payload", mode: 0o644}}
}

func extractTestOptions(t *testing.T, archivePath, destination, format string) ExtractOptions {
	t.Helper()
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return ExtractOptions{
		ArchivePath: archivePath, Destination: destination, Format: format,
		SHA256: hex.EncodeToString(digest[:]), AllowedRoots: []string{"tool"},
	}
}

func writeTestArchive(t *testing.T, destination, format string, members []testArchiveMember) {
	t.Helper()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	switch format {
	case "zip":
		writer := zip.NewWriter(output)
		for _, member := range members {
			header := &zip.FileHeader{Name: member.name, Method: zip.Deflate}
			mode := member.mode
			if member.isDir {
				mode |= os.ModeDir
			}
			header.SetMode(mode)
			entry, err := writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(entry, member.body); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	case "tar.gz":
		gzipWriter := gzip.NewWriter(output)
		writer := tar.NewWriter(gzipWriter)
		for _, member := range members {
			typeFlag := member.tarType
			if typeFlag == 0 {
				if member.isDir {
					typeFlag = tar.TypeDir
				} else {
					typeFlag = tar.TypeReg
				}
			}
			format := member.format
			if format == tar.FormatUnknown {
				format = tar.FormatUSTAR
			}
			size := int64(len(member.body))
			if typeFlag != tar.TypeReg && typeFlag != tar.TypeRegA {
				size = 0
			}
			header := &tar.Header{
				Name: member.name, Mode: int64(member.mode.Perm()), Size: size,
				Typeflag: typeFlag, Format: format,
			}
			if typeFlag == tar.TypeSymlink || typeFlag == tar.TypeLink {
				header.Linkname = member.linkname
				if header.Linkname == "" {
					header.Linkname = "target"
				}
			}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if size > 0 {
				if _, err := io.WriteString(writer, member.body); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported test archive format %q", format)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertTestTarFormat(t *testing.T, archivePath string, format tar.Format, name string) {
	t.Helper()
	input, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	seen := false
	err = walkTarGZ(
		context.Background(),
		input,
		DefaultExtractionLimits.MaxTarTrailingBytes,
		func(header *tar.Header, reader io.Reader) error {
			seen = true
			if header.Format != format {
				t.Fatalf("test archive member format=%s want=%s", header.Format, format)
			}
			if format == tar.FormatPAX && header.PAXRecords["path"] != name {
				t.Fatalf("PAX path was not resolved: %+v", header.PAXRecords)
			}
			return discardExpected(context.Background(), reader, uint64(header.Size))
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("test tar archive contained no member")
	}
}

func addTarGZTrailing(t *testing.T, archivePath string, trailing []byte) {
	t.Helper()
	compressed, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	expanded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	expanded = append(expanded, trailing...)
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(expanded); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertPathAbsent(t *testing.T, name string) {
	t.Helper()
	if _, err := os.Lstat(name); !os.IsNotExist(err) {
		t.Fatalf("path should be absent: %s err=%v", name, err)
	}
}

func assertNoExtractStages(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".extract-") {
			t.Fatalf("private extraction stage was not cleaned: %s", entry.Name())
		}
	}
}

func mutateZIPUint16(t *testing.T, data []byte, localOffset, centralOffset int, value uint16) {
	t.Helper()
	for signature, offset := range map[string]int{
		"PK\x03\x04": localOffset,
		"PK\x01\x02": centralOffset,
	} {
		index := bytes.Index(data, []byte(signature))
		if index < 0 || index+offset+2 > len(data) {
			t.Fatalf("ZIP signature %q is unavailable", signature)
		}
		data[index+offset] = byte(value)
		data[index+offset+1] = byte(value >> 8)
	}
}

func mutateZIPUint32(t *testing.T, data []byte, localOffset, centralOffset int, value uint32) {
	t.Helper()
	for signature, offset := range map[string]int{
		"PK\x03\x04": localOffset,
		"PK\x01\x02": centralOffset,
	} {
		index := bytes.Index(data, []byte(signature))
		if index < 0 || index+offset+4 > len(data) {
			t.Fatalf("ZIP signature %q is unavailable", signature)
		}
		data[index+offset] = byte(value)
		data[index+offset+1] = byte(value >> 8)
		data[index+offset+2] = byte(value >> 16)
		data[index+offset+3] = byte(value >> 24)
	}
}

type stageAwareCancellation struct {
	parent string
	prefix string
}

func (ctx stageAwareCancellation) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx stageAwareCancellation) Done() <-chan struct{}       { return nil }
func (ctx stageAwareCancellation) Value(any) any               { return nil }
func (ctx stageAwareCancellation) Err() error {
	entries, err := os.ReadDir(ctx.parent)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ctx.prefix) {
			return context.Canceled
		}
	}
	return nil
}

func TestWriteBearerHeaderRejectsDanglingOutputSymlink(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "header")
	if err := os.Symlink(filepath.Join(root, "missing"), output); err != nil {
		t.Fatal(err)
	}
	err := WriteBearerHeader(bytes.NewBufferString(`{"token":"valid-token"}`), output)
	if err == nil {
		t.Fatal("dangling authorization header symlink was accepted")
	}
}
