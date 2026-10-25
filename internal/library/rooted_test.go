package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
	"aos-cx-docs-dldr/internal/testutil"
)

type unavailableFetcher struct{}

func (unavailableFetcher) Get(context.Context, string, bool) (model.Resource, error) {
	return model.Resource{}, errors.New("offline fixture")
}

func offlineHTMLPlan(id string) model.DocumentPlan {
	return model.DocumentPlan{
		Document: model.Document{ID: id, Title: id + " Guide", Platform: "6300", Version: "10.10",
			URL: "https://example.test/" + id + "/Default.htm", Kind: "flare"},
		Topics:    []model.Topic{{URL: "https://example.test/" + id + "/topic.htm", Title: "Topic"}},
		Inventory: &model.InventoryEvidence{Complete: true, Kind: "flare"},
	}
}

func writeGuideTree(t *testing.T, directory, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(directory, "pages"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"index.html", "manifest.json", "pages/keep.html"} {
		if err := os.WriteFile(filepath.Join(directory, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGuideTree(t *testing.T, directory, body string) {
	t.Helper()
	for _, name := range []string{"index.html", "manifest.json", "pages/keep.html"} {
		data, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(name)))
		if err != nil || string(data) != body {
			t.Fatalf("%s changed through a swapped staging path: %q %v", name, data, err)
		}
	}
}

// swapStaging replaces <platform>/.staging with a symlink to replacement, as an
// account able to write the platform directory could after GuideOutput, and
// restores it when the test ends so the lock can be released.
func swapStaging(t *testing.T, run *Run, replacement string) {
	t.Helper()
	staging := filepath.Join(run.Destination, run.parent, ".staging")
	if err := os.Rename(staging, staging+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, staging); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() {
		os.Remove(staging)
		os.Rename(staging+"-real", staging)
	})
}

func TestGuideOutputRootIgnoresStagingSwapOutsideDestination(t *testing.T) {
	base := t.TempDir()
	victim := filepath.Join(base, "victim", "6300", "10.16")
	writeGuideTree(t, filepath.Join(victim, "guide1"), "victim")
	run, err := Open(filepath.Join(base, "dest"), "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	guide, output := guideOutput(t, run, "guide1")
	evil := filepath.Join(base, "evil")
	if err := os.MkdirAll(filepath.Join(evil, path.Base(run.stage)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(evil, path.Base(run.stage), ".work")); err != nil {
		t.Skip(err)
	}
	swapStaging(t, run, evil)
	if _, err := archive.HTMLInRoot(context.Background(), offlineHTMLPlan("guide1"), unavailableFetcher{},
		guide, output, false, 1<<20, 1<<20, 1, nil); err == nil {
		t.Fatal("offline archive unexpectedly succeeded")
	}
	assertGuideTree(t, filepath.Join(victim, "guide1"), "victim")
}

func TestGuideOutputRootIgnoresStagingSwapToPublishedRelease(t *testing.T) {
	base := t.TempDir()
	first, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	acceptHTML(t, first, "routing")
	if _, err := first.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	published := filepath.Join(base, "6300", "10.10", "routing")
	before, err := storage.TreeDigest(context.Background(), mustOpenRoot(t, base), "6300/10.10/routing")
	if err != nil {
		t.Fatal(err)
	}

	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	guide, output := guideOutput(t, run, "routing")
	// The swapped staging resolves inside the destination, where os.Root would
	// follow it, and leads the guide's work path to the published guide.
	redirect := filepath.Join(base, "redirect", path.Base(run.stage))
	if err := os.MkdirAll(redirect, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../6300/10.10", filepath.Join(redirect, ".work")); err != nil {
		t.Skip(err)
	}
	swapStaging(t, run, "../redirect")
	if _, err := archive.HTMLInRoot(context.Background(), offlineHTMLPlan("routing"), unavailableFetcher{},
		guide, output, false, 1<<20, 1<<20, 1, nil); err == nil {
		t.Fatal("offline archive unexpectedly succeeded")
	}
	after, err := storage.TreeDigest(context.Background(), mustOpenRoot(t, base), "6300/10.10/routing")
	if err != nil || after != before {
		t.Fatalf("published guide %s changed through a swapped staging path: %v", published, err)
	}
}

func TestOpenRefusesManagedDirectoriesWritableByOthers(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(probe); err != nil || info.Mode().Perm() != 0o600 {
		t.Skipf("temporary filesystem does not keep file modes: %v", err)
	}
	for _, managed := range []string{"6300", "6300/.staging", "6300/.snapshots", "6300/.incomplete"} {
		for _, mode := range []os.FileMode{0o777, 0o770, 0o1777} {
			base := t.TempDir()
			directory := filepath.Join(base, filepath.FromSlash(managed))
			if err := os.MkdirAll(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(directory, mode); err != nil {
				t.Fatal(err)
			}
			run, err := Open(base, "6300", "10.10")
			if err == nil {
				run.Close()
				t.Fatalf("%s with mode %#o accepted", managed, mode)
			}
			if !strings.Contains(err.Error(), "writable by other accounts") {
				t.Fatalf("%s with mode %#o: unexpected error %v", managed, mode, err)
			}
			if _, statErr := os.Lstat(filepath.Join(base, "6300", ".10.10.lock")); !os.IsNotExist(statErr) {
				t.Fatalf("refused open left its lock behind: %v", statErr)
			}
		}
	}
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "6300", ".staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"6300", "6300/.staging"} {
		if err := os.Chmod(filepath.Join(base, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("read-only shared managed directories refused: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesManagedDirectoryOwnedByAnotherAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a directory's owner requires root; managedDirectoryPolicy is covered without it")
	}
	base := t.TempDir()
	platform := filepath.Join(base, "6300")
	if err := os.Mkdir(platform, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(platform, 4242, 4242); err != nil {
		t.Skip(err)
	}
	if _, err := Open(base, "6300", "10.10"); err == nil || !strings.Contains(err.Error(), "owned by UID 4242") {
		t.Fatalf("directory owned by another account accepted: %v", err)
	}
}

func TestReleaseSearchRefusesRecordsOutsideGuideTopics(t *testing.T) {
	for _, link := range []string{
		"../javascript:alert(1)", "javascript:alert(1)", "pages/../index.html", "/etc/passwd",
		"pages\\keep.html", "pages/not-a-topic.html", "https://example.test/x",
	} {
		t.Run(link, func(t *testing.T) {
			base := t.TempDir()
			run, err := Open(base, "6300", "10.10")
			if err != nil {
				t.Fatal(err)
			}
			defer run.Close()
			acceptHTML(t, run, "routing")
			// Integrity hashes detect corruption, not tampering: rewrite
			// search.json together with the hashes that describe it.
			guide := path.Join(run.stage, "routing")
			if err := storage.WriteJSON(run.root, path.Join(guide, "search.json"),
				[]map[string]string{{"title": "Topic", "url": link, "text": "Offline text"}}); err != nil {
				t.Fatal(err)
			}
			hash, _, err := storage.Hash(run.root, path.Join(guide, "search.json"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			result := run.guides["routing"]
			result.HTML.Integrity.SearchSHA256 = hash
			run.guides["routing"] = result
			if err := storage.WriteJSON(run.root, path.Join(guide, "manifest.json"), result.HTML); err != nil {
				t.Fatal(err)
			}
			if err := run.verifyGuide(guide, result); err != nil {
				t.Fatalf("tampered guide no longer verifies, so the test proves nothing: %v", err)
			}
			if _, err := run.Publish(false); err == nil || !strings.Contains(err.Error(), "archived topic page") {
				t.Fatalf("search record %q accepted: %v", link, err)
			}
			if _, err := os.Lstat(run.Target); !os.IsNotExist(err) {
				t.Fatalf("release published with an unsafe search link: %v", err)
			}
		})
	}
}

func TestPublisherPDFKeepsLegacyNameWithFormatCharacters(t *testing.T) {
	base := t.TempDir()
	run, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	title := "Job\u202eScheduler\u200b Guide"
	document := model.Document{ID: "job", Title: title, Platform: "6300", Version: "10.10", Kind: "pdf",
		URL: "https://example.test/job.pdf", RouteOrigin: "publisher-url"}
	guide, output := guideOutput(t, run, "job")
	result, err := archive.PublisherPDFInRoot(context.Background(), model.DocumentPlan{Document: document, PDFURL: document.URL},
		pdfFetcher(testutil.PDF("legacy")), guide, output, false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(result.PDF.Path, "\u202e\u200b") {
		t.Fatalf("new PDF name keeps format characters: %q", result.PDF.Path)
	}
	// A library written by 0.8 holds the name with the format characters.
	legacy := "6300 - 10.10 - " + title + ".pdf"
	if err := guide.Rename(result.PDF.Path, legacy); err != nil {
		t.Fatal(err)
	}
	wrong := result
	wrongPDF := *result.PDF
	wrongPDF.Path = "6300 - 10.10 - Other.pdf"
	wrong.PDF = &wrongPDF
	if err := run.verifyGuide(path.Join(run.stage, ".work", "job"), wrong); err == nil {
		t.Fatal("unrelated PDF filename accepted")
	}
	result.PDF.Path = legacy
	if err := run.Accept(result); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Publish(false); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(base, "6300", "10.10")
	if err != nil {
		t.Fatalf("library holding a 0.8 PDF name no longer opens: %v", err)
	}
	if got := reopened.previous.PDFGuides["job"].PDF.Path; got != legacy {
		t.Fatalf("legacy PDF name not retained: %q", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(base, "6300", "10.10", "job", legacy)); err != nil || !bytes.Equal(data, testutil.PDF("legacy")) {
		t.Fatalf("legacy PDF not published: %v", err)
	}
}
