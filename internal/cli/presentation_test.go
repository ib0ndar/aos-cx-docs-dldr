package cli

import (
	"bytes"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"github.com/charmbracelet/x/ansi"
)

func TestColorPolicyRespectsTerminalNOColorAndDumb(t *testing.T) {
	for _, test := range []struct {
		name              string
		terminal, noColor bool
		term              string
		want              bool
	}{
		{"terminal", true, false, "xterm-256color", true},
		{"redirected", false, false, "xterm-256color", false},
		{"no-color", true, true, "xterm-256color", false},
		{"dumb", true, false, "dumb", false},
		{"dumb-case", true, false, " DUMB ", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := colorsAllowed(test.terminal, test.noColor, test.term); got != test.want {
				t.Fatalf("colorsAllowed=%v want %v", got, test.want)
			}
		})
	}
	var redirected bytes.Buffer
	presentation := newHumanPresentation(&redirected)
	if strings.Contains(presentation.warning("warning"), "\x1b[") {
		t.Fatal("redirected output received terminal colors")
	}
}

func TestPresentationStylesSafeTextAndHumanSemanticSurfaces(t *testing.T) {
	var output bytes.Buffer
	presentation := newHumanPresentationWith(&output, true)
	untrusted := "Guide \x1b[2J\x1b]0;owned\a 文档"
	styled := presentation.warning(untrusted)
	if !strings.Contains(styled, "\x1b[") {
		t.Fatal("enabled presentation emitted no color")
	}
	plain := ansi.Strip(styled)
	if strings.ContainsAny(plain, "\x1b\a") || !strings.Contains(plain, `\u001B`) ||
		!strings.Contains(plain, "文档") {
		t.Fatalf("styling preceded escaping or lost Unicode: %q", plain)
	}

	progress := newProgressReporter(&output, false, presentation)
	progress.Phase("phase")
	progress.Warning("warning")
	progress.Error("error")
	progress.Done("complete")
	text := output.String()
	for _, expected := range []string{"phase", "warning", "error", "complete"} {
		if !strings.Contains(ansi.Strip(text), expected) {
			t.Fatalf("semantic progress output omitted %q: %q", expected, text)
		}
	}
	if strings.Count(text, "\x1b[") < 4 {
		t.Fatalf("phase/warning/error/success surfaces were not colored: %q", text)
	}
}

func TestListingHeadingsStatusesAndWarningsUsePresentation(t *testing.T) {
	var output bytes.Buffer
	presentation := newHumanPresentationWith(&output, true)
	listing := Listing{Catalog: model.Catalog{
		FetchedAt: "now", SourceURL: "https://example.test",
		Platforms: []string{"6300"}, Versions: []string{"10.16"},
		Guides: []model.Guide{
			{ID: "ok", Title: "Complete", Mappings: map[string]map[string]string{}},
			{ID: "bad", Title: "Broken", Error: "failed", Mappings: map[string]map[string]string{}},
		},
		Warnings: []string{"publisher warning"},
	}, Complete: false}
	if err := printListingStyled(&output, listing, options{}, presentation); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "\x1b[") < 4 {
		t.Fatalf("listing headings/status/warnings were not styled: %q", output.String())
	}
	plain := ansi.Strip(output.String())
	for _, expected := range []string{
		"Guide ID  Document  Availability / format", "Complete", "Broken",
		"INCOMPLETE", "publisher warning",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("styled listing omitted %q: %q", expected, plain)
		}
	}
}

func TestPromptColorsDoNotChangeVisibleCellMeasurement(t *testing.T) {
	var output bytes.Buffer
	presentation := newHumanPresentationWith(&output, true)
	model := newInputPrompt("Destination", promptConfig{presentation: presentation})
	model.width, model.height = 48, 10
	model.menuActive = true
	model.suggestions = []directorySuggestion{
		{value: "Alpha/", label: "Alpha/"},
		{value: "文档/", label: "文档/"},
		{value: "Third/", label: "Third/"},
	}

	model.completionIndex = 1
	view := model.View()
	if !strings.Contains(view, "\x1b[") || !strings.Contains(ansi.Strip(view), "> 文档/") {
		t.Fatalf("prompt theme lost selected completion cue: %q", view)
	}
	for _, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
		if ansi.StringWidth(line) > model.width {
			t.Fatalf("ANSI bytes affected visible layout width %d: %q", ansi.StringWidth(line), line)
		}
	}
}

func TestSelectionPromptUsesQuestionAnswerAndDisabledStyles(t *testing.T) {
	var output bytes.Buffer
	presentation := newHumanPresentationWith(&output, true)
	model, err := newSelectPrompt("Switch platform", []selectionChoice{
		{Label: "6300", Value: "6300"},
		{Label: "6400", Value: "6400", Disabled: "no mapped guides"},
	}, false, promptConfig{presentation: presentation})
	if err != nil {
		t.Fatal(err)
	}
	model.width = 80
	view := model.View()
	if strings.Count(view, "\x1b[") < 3 {
		t.Fatalf("question, current answer, disabled row, and help were not styled: %q", view)
	}
	plain := ansi.Strip(view)
	for _, expected := range []string{"? Switch platform", "> 6300", "not selectable: no mapped guides"} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("styled selection lost %q: %q", expected, plain)
		}
	}
}
