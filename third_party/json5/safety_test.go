package json5

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodedDuplicateKeysAndScopedObjects(t *testing.T) {
	for _, input := range []string{
		`{a:1,a:2}`, `{"a":1,'a':2}`, `{foo:1,f\u006fo:2}`,
		`{a:1,"\u0061":2}`, `{a:1,'\x61':2}`,
		`{outer:{x:1,x:2}}`, `[{x:1,x:2}]`,
	} {
		var out any
		err := UnmarshalWithOptions([]byte(input), &out, Options{DisallowDuplicateKeys: true, MaxDepth: 216, MaxValues: 1000})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate lost in %s: %v", input, err)
		}
	}
	for _, input := range []string{`{one:{x:1},two:{x:2}}`, `[{x:1},{x:2}]`,
		"{one:{'a\\\nb':1},two:{ab:2}}", `{s:'{a:a}',x:1 /* x:2, } */}`} {
		var out any
		if err := UnmarshalWithOptions([]byte(input), &out, Options{DisallowDuplicateKeys: true}); err != nil {
			t.Fatalf("valid scopes rejected: %s: %v", input, err)
		}
	}
	var typed struct{ A int }
	if err := UnmarshalWithOptions([]byte(`{A:1,A:2}`), &typed, Options{DisallowDuplicateKeys: true}); err == nil {
		t.Fatal("typed duplicate accepted")
	}
}

func TestRawNumbersLimitsAndInvalidUnicode(t *testing.T) {
	var out []any
	raw := `[2,2.0,2e0,+2,0x2,-0,NaN,+NaN,-NaN,Infinity,+Infinity,-Infinity]`
	if err := UnmarshalWithOptions([]byte(raw), &out, Options{UseNumber: true}); err != nil {
		t.Fatal(err)
	}
	expected := []string{"2", "2.0", "2e0", "+2", "0x2", "-0", "NaN", "+NaN", "-NaN", "Infinity", "+Infinity", "-Infinity"}
	for i, v := range out {
		number, ok := v.(Number)
		if !ok || number.String() != expected[i] {
			t.Fatalf("raw number lost: %#v", v)
		}
	}
	for _, raw := range []string{`+NaN`, `-NaN`, `Infinity`} {
		var value any
		if err := UnmarshalWithOptions([]byte(raw), &value, Options{UseNumber: true}); err != nil {
			t.Fatal(err)
		}
		if _, ok := value.(Number); !ok {
			t.Fatalf("top-level raw number lost: %#v", value)
		}
	}
	for _, raw := range []string{`+.`, `+..1`, `.e1`, `0x`, `1e+`, `1_0`} {
		var value any
		if err := UnmarshalWithOptions([]byte(raw), &value, Options{UseNumber: true}); err == nil {
			t.Fatalf("invalid raw number accepted: %s", raw)
		}
	}
	var value any
	if err := UnmarshalWithOptions([]byte(strings.Repeat("[", 220)+"0"+strings.Repeat("]", 220)), &value, Options{MaxDepth: 216}); err == nil {
		t.Fatal("depth limit ignored")
	}
	if err := UnmarshalWithOptions([]byte(`[1,2,3]`), &value, Options{MaxValues: 2}); err == nil {
		t.Fatal("value limit ignored")
	}
	if err := UnmarshalWithOptions([]byte(`{"a":1}`), &value, Options{MaxBytes: 3}); err == nil {
		t.Fatal("byte limit ignored")
	}
	for _, raw := range [][]byte{[]byte(`{s:'\uD800'}`), []byte(`{s:'\uDC00'}`), {'\'', 0xff, '\''}} {
		if err := UnmarshalWithOptions(raw, &value, Options{}); err == nil {
			t.Fatalf("unrepresentable Unicode silently replaced: %q", raw)
		}
	}
	err := UnmarshalWithOptions([]byte(`{a:1,a:2}`), &value, Options{DisallowDuplicateKeys: true})
	var syntax *SyntaxError
	if !errors.As(err, &syntax) || syntax.Offset <= 0 {
		t.Fatalf("error offset not retained: %v", err)
	}
}

func TestStreamingDecodeUnicode(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("\u00a0{f\\u006fo:'\\x41'}\n"))
	decoder.UseNumber()
	decoder.DisallowDuplicateKeys()
	decoder.SetLimits(216, 1000)
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	if value["foo"] != "A" {
		t.Fatalf("streaming decoder disagrees: %#v", value)
	}
}

func FuzzBoundedJSON5(f *testing.F) {
	for _, seed := range []string{`{x:'\x41'}`, `{f\u006fo:1}`, `[NaN,0x2]`, `{} /*`, `{x:[1,2,]}`, `'\\'`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		var value any
		err := UnmarshalWithOptions(data, &value, Options{UseNumber: true, DisallowDuplicateKeys: true, MaxDepth: 32, MaxValues: 1024, MaxBytes: 4096})
		if err == nil {
			// A strict JSON serialization must not corrupt strings or panic.
			if _, err := json.Marshal(value); err != nil {
				t.Fatal(err)
			}
		}
	})
}
