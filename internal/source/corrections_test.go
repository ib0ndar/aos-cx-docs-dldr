package source

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestAbsoluteMappedDocumentPreservesBookmark(t *testing.T) {
	for _, tc := range []struct{ target, want string }{
		{"https://publisher.example/guide.html#installation", "https://publisher.example/guide.html#installation"},
		{"https://publisher.example/some guide%20name.html#install step%202", "https://publisher.example/some%20guide%20name.html#install%20step%202"},
		{"https://publisher.example/some%2Fguide name.html#installation%2Fpart 1", "https://publisher.example/some%2Fguide%20name.html#installation%2Fpart%201"},
	} {
		d, err := resolve(model.Guide{ID: "guide"}, "6300", "10.16", tc.target)
		if err != nil || d.URL != tc.want {
			t.Fatalf("mapped bookmark lost: got=%q want=%q error=%v", d.URL, tc.want, err)
		}
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r cancelOnClose) Close() error {
	r.cancel()
	return r.ReadCloser.Close()
}

type finalParseCanceller struct {
	*fixtureFetcher
	cancel context.CancelFunc
}

func (f finalParseCanceller) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	r, err := f.fixtureFetcher.Get(ctx, raw, refresh)
	if err == nil && strings.HasSuffix(raw, "/webUI.json") {
		r.Body = cancelOnClose{r.Body, f.cancel}
	}
	return r, err
}

func TestCancellationDuringFinalCatalogueParse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := finalParseCanceller{fixtures(), cancel}
	_, err := LoadCatalog(ctx, f, "https://example.test/portal/aoscx.html")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("final parse cancellation became a completed catalogue: %v", err)
	}
}
