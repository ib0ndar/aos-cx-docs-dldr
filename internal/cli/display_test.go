package cli

import (
	"bytes"
	"strings"
	"testing"
	"unicode"

	"aos-cx-docs-dldr/internal/archive"
)

func TestHumanDisplayEscapesControlsAndPreservesUnicode(t *testing.T) {
	untrusted := "Guide \x1b[2J\x1b]0;owned\a\r\n\t\u009b31m\u202e 文档"
	safe := safeDisplayText(untrusted)
	for _, char := range safe {
		if unicode.In(char, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("safe display retained control U+%04X in %q", char, safe)
		}
	}
	if !strings.Contains(safe, `\u001B`) || !strings.Contains(safe, `\u009B`) ||
		!strings.Contains(safe, `\u202E`) || !strings.Contains(safe, "文档") {
		t.Fatalf("display escaping lost safety or Unicode: %q", safe)
	}

	prompt, err := newSelectPrompt(untrusted, []selectionChoice{
		{Label: untrusted, Value: untrusted},
		{Label: untrusted, Value: "disabled", Disabled: untrusted},
	}, false, promptConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(prompt.View(), "\x1b\a\r\t\u009b\u202e") {
		t.Fatalf("prompt view retained untrusted controls: %q", prompt.View())
	}

	var output bytes.Buffer
	progress := newProgressReporter(&output, false)
	progress.Message(untrusted)
	progress.HTML(1, 1, untrusted, archive.Progress{
		TopicsValidated: 1, PlannedTopics: 1, Final: true,
	})
	if strings.ContainsAny(output.String(), "\x1b\a\r\t\u009b\u202e") {
		t.Fatalf("progress output retained untrusted controls: %q", output.String())
	}
	if !strings.Contains(output.String(), "文档") {
		t.Fatalf("progress output lost ordinary Unicode: %q", output.String())
	}
}
