package pdfgen

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func encodedFileURL(name string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(name)}).String()
}

// TestAllowRenderRequestDecodesThePathExactlyOnce covers guide roots whose
// names contain "%" or an already escaped sequence. Chrome requests them with
// "%25", and decoding the parsed path a second time used to reject every
// resource of such a library.
func TestAllowRenderRequestDecodesThePathExactlyOnce(t *testing.T) {
	for _, name := range []string{"100%", "Docs%20Lib", "a b", "%2e%2e", "文档 #1?", "%"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), name, "guide")
			input := filepath.Join(root, ".print.html")
			asset := filepath.Join(root, "assets", "image 1%.png")
			if err := os.MkdirAll(filepath.Dir(asset), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, file := range []string{input, asset} {
				if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, request := range []string{encodedFileURL(input), encodedFileURL(asset)} {
				if allowed, kind, reason := allowRenderRequest(request, root, input); !allowed || kind != "file" || reason != "" {
					t.Fatalf("local request %s was rejected: kind=%s reason=%q", request, kind, reason)
				}
			}
		})
	}
}

func TestAllowRenderRequestKeepsRequestsInsideTheGuideRoot(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "100%")
	root := filepath.Join(parent, "guide")
	input := filepath.Join(root, ".print.html")
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret.txt")
	sibling := filepath.Join(parent, "guide-other", "secret.txt")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{outside, sibling} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := strings.TrimSuffix(encodedFileURL(root), "/")
	for name, request := range map[string]string{
		"parent file":                  encodedFileURL(outside),
		"sibling with the root prefix": encodedFileURL(sibling),
		"literal dot segments":         base + "/assets/../../secret.txt",
		"encoded dot segments":         base + "/assets/%2e%2e/%2E%2E/secret.txt",
		"encoded slash":                base + "/assets%2F..%2F..%2Fsecret.txt",
		"lowercase encoded slash":      base + "/assets%2f..%2f..%2fsecret.txt",
		"encoded slash inside root":    base + "/assets%2F.print.html",
		"double-encoded dot segments":  base + "/assets/%252e%252e/%252e%252e/secret.txt",
		"double-encoded percent root":  strings.Replace(base, "100%25", "100%2525", 1) + "/.print.html",
		"encoded NUL":                  base + "/.print.html%00.png",
		"guide root itself":            base,
		"remote host":                  "file://example.test" + strings.TrimPrefix(encodedFileURL(input), "file://"),
		"query":                        encodedFileURL(input) + "?x=1",
		"opaque file URL":              "file:.print.html",
		"user info":                    "file://user@" + strings.TrimPrefix(encodedFileURL(input), "file://"),
		"invalid escape":               base + "/%zz.png",
		"network":                      "https://example.test/image.png",
	} {
		if allowed, _, reason := allowRenderRequest(request, root, input); allowed {
			t.Fatalf("%s request %s was accepted", name, request)
		} else if reason == "" {
			t.Fatalf("%s request %s was rejected without a reason", name, request)
		}
	}
}
