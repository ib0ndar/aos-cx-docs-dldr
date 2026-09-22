package pdfgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/publication"
)

func outlineTestDocument(t *testing.T) qpdfDocument {
	t.Helper()
	objects := map[string]json.RawMessage{}
	add := func(key string, value map[string]any) {
		body, err := json.Marshal(qpdfObject{Value: value})
		if err != nil {
			t.Fatal(err)
		}
		objects[key] = body
	}
	add("trailer", map[string]any{"/Root": "1 0 R"})
	add("obj:1 0 R", map[string]any{
		"/Type":  "/Catalog",
		"/Pages": "2 0 R",
		"/Dests": map[string]any{
			"/aos-cx-docs-dldr-print-contents": []any{"3 0 R", "/XYZ", float64(0), float64(700), float64(0)},
			"/topic-one":                       []any{"3 0 R", "/XYZ", float64(0), float64(700), float64(0)},
			"/topic-two--bookmark-fragment": []any{
				"4 0 R", "/XYZ", float64(0), float64(500), float64(0),
			},
		},
	})
	add("obj:2 0 R", map[string]any{
		"/Type": "/Pages", "/Count": float64(2), "/Kids": []any{"3 0 R", "4 0 R"},
	})
	add("obj:3 0 R", map[string]any{"/Type": "/Page", "/Parent": "2 0 R"})
	add("obj:4 0 R", map[string]any{"/Type": "/Page", "/Parent": "2 0 R"})
	return qpdfDocument{
		Header:  qpdfHeader{JSONVersion: 2, MaxObjectID: 4},
		Objects: objects,
		Pages: []qpdfPage{
			{Object: "3 0 R", PagePosition: 1},
			{Object: "4 0 R", PagePosition: 2},
		},
	}
}

func outlineTestPlan() publication.OutlinePlan {
	return publication.OutlinePlan{
		SchemaVersion: publication.OutlineSchemaVersion,
		GuideTitle:    "Guide",
		EntryCount:    5,
		MaxDepth:      3,
		Entries: []publication.OutlineEntry{
			{
				Title:  "Group",
				Target: publication.OutlineTarget{Name: "topic-one"},
				Children: []publication.OutlineEntry{
					{Title: "Duplicate", Target: publication.OutlineTarget{Name: "topic-one"}},
					{Title: "Duplicate", Target: publication.OutlineTarget{Name: "topic-two--bookmark-fragment"}},
				},
			},
			{Title: "External", Target: publication.OutlineTarget{URI: "https://example.test/help"}},
			{Title: "Repeated", Target: publication.OutlineTarget{Name: "topic-one"}},
		},
	}
}

func TestQPDFOutlineUpdateAndValidationPreserveOccurrenceTree(t *testing.T) {
	document := outlineTestDocument(t)
	update, expected, err := buildQPDFOutlineUpdate(document, outlineTestPlan())
	if err != nil {
		t.Fatal(err)
	}
	qpdf := update["qpdf"].([]any)
	objects := qpdf[1].(map[string]any)
	for key, value := range objects {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		document.Objects[key] = body
	}
	if err := validateQPDFOutline(document, expected); err != nil {
		t.Fatal(err)
	}
	flat := flattenOutline(expected)
	if len(flat) != 7 || flat[0].Title != "Cover" || flat[1].Title != "Contents" ||
		flat[2].Title != "Group" || descendantCount(flat[2]) != 2 ||
		flat[5].Title != "External" || flat[6].Title != "Repeated" {
		t.Fatalf("authoritative roots or occurrences changed: %+v", flat)
	}
}

func TestQPDFOutlineValidationRejectsCorruptionAndUnsafeActions(t *testing.T) {
	tests := map[string]func(map[string]json.RawMessage, []*pdfOutlineNode){
		"parent": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			mutateQPDFObject(t, objects, referenceFor(nodes[2].Children[0]), func(value map[string]any) {
				value["/Parent"] = "1 0 R"
			})
		},
		"cycle": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			mutateQPDFObject(t, objects, referenceFor(nodes[2].Children[0]), func(value map[string]any) {
				value["/Next"] = referenceFor(nodes[2].Children[0])
			})
		},
		"count": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			mutateQPDFObject(t, objects, referenceFor(nodes[2]), func(value map[string]any) {
				value["/Count"] = float64(999)
			})
		},
		"javascript action": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			external := nodes[3]
			mutateQPDFObject(t, objects, referenceFor(external), func(value map[string]any) {
				value["/A"] = map[string]any{"/S": "/JavaScript", "/JS": "u:alert(1)"}
			})
		},
		"launch action": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			external := nodes[3]
			mutateQPDFObject(t, objects, referenceFor(external), func(value map[string]any) {
				value["/A"] = map[string]any{"/S": "/Launch", "/F": "u:file"}
			})
		},
		"wrong destination": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			mutateQPDFObject(t, objects, referenceFor(nodes[0]), func(value map[string]any) {
				value["/Dest"] = []any{"4 0 R", "/Fit"}
			})
		},
		"malformed title": func(objects map[string]json.RawMessage, nodes []*pdfOutlineNode) {
			mutateQPDFObject(t, objects, referenceFor(nodes[0]), func(value map[string]any) {
				value["/Title"] = "b:00ff"
			})
		},
	}
	for name, corrupt := range tests {
		t.Run(name, func(t *testing.T) {
			document := outlineTestDocument(t)
			update, expected, err := buildQPDFOutlineUpdate(document, outlineTestPlan())
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range update["qpdf"].([]any)[1].(map[string]any) {
				body, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				document.Objects[key] = body
			}
			corrupt(document.Objects, expected)
			if err := validateQPDFOutline(document, expected); err == nil {
				t.Fatal("corrupt or unsafe outline was accepted")
			}
		})
	}
}

func TestQPDFOutlinePlanRejectsMissingDestinationAndInvalidTitle(t *testing.T) {
	document := outlineTestDocument(t)
	plan := outlineTestPlan()
	plan.Entries[0].Target.Name = "missing"
	if _, _, err := buildQPDFOutlineUpdate(document, plan); err == nil ||
		!strings.Contains(err.Error(), "missing named destination") {
		t.Fatalf("missing destination was accepted: %v", err)
	}
	plan = outlineTestPlan()
	plan.Entries[0].Title = "bad\x00title"
	if _, _, err := buildQPDFOutlineUpdate(document, plan); err == nil {
		t.Fatal("malformed Unicode/control title was accepted")
	}
}

func mutateQPDFObject(
	t *testing.T,
	objects map[string]json.RawMessage,
	reference string,
	change func(map[string]any),
) {
	t.Helper()
	key, err := qpdfReferenceKey(reference)
	if err != nil {
		t.Fatal(err)
	}
	var object qpdfObject
	if err := json.Unmarshal(objects[key], &object); err != nil {
		t.Fatal(err)
	}
	change(object.Value)
	objects[key], err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunQPDFBoundsOutputDiagnosticsAndProcessGroup(t *testing.T) {
	t.Run("output", func(t *testing.T) {
		var output bytes.Buffer
		file, err := os.CreateTemp(t.TempDir(), "qpdf-output-*.bin")
		if err != nil {
			t.Fatal(err)
		}
		writer := &limitedFileWriter{file: file, limit: 4}
		_, runErr := runQPDF(
			context.Background(), QPDFSidecar{Path: "/bin/sh"}, writer, 1<<30,
			"-c", "printf 12345",
		)
		closeErr := file.Close()
		if runErr == nil || closeErr != nil {
			t.Fatalf("qpdf output limit was not enforced: run=%v close=%v output=%q", runErr, closeErr, output.String())
		}
	})
	t.Run("diagnostics", func(t *testing.T) {
		_, err := runQPDF(
			context.Background(), QPDFSidecar{Path: "/bin/sh"}, nil, 1<<30,
			"-c", "i=0; while [ $i -lt 1100000 ]; do printf x >&2; i=$((i+1)); done; exit 9",
		)
		if err == nil || !strings.Contains(err.Error(), "[qpdf diagnostics truncated]") ||
			len(err.Error()) > QPDFOutlineProcessMaxLogBytes+1024 {
			t.Fatalf("qpdf diagnostics were not bounded: %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "child.pid")
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		_, err := runQPDF(
			ctx, QPDFSidecar{Path: "/bin/sh"}, nil, 1<<30,
			"-c", "sleep 30 & echo $! > \"$1\"; wait", "qpdf-test", pidFile,
		)
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("qpdf cancellation was not preserved: %v", err)
		}
		body, readErr := os.ReadFile(pidFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if killErr := syscall.Kill(pid, 0); !errors.Is(killErr, syscall.ESRCH) {
			t.Fatalf("qpdf child process survived cancellation: pid=%d err=%v", pid, killErr)
		}
	})
}

func TestInspectWithQPDFRejectsInvalidJSON(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "fake-qpdf")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '{\"qpdf\":['\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(root, "input.pdf")
	if err := os.WriteFile(input, []byte("%PDF-1.7\n%%EOF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := inspectWithQPDF(
		context.Background(), QPDFSidecar{Path: script}, input, root, "invalid",
		1<<30,
	)
	if err == nil {
		t.Fatal("invalid qpdf JSON was accepted")
	}
}
