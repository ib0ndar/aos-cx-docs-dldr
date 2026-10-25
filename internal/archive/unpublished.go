package archive

import (
	"errors"
	"fmt"
	"time"

	"aos-cx-docs-dldr/internal/model"
)

// Publications can list topics in their TOC, or link them from other topics,
// that the publisher never deployed: the publisher answers HTTP 404 or 410 on
// its own site too. Such a topic gets no page. Its TOC entry and the links to
// it lead to the publisher's URL, it is recorded in unpublished_topics with a
// warning, and the guide is degraded, so that it never replaces a complete
// copy and a later copy replaces it only when no more topics are missing.

// unpublish records page as a topic the publisher answers with status.
func (a *htmlArchiver) unpublish(page *pageInput, status int, planned bool) {
	page.unpublished = true
	a.unpublished = append(a.unpublished, model.UnpublishedTopic{
		URL: page.topic.URL, Title: page.title, HTTPStatus: status,
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	a.warnings = append(a.warnings, fmt.Sprintf(
		"Publisher topic %q at %s returned HTTP %d; its TOC entry and the links to it lead to the publisher's page",
		page.title, page.topic.URL, status))
	if planned {
		a.progress.TopicsValidated++
		a.reportProgress()
	}
}

// dropUnpublished removes the pages of unpublished topics from the archive.
func (a *htmlArchiver) dropUnpublished() error {
	if len(a.unpublished) == 0 {
		return nil
	}
	pages := make([]*pageInput, 0, len(a.pages))
	for _, page := range a.pages {
		if !page.unpublished {
			pages = append(pages, page)
		}
	}
	if len(pages) == 0 {
		return errors.New("the publisher serves none of the guide's topics")
	}
	a.pages = pages
	for key, page := range a.pageByURL {
		if page.unpublished {
			delete(a.pageByURL, key)
		}
	}
	return nil
}
