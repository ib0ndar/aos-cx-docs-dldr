package library

import (
	"testing"

	"aos-cx-docs-dldr/internal/model"
)

func TestValidGeneratedPDFValidation(t *testing.T) {
	valid := model.GeneratedPDFValidation{
		PageCount: 2, LetterMediaBoxes: 2, FontObjects: 1, ImageObjects: 1,
		AnnotationObjects: 1, OutlineObjects: 1, TOCEntries: 3, TOCLinksChecked: 3,
		LocalRequests: 2, InlineDataRequests: 0, ScriptElements: 1, LoadedImages: 1,
		InternalLinksChecked: 4, FailedImages: 0, MissingTargets: 0, ExternalRequests: 0,
		CanonicalCoverTitles: 1, PublisherChromeElements: 0, FixedOrStickyElements: 0,
		OutlineEntries: 6, OutlineMaxDepth: 3, OutlineExternalURIs: 1,
		OutlineRepeatedTargets: 1, OutlineSupplementaryGroups: 0,
		FontsLoaded: true, ScriptExecutionDisabled: true, ResourceClosureValidated: true,
		CoverTypographyValidated: true, HeadingTypographyValidated: true,
		HeadingPaginationValidated: true, FooterOverlayValidated: true,
		OutlineValidated: true, StructuralValidated: true,
	}
	if !validGeneratedPDFValidation(valid) {
		t.Fatal("valid generated PDF audit was rejected")
	}
	tests := map[string]func(*model.GeneratedPDFValidation){
		"page count":         func(value *model.GeneratedPDFValidation) { value.PageCount = 0 },
		"media boxes":        func(value *model.GeneratedPDFValidation) { value.LetterMediaBoxes-- },
		"fonts":              func(value *model.GeneratedPDFValidation) { value.FontObjects = 0 },
		"TOC audit":          func(value *model.GeneratedPDFValidation) { value.TOCLinksChecked-- },
		"local requests":     func(value *model.GeneratedPDFValidation) { value.LocalRequests = 0 },
		"external requests":  func(value *model.GeneratedPDFValidation) { value.ExternalRequests = 1 },
		"canonical cover":    func(value *model.GeneratedPDFValidation) { value.CanonicalCoverTitles = 0 },
		"publisher chrome":   func(value *model.GeneratedPDFValidation) { value.PublisherChromeElements = 1 },
		"fixed positioning":  func(value *model.GeneratedPDFValidation) { value.FixedOrStickyElements = 1 },
		"font readiness":     func(value *model.GeneratedPDFValidation) { value.FontsLoaded = false },
		"script disablement": func(value *model.GeneratedPDFValidation) { value.ScriptExecutionDisabled = false },
		"resource closure":   func(value *model.GeneratedPDFValidation) { value.ResourceClosureValidated = false },
		"cover typography":   func(value *model.GeneratedPDFValidation) { value.CoverTypographyValidated = false },
		"heading typography": func(value *model.GeneratedPDFValidation) { value.HeadingTypographyValidated = false },
		"heading pagination": func(value *model.GeneratedPDFValidation) { value.HeadingPaginationValidated = false },
		"footer overlay":     func(value *model.GeneratedPDFValidation) { value.FooterOverlayValidated = false },
		"missing targets":    func(value *model.GeneratedPDFValidation) { value.MissingTargets = 1 },
		"failed image details": func(value *model.GeneratedPDFValidation) {
			value.FailedImageDetails = []string{`https://publisher.example/topic.htm -> src="missing.png"`}
		},
		"structural status":    func(value *model.GeneratedPDFValidation) { value.StructuralValidated = false },
		"outline status":       func(value *model.GeneratedPDFValidation) { value.OutlineValidated = false },
		"outline count":        func(value *model.GeneratedPDFValidation) { value.OutlineEntries = 2 },
		"outline depth":        func(value *model.GeneratedPDFValidation) { value.OutlineMaxDepth = 0 },
		"negative image count": func(value *model.GeneratedPDFValidation) { value.ImageObjects = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			invalid := valid
			mutate(&invalid)
			if validGeneratedPDFValidation(invalid) {
				t.Fatalf("invalid generated PDF audit passed: %+v", invalid)
			}
		})
	}
}
