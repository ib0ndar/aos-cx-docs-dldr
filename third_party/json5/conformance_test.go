package json5

import (
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type conformanceCase struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Valid bool   `json:"valid"`
	Want  any    `json:"-"`
}

var conformanceCases = []conformanceCase{
	{"basic", `{key:'value',array:[1,2,],}`, true, map[string]any{"key": "value", "array": []any{float64(1), float64(2)}}},
	{"hex-escape", `{s:'\x41'}`, true, map[string]any{"s": "A"}},
	{"vertical-escape", `{s:'\v'}`, true, map[string]any{"s": "\v"}},
	{"null-escape", `{s:'\0'}`, true, map[string]any{"s": "\x00"}},
	{"non-escape", `{s:'\q'}`, true, map[string]any{"s": "q"}},
	{"escaped-key", `{f\u006fo:1}`, true, map[string]any{"foo": float64(1)}},
	{"astral-escaped-key", `{\uD801\uDC00:1}`, true, map[string]any{"\U00010400": float64(1)}},
	{"unicode-key", "{\u03b1:1,a\u0301:2,a\u200c:3}", true, map[string]any{"\u03b1": float64(1), "a\u0301": float64(2), "a\u200c": float64(3)}},
	{"unicode-space", "\ufeff{\u00a0x:\u20031\u2028}\u3000", true, map[string]any{"x": float64(1)}},
	{"continuation-lf", "{s:'a\\\nb'}", true, map[string]any{"s": "ab"}},
	{"continuation-crlf", "{s:'a\\\r\nb'}", true, map[string]any{"s": "ab"}},
	{"continuation-ls", "{s:'a\\\u2028b'}", true, map[string]any{"s": "ab"}},
	{"continuation-ps", "{s:'a\\\u2029b'}", true, map[string]any{"s": "ab"}},
	{"quotes-backslashes", `{s:'a\'b\\c'}`, true, map[string]any{"s": "a'b\\c"}},
	{"unicode-string", `{s:'\u03b1\uD83D\uDE80'}`, true, map[string]any{"s": "\u03b1\U0001f680"}},
	{"comments", "/* { : ' */ {x:1,// }\u2028 y:2,} // end", true, map[string]any{"x": float64(1), "y": float64(2)}},
	{"comment-star", `{x:1/* * / still comment */}`, true, map[string]any{"x": float64(1)}},
	{"number-forms", `[+2,-2,0x10,-0x10,.5,-.5,5.,1e2]`, true, []any{float64(2), float64(-2), float64(16), float64(-16), 0.5, -0.5, float64(5), float64(100)}},
	{"nan", `[NaN,+NaN,-NaN,Infinity,+Infinity,-Infinity]`, true, nil},
	{"expression", `{x:1+2}`, false, nil},
	{"function", `{x:function(){return 1}}`, false, nil},
	{"shorthand", `{x}`, false, nil},
	{"computed", `{['x']:1}`, false, nil},
	{"spread", `{...x}`, false, nil},
	{"template", "{x:`value`}", false, nil},
	{"undefined", `{x:undefined}`, false, nil},
	{"holes", `[1,,2]`, false, nil},
	{"unterminated-string", `{x:'open}`, false, nil},
	{"unterminated-comment", `{x:1/*}`, false, nil},
	{"unterminated-trailing-comment", `{x:1} /*`, false, nil},
	{"bad-hex-escape", `{s:'\x4Z'}`, false, nil},
	{"bad-unicode", `{s:'\u00ZZ'}`, false, nil},
	{"bad-null-escape", `{s:'\01'}`, false, nil},
	{"octal-escape", `{s:'\1'}`, false, nil},
	{"raw-newline", "{s:'a\nb'}", false, nil},
	{"invalid-key-start", `{\u0031key:1}`, false, nil},
	{"invalid-key-punctuation", `{a-b:1}`, false, nil},
	{"numeric-separator", `{n:1_000}`, false, nil},
	{"octal-number", `{n:012}`, false, nil},
	{"malformed-sign", `{n:+..1}`, false, nil},
	{"trailing-data", `{} {}`, false, nil},
}

func TestConformanceCorpus(t *testing.T) {
	for _, tc := range conformanceCases {
		t.Run(tc.Name, func(t *testing.T) {
			var got any
			err := Unmarshal([]byte(tc.Input), &got)
			if (err == nil) != tc.Valid {
				t.Fatalf("valid=%v error=%v value=%#v", tc.Valid, err, got)
			}
			if tc.Valid && tc.Want != nil && !reflect.DeepEqual(got, tc.Want) {
				t.Fatalf("value=%#v want=%#v", got, tc.Want)
			}
		})
	}
}

func TestUpstreamParseCorpusWithoutExecution(t *testing.T) {
	err := filepath.WalkDir("testdata", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".json" && ext != ".json5" && ext != ".js" && ext != ".txt" {
			return nil
		}
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got any
			err = Unmarshal(data, &got)
			valid := ext == ".json" || ext == ".json5"
			if (err == nil) != valid {
				t.Fatalf("expected valid=%v got error=%v", valid, err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func normalized(value any) any {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) {
			return "NaN"
		}
		if math.IsInf(v, 1) {
			return "Infinity"
		}
		if math.IsInf(v, -1) {
			return "-Infinity"
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = normalized(item)
		}
		return v
	case map[string]any:
		for key, item := range v {
			v[key] = normalized(item)
		}
		return v
	default:
		return v
	}
}

func TestPythonReferenceDifferential(t *testing.T) {
	python := os.Getenv("AOSCX_JSON5_PYTHON")
	if python == "" {
		t.Skip("Python JSON5 comparison is explicit development-only tooling")
	}
	data, err := json.Marshal(conformanceCases)
	if err != nil {
		t.Fatal(err)
	}
	script := `import json,json5,math,sys
def norm(v):
 if isinstance(v,float) and not math.isfinite(v): return "NaN" if math.isnan(v) else ("Infinity" if v>0 else "-Infinity")
 if isinstance(v,list): return [norm(x) for x in v]
 if isinstance(v,dict): return {k:norm(x) for k,x in v.items()}
 return v
out=[]
for c in json.load(sys.stdin):
 try: out.append({"valid":True,"value":norm(json5.loads(c["input"]))})
 except ValueError: out.append({"valid":False})
json.dump(out,sys.stdout,ensure_ascii=True,allow_nan=False)`
	command := exec.Command(python, "-c", script)
	command.Stdin = strings.NewReader(string(data))
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var reference []struct {
		Valid bool `json:"valid"`
		Value any  `json:"value"`
	}
	if err := json.Unmarshal(output, &reference); err != nil {
		t.Fatal(err)
	}
	for i, tc := range conformanceCases {
		t.Run(tc.Name, func(t *testing.T) {
			var got any
			err := Unmarshal([]byte(tc.Input), &got)
			if tc.Name == "invalid-key-start" && reference[i].Valid {
				if err == nil {
					t.Fatal("escaped digit illegally accepted as IdentifierStart")
				}
				t.Log("Python reference accepts this invalid IdentifierStart; local parser deliberately rejects it")
				return
			}
			if (err == nil) != reference[i].Valid {
				t.Fatalf("Python valid=%v, Go error=%v", reference[i].Valid, err)
			}
			if err == nil && !reflect.DeepEqual(normalized(got), reference[i].Value) {
				t.Fatalf("Go %#v != Python %#v", got, reference[i].Value)
			}
		})
	}
}
