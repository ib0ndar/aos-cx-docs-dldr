package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

type availabilityFetcher struct {
	resource model.Resource
	err      error
	calls    []string
}

func (f *availabilityFetcher) Get(_ context.Context, raw string, refresh bool) (model.Resource, error) {
	f.calls = append(f.calls, raw)
	if !refresh {
		return model.Resource{}, errors.New("availability source was not refreshed")
	}
	if f.err != nil {
		return model.Resource{}, f.err
	}
	return f.resource, nil
}

func TestMappedHTMLSourceAvailabilityClassification(t *testing.T) {
	document := model.Document{
		ID: "guide", Title: "Guide", Kind: "static",
		URL: "https://publisher.example/guide/index.html",
	}
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fetcher := &availabilityFetcher{err: &fetch.StatusError{URL: document.URL, Status: status}}
			sourceStatus, pdfStatus, err := ProbeDocumentAvailability(
				context.Background(), fetcher, &probeTransport{}, document, true,
			)
			if err != nil || !sourceStatus.Checked || sourceStatus.Available ||
				sourceStatus.Reason != fmt.Sprintf("mapped source HTTP %d", status) ||
				pdfStatus.Checked || len(fetcher.calls) != 1 {
				t.Fatalf("definitive source absence misclassified: source=%+v pdf=%+v err=%v calls=%v",
					sourceStatus, pdfStatus, err, fetcher.calls)
			}
		})
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"timeout", context.DeadlineExceeded},
		{"server-error", &fetch.StatusError{URL: document.URL, Status: http.StatusServiceUnavailable}},
		{"validation", errors.New("publisher front did not contain valid HTML")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetcher := &availabilityFetcher{err: tc.err}
			sourceStatus, _, err := ProbeDocumentAvailability(
				context.Background(), fetcher, &probeTransport{}, document, true,
			)
			if err != nil || sourceStatus.Checked || sourceStatus.Available ||
				!strings.Contains(sourceStatus.Reason, tc.err.Error()) {
				t.Fatalf("ambiguous source failure became definitive: source=%+v err=%v", sourceStatus, err)
			}
		})
	}

	fetcher := &availabilityFetcher{resource: model.Resource{
		URL: document.URL, Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"text/html"}},
		Body:    io.NopCloser(strings.NewReader(`<html><body><main>Guide</main></body></html>`)),
	}}
	sourceStatus, pdfStatus, err := ProbeDocumentAvailability(
		context.Background(), fetcher, &probeTransport{}, document, true,
	)
	if err != nil || !sourceStatus.Checked || !sourceStatus.Available ||
		!pdfStatus.Checked || pdfStatus.Available {
		t.Fatalf("valid mapped HTML front was not available: source=%+v pdf=%+v err=%v",
			sourceStatus, pdfStatus, err)
	}
}

type availabilityProbeTransport struct {
	resource model.Resource
	err      error
	requests []model.Request
}

func (t *availabilityProbeTransport) Download(
	_ context.Context,
	request model.Request,
	consume func(model.Resource) error,
) (model.Resource, error) {
	t.requests = append(t.requests, request)
	if t.err != nil {
		return model.Resource{}, t.err
	}
	if err := consume(t.resource); err != nil {
		return model.Resource{}, err
	}
	return t.resource, nil
}

func TestDirectMappedPDFSourceUsesOnlyBodyFreeHEAD(t *testing.T) {
	document := model.Document{
		ID: "cli", Title: "CLI", Kind: "pdf",
		URL: "https://publisher.example/cli.pdf",
	}
	for _, tc := range []struct {
		name      string
		resource  model.Resource
		err       error
		checked   bool
		available bool
	}{
		{"positive", model.Resource{URL: document.URL, Status: http.StatusOK, Body: http.NoBody}, nil, true, true},
		{"not-found", model.Resource{}, &fetch.StatusError{URL: document.URL, Status: http.StatusNotFound}, true, false},
		{"gone", model.Resource{}, &fetch.StatusError{URL: document.URL, Status: http.StatusGone}, true, false},
		{"timeout", model.Resource{}, context.DeadlineExceeded, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &availabilityProbeTransport{resource: tc.resource, err: tc.err}
			sourceStatus, pdfStatus, err := ProbeDocumentAvailability(
				context.Background(), nil, transport, document, true,
			)
			if err != nil || sourceStatus.Checked != tc.checked ||
				sourceStatus.Available != tc.available || pdfStatus.Checked ||
				len(transport.requests) != 1 ||
				transport.requests[0].Method != http.MethodHead ||
				transport.requests[0].URL != document.URL {
				t.Fatalf("direct PDF source probe mismatch: source=%+v pdf=%+v err=%v requests=%+v",
					sourceStatus, pdfStatus, err, transport.requests)
			}
		})
	}
}

func TestHPESourceAndOptionalExportAvailabilityStaySeparate(t *testing.T) {
	const id = "sd-availability-en_us"
	document := hpeDoc(id)
	fetcher := &availabilityFetcher{resource: model.Resource{
		URL: hpeAPI + id + "?ignorePayload=true", Status: http.StatusOK,
		Headers: http.Header{"Content-Type": {"multiPage;charset=UTF-8"}, "Doc-Id": {id}},
		Body: io.NopCloser(strings.NewReader(
			`<html><body><main class="ditasrc"><h1>Guide</h1></main></body></html>`,
		)),
	}}
	export := hpeAPI + id + "/exportpdf?exportType=all"
	transport := &availabilityProbeTransport{err: &fetch.StatusError{
		URL: export, Status: http.StatusNotFound,
	}}
	sourceStatus, pdfStatus, err := ProbeDocumentAvailability(
		context.Background(), fetcher, transport, document, true,
	)
	if err != nil || !sourceStatus.Checked || !sourceStatus.Available ||
		!pdfStatus.Checked || pdfStatus.Available ||
		pdfStatus.Origin != PDFOriginHPEExportAll ||
		len(fetcher.calls) != 1 || len(transport.requests) != 1 ||
		transport.requests[0].Method != http.MethodHead {
		t.Fatalf("HPE source/export states were conflated: source=%+v pdf=%+v err=%v calls=%v requests=%+v",
			sourceStatus, pdfStatus, err, fetcher.calls, transport.requests)
	}
}
