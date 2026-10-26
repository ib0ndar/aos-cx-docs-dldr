package library

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"path"
	"strings"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
)

//go:embed index.html
var indexHTML string

//go:embed search.js
var searchJS []byte

type indexEntry struct {
	Title  string
	Status string
	URL    string
	PDFURL string
}

type searchEntry struct {
	Guide string `json:"guide"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
}

type indexCopy struct {
	Description       string
	SearchLabel       string
	SearchPlaceholder string
	SearchEmpty       string
	LibraryLabel      string
	PDFNote           string
}

func describeIndex(manifest Manifest) indexCopy {
	// generatedCount counts HTML guides the library keeps only as their
	// generated PDF.
	htmlCount, pdfCount, generatedCount := 0, 0, 0
	for _, guide := range manifest.Guides {
		switch {
		case guide.Format == "html" && guide.HTMLRemoved:
			generatedCount++
		case guide.Format == "html":
			htmlCount++
		case guide.Format == "pdf":
			pdfCount++
		}
	}
	switch {
	case htmlCount > 0 && pdfCount+generatedCount > 0:
		description := "Downloaded HTML guides and original publisher PDFs are available offline."
		if generatedCount > 0 {
			description = "Downloaded HTML guides and PDFs are available offline."
		}
		return indexCopy{
			Description:       description,
			SearchLabel:       "Search downloaded guides, topics, and HTML text",
			SearchPlaceholder: "Guide, topic, or archived HTML text",
			SearchEmpty:       "No matching downloaded guide, topic, or HTML text.",
			LibraryLabel:      "native mixed-format library",
			PDFNote:           "PDF contents are not extracted or indexed.",
		}
	case htmlCount > 0:
		return indexCopy{
			Description:       "Downloaded HTML guides are available for offline browsing.",
			SearchLabel:       "Search downloaded HTML guides, topics, and text",
			SearchPlaceholder: "Guide, topic, or archived HTML text",
			SearchEmpty:       "No matching downloaded HTML guide, topic, or text.",
			LibraryLabel:      "native HTML library",
		}
	case pdfCount+generatedCount > 0:
		description := "Original publisher PDFs are saved without conversion."
		switch {
		case pdfCount > 0 && generatedCount > 0:
			description = "Original publisher PDFs and PDFs generated from downloaded HTML guides are saved offline."
		case generatedCount > 0:
			description = "PDFs generated from downloaded HTML guides are saved offline."
		}
		return indexCopy{
			Description:       description,
			SearchLabel:       "Search downloaded PDF titles",
			SearchPlaceholder: "Guide title",
			SearchEmpty:       "No matching downloaded PDF titles.",
			LibraryLabel:      "native PDF library",
			PDFNote:           "PDF contents are not extracted or indexed.",
		}
	default:
		return indexCopy{
			Description:       "No complete guides are currently available in this library.",
			SearchLabel:       "Search downloaded content",
			SearchPlaceholder: "No downloaded content",
			SearchEmpty:       "No downloaded content is available to search.",
			LibraryLabel:      "native library",
		}
	}
}

func jsonBytes(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	return append(data, '\n'), err
}

// guideSearchEntries lifts a guide's topic search records into the release
// search. Integrity checks prove search.json unchanged since archiving, not
// that its links are safe, so each record must name one of the guide's
// archived topic pages by a clean relative path before it becomes a link.
func (r *Run) guideSearchEntries(directory string, guide Guide, result model.ArchiveResult) ([]searchEntry, error) {
	var records []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Text  string `json:"text"`
	}
	if err := storage.ReadJSON(r.root, path.Join(directory, guide.Path, "search.json"), &records); err != nil {
		return nil, err
	}
	topics := map[string]bool{}
	if result.HTML != nil {
		for _, topic := range result.HTML.Topics {
			topics[topic.Path] = true
		}
	}
	entries := make([]searchEntry, 0, len(records))
	for _, record := range records {
		if !topics[record.URL] || storage.SafePath(record.URL) != nil || !strings.HasPrefix(record.URL, "pages/") {
			return nil, fmt.Errorf("search record in guide %s does not link to an archived topic page: %q", guide.Path, record.URL)
		}
		entries = append(entries, searchEntry{Guide: guide.Title, Title: record.Title,
			URL: (&url.URL{Path: path.Join(guide.Path, record.URL)}).EscapedPath(), Text: record.Text})
	}
	return entries, nil
}

func (r *Run) writeIndex(manifest Manifest, history []History) error {
	return r.writeIndexAt(r.stage, manifest, history)
}

func (r *Run) writeIndexAt(directory string, manifest Manifest, history []History) error {
	var entries []indexEntry
	search := []searchEntry{}
	for _, guide := range manifest.Guides {
		link := (&url.URL{Path: path.Join(guide.Path, "index.html")}).EscapedPath()
		text := "Offline HTML guide."
		if guide.Format == "pdf" {
			pdf := manifest.PDFGuides[guide.Path].PDF
			link = (&url.URL{Path: path.Join(guide.Path, pdf.Path)}).EscapedPath()
			text = ""
		} else if guide.HTMLRemoved {
			generated := manifest.HTMLGuides[guide.Path].HTML.GeneratedPDF
			link = (&url.URL{Path: path.Join(guide.Path, generated.Path)}).EscapedPath()
			text = ""
		} else if generated := manifest.HTMLGuides[guide.Path].HTML.GeneratedPDF; generated != nil &&
			generated.Status == "complete" {
			pdfURL := (&url.URL{Path: path.Join(guide.Path, generated.Path)}).EscapedPath()
			entries = append(entries, indexEntry{Title: guide.Title, Status: guide.Status, URL: link, PDFURL: pdfURL})
			search = append(search, searchEntry{Guide: guide.Title, Title: guide.Title, URL: link, Text: text})
			records, err := r.guideSearchEntries(directory, guide, manifest.HTMLGuides[guide.Path])
			if err != nil {
				return err
			}
			search = append(search, records...)
			continue
		}
		entries = append(entries, indexEntry{Title: guide.Title, Status: guide.Status, URL: link})
		search = append(search, searchEntry{Guide: guide.Title, Title: guide.Title, URL: link, Text: text})
		if guide.Format == "html" && !guide.HTMLRemoved {
			records, err := r.guideSearchEntries(directory, guide, manifest.HTMLGuides[guide.Path])
			if err != nil {
				return err
			}
			search = append(search, records...)
		}
	}
	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	if err := tmpl.Execute(&body, struct {
		Manifest Manifest
		Entries  []indexEntry
		Copy     indexCopy
	}{manifest, entries, describeIndex(manifest)}); err != nil {
		return err
	}
	searchData, err := json.Marshal(search)
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"index.html": body.Bytes(), "search.js": searchJS,
		"search-index.js": append(append([]byte("window.AOSCX_SEARCH = "), searchData...), ';', '\n'),
	} {
		if err := storage.Atomic(r.root, path.Join(directory, name), data); err != nil {
			return err
		}
	}
	if err := storage.WriteJSON(r.root, path.Join(directory, "history.json"), history); err != nil {
		return err
	}
	return storage.WriteJSON(r.root, path.Join(directory, "manifest.json"), manifest)
}
