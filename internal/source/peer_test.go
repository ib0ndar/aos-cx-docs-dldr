package source

import (
	"context"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

// peerCatalog reproduces the publisher's CoPP mappings for 10.15 to 10.17,
// including the 10.16 mapping for 8325 that names a document it does not serve.
func peerCatalog() model.Catalog {
	shared := func(target string, platforms ...string) map[string]string {
		m := map[string]string{}
		for _, platform := range platforms {
			m[platform] = target
		}
		return m
	}
	with := func(m map[string]string, platform, target string) map[string]string {
		m[platform] = target
		return m
	}
	return model.Catalog{
		Platforms: []string{"6100", "8100", "8320", "8325", "8360", "8400", "9300", "10000", "10040"},
		Versions:  []string{"10.17", "10.16", "10.15"},
		RouteRoot: "https://example.test/techdocs/",
		Guides: []model.Guide{{ID: "copp", Title: "CoPP Guide", Mappings: map[string]map[string]string{
			"10.15": with(shared("copp_8100-83xx-9300-10000", "8100", "8320", "8325", "8360", "9300", "10000"), "8400", "copp_8400"),
			"10.16": with(with(shared("copp_8100-83xx-9300-10000", "8100", "8320", "8360", "9300", "10000"),
				"8325", "acopp_8100-83xx-9300-10000"), "8400", "copp_8400"),
			"10.17": with(shared("CoPP_8100-83xx-93xx-100xx", "8100", "8320", "8325", "8360", "9300", "10000", "10040"), "8400", "CoPP_8400"),
		}}},
	}
}

func mappedDocument(t *testing.T, c model.Catalog, platform, version string) model.Document {
	t.Helper()
	documents, diagnostics, err := ResolveDocuments(c, platform, version)
	if err != nil || len(diagnostics) > 0 || len(documents) != 1 {
		t.Fatalf("resolve %s/%s: documents=%v diagnostics=%v err=%v", platform, version, documents, diagnostics, err)
	}
	return documents[0]
}

func TestReplacementsFollowPeersThenReleasePDFs(t *testing.T) {
	c := peerCatalog()
	mapped := mappedDocument(t, c, "8325", "10.16")
	replacements := FindReplacements(c, mapped)
	root := "https://example.test/techdocs/AOS-CX/10.16/"
	want := []struct{ url, kind, origin, target string }{
		{root + "HTML/copp_8100-83xx-9300-10000/Content/home.htm", "flare", RouteOriginPeerMapping, "copp_8100-83xx-9300-10000"},
		{root + "PDF/acopp_8100-83xx-9300-10000.pdf", "pdf", RouteOriginReleasePDF, "acopp_8100-83xx-9300-10000"},
		{root + "PDF/copp_8100-83xx-9300-10000.pdf", "pdf", RouteOriginPeerReleasePDF, "copp_8100-83xx-9300-10000"},
	}
	if len(replacements) != len(want) {
		t.Fatalf("replacements = %+v", replacements)
	}
	for i, w := range want {
		r := replacements[i]
		if r.Document.URL != w.url || r.Document.Kind != w.kind || r.Document.RouteOrigin != w.origin || r.Target != w.target ||
			r.Document.ID != "copp" || r.Document.Title != "CoPP Guide" || r.Document.Platform != "8325" ||
			r.Document.Version != "10.16" || r.Mapped.URL != mapped.URL {
			t.Fatalf("replacement %d = %+v, want %+v", i, r, w)
		}
	}
	for _, i := range []int{0, 2} {
		if !slices.Equal(replacements[i].Peers, []string{"8100", "8320", "8360", "9300", "10000"}) ||
			!slices.Equal(replacements[i].Releases, []string{"10.15", "10.17"}) {
			t.Fatalf("replacement %d peers=%v releases=%v", i, replacements[i].Peers, replacements[i].Releases)
		}
	}
	if len(replacements[1].Peers) != 0 {
		t.Fatalf("own target listed peers: %+v", replacements[1])
	}
	note := replacements[0].Note()
	for _, part := range []string{"acopp_8100-83xx-9300-10000", want[0].url, "8100, 8320, 8360, 9300, 10000", "10.15 and 10.17"} {
		if !strings.Contains(note, part) {
			t.Fatalf("note %q lacks %q", note, part)
		}
	}
	replacements[2].Title = "AOS-CX 10.16.xxxx CoPP Guide (8100, 8320, 8325)"
	if note := replacements[2].Note(); !strings.Contains(note, "PDF of copp_8100-83xx-9300-10000") ||
		!strings.Contains(note, replacements[2].Title) {
		t.Fatalf("PDF note %q", note)
	}
}

func TestReplacementsNeedAgreeingPeers(t *testing.T) {
	peerOf := func(replacements []Replacement) []Replacement {
		return slices.DeleteFunc(slices.Clone(replacements), func(r Replacement) bool {
			return r.Document.RouteOrigin != RouteOriginPeerMapping
		})
	}
	// A platform with its own document in the neighbouring releases has no
	// peers to follow, only its release PDF.
	c := peerCatalog()
	if replacements := FindReplacements(c, mappedDocument(t, c, "8400", "10.16")); len(replacements) != 1 ||
		replacements[0].Document.URL != "https://example.test/techdocs/AOS-CX/10.16/PDF/copp_8400.pdf" {
		t.Fatalf("platform without peers: %+v", replacements)
	}
	// A platform that already maps to its peers' document needs none.
	if peers := peerOf(FindReplacements(c, mappedDocument(t, c, "8320", "10.16"))); len(peers) != 0 {
		t.Fatalf("platform sharing its peers' target got a replacement: %+v", peers)
	}
	// Peers that disagree in the release leave no single replacement.
	c = peerCatalog()
	c.Guides[0].Mappings["10.16"]["9300"] = "copp_9300"
	if peers := peerOf(FindReplacements(c, mappedDocument(t, c, "8325", "10.16"))); len(peers) != 0 {
		t.Fatalf("disagreeing peers gave a replacement: %+v", peers)
	}
	// Each neighbouring release that maps the platform must show peers.
	c = peerCatalog()
	c.Guides[0].Mappings["10.17"]["8325"] = "CoPP_8325"
	if peers := peerOf(FindReplacements(c, mappedDocument(t, c, "8325", "10.16"))); len(peers) != 0 {
		t.Fatalf("platform alone in 10.17 got a replacement: %+v", peers)
	}
	// One neighbouring release is enough at either end of the mappings.
	c = peerCatalog()
	delete(c.Guides[0].Mappings, "10.17")
	if peers := peerOf(FindReplacements(c, mappedDocument(t, c, "8325", "10.16"))); len(peers) != 1 {
		t.Fatal("no replacement with only the earlier release as evidence")
	}
	// Releases without a PDF route have no release PDF: Oxygen releases.
	c = peerCatalog()
	c.Guides[0].Mappings["10.16"]["8325"] = "copp_8100-83xx-9300-10000"
	c.Guides[0].Mappings["10.17"]["8325"] = "aCoPP"
	if replacements := FindReplacements(c, mappedDocument(t, c, "8325", "10.17")); len(replacements) != 1 ||
		replacements[0].Document.RouteOrigin != RouteOriginPeerMapping {
		t.Fatalf("Oxygen release: %+v", replacements)
	}
	// A guide whose mappings failed to load offers nothing.
	c = peerCatalog()
	c.Guides[0].Error = "mapping unavailable"
	if replacements := FindReplacements(c, model.Document{ID: "copp", Platform: "8325", Version: "10.16"}); len(replacements) != 0 {
		t.Fatalf("guide with a mapping error gave a replacement: %+v", replacements)
	}
}

func TestPDFTitleMustNameTheGuide(t *testing.T) {
	for _, tc := range []struct {
		guide, origin, title string
		ok                   bool
	}{
		// The platform's own target: the title names the guide.
		{"Introduction to the Web UI Guide", RouteOriginReleasePDF, "AOS-CX Web UI Guide", true},
		{"ACLs and Classifiers Policy Guide", RouteOriginReleasePDF, "AOS-CX 10.16 ACLs and Classifier Policies Guide", true},
		{"CoPP Guide", RouteOriginReleasePDF, "AOS-CX 10.16 VSX Guide", false},
		{"Introduction to the Web UI Guide", RouteOriginReleasePDF, "AOS-CX Guide", false},
		{"CoPP Guide", RouteOriginReleasePDF, "", false},
		// The peers' target: the title also names the release and platform.
		{"CoPP Guide", RouteOriginPeerReleasePDF, "AOS-CX 10.16.xxxx CoPP Guide (8100, 8320, 8325, 8360, 93xx, 100xx Switch Series) ", true},
		{"CoPP Guide", RouteOriginPeerReleasePDF, "AOS-CX 10.15.xxxx CoPP Guide (8100, 8320, 8325)", false},
		{"CoPP Guide", RouteOriginPeerReleasePDF, "AOS-CX 10.16.xxxx CoPP Guide (8100, 83xx, 93xx)", false},
		{"CoPP Guide", RouteOriginPeerReleasePDF, "AOS-CX 10.16.xxxx VSX Guide (8325)", false},
		{"CoPP Guide", RouteOriginPeerReleasePDF, "AOS-CX 10.161 CoPP Guide 8325", false},
	} {
		d := model.Document{Title: tc.guide, Platform: "8325", Version: "10.16", RouteOrigin: tc.origin}
		if err := CheckPDFTitle(d, tc.title); (err == nil) != tc.ok {
			t.Errorf("%s %q for %q: err=%v, want ok=%v", tc.origin, tc.title, tc.guide, err, tc.ok)
		}
	}
}

type fileFetcher struct {
	t      *testing.T
	bodies map[string][]byte
}

func (f fileFetcher) Get(_ context.Context, raw string, refresh bool) (model.Resource, error) {
	if !refresh {
		f.t.Errorf("release PDF probe of %s was not fresh", raw)
	}
	body, ok := f.bodies[raw]
	if !ok {
		return model.Resource{}, &fetch.StatusError{URL: raw, Status: 404}
	}
	file, err := os.CreateTemp(f.t.TempDir(), "body")
	if err != nil {
		return model.Resource{}, err
	}
	if _, err := file.Write(body); err != nil {
		return model.Resource{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return model.Resource{}, err
	}
	headers := http.Header{"Content-Type": []string{"application/pdf"}}
	return model.Resource{URL: raw, Status: 200, Headers: headers, Body: file}, nil
}

func TestProbeReleasePDFRequiresItsTitle(t *testing.T) {
	d := model.Document{ID: "copp", Title: "CoPP Guide", Platform: "8325", Version: "10.16", Kind: "pdf",
		URL: "https://example.test/techdocs/AOS-CX/10.16/PDF/copp_8100-83xx-9300-10000.pdf", RouteOrigin: RouteOriginPeerReleasePDF}
	title := "AOS-CX 10.16.xxxx CoPP Guide (8100, 8320, 8325, 8360)"
	for name, tc := range map[string]struct {
		body      []byte
		available bool
		reason    string
	}{
		"identified":  {testutil.TitledPDF("copp", "("+title+")", false), true, ""},
		"other guide": {testutil.TitledPDF("vsx", "(AOS-CX 10.16 VSX Guide 8325)", false), false, "not identified as this guide"},
		"no title":    {testutil.PDF("untitled"), false, "not identified as this guide"},
		"not a PDF":   {[]byte("<html>not found</html>"), false, "not complete"},
		"not served":  {nil, false, "mapped source HTTP 404"},
	} {
		t.Run(name, func(t *testing.T) {
			bodies := map[string][]byte{}
			if tc.body != nil {
				bodies[d.URL] = tc.body
			}
			status, got, err := ProbeReleasePDF(context.Background(), fileFetcher{t, bodies}, d)
			if err != nil || !status.Checked || status.Available != tc.available || !strings.Contains(status.Reason, tc.reason) ||
				(tc.available && got != title) {
				t.Fatalf("status=%+v title=%q err=%v", status, got, err)
			}
		})
	}
}
