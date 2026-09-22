package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/cache"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"golang.org/x/net/html"
)

type offlineTransport struct{}

func (offlineTransport) Download(context.Context, model.Request, func(model.Resource) error) (model.Resource, error) {
	return model.Resource{}, errors.New("retained-input replay attempted a network request")
}

// TestM3CLiveRetainedInputReplay is an explicit evidence gate. It is skipped in
// ordinary suites because the publisher inputs are intentionally uncommitted.
func TestM3CLiveRetainedInputReplay(t *testing.T) {
	cachePath := os.Getenv("AOSCX_M3C_REPLAY_CACHE")
	if cachePath == "" {
		t.Skip("set AOSCX_M3C_REPLAY_CACHE to replay retained live inputs")
	}
	store, err := cache.Open(cachePath, offlineTransport{}, 16<<20, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	document := model.Document{
		ID: "jobscheduler", Title: "Job Scheduler Guide", Platform: "6300", Version: "10.16", Kind: "flare",
		URL: "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/job_scheduler/Content/home.htm",
	}
	plan, err := source.LoadPlan(context.Background(), store, document)
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	result, err := HTML(context.Background(), plan, store, output, false, 16<<20, 64<<20)
	if err != nil || result.Status != "complete" {
		t.Fatalf("retained live inputs did not produce a complete archive: status=%s topics=%d assets=%d errors=%v err=%v",
			result.Status, len(result.HTML.Topics), len(result.HTML.Assets), result.Errors, err)
	}
	t.Logf("retained live replay: topics=%d assets=%d checked_links=%d",
		len(result.HTML.Topics), len(result.HTML.Assets), result.HTML.Integrity.CheckedLinks)
	tables, cells, preBlocks, screenBlocks := 0, 0, 0, 0
	for _, record := range result.HTML.Topics {
		resource, err := store.Get(context.Background(), record.URL, false)
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(resource.Body)
		closeErr := resource.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read retained topic: %v %v", readErr, closeErr)
		}
		sourceDoc, err := source.DecodeHTML(context.Background(), raw, resource.Headers.Get("Content-Type"), resource.URL)
		if err != nil {
			t.Fatal(err)
		}
		sourceContent := findContent(sourceDoc, "flare")
		generated, err := os.ReadFile(filepath.Join(output, filepath.FromSlash(record.Path)))
		if err != nil {
			t.Fatal(err)
		}
		generatedDoc, err := html.Parse(bytes.NewReader(generated))
		if err != nil {
			t.Fatal(err)
		}
		generatedContent := findNode(generatedDoc, func(node *html.Node) bool {
			return node.Type == html.ElementNode && hasClass(node, "archive-content")
		})
		sourcePre := tagTexts(sourceContent, "pre")
		generatedPre := tagTexts(generatedContent, "pre")
		if !reflect.DeepEqual(sourcePre, generatedPre) {
			t.Fatalf("command whitespace changed for %s", record.URL)
		}
		sourceScreen := classTexts(sourceContent, "screen")
		generatedScreen := classTexts(generatedContent, "screen")
		if !reflect.DeepEqual(sourceScreen, generatedScreen) {
			t.Fatalf("screen command whitespace changed for %s", record.URL)
		}
		if countTags(sourceContent, "table") != countTags(generatedContent, "table") ||
			countTags(sourceContent, "th")+countTags(sourceContent, "td") !=
				countTags(generatedContent, "th")+countTags(generatedContent, "td") {
			t.Fatalf("table structure changed for %s", record.URL)
		}
		tables += countTags(sourceContent, "table")
		cells += countTags(sourceContent, "th") + countTags(sourceContent, "td")
		preBlocks += len(sourcePre)
		screenBlocks += len(sourceScreen)
	}
	t.Logf("retained markup comparison: tables=%d cells=%d exact_pre_blocks=%d exact_screen_blocks=%d",
		tables, cells, preBlocks, screenBlocks)
}

func countTags(root *html.Node, name string) int {
	count := 0
	walk(root, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == name {
			count++
		}
	})
	return count
}

func tagTexts(root *html.Node, name string) []string {
	var values []string
	walk(root, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == name {
			var text strings.Builder
			walk(node, func(child *html.Node) {
				if child.Type == html.TextNode {
					text.WriteString(child.Data)
				}
			})
			values = append(values, text.String())
		}
	})
	return values
}

func classTexts(root *html.Node, class string) []string {
	var values []string
	walk(root, func(node *html.Node) {
		if node.Type == html.ElementNode && hasClass(node, class) {
			var text strings.Builder
			walk(node, func(child *html.Node) {
				if child.Type == html.TextNode {
					text.WriteString(child.Data)
				}
			})
			values = append(values, text.String())
		}
	})
	return values
}
