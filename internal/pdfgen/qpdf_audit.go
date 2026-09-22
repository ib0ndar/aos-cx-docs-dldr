package pdfgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const QPDFAcceptanceTimeout = 5 * time.Minute

type OutlineAuditEntry struct {
	Depth        int
	PhysicalPage int
	Title        string
}

type OutlineAudit struct {
	BundleSHA256     string
	Entries          []OutlineAuditEntry
	ExecutablePath   string
	ExecutableSHA256 string
	Version          string
}

func AuditOutline(
	ctx context.Context,
	explicitQPDFPath, pdfPath, tempBase string,
) (OutlineAudit, error) {
	if ctx == nil {
		return OutlineAudit{}, errors.New("qpdf outline audit requires a context")
	}
	sidecar, err := ResolveQPDFSidecar(explicitQPDFPath)
	if err != nil {
		return OutlineAudit{}, err
	}
	info, err := os.Lstat(pdfPath)
	if err != nil {
		return OutlineAudit{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() < 32 || info.Size() > QPDFOutlineValidationMaxBytes {
		return OutlineAudit{}, errors.New("qpdf acceptance input is not a bounded nonsymlink regular PDF")
	}
	if tempBase == "" {
		tempBase = os.TempDir()
	}
	tempRoot, err := os.MkdirTemp(tempBase, "aoscx-pdf-outline-audit-")
	if err != nil {
		return OutlineAudit{}, err
	}
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		return OutlineAudit{}, err
	}
	tempInfo, err := os.Lstat(tempRoot)
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return OutlineAudit{}, err
	}
	defer func() {
		if current, err := os.Lstat(tempRoot); err == nil && os.SameFile(tempInfo, current) &&
			current.IsDir() && current.Mode()&os.ModeSymlink == 0 {
			_ = os.RemoveAll(tempRoot)
		}
	}()
	confirmed, err := reverifyQPDFSidecar(sidecar.Path)
	if err != nil {
		return OutlineAudit{}, fmt.Errorf("revalidate qpdf sidecar immediately before acceptance audit: %w", err)
	}
	if confirmed != sidecar {
		return OutlineAudit{}, errors.New("qpdf sidecar identity changed before acceptance audit")
	}
	auditCtx, cancel := context.WithTimeout(ctx, QPDFAcceptanceTimeout)
	defer cancel()
	document, _, err := inspectWithQPDF(
		auditCtx, sidecar, pdfPath, tempRoot, "acceptance", QPDFOutlineProcessMaxRSSBytes,
	)
	if err != nil {
		return OutlineAudit{}, err
	}
	current, err := os.Lstat(pdfPath)
	if err != nil || !os.SameFile(info, current) {
		return OutlineAudit{}, errors.New("qpdf acceptance input identity changed during audit")
	}
	entries, err := auditActualOutline(document)
	if err != nil {
		return OutlineAudit{}, err
	}
	return OutlineAudit{
		BundleSHA256: sidecar.BundleSHA256, Entries: entries,
		ExecutablePath: sidecar.Path, ExecutableSHA256: sidecar.ExecutableSHA256,
		Version: sidecar.Version,
	}, nil
}

func auditActualOutline(document qpdfDocument) ([]OutlineAuditEntry, error) {
	_, catalog, err := qpdfCatalog(document)
	if err != nil {
		return nil, err
	}
	destinations, err := qpdfNamedDestinations(document, catalog)
	if err != nil {
		return nil, err
	}
	rootReference, ok := catalog["/Outlines"].(string)
	if !ok {
		return nil, errors.New("acceptance PDF catalog has no outline root")
	}
	root, err := qpdfDictionary(document, rootReference)
	if err != nil {
		return nil, err
	}
	first, firstOK := root["/First"].(string)
	last, lastOK := root["/Last"].(string)
	count := intValue(root["/Count"])
	if root["/Type"] != "/Outlines" || !firstOK || !lastOK ||
		count < 1 || count > QPDFOutlineProcessMaxNodeCount {
		return nil, errors.New("acceptance PDF outline root is inconsistent")
	}
	visited := map[string]bool{}
	entries, actualLast, err := auditActualOutlineSiblings(
		document, destinations, rootReference, first, 1, visited,
	)
	if err != nil {
		return nil, err
	}
	if actualLast != last || len(entries) != count || len(visited) != count {
		return nil, errors.New("acceptance PDF outline root count or /Last is inconsistent")
	}
	return entries, nil
}

func auditActualOutlineSiblings(
	document qpdfDocument,
	destinations map[string]any,
	parentReference, firstReference string,
	depth int,
	visited map[string]bool,
) ([]OutlineAuditEntry, string, error) {
	if depth < 1 || depth > QPDFOutlineProcessMaxDepth {
		return nil, "", errors.New("acceptance PDF outline exceeds depth limit")
	}
	reference := firstReference
	previous := ""
	last := ""
	var result []OutlineAuditEntry
	for reference != "" {
		if len(visited) >= QPDFOutlineProcessMaxNodeCount || visited[reference] {
			return nil, "", fmt.Errorf("acceptance PDF outline cycle or count overflow at %s", reference)
		}
		visited[reference] = true
		actual, err := qpdfDictionary(document, reference)
		if err != nil {
			return nil, "", err
		}
		if actual["/Parent"] != parentReference {
			return nil, "", fmt.Errorf("acceptance PDF outline parent mismatch at %s", reference)
		}
		if previous == "" {
			if _, ok := actual["/Prev"]; ok {
				return nil, "", fmt.Errorf("acceptance PDF first sibling has /Prev at %s", reference)
			}
		} else if actual["/Prev"] != previous {
			return nil, "", fmt.Errorf("acceptance PDF outline /Prev mismatch at %s", reference)
		}
		rawTitle, ok := actual["/Title"].(string)
		title := strings.TrimPrefix(rawTitle, "u:")
		if !ok || rawTitle == title || !validOutlineTitle(title) {
			return nil, "", fmt.Errorf("acceptance PDF outline title is invalid at %s", reference)
		}
		page, err := auditActualOutlineTarget(document, actual, destinations)
		if err != nil {
			return nil, "", fmt.Errorf("acceptance PDF outline target for %q: %w", title, err)
		}
		result = append(result, OutlineAuditEntry{Depth: depth, PhysicalPage: page, Title: title})
		childFirst, hasChildren := actual["/First"].(string)
		if hasChildren {
			childLast, ok := actual["/Last"].(string)
			childCount := intValue(actual["/Count"])
			if !ok || childCount < 1 {
				return nil, "", fmt.Errorf("acceptance PDF outline child links/count are invalid for %q", title)
			}
			children, actualChildLast, err := auditActualOutlineSiblings(
				document, destinations, reference, childFirst, depth+1, visited,
			)
			if err != nil {
				return nil, "", err
			}
			if actualChildLast != childLast || len(children) != childCount {
				return nil, "", fmt.Errorf("acceptance PDF outline child /Last/count mismatch for %q", title)
			}
			result = append(result, children...)
		} else if _, lastOK := actual["/Last"]; lastOK {
			return nil, "", fmt.Errorf("acceptance PDF outline leaf has /Last for %q", title)
		} else if _, countOK := actual["/Count"]; countOK {
			return nil, "", fmt.Errorf("acceptance PDF outline leaf has /Count for %q", title)
		}
		last = reference
		previous = reference
		next, hasNext := actual["/Next"].(string)
		if hasNext {
			reference = next
		} else {
			reference = ""
		}
	}
	return result, last, nil
}

func auditActualOutlineTarget(
	document qpdfDocument,
	actual map[string]any,
	destinations map[string]any,
) (int, error) {
	destination, hasDestination := actual["/Dest"]
	action, hasAction := actual["/A"]
	if hasDestination && hasAction {
		return 0, errors.New("entry has both /Dest and /A")
	}
	if hasAction {
		dictionary, ok := action.(map[string]any)
		if !ok || len(dictionary) != 2 || dictionary["/S"] != "/URI" {
			return 0, errors.New("external entry is not an exact URI-only action")
		}
		uri, ok := dictionary["/URI"].(string)
		if !ok || !strings.HasPrefix(uri, "u:") || len(uri) <= 2 {
			return 0, errors.New("external URI action is invalid")
		}
		return -1, nil
	}
	if !hasDestination {
		return 0, errors.New("entry has no destination")
	}
	if name, ok := destination.(string); ok {
		value, exists := destinations[name]
		if !exists {
			return 0, errors.New("named destination is absent")
		}
		destination = value
	}
	return auditDestinationPage(document, destination)
}

func auditDestinationPage(document qpdfDocument, value any) (int, error) {
	if reference, ok := value.(string); ok {
		resolved, err := qpdfValue(document, reference)
		if err != nil {
			return 0, err
		}
		value = resolved
	}
	array, ok := value.([]any)
	if !ok || len(array) < 2 {
		return 0, errors.New("destination is not an array")
	}
	pageReference, ok := array[0].(string)
	if !ok {
		return 0, errors.New("destination has no page reference")
	}
	mode, ok := array[1].(string)
	if !ok || !strings.HasPrefix(mode, "/") {
		return 0, errors.New("destination mode is invalid")
	}
	for index, page := range document.Pages {
		if page.Object == pageReference {
			return index + 1, nil
		}
	}
	return 0, errors.New("destination page is absent")
}
