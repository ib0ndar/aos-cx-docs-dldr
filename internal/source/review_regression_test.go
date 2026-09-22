package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

func minimalFlare(chunk string) (*planFixture, model.Document) {
	root := "https://publisher.example.test/guide/"
	home := root + "Content/home.htm"
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(home, `<html data-mc-path-to-help-system="../"><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul></html>`, "text/html")
	f.set(root+"Data/Tocs/Guide.js", `define({numchunks:1,prefix:'Chunk',tree:{n:[{i:0,c:0}]}});`, "application/javascript")
	f.set(root+"Data/Tocs/Chunk0.js", chunk, "application/javascript")
	return f, model.Document{ID: "guide", Title: "Guide", Platform: "6300", Version: "10.16", Kind: "flare", URL: home}
}

func TestFlareAdjacentCommentsPreserveLineContinuationURL(t *testing.T) {
	chunk := "define({/**//**/'/Content/a\\" + "\u2028" + "b.htm':{i:[0],t:['Topic'],b:['']}});"
	f, doc := minimalFlare(chunk)
	plan, err := LoadPlan(context.Background(), f, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Topics) != 2 || plan.Topics[1].URL != "https://publisher.example.test/guide/Content/ab.htm" {
		t.Fatalf("line continuation became a different source URL: %+v", plan.Topics)
	}
}

func TestFlareTitleNULLookaheadDoesNotLeakToLaterDigit(t *testing.T) {
	f, doc := minimalFlare(`define({'/Content/a.htm':{i:[0],t:['\0a1'],b:['']}});`)
	plan, err := LoadPlan(context.Background(), f, doc)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Topics[1].Title != "\x00a1" {
		t.Fatalf("NUL title changed: %q", plan.Topics[1].Title)
	}
}

func TestHPEAdvertisedPDFRequiresEveryIdentityToMatch(t *testing.T) {
	id := "doc-A"
	valid := string(testutil.PDF("original"))
	for _, tc := range []struct {
		name, advertised, final, responseID string
		expectFetch                         bool
	}{
		{"conflicting-path-query", hpeAPI + "doc-B/original.pdf?docId=doc-A", "", "", false},
		{"multiple-query-identities", hpeAPI + "doc-A/original.pdf?docId=doc-A&docId=doc-B", "", "", false},
		{"conflicting-response-header", hpeAPI + "doc-A/original.pdf", "", "doc-B", true},
		{"conflicting-final-path", hpeAPI + "doc-A/original.pdf", hpeAPI + "doc-B/original.pdf?docId=doc-A", "", true},
		{"legitimate-no-header", hpeAPI + "doc-A/original.pdf", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hpeFixture(id)
			f.set(hpeAPI+id+"?ignorePayload=true",
				`<html><body><embed type="application/pdf" src="`+tc.advertised+`"></body></html>`, "text/html")
			if tc.expectFetch || tc.name == "conflicting-path-query" {
				final := tc.final
				if final == "" {
					final = tc.advertised
				}
				headers := http.Header{"Content-Type": {"application/pdf"}}
				if tc.responseID != "" {
					headers.Set("Doc-Id", tc.responseID)
				}
				f.responses[tc.advertised] = model.Resource{URL: final, Status: 200, Headers: headers,
					Body: io.NopCloser(strings.NewReader(valid))}
			}
			plan, err := LoadPlan(context.Background(), f, hpeDoc(id))
			if tc.name == "legitimate-no-header" {
				if err != nil || !plan.PDFVerified || plan.PDFURL != tc.advertised {
					t.Fatalf("legitimate advertised PDF rejected: %+v %v", plan, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("conflicting identity accepted as selected document: %+v", plan)
			}
			wantCalls := 1
			if tc.expectFetch {
				wantCalls = 2
			}
			if len(f.calls) != wantCalls {
				t.Fatalf("unexpected PDF fetch behavior: calls=%v", f.calls)
			}
		})
	}
}

func catalogueHTML(title []byte, meta, headerCharset string, bom bool) model.Resource {
	body := []byte{}
	if bom {
		body = append(body, 0xef, 0xbb, 0xbf)
	}
	body = append(body, []byte("<html><head>"+meta+`</head><select id="platform"><option>6300</option></select>
<select id="ver"><option>10.16</option></select><div id="menu1"><table><tr>
<td id="guide" onclick="openFile('HTML','guide','aoscx')">`)...)
	body = append(body, title...)
	body = append(body, []byte(`</td></tr></table></div></html>`)...)
	contentType := "text/html"
	if headerCharset != "" {
		contentType += "; charset=" + headerCharset
	}
	return model.Resource{URL: model.PortalURL, Status: 200, Headers: http.Header{"Content-Type": {contentType}},
		Body: io.NopCloser(bytes.NewReader(body))}
}

func TestHTMLDecodingHonorsBOMHeaderAndMetaCharset(t *testing.T) {
	base := strings.TrimSuffix(model.PortalURL, "aoscx.html") + "json/aoscx/guide.json"
	for _, tc := range []struct {
		name     string
		resource model.Resource
	}{
		{"meta", catalogueHTML([]byte{'C', 'a', 'f', 0xe9}, `<meta charset="windows-1252">`, "", false)},
		{"http-equiv", catalogueHTML([]byte{'C', 'a', 'f', 0xe9},
			`<meta http-equiv="content-type" content="text/html; charset=windows-1252">`, "", false)},
		{"header-over-meta", catalogueHTML([]byte("Café"), `<meta charset="windows-1252">`, "utf-8", false)},
		{"bom-over-header", catalogueHTML([]byte("Café"),
			`<meta charset="windows-1252">`, "windows-1252", true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapping := model.Resource{URL: base, Status: 200, Headers: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"10.16":{"6300":"guide"}}`))}
			f := &planFixture{responses: map[string]model.Resource{model.PortalURL: tc.resource, base: mapping}}
			catalog, err := LoadCatalog(context.Background(), f, model.PortalURL)
			if err != nil {
				t.Fatal(err)
			}
			if catalog.Guides[0].Title != "Café" {
				t.Fatalf("HTML charset priority produced %q", catalog.Guides[0].Title)
			}
		})
	}
}

func TestNonUTF8StaticTOCLabelAndHTMLCancellation(t *testing.T) {
	home := "https://publisher.example.test/book/index.html"
	body := append([]byte(`<html><head><meta charset="windows-1252"></head><nav id="toc"><ul><li><a href="topic.html">Caf`),
		0xe9)
	body = append(body, []byte(`</a></li></ul></nav></html>`)...)
	f := &planFixture{responses: map[string]model.Resource{home: {
		URL: home, Status: 200, Headers: http.Header{"Content-Type": {"text/html"}},
		Body: io.NopCloser(bytes.NewReader(body)),
	}}}
	doc := model.Document{ID: "guide", Title: "Guide", Platform: "6300", Version: "10.16", Kind: "static", URL: home}
	plan, err := LoadPlan(context.Background(), f, doc)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Topics[1].Title != "Café" {
		t.Fatalf("static TOC label decoded as %q", plan.Topics[1].Title)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadPlan(ctx, f, doc); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled HTML decoding succeeded: %v", err)
	}
}
