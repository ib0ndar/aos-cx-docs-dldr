package source

import (
	"context"

	"golang.org/x/net/html"
)

// DecodeHTML applies the bounded charset and browser-compatible parser used by
// source discovery. Archive callers perform source-specific content validation
// after selecting the authoritative topic container.
func DecodeHTML(ctx context.Context, body []byte, contentType, sourceURL string) (*html.Node, error) {
	return decodePublisherHTML(ctx, body, contentType, sourceURL)
}

// GuideRoot returns the selected Flare guide root, including its trailing slash.
func GuideRoot(raw string) (string, error) {
	return documentRoot(raw)
}
