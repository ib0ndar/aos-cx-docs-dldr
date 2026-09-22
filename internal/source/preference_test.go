package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

func TestHPEPreferredWholeDocumentPDFAndFallbacks(t *testing.T) {
	const id = "sd-preference-en_us"
	export := hpeAPI + id + "/exportpdf?exportType=all&release=10.18"
	document := hpeDoc(id)
	document.URL += "&release=10.18&page=GUID-ignored.html"

	t.Run("available", func(t *testing.T) {
		f := hpePreferenceFixture(id)
		body := testutil.PDF("HPE whole document")
		f.set(export, string(body), "application/pdf")
		plan, err := LoadPlanWithPreference(context.Background(), f, document, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Document.Kind != "pdf" || plan.PDFURL != export || !plan.PDFVerified ||
			!plan.PDFPreferred || plan.PDFOrigin != PDFOriginHPEExportAll ||
			len(plan.Topics) != 0 || plan.Inventory == nil || plan.Inventory.RootURL != export {
			t.Fatalf("incorrect preferred HPE PDF plan: %+v", plan)
		}
		if len(f.calls) != 2 || f.calls[0] != hpeAPI+id+"?ignorePayload=true&release=10.18" ||
			f.calls[1] != export {
			t.Fatalf("HPE preference requested anything except front and export-all: %v", f.calls)
		}
	})

	t.Run("same-document-redirect", func(t *testing.T) {
		f := hpePreferenceFixture(id)
		final := hpeAPI + id + "/downloads/original.pdf?release=10.18"
		body := testutil.PDF("redirected HPE whole document")
		f.responses[export] = model.Resource{
			URL: final, Status: 200,
			Headers: http.Header{"Content-Type": {"application/pdf"}, "Doc-Id": {id}},
			Body:    io.NopCloser(bytes.NewReader(body)),
		}
		plan, err := LoadPlanWithPreference(context.Background(), f, document, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.PDFURL != final || plan.PDFOrigin != PDFOriginHPEExportAll ||
			len(plan.Inputs) != 2 || plan.Inputs[1].RequestedURL != export ||
			plan.Inputs[1].FinalURL != final {
			t.Fatalf("same-document redirect identity was not preserved: %+v", plan)
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(*planFixture)
		match string
	}{
		{
			name:  "unavailable",
			setup: func(*planFixture) {},
			match: "missing fixture",
		},
		{
			name: "invalid-bytes",
			setup: func(f *planFixture) {
				f.set(export, "<html>not a PDF</html>", "application/pdf")
			},
			match: "expected complete original PDF bytes",
		},
		{
			name: "wrong-document-redirect",
			setup: func(f *planFixture) {
				body := testutil.PDF("foreign")
				f.responses[export] = model.Resource{
					URL:    hpeAPI + "other/exportpdf?exportType=all&release=10.18",
					Status: 200, Headers: http.Header{"Content-Type": {"application/pdf"}},
					Body: io.NopCloser(bytes.NewReader(body)),
				}
			},
			match: "redirected away",
		},
		{
			name: "wrong-document-header",
			setup: func(f *planFixture) {
				body := testutil.PDF("wrong header")
				f.responses[export] = model.Resource{
					URL: export, Status: 200,
					Headers: http.Header{"Content-Type": {"application/pdf"}, "Doc-Id": {"other"}},
					Body:    io.NopCloser(bytes.NewReader(body)),
				}
			},
			match: "document mismatch",
		},
		{
			name: "added-document-query",
			setup: func(f *planFixture) {
				body := testutil.PDF("changed query")
				f.responses[export] = model.Resource{
					URL: export + "&other=changed", Status: 200,
					Headers: http.Header{"Content-Type": {"application/pdf"}, "Doc-Id": {id}},
					Body:    io.NopCloser(bytes.NewReader(body)),
				}
			},
			match: "added document query identity",
		},
		{
			name: "topic-only-header",
			setup: func(f *planFixture) {
				body := testutil.PDF("topic only")
				f.responses[export] = model.Resource{
					URL: export, Status: 200,
					Headers: http.Header{"Content-Type": {"application/pdf"}, "Doc-Page-Name": {"GUID-topic.html"}},
					Body:    io.NopCloser(bytes.NewReader(body)),
				}
			},
			match: "unexpectedly identified topic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := hpePreferenceFixture(id)
			tc.setup(f)
			plan, err := LoadPlanWithPreference(context.Background(), f, document, false, true)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Document.Kind != "hpe" || len(plan.Topics) < 2 || plan.PDFVerified ||
				!strings.Contains(strings.Join(plan.Warnings, "\n"), tc.match) ||
				!strings.Contains(strings.Join(plan.Warnings, "\n"), "archived complete HTML instead") ||
				noticeContains(plan.Notices, tc.match) ||
				noticeContains(plan.Notices, "archived complete HTML instead") {
				t.Fatalf("HPE preference did not fall back durably: %+v", plan)
			}
			for _, call := range f.calls {
				if strings.Contains(call, "exportpdf") && call != export {
					t.Fatalf("topic/subtree or guessed export requested: %v", f.calls)
				}
			}

		})
	}
}

func hpePreferenceFixture(id string) *planFixture {
	f := hpeFixture(id)
	for _, suffix := range []string{"?ignorePayload=true", "?page=content.json"} {
		resource := f.responses[hpeAPI+id+suffix]
		delete(f.responses, hpeAPI+id+suffix)
		key := hpeAPI + id + suffix + "&release=10.18"
		resource.URL = key
		f.responses[key] = resource
	}
	return f
}

func TestPreferredPDFCancellationDoesNotFallBack(t *testing.T) {
	const id = "sd-cancel-en_us"
	base := hpeFixture(id)
	fetcher := preferenceErrorFetcher{
		base: base,
		at:   hpeAPI + id + "/exportpdf?exportType=all",
		err:  context.Canceled,
	}
	if _, err := LoadPlanWithPreference(context.Background(), fetcher, hpeDoc(id), false, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("preference cancellation was hidden by HTML fallback: %v", err)
	}
}

type preferenceErrorFetcher struct {
	base *planFixture
	at   string
	err  error
}

func (f preferenceErrorFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	if raw == f.at {
		return model.Resource{}, f.err
	}
	return f.base.Get(ctx, raw, refresh)
}

func TestStaticPreferredOriginalPDFRequiresOneActiveAdvertisement(t *testing.T) {
	const (
		home  = "https://publisher.example.test/guide/index.html"
		topic = "https://publisher.example.test/guide/topic.html"
		pdf   = "https://publisher.example.test/guide/original.pdf"
		other = "https://publisher.example.test/guide/alternate.pdf"
	)
	staticDoc := document("static")
	staticDoc.URL = home
	html := func(links string) string {
		return `<html><head>` + links + `</head><body><nav id="toc"><ul>` +
			`<li><a href="topic.html">Topic</a></li></ul></nav></body></html>`
	}

	t.Run("unique", func(t *testing.T) {
		f := &planFixture{responses: map[string]model.Resource{}}
		f.set(home, html(`<link rel="alternate" type="application/pdf" href="original.pdf">`), "text/html")
		body := testutil.PDF("static original")
		f.set(pdf, string(body), "application/pdf")
		plan, err := LoadPlanWithPreference(context.Background(), f, staticDoc, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if plan.PDFURL != pdf || plan.PDFOrigin != PDFOriginSourceAdvertised ||
			!plan.PDFPreferred || !plan.PDFVerified || len(f.calls) != 2 {
			t.Fatalf("unique advertised PDF not selected: plan=%+v calls=%v", plan, f.calls)
		}
	})

	for _, tc := range []struct {
		name, links string
		addInvalid  bool
		warning     string
	}{
		{"none", `<a href="archive.zip" download>Download archive</a>`, false, "does not advertise"},
		{"malformed", `<link rel="alternate" type="application/pdf" href="javascript:download()">`,
			false, "unsupported publisher URL"},
		{"ambiguous",
			`<link rel="alternate" type="application/pdf" href="original.pdf">` +
				`<a href="alternate.pdf">Download PDF</a>`,
			false, "multiple possible PDFs"},
		{"invalid", `<link rel="alternate" type="application/pdf" href="original.pdf">`,
			true, "expected complete original PDF bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &planFixture{responses: map[string]model.Resource{}}
			f.set(home, html(tc.links), "text/html")
			f.set(topic, `<main><h1>Topic</h1></main>`, "text/html")
			if tc.addInvalid {
				f.set(pdf, "<html>not a PDF</html>", "application/pdf")
			}
			plan, err := LoadPlanWithPreference(context.Background(), f, staticDoc, false, true)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Document.Kind != "static" || len(plan.Topics) != 2 ||
				!strings.Contains(strings.Join(plan.Warnings, "\n"), tc.warning) ||
				noticeContains(plan.Notices, tc.warning) {
				t.Fatalf("static preference fallback missing: %+v", plan)
			}
			if tc.name == "ambiguous" {
				for _, call := range f.calls {
					if call == pdf || call == other {
						t.Fatalf("ambiguous PDF advertisement was fetched: %v", f.calls)
					}
				}
			}
		})
	}
}

func TestFlarePreferredOriginalPDFRequiresUniqueFrontAdvertisement(t *testing.T) {
	pdf := planRoot + "PDF/original.pdf"
	f := flareFixture()
	f.set(planHome, `<html data-mc-path-to-help-system="../"><head>`+
		`<link rel="alternate" type="application/pdf" href="../PDF/original.pdf">`+
		`</head><body><a href="contents.htm">Table of Contents</a></body></html>`, "text/html")
	body := testutil.PDF("Flare original")
	f.set(pdf, string(body), "application/pdf")
	plan, err := LoadPlanWithPreference(context.Background(), f, document("flare"), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PDFURL != pdf || plan.PDFOrigin != PDFOriginSourceAdvertised ||
		!plan.PDFPreferred || !plan.PDFVerified || len(f.calls) != 2 {
		t.Fatalf("unique Flare PDF advertisement was not selected: plan=%+v calls=%v", plan, f.calls)
	}

	f = flareFixture()
	f.set(planHome, `<html data-mc-path-to-help-system="../"><head>`+
		`<link rel="alternate" type="application/pdf" href="../PDF/original.pdf">`+
		`<a href="../PDF/other.pdf">Download PDF</a>`+
		`</head><body><a href="contents.htm">Table of Contents</a></body></html>`, "text/html")
	plan, err = LoadPlanWithPreference(context.Background(), f, document("flare"), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Document.Kind != "flare" || len(plan.Topics) != 5 ||
		!strings.Contains(strings.Join(plan.Warnings, "\n"), "multiple possible PDFs") ||
		noticeContains(plan.Notices, "multiple possible PDFs") {
		t.Fatalf("ambiguous Flare advertisements did not retain HTML: %+v", plan)
	}
	for _, call := range f.calls {
		if strings.HasSuffix(call, ".pdf") {
			t.Fatalf("ambiguous Flare PDF was fetched: %v", f.calls)
		}
	}
}

func noticeContains(notices []model.Notice, text string) bool {
	for _, notice := range notices {
		if strings.Contains(notice.Message, text) {
			return true
		}
	}
	return false
}

type probeTransport struct {
	resource model.Resource
	err      error
	request  model.Request
}

func (p *probeTransport) Download(_ context.Context, request model.Request, consume func(model.Resource) error) (model.Resource, error) {
	p.request = request
	if p.err != nil {
		return model.Resource{}, p.err
	}
	if err := consume(p.resource); err != nil {
		return model.Resource{}, err
	}
	p.resource.Body = http.NoBody
	return p.resource, nil
}

type refreshRecordingFetcher struct {
	base      *planFixture
	calls     []string
	refreshes []bool
}

func (f *refreshRecordingFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	f.calls = append(f.calls, raw)
	f.refreshes = append(f.refreshes, refresh)
	return f.base.Get(ctx, raw, refresh)
}

func TestFlareAvailabilityUsesOnlyFreshFrontAndBodyFreeCandidate(t *testing.T) {
	pdf := planRoot + "PDF/original.pdf"
	front := func(advertisements string) string {
		return `<html data-mc-path-to-help-system="../"><head>` + advertisements +
			`</head><body><a href="contents.htm">Table of Contents</a></body></html>`
	}
	makeFetcher := func(advertisements string) *refreshRecordingFetcher {
		fixture := flareFixture()
		fixture.set(planHome, front(advertisements), "text/html")
		return &refreshRecordingFetcher{base: fixture}
	}

	t.Run("unique-positive", func(t *testing.T) {
		fetcher := makeFetcher(`<link rel="alternate" type="application/pdf" href="../PDF/original.pdf">`)
		transport := &probeTransport{resource: model.Resource{
			URL: pdf, Status: http.StatusOK,
			Headers: http.Header{"Content-Type": {"application/pdf"}},
			Body:    http.NoBody,
		}}
		availability, err := ProbePreferredPDFAvailability(
			context.Background(), fetcher, transport, document("flare"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !availability.Checked || !availability.Available ||
			availability.URL != pdf || availability.FinalURL != pdf ||
			availability.Origin != PDFOriginSourceAdvertised ||
			transport.request.Method != http.MethodHead || transport.request.URL != pdf {
			t.Fatalf("unique source advertisement was not body-free verified: result=%+v request=%+v",
				availability, transport.request)
		}
		if len(fetcher.calls) != 1 || fetcher.calls[0] != planHome ||
			len(fetcher.refreshes) != 1 || !fetcher.refreshes[0] {
			t.Fatalf("availability scan fetched more than the fresh exact front: calls=%v refresh=%v",
				fetcher.calls, fetcher.refreshes)
		}
	})

	for _, tc := range []struct {
		name, advertisements string
	}{
		{"none", ""},
		{"ambiguous", `<link rel="alternate" type="application/pdf" href="../PDF/original.pdf">` +
			`<a href="../PDF/other.pdf">Download PDF</a>`},
		{"foreign", `<link rel="alternate" type="application/pdf" href="https://other.example/guide.pdf">`},
		{"inactive", `<span href="../PDF/original.pdf">PDF</span>`},
		{"bare-link", `<a href="../PDF/original.pdf">Reference</a>`},
		{"bare-non-pdf-download", `<a href="../bundle.zip" download>Download archive</a>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := makeFetcher(tc.advertisements)
			transport := &probeTransport{}
			availability, err := ProbePreferredPDFAvailability(
				context.Background(), fetcher, transport, document("flare"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !availability.Checked || availability.Available || transport.request.URL != "" ||
				len(fetcher.calls) != 1 || !fetcher.refreshes[0] {
				t.Fatalf("ineligible advertisement became a PDF offer: result=%+v request=%+v calls=%v",
					availability, transport.request, fetcher.calls)
			}
		})
	}

	t.Run("non-pdf-head", func(t *testing.T) {
		fetcher := makeFetcher(`<a href="../PDF/original.pdf">Download PDF</a>`)
		transport := &probeTransport{resource: model.Resource{
			URL: pdf, Status: http.StatusOK,
			Headers: http.Header{"Content-Type": {"text/html"}},
			Body:    http.NoBody,
		}}
		availability, err := ProbePreferredPDFAvailability(
			context.Background(), fetcher, transport, document("flare"),
		)
		if err != nil || !availability.Checked || availability.Available ||
			!strings.Contains(availability.Reason, "not application/pdf") {
			t.Fatalf("non-PDF HEAD became available: result=%+v err=%v", availability, err)
		}
	})

	t.Run("wrong-redirect", func(t *testing.T) {
		fetcher := makeFetcher(`<a href="../PDF/original.pdf">Download PDF</a>`)
		transport := &probeTransport{resource: model.Resource{
			URL: "https://other.example/original.pdf", Status: http.StatusOK,
			Headers: http.Header{"Content-Type": {"application/pdf"}},
			Body:    http.NoBody,
		}}
		if _, err := ProbePreferredPDFAvailability(
			context.Background(), fetcher, transport, document("flare"),
		); err == nil || !strings.Contains(err.Error(), "outside the publisher origin") {
			t.Fatalf("wrong redirect was offered: %v", err)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		fetcher := makeFetcher(`<a href="../PDF/original.pdf">Download PDF</a>`)
		transport := &probeTransport{err: context.Canceled}
		if _, err := ProbePreferredPDFAvailability(
			context.Background(), fetcher, transport, document("flare"),
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("availability cancellation was hidden: %v", err)
		}
	})
}

func TestHPEAvailabilityProbeIsBodyFreeAndIdentityChecked(t *testing.T) {
	const id = "sd-probe-en_us"
	document := hpeDoc(id)
	export := hpeAPI + id + "/exportpdf?exportType=all"
	transport := &probeTransport{resource: model.Resource{
		URL: export, Status: 200,
		Headers: http.Header{"Content-Type": {"application/pdf;charset=UTF-8"}, "Doc-Id": {id}},
		Body:    http.NoBody,
	}}
	availability, err := ProbePreferredPDFAvailability(context.Background(), nil, transport, document)
	if err != nil {
		t.Fatal(err)
	}
	if transport.request.Method != http.MethodHead || transport.request.URL != export ||
		!availability.Checked || !availability.Available || availability.Origin != PDFOriginHPEExportAll {
		t.Fatalf("incorrect body-free availability result: request=%+v result=%+v", transport.request, availability)
	}

	transport.resource.Headers.Set("Doc-Id", "other")
	if _, err := ProbePreferredPDFAvailability(context.Background(), nil, transport, document); err == nil {
		t.Fatal("wrong-document availability response was advertised")
	}

	transport.err = &fetch.StatusError{URL: export, Status: http.StatusNotFound}
	availability, err = ProbePreferredPDFAvailability(context.Background(), nil, transport, document)
	if err != nil || !availability.Checked || availability.Available {
		t.Fatalf("definitive unavailable HEAD was not represented: result=%+v err=%v", availability, err)
	}
}
