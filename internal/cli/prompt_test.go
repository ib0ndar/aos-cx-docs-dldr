package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func updateSelect(t *testing.T, model selectPrompt, key tea.KeyMsg) selectPrompt {
	t.Helper()
	next, _ := model.Update(key)
	return next.(selectPrompt)
}

func TestSelectPromptSkipsDisabledRowsAndBoundsLargeLists(t *testing.T) {
	choices := make([]selectionChoice, 30)
	for index := range choices {
		choices[index] = selectionChoice{Label: "Choice", Value: string(rune('a' + index))}
	}
	choices[1].Disabled = "no mapped guides"
	model, err := newSelectPrompt("Release", choices, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	resized, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 18})
	model = resized.(selectPrompt)
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if model.cursor != 2 {
		t.Fatalf("cursor selected disabled row: %d", model.cursor)
	}
	for range 15 {
		model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyDown})
	}
	view := model.View()
	if strings.Count(view, "\n") > 18 || !strings.Contains(view, "earlier choices") ||
		!strings.Contains(view, "later choices") {
		t.Fatalf("large prompt was not windowed: %q", view)
	}
}

func TestSelectPromptShowsEveryChoiceWhenTerminalHasRoom(t *testing.T) {
	for _, count := range []int{8, 15, 30} {
		for _, multiple := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d-multi%v", count, multiple), func(t *testing.T) {
				choices := make([]selectionChoice, count)
				for i := range choices {
					choices[i] = selectionChoice{Label: fmt.Sprintf("Option %02d", i+1), Value: fmt.Sprint(i)}
				}
				model, err := newSelectPrompt("Choose", choices, multiple, promptConfig{})
				if err != nil {
					t.Fatal(err)
				}
				next, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: count + 5})
				model = next.(selectPrompt)
				view := model.View()
				if strings.Contains(view, "earlier choices") || strings.Contains(view, "later choices") {
					t.Fatalf("choices were hidden despite available space:\n%s", view)
				}
				for _, choice := range choices {
					if !strings.Contains(view, choice.Label) {
						t.Fatalf("missing visible choice %q", choice.Label)
					}
				}
			})
		}
	}
}

func TestPromptSessionUsesAndPropagatesTerminalResize(t *testing.T) {
	choices := make([]selectionChoice, 30)
	for i := range choices {
		choices[i] = selectionChoice{Label: fmt.Sprintf("Choice %02d", i+1), Value: fmt.Sprint(i)}
	}
	model, err := newSelectPrompt("Choose", choices, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	session := promptSession{}
	next, _ := session.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	session = next.(promptSession)
	response := make(chan promptResult, 1)
	next, _ = session.Update(promptRequest{model: model, response: response})
	session = next.(promptSession)
	if promptViewRows(session.View(), 80) > 10 || !strings.Contains(session.View(), "later choices") {
		t.Fatalf("new prompt ignored the current small terminal:\n%s", session.View())
	}
	next, _ = session.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	session = next.(promptSession)
	if !strings.Contains(session.View(), "Choice 30") || strings.Contains(session.View(), "later choices") {
		t.Fatalf("resized prompt still hid choices:\n%s", session.View())
	}
	next, _ = session.Update(tea.KeyMsg{Type: tea.KeyEnter})
	session = next.(promptSession)
	<-response
	next, _ = session.Update(promptRequest{model: model, response: make(chan promptResult, 1)})
	session = next.(promptSession)
	if !strings.Contains(session.View(), "Choice 30") || strings.Contains(session.View(), "later choices") {
		t.Fatal("the next question lost the terminal size")
	}
}

func TestSelectPromptAccountsForWrappedChoiceLines(t *testing.T) {
	choices := make([]selectionChoice, 15)
	for i := range choices {
		choices[i] = selectionChoice{Label: fmt.Sprintf("Option %02d %s", i+1, strings.Repeat("界", 16)), Value: fmt.Sprint(i)}
	}
	choices[1].Disabled = "no mapped guides available"
	model, err := newSelectPrompt("Select guides", choices, true, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	next, _ := model.Update(tea.WindowSizeMsg{Width: 35, Height: 12})
	model = next.(selectPrompt)
	for range 25 {
		view := model.View()
		if promptViewRows(view, 35) > 12 || !strings.Contains(view, fmt.Sprintf("Option %02d", model.cursor+1)) {
			t.Fatalf("wrapped labels overflowed or hid the current choice:\n%s", view)
		}
		model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyDown})
	}
}

func promptViewRows(view string, width int) int {
	rows := 1
	for _, line := range strings.Split(strings.TrimSuffix(view, "\n"), "\n") {
		rows += max(1, (ansi.StringWidth(line)+width-1)/width)
	}
	return rows
}

func TestMultiSelectRequiresAChoiceAndPreservesSourceOrder(t *testing.T) {
	model, err := newSelectPrompt("Guides", []selectionChoice{
		{Label: "First", Value: "first"},
		{Label: "Second", Value: "second"},
	}, true, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	if model.message == "" {
		t.Fatal("empty multi-select was accepted")
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyDown})
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeySpace})
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyUp})
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeySpace})
	if got := strings.Join(model.values(), ","); got != "first,second" {
		t.Fatalf("selected values not returned in source order: %s", got)
	}
}

func TestSelectPromptFreezesConfirmedChoice(t *testing.T) {
	model, err := newSelectPrompt("Platform", []selectionChoice{
		{Label: "6300", Value: "6300"},
		{Label: "6400", Value: "6400"},
	}, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyDown})
	if values := model.values(); len(values) != 1 || values[0] != "6300" {
		t.Fatalf("confirmed choice mutated after Enter: cursor=%d values=%v", model.cursor, values)
	}
}

func TestSelectionChoiceStaticEmphasisIsWidthSafeAndColorOptional(t *testing.T) {
	choice := selectionChoice{
		Label: "All available mapped guides (2)", Value: "all", Emphasis: "available",
	}
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		model, err := newSelectPrompt("Documents", []selectionChoice{choice}, false,
			promptConfig{presentation: newHumanPresentationWith(&output, enabled)})
		if err != nil {
			t.Fatal(err)
		}
		line := model.choiceLine(0)
		if ansi.StringWidth(line) != len("> All available mapped guides (2)") ||
			ansi.Strip(line) != "> All available mapped guides (2)" {
			t.Fatalf("emphasis changed width/value enabled=%v line=%q", enabled, line)
		}
		if strings.Contains(line, "\x1b[") != enabled {
			t.Fatalf("emphasis color mismatch enabled=%v line=%q", enabled, line)
		}
	}
	if strings.Contains(choice.Value, "\x1b") {
		t.Fatal("selection value contains styling")
	}
}

func TestLeftArrowBackIsDistinctFromCancellation(t *testing.T) {
	model, err := newSelectPrompt("Platform", []selectionChoice{
		{Label: "6300", Value: "6300"},
		{Label: "6400", Value: "6400"},
	}, false, promptConfig{Initial: "6400", AllowBack: true})
	if err != nil {
		t.Fatal(err)
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyLeft})
	values := model.draftValues()
	if !model.back || model.cancelled || len(values) != 1 || values[0] != "6400" {
		t.Fatalf("Left did not preserve the current draft as Back: %+v", model)
	}
	if !strings.Contains(model.help(), "Left: back") || strings.Contains(model.help(), "Shift+Tab") {
		t.Fatalf("Back help does not match the selected key: %q", model.help())
	}

	model, err = newSelectPrompt("Platform", []selectionChoice{
		{Label: "6300", Value: "6300"},
	}, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyLeft})
	if model.back || model.cancelled || model.message != "This is the first question." {
		t.Fatalf("Back at the first question did not stay put: %+v", model)
	}

	input := newInputPrompt("Destination", promptConfig{Initial: "abc", AllowBack: true})
	next, _ := input.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	input = next.(inputPrompt)
	if string(input.value) != "ab" || input.back {
		t.Fatalf("Backspace was confused with Back: %+v", input)
	}
	next, _ = input.Update(tea.KeyMsg{Type: tea.KeyLeft})
	input = next.(inputPrompt)
	if !input.back || input.cancelled || string(input.value) != "ab" {
		t.Fatalf("Left did not return the partially typed destination: %+v", input)
	}
}

func TestPromptSessionBoundsQueuedTypeahead(t *testing.T) {
	session := promptSession{}
	next, _ := session.Update(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(strings.Repeat("x", maxPendingPromptRunes+1)),
	})
	session = next.(promptSession)
	response := make(chan promptResult, 1)
	model, err := newSelectPrompt("Platform", []selectionChoice{{Label: "6300", Value: "6300"}}, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	next, _ = session.Update(promptRequest{model: model, response: response})
	session = next.(promptSession)
	select {
	case result := <-response:
		if result.err == nil || !strings.Contains(result.err.Error(), "too much input") {
			t.Fatalf("queued typeahead overflow was not explicit: %+v", result)
		}
	default:
		t.Fatal("queued typeahead overflow left the next prompt waiting")
	}
}

func TestPromptCancellationAndUnicodeDestination(t *testing.T) {
	model, err := newSelectPrompt("Platform", []selectionChoice{{Label: "6300", Value: "6300"}}, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if !model.cancelled {
		t.Fatal("Escape did not cancel selection")
	}
	model, err = newSelectPrompt("Platform", []selectionChoice{{Label: "6300", Value: "6300"}}, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	model = updateSelect(t, model, tea.KeyMsg{Type: tea.KeyCtrlD})
	if !model.cancelled {
		t.Fatal("Ctrl-D did not cancel selection")
	}
	input := inputPrompt{title: "Destination"}
	next, _ := input.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("~/AOS CX/文档")})
	input = next.(inputPrompt)
	next, _ = input.Update(tea.KeyMsg{Type: tea.KeySpace})
	input = next.(inputPrompt)
	next, _ = input.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	input = next.(inputPrompt)
	if string(input.value) != "~/AOS CX/文档" {
		t.Fatalf("Unicode destination editing failed: %q", string(input.value))
	}
	next, _ = input.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !next.(inputPrompt).cancelled {
		t.Fatal("Ctrl-C did not cancel input")
	}
}

func TestCancelledScriptedPromptPropagatesContextCancellation(t *testing.T) {
	prompts := &scriptedPrompts{}
	o := options{}
	_, err := selectDocuments(context.Background(), selectionCatalog(), &o, prompts, func(string) {})
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("prompt cancellation was hidden: %v", err)
	}
}

func TestClosedPromptInputCancelsInsteadOfHanging(t *testing.T) {
	prompts := &terminalPrompts{input: bytes.NewReader(nil), output: io.Discard}
	_, err := prompts.Select(context.Background(), "Platform", []selectionChoice{{Label: "6300", Value: "6300"}}, promptConfig{})
	if err == nil {
		t.Fatal("closed prompt input succeeded")
	}
}
