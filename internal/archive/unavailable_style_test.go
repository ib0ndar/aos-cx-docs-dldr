package archive

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestUnservedPageStylesheetIsOmitted(t *testing.T) {
	fonts := ordinaryGuideRoot + "oxygen-webhelp/css/fonts_wh.css?buildId=1"
	topicStyles := ordinaryGuideRoot + "oxygen-webhelp/css/topic.css?buildId=1"
	page := func(title string) []byte {
		return []byte(`<html><head>
<link rel="stylesheet" href="../../oxygen-webhelp/css/topic.css?buildId=1">
<link rel="stylesheet" href="../../oxygen-webhelp/css/fonts_wh.css?buildId=1">
</head><body><main><h1>` + title + `</h1><p class="used">Text</p></main></body></html>`)
	}
	archiveWith := func(t *testing.T, status int) (model.ArchiveResult, *archiveFixture, string) {
		t.Helper()
		fetcher := &archiveFixture{resources: map[string]model.Resource{
			ordinaryTopic:  fixtureResource(ordinaryTopic, "text/html", page("One")),
			ordinarySecond: fixtureResource(ordinarySecond, "text/html", page("Two")),
			topicStyles:    fixtureResource(topicStyles, "text/css", []byte(`.used{color:#123}`)),
			fonts:          statusResource(fonts, status),
		}, calls: map[string]int{}}
		output := t.TempDir()
		result, err := HTML(context.Background(), ordinaryStylePlan("static", ordinaryTopic, ordinarySecond),
			fetcher, output, false, 1<<20, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		return result, fetcher, output
	}

	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			result, fetcher, output := archiveWith(t, status)
			want := []model.StylesheetRecovery{{
				Kind: unavailableStylesheetKind, BrokenURL: fonts, HTTPStatus: status,
				TableStyleFamilies: []string{}, AffectedTopics: []string{ordinarySecond, ordinaryTopic},
			}}
			if result.Status != "complete" || len(result.Errors) != 0 ||
				!slices.EqualFunc(result.StylesheetRecoveries, want, func(a, b model.StylesheetRecovery) bool {
					return a.Kind == b.Kind && a.BrokenURL == b.BrokenURL && a.HTTPStatus == b.HTTPStatus &&
						slices.Equal(a.AffectedTopics, b.AffectedTopics)
				}) {
				t.Fatalf("unserved stylesheet was not omitted: %+v", result)
			}
			warned := 0
			for _, warning := range result.Warnings {
				if strings.Contains(warning, fonts) {
					warned++
				}
			}
			if warned != 1 || fetcher.calls[fonts] != 1 {
				t.Fatalf("warnings=%d requests=%d: %v", warned, fetcher.calls[fonts], result.Warnings)
			}
			if len(result.HTML.Assets) != 1 || result.HTML.Assets[0].URL != topicStyles {
				t.Fatalf("assets: %+v", result.HTML.Assets)
			}
			for _, topic := range result.HTML.Topics {
				body, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(topic.Path)))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), result.HTML.Assets[0].Path) || strings.Contains(string(body), "fonts_wh") {
					t.Fatalf("%s links the wrong stylesheets: %s", topic.Path, body)
				}
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		result, _, _ := archiveWith(t, http.StatusForbidden)
		if result.Status != "incomplete" || len(result.StylesheetRecoveries) != 0 ||
			!slices.ContainsFunc(result.Errors, func(message string) bool { return strings.Contains(message, fonts) }) {
			t.Fatalf("refused stylesheet did not fail closed: %+v", result)
		}
	})
}

func TestUnservedFullSizeImageLinkIsDropped(t *testing.T) {
	full := ordinaryGuideRoot + "GUID-7B522EDB-high.png"
	shown := ordinaryGuideRoot + "images/ifa-header.png"
	body := []byte(`<html><head></head><body><main><h1>Packet headers</h1>
<p><a class="xref" href="../../GUID-7B522EDB-high.png"><img src="../../images/ifa-header.png" alt="IFA header"></a></p>
<p><a class="xref" href="../../GUID-7B522EDB-high.png#zoom">Full size</a></p>
</main></body></html>`)
	archiveWith := func(t *testing.T, status int) (model.ArchiveResult, *archiveFixture, string) {
		t.Helper()
		fetcher := &archiveFixture{resources: map[string]model.Resource{
			ordinaryTopic: fixtureResource(ordinaryTopic, "text/html", body),
			shown:         fixtureResource(shown, "image/png", renderPNG),
			full:          statusResource(full, status),
		}, calls: map[string]int{}}
		output := t.TempDir()
		result, err := HTML(context.Background(), ordinaryStylePlan("static", ordinaryTopic),
			fetcher, output, false, 1<<20, 8<<20)
		if err != nil {
			t.Fatal(err)
		}
		return result, fetcher, output
	}

	t.Run("not found", func(t *testing.T) {
		result, fetcher, output := archiveWith(t, http.StatusNotFound)
		if result.Status != "complete" || len(result.Errors) != 0 || fetcher.calls[full] != 1 ||
			!slices.ContainsFunc(result.Warnings, func(warning string) bool {
				return strings.Contains(warning, "link to full-size image "+full+" removed")
			}) {
			t.Fatalf("unserved full-size image link was not dropped: calls=%d %+v", fetcher.calls[full], result)
		}
		page, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(result.HTML.Topics[0].Path)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(page), "high.png") || !strings.Contains(string(page), `alt="IFA header"`) ||
			!strings.Contains(string(page), "Full size") {
			t.Fatalf("the link was not dropped with its content kept: %s", page)
		}
	})

	t.Run("server error", func(t *testing.T) {
		result, _, _ := archiveWith(t, http.StatusInternalServerError)
		if result.Status != "incomplete" ||
			!slices.ContainsFunc(result.Errors, func(message string) bool { return strings.Contains(message, "linked asset") }) {
			t.Fatalf("failed full-size image did not fail closed: %+v", result)
		}
	})
}
