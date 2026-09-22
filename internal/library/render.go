package library

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"html/template"
	"net/url"
	"path"

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
	htmlCount, pdfCount := 0, 0
	for _, guide := range manifest.Guides {
		switch guide.Format {
		case "html":
			htmlCount++
		case "pdf":
			pdfCount++
		}
	}
	switch {
	case htmlCount > 0 && pdfCount > 0:
		return indexCopy{
			Description:       "Downloaded HTML guides and original publisher PDFs are available offline.",
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
	case pdfCount > 0:
		return indexCopy{
			Description:       "Original publisher PDFs are saved without conversion.",
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
		} else if generated := manifest.HTMLGuides[guide.Path].HTML.GeneratedPDF; generated != nil &&
			generated.Status == "complete" {
			pdfURL := (&url.URL{Path: path.Join(guide.Path, generated.Path)}).EscapedPath()
			entries = append(entries, indexEntry{Title: guide.Title, Status: guide.Status, URL: link, PDFURL: pdfURL})
			search = append(search, searchEntry{Guide: guide.Title, Title: guide.Title, URL: link, Text: text})
			var records []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
				Text  string `json:"text"`
			}
			if err := storage.ReadJSON(r.root, path.Join(directory, guide.Path, "search.json"), &records); err != nil {
				return err
			}
			for _, record := range records {
				search = append(search, searchEntry{Guide: guide.Title, Title: record.Title,
					URL: (&url.URL{Path: path.Join(guide.Path, record.URL)}).EscapedPath(), Text: record.Text})
			}
			continue
		}
		entries = append(entries, indexEntry{Title: guide.Title, Status: guide.Status, URL: link})
		search = append(search, searchEntry{Guide: guide.Title, Title: guide.Title, URL: link, Text: text})
		if guide.Format == "html" {
			var records []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
				Text  string `json:"text"`
			}
			if err := storage.ReadJSON(r.root, path.Join(directory, guide.Path, "search.json"), &records); err != nil {
				return err
			}
			for _, record := range records {
				search = append(search, searchEntry{Guide: guide.Title, Title: record.Title,
					URL: (&url.URL{Path: path.Join(guide.Path, record.URL)}).EscapedPath(), Text: record.Text})
			}
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
