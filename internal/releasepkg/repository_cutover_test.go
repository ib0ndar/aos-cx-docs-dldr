package releasepkg

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var markdownLinkPattern = regexp.MustCompile(`\[[^]]+\]\(([^)]+)\)`)

func TestRepositoryContainsOnlyNativeImplementation(t *testing.T) {
	root := repositoryRoot(t)
	for _, name := range []string{
		"pyproject.toml",
		"src/aoscx_docs",
		"tests/test_archive.py",
		"tests/test_cli.py",
		"tests/test_library.py",
		"tests/test_live_compatibility.py",
		"tests/test_pdf.py",
		"tests/test_prompts.py",
		"tests/test_sources.py",
		"tests/test_transport.py",
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("retired Python path remains: %s (%v)", name, err)
		}
	}
	if err := filepath.WalkDir(root, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if entry.IsDir() && (relative == ".git" || relative == "build") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".py" {
			t.Errorf("Python source remains in current tree: %s", filepath.ToSlash(relative))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestActiveToolingAndDocumentationAreNativeOnly(t *testing.T) {
	root := repositoryRoot(t)
	scriptForbidden := []string{"python3", "python -m", "pytest", ".venv/"}
	if err := filepath.WalkDir(filepath.Join(root, "scripts"), func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		for _, forbidden := range scriptForbidden {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("active script %s contains retired Python tooling %q",
					filepath.ToSlash(name), forbidden)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	activeDocs := []string{
		"README.md",
		"docs/go-architecture.md",
		"docs/native-macos-arm64-release.md",
		"docs/container.md",
		"docs/chrome-sidecar.md",
		"docs/qpdf-sidecar.md",
	}
	docForbidden := []string{
		"Python 3.11+ is supported",
		"pip install",
		"python -m",
		"pytest",
		"](../src/aoscx_docs",
		"`src/aoscx_docs/",
		"Version **0.6**",
		"current application version remains `0.6`",
		"Version **0.7**",
		"current application version remains `0.7`",
	}
	for _, relative := range activeDocs {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range docForbidden {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("active document %s contains retired instruction/reference %q", relative, forbidden)
			}
		}
	}

	ignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{
		".venv/", "__pycache__/", "*.py[cod]", ".pytest_cache/",
		".ruff_cache/", "*.egg-info/", "dist/",
	} {
		if strings.Contains(string(ignore), retired) {
			t.Errorf(".gitignore retains Python-only entry %q", retired)
		}
	}
}

func TestCurrentDocumentationRelativeLinksExist(t *testing.T) {
	root := repositoryRoot(t)
	documents := []string{
		"README.md",
		"CHANGELOG.md",
		"docs/go-architecture.md",
		"docs/native-macos-arm64-release.md",
		"docs/container.md",
		"docs/chrome-sidecar.md",
		"docs/qpdf-sidecar.md",
	}
	for _, relative := range documents {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range markdownLinkPattern.FindAllStringSubmatch(string(body), -1) {
			target := strings.TrimSpace(match[1])
			if strings.HasPrefix(target, "#") ||
				strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "http://") ||
				strings.HasPrefix(target, "mailto:") {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			decoded, err := url.PathUnescape(target)
			if err != nil {
				t.Errorf("%s has invalid relative link %q: %v", relative, target, err)
				continue
			}
			resolved := filepath.Clean(filepath.Join(root, filepath.Dir(relative), filepath.FromSlash(decoded)))
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s has broken relative link %q: %v", relative, target, err)
			}
		}
	}
}

func TestReadmeUsesGuideIDPlaceholder(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repositoryRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `--guides "GUIDE_ID"`) {
		t.Fatal("README download example does not identify its guide ID placeholder")
	}
	if !strings.Contains(string(body), ".<version>.lock") ||
		!strings.Contains(string(body), ".<version>.transaction.json") {
		t.Fatal("README output layout does not show version-specific lock and journal names")
	}
	if strings.Contains(string(body), ".version.lock") ||
		strings.Contains(string(body), ".version.transaction.json") {
		t.Fatal("README output layout contains literal version lock or journal names")
	}
}

func TestProductionCLIHelpDoesNotUseCheckpointFraming(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repositoryRoot(t), "internal", "cli", "cli.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "native checkpoint") {
		t.Fatal("production CLI help still uses checkpoint framing")
	}
	if !strings.Contains(string(body), "This native application supports") {
		t.Fatal("production CLI help lacks the native application support statement")
	}
}

func TestActiveNativeSourceAvoidsRetiredProductFraming(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := []string{
		"native checkpoint",
		"manual Python-side inspection",
		"no migration or shared state is supported",
		"parity with Python",
	}
	if err := filepath.WalkDir(filepath.Join(root, "internal"), func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") || filepath.Ext(entry.Name()) != ".go" {
			return nil
		}
		body, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		for _, phrase := range forbidden {
			if strings.Contains(string(body), phrase) {
				t.Errorf("active native source %s contains retired product framing %q",
					filepath.ToSlash(name), phrase)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate repository audit source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}
