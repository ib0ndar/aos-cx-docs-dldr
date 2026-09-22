package json5

import (
	"strings"
	"testing"
)

func TestAdjacentCommentsDoNotCorruptUnicodeLineContinuation(t *testing.T) {
	input := "{/**//**/'/Content/a\\" + "\u2028" + "b.htm':1}"
	var value map[string]any
	if err := UnmarshalWithOptions([]byte(input), &value, Options{
		DisallowDuplicateKeys: true, MaxDepth: 16, MaxValues: 32,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["/Content/ab.htm"]; !ok || len(value) != 1 {
		t.Fatalf("adjacent comments corrupted decoded key: %#v", value)
	}
}

func TestMixedCommentsAroundStringAndUnicodeTerminators(t *testing.T) {
	for _, input := range []string{
		"{/*a*///b\r\n's\\\u2028x':1}",
		"{//a\u2028/*b*/'s\\\u2029x':1}",
		"{/*a*//*b*/'s\\\u2028x':1}",
	} {
		var value map[string]any
		if err := UnmarshalWithOptions([]byte(input), &value, Options{
			DisallowDuplicateKeys: true, MaxDepth: 16, MaxValues: 32,
		}); err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		if _, ok := value["sx"]; !ok || len(value) != 1 {
			t.Fatalf("comment/string state leaked for %q: %#v", input, value)
		}
	}
}

func TestNULLookaheadAppliesToImmediateCharacterOnly(t *testing.T) {
	for _, suffix := range []string{"a1", "_1", "x99", "-7"} {
		var value map[string]any
		input := `{s:'\0` + suffix + `'}`
		if err := UnmarshalWithOptions([]byte(input), &value, Options{}); err != nil {
			t.Fatalf("ordinary character did not end NUL lookahead for %q: %v", suffix, err)
		}
		if got := value["s"].(string); got != "\x00"+suffix {
			t.Fatalf("NUL value changed: %q", got)
		}
	}
	for _, suffix := range []string{"1", "9"} {
		var value any
		err := UnmarshalWithOptions([]byte(`{s:'\0`+suffix+`'}`), &value, Options{})
		if err == nil || !strings.Contains(err.Error(), "null escape") {
			t.Fatalf("immediate digit after NUL accepted: suffix=%q err=%v", suffix, err)
		}
	}
}
