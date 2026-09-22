package model

import (
	"context"
	"io"
	"net/http"
)

const Version = "0.8"
const ExecutableName = "aos-cx-docs-dldr"
const PortalURL = "https://arubanetworking.hpe.com/techdocs/ArubaDocPortal/content/new-portal/aoscx.html"

// Response bodies belong to the caller and must be closed, including on errors.
type Resource struct {
	URL     string
	Status  int
	Headers http.Header
	Body    io.ReadCloser
}

type Request struct {
	URL          string
	Method       string
	Refresh      bool
	ValidatorURL string
	ETag         string
	Modified     string
}

type Transport interface {
	// Download owns the response body and may call consume again after a
	// transient read failure. Each call must replace, not append to, staged data.
	// On success the returned resource contains metadata and a closed body.
	Download(ctx context.Context, request Request, consume func(Resource) error) (Resource, error)
}

type Fetcher interface {
	Get(ctx context.Context, url string, refresh bool) (Resource, error)
}

type Guide struct {
	ID       string                       `json:"id"`
	Title    string                       `json:"title"`
	Mappings map[string]map[string]string `json:"mappings"`
	Error    string                       `json:"error,omitempty"`
}

type Catalog struct {
	Platforms []string `json:"platforms"`
	Versions  []string `json:"versions"`
	Guides    []Guide  `json:"guides"`
	SourceURL string   `json:"source_url"`
	FetchedAt string   `json:"fetched_at"`
	Warnings  []string `json:"warnings"`
}

type Document struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Platform    string `json:"platform"`
	Version     string `json:"version"`
	URL         string `json:"url"`
	Kind        string `json:"kind"`
	RouteOrigin string `json:"route_origin,omitempty"`
}
