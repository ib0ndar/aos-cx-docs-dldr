package library

import (
	stdzip "archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"aos-cx-docs-dldr/internal/testutil"
)

func acceptNamedPDF(t *testing.T, run *Run, id, title, label string) model.ArchiveResult {
	t.Helper()
	document := model.Document{
		ID: id, Title: title, Platform: "6300", Version: "10.10", Kind: "pdf",
		URL: "https://example.test/" + id + ".pdf", RouteOrigin: "publisher-url",
	}
	output, err := run.GuideOutput(id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := archive.PublisherPDF(
		context.Background(),
		model.DocumentPlan{Document: document, PDFURL: document.URL},
		pdfFetcher(testutil.PDF(label)),
		output,
		false,
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Accept(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func publishZIPFixture(t *testing.T, kind string) (string, Manifest) {
	t.Helper()
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "html":
		acceptHTMLWithDiagnostics(t, run, "html", []model.Notice{{
			Kind: model.NoticeCSSCleanup, Message: "Removed one publisher navigation rule",
		}}, stylesheetRecoveryFixture())
	case "pdf":
		acceptNamedPDF(t, run, "pdf", "Réseau 指南", "pdf bytes")
	case "mixed":
		acceptHTMLWithDiagnostics(t, run, "html", []model.Notice{{
			Kind: model.NoticeCSSCleanup, Message: "Removed one publisher navigation rule",
		}}, stylesheetRecoveryFixture())
		acceptNamedPDF(t, run, "pdf", "Réseau 指南", "pdf bytes")
	case "incomplete":
		acceptHTML(t, run, "html")
		output, err := run.GuideOutput("failed")
		if err != nil {
			t.Fatal(err)
		}

		result := model.ArchiveResult{
			Document: model.Document{
				ID: "failed", Title: "Failed Guide", Platform: "6300", Version: "10.10",
				Kind: "pdf", URL: "https://example.test/failed.pdf",
			},
			OutputDir: output, Status: "failed", Format: "pdf", Errors: []string{"publisher unavailable"},
		}
		if err := run.Accept(result); err != nil {
			t.Fatal(err)
		}
	case "empty":
	default:
		t.Fatal("unknown ZIP fixture kind", kind)
	}
	manifest, err := run.Publish(kind == "empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return run.Target, manifest
}

func stylesheetRecoveryFixture() []model.StylesheetRecovery {
	return []model.StylesheetRecovery{{
		BrokenURL: "https://example.test/broken.css", HTTPStatus: 404,
		ReplacementURL:          "https://example.test/guide/Content/Resources/TableStyles/Table.css",
		ReplacementFinalURL:     "https://example.test/guide/Content/Resources/TableStyles/Table.css",
		ReplacementSourceSHA256: strings.Repeat("a", 64), ReplacementSourceSize: 123,
		TableStyleFamilies: []string{"TableStyle-Table"},
		AffectedTopics:     []string{"https://example.test/guide/Content/home.htm"},
	}}
}

func readZIP(t *testing.T, name string) *stdzip.ReadCloser {
	t.Helper()
	reader, err := stdzip.OpenReader(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

func eligibleFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil || relative == "." {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if slices.ContainsFunc(parts, func(part string) bool { return strings.HasPrefix(part, ".") }) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			body, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			result[filepath.ToSlash(filepath.Join(filepath.Base(root), relative))] = body
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestZIPExportsVerifiedHTMLPDFMixedIncompleteAndEmptyLibraries(t *testing.T) {
	for _, kind := range []string{"html", "pdf", "mixed", "incomplete", "empty"} {
		t.Run(kind, func(t *testing.T) {
			root, manifest := publishZIPFixture(t, kind)
			target, err := ExportZIP(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}

			if target != root+".zip" {
				t.Fatalf("wrong ZIP target: %s", target)
			}
			expected := eligibleFiles(t, root)
			reader := readZIP(t, target)
			if len(reader.File) != len(expected) {
				t.Fatalf("ZIP member count=%d want=%d", len(reader.File), len(expected))
			}
			for _, member := range reader.File {
				want, ok := expected[member.Name]
				if !ok {
					t.Fatalf("unexpected ZIP member %q", member.Name)
				}
				body, err := member.Open()
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(body)
				closeErr := body.Close()
				if err := errors.Join(readErr, closeErr); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("ZIP changed bytes for %s", member.Name)
				}
				delete(expected, member.Name)
			}
			if len(expected) != 0 {
				keys := mapsKeys(expected)
				slices.Sort(keys)
				t.Fatalf("ZIP omitted members: %v", keys)
			}
			manifestMember := filepath.ToSlash(filepath.Join(filepath.Base(root), "manifest.json"))
			var archived Manifest
			for _, member := range reader.File {
				if member.Name != manifestMember {
					continue
				}
				body, _ := member.Open()
				if err := json.NewDecoder(body).Decode(&archived); err != nil {
					t.Fatal(err)
				}
				body.Close()
			}
			if archived.Status != manifest.Status {
				t.Fatalf("ZIP relabelled library status %q as %q", manifest.Status, archived.Status)
			}
			if (kind == "pdf" || kind == "mixed") &&
				!slices.ContainsFunc(reader.File, func(member *stdzip.File) bool {
					return strings.HasSuffix(member.Name, "6300 - 10.10 - Réseau 指南.pdf")
				}) {
				t.Fatal("Unicode publisher PDF filename was not preserved")
			}
		})
	}
}

func TestZIPRejectsLibraryFromAnotherApplicationVersionWithoutRewritingSource(t *testing.T) {
	root, _ := publishZIPFixture(t, "mixed")
	rewritePublishedApplicationVersion(t, root, "0.5")
	before, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExportZIP(context.Background(), root); err == nil ||
		!strings.Contains(err.Error(), "application version is incompatible") {
		t.Fatalf("ZIP exported a library from an unsupported version: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected ZIP export rewrote the source manifest: %v", err)
	}
}

func mapsKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func TestZIPAllowsTrulyEmptyRootWithoutDirectoryMember(t *testing.T) {
	root := filepath.Join(t.TempDir(), "10.10")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err := ExportZIP(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if members := readZIP(t, target).File; len(members) != 0 {
		t.Fatalf("truly empty root produced members: %v", members)
	}
}

func TestZIPExcludesFinderMetadataAndRejectsOtherHiddenComponents(t *testing.T) {
	root, _ := publishZIPFixture(t, "mixed")
	for _, directory := range []string{root, filepath.Join(root, "html"), filepath.Join(root, "html", "pages"), filepath.Join(root, "html", "assets")} {
		if err := os.WriteFile(filepath.Join(directory, ".DS_Store"), []byte("Finder metadata"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target, err := ExportZIP(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range readZIP(t, target).File {
		relative := strings.TrimPrefix(member.Name, filepath.Base(root)+"/")
		if slices.ContainsFunc(strings.Split(relative, "/"), func(part string) bool {
			return strings.HasPrefix(part, ".")
		}) {
			t.Fatalf("hidden component exported: %s", member.Name)
		}
	}

	if err := os.WriteFile(filepath.Join(root, "html", ".hidden"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportZIP(context.Background(), root); err == nil {
		t.Fatal("arbitrary hidden file was accepted in ZIP source")
	}
}

func TestZIPRejectsUnsafeAndCollidingMemberNames(t *testing.T) {
	for _, entries := range [][]zipEntry{
		{{member: "10.10/file"}, {member: "10.10/file"}},
		{{member: "10.10/Foo"}, {member: "10.10/foo"}},
		{{member: "10.10/../escape"}},
		{{member: "/absolute"}},
		{{member: `10.10\file`}},
	} {
		if err := validateZIPMembers(entries); err == nil {
			t.Fatalf("unsafe/colliding ZIP members accepted: %+v", entries)
		}
	}
}

func TestZIPCancellationAndReplacementFailurePreservePriorArchive(t *testing.T) {
	root, _ := publishZIPFixture(t, "html")
	target, err := ExportZIP(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	_, err = exportZIP(cancelled, root, zipExportOptions{
		beforeWrite: func(string) error {
			cancel()
			return cancelled.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ZIP cancellation returned %v", err)
	}
	assertPriorZIPAndNoTemp(t, root, prior)

	injected := errors.New("injected ZIP replacement failure")
	_, err = exportZIP(context.Background(), root, zipExportOptions{
		rename: func(*os.Root, string, string) error { return injected },
	})
	if !errors.Is(err, injected) {
		t.Fatalf("ZIP replacement failure returned %v", err)
	}
	assertPriorZIPAndNoTemp(t, root, prior)
}

func assertPriorZIPAndNoTemp(t *testing.T, root string, prior []byte) {
	t.Helper()
	current, err := os.ReadFile(root + ".zip")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, prior) {
		t.Fatal("failed ZIP export changed the prior archive")
	}
	parts, err := filepath.Glob(filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+"-*.zip.part"))
	if err != nil || len(parts) != 0 {
		t.Fatalf("owned ZIP temporary file leaked: %v %v", parts, err)
	}
}

func TestZIPRejectsSymlinkTargetAndInvalidCentralMetadata(t *testing.T) {
	root, _ := publishZIPFixture(t, "html")
	target := root + ".zip"
	if err := os.Symlink(filepath.Join(root, "manifest.json"), target); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportZIP(context.Background(), root); err == nil || !strings.Contains(err.Error(), "ZIP target") {
		t.Fatalf("symlink ZIP target was accepted: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(root, "history.json")
	if err := os.WriteFile(historyPath, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportZIP(context.Background(), root); err == nil || !strings.Contains(err.Error(), "history is empty") {
		t.Fatalf("invalid central metadata was accepted: %v", err)
	}
}

func TestRunZIPRequiresPublicationAndRetainsEstablishedTimestampSemantics(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	if _, err := run.ExportZIP(context.Background()); err == nil {
		t.Fatal("unpublished run exported a ZIP")
	}
	acceptHTML(t, run, "html")
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	target, err := run.ExportZIP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := readZIP(t, target)
	source, err := os.Stat(filepath.Join(run.Target, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range reader.File {
		if member.Name == "10.10/manifest.json" {
			delta := member.Modified.Sub(source.ModTime())
			if delta < -2e9 || delta > 2e9 {
				t.Fatalf("ZIP timestamp does not reflect source mtime: source=%s member=%s", source.ModTime(), member.Modified)
			}
			return
		}
	}
	t.Fatal("ZIP manifest member missing")
}

func TestZIPFileNamePreservesEstablishedSuffixBehavior(t *testing.T) {
	for _, version := range []string{"10", "10.16", "10.17.1000"} {
		root := filepath.Join(t.TempDir(), version)
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		target, err := ExportZIP(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if target != root+".zip" {
			t.Fatalf("ZIP name=%s want=%s", target, root+".zip")
		}
	}
}

func TestZIPPDFMemberMatchesPublishedHash(t *testing.T) {
	root, manifest := publishZIPFixture(t, "pdf")
	target, err := ExportZIP(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	record := manifest.PDFGuides["pdf"].PDF
	sourceHash, _, err := storage.Hash(mustOpenRoot(t, root), filepath.ToSlash(filepath.Join("pdf", record.Path)), record.Size)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range readZIP(t, target).File {
		if strings.HasSuffix(member.Name, "/"+record.Path) {
			body, err := member.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(body)
			body.Close()
			if err != nil || archiveHash(data) != sourceHash || sourceHash != record.SHA256 {
				t.Fatalf("PDF hash changed: source=%s zip=%s record=%s err=%v", sourceHash, archiveHash(data), record.SHA256, err)
			}
			return
		}
	}
	t.Fatal("publisher PDF missing from ZIP")
}

func mustOpenRoot(t *testing.T, directory string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func archiveHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
