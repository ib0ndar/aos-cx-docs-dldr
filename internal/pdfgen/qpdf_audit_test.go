package pdfgen

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAuditActualOutlineUsesValidatedQPDFObjectGraph(t *testing.T) {
	document := outlineTestDocument(t)
	update, _, err := buildQPDFOutlineUpdate(document, outlineTestPlan())
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
	entries, err := auditActualOutline(document)
	if err != nil {
		t.Fatal(err)
	}
	var got []OutlineAuditEntry
	got = append(got, entries...)
	want := []OutlineAuditEntry{
		{Depth: 1, PhysicalPage: 1, Title: "Cover"},
		{Depth: 1, PhysicalPage: 1, Title: "Contents"},
		{Depth: 1, PhysicalPage: 1, Title: "Group"},
		{Depth: 2, PhysicalPage: 1, Title: "Duplicate"},
		{Depth: 2, PhysicalPage: 2, Title: "Duplicate"},
		{Depth: 1, PhysicalPage: -1, Title: "External"},
		{Depth: 1, PhysicalPage: 1, Title: "Repeated"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outline audit=%+v want=%+v", got, want)
	}
}

func TestAuditActualOutlineRejectsUnsafeActionWithoutWeakeningValidator(t *testing.T) {
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
	external := expected[3]
	mutateQPDFObject(t, document.Objects, referenceFor(external), func(value map[string]any) {
		value["/A"] = map[string]any{"/S": "/Launch", "/F": "u:file"}
	})
	if err := validateQPDFOutline(document, expected); err == nil {
		t.Fatal("existing generated-PDF validator accepted unsafe action")
	}
	if _, err := auditActualOutline(document); err == nil {
		t.Fatal("acceptance audit accepted unsafe action")
	}
}
