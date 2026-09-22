package pdfgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"aos-cx-docs-dldr/internal/publication"
)

type qpdfOutlineMetrics struct {
	PlanSHA256          string
	EntryCount          int
	MaxDepth            int
	ExternalURIs        int
	RepeatedTargets     int
	SupplementaryGroups int
	Duration            time.Duration
	PeakRSS             int64
}

type qpdfJSON struct {
	QPDF  []json.RawMessage `json:"qpdf"`
	Pages []qpdfPage        `json:"pages"`
}

type qpdfHeader struct {
	JSONVersion int `json:"jsonversion"`
	MaxObjectID int `json:"maxobjectid"`
}

type qpdfPage struct {
	Object       string `json:"object"`
	PagePosition int    `json:"pageposfrom1"`
}

type qpdfObject struct {
	Value map[string]any `json:"value"`
}

type qpdfDocument struct {
	Header  qpdfHeader
	Objects map[string]json.RawMessage
	Pages   []qpdfPage
}

type pdfOutlineTarget struct {
	Name string
	URI  string
	Page int
}

type pdfOutlineNode struct {
	ID       int
	Title    string
	Target   pdfOutlineTarget
	Parent   *pdfOutlineNode
	Previous *pdfOutlineNode
	Next     *pdfOutlineNode
	Children []*pdfOutlineNode
}

func postprocessOutline(
	ctx context.Context,
	sidecar QPDFSidecar,
	rawPDF, outputPDF string,
	plan publication.OutlinePlan,
	maxPDFBytes, maxRSSBytes int64,
	tempBase string,
) (metrics qpdfOutlineMetrics, err error) {
	started := time.Now()
	if plan.SchemaVersion != publication.OutlineSchemaVersion ||
		plan.EntryCount < 1 || plan.EntryCount > QPDFOutlineProcessMaxNodeCount-2 ||
		plan.MaxDepth < 1 || plan.MaxDepth > QPDFOutlineProcessMaxDepth {
		return metrics, errors.New("generated PDF outline plan is outside supported bounds")
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return metrics, err
	}
	sum := sha256.Sum256(body)
	metrics.PlanSHA256 = hex.EncodeToString(sum[:])
	metrics.EntryCount = plan.EntryCount + 2
	metrics.MaxDepth = plan.MaxDepth
	metrics.ExternalURIs, metrics.RepeatedTargets, metrics.SupplementaryGroups = outlinePlanMetrics(plan)

	confirmed, err := reverifyQPDFSidecar(sidecar.Path)
	if err != nil {
		return metrics, fmt.Errorf("revalidate qpdf sidecar immediately before outline postprocessing: %w", err)
	}
	if confirmed != sidecar {
		return metrics, errors.New("qpdf sidecar identity changed between preflight and postprocessing")
	}
	tempRoot, err := os.MkdirTemp(tempBase, "aos-cx-docs-dldr-qpdf-")
	if err != nil {
		return metrics, fmt.Errorf("create qpdf temp root: %w", err)
	}
	if err := os.Chmod(tempRoot, 0o700); err != nil {
		_ = os.RemoveAll(tempRoot)
		return metrics, err
	}
	defer os.RemoveAll(tempRoot)

	rawDocument, peak, err := inspectWithQPDF(ctx, sidecar, rawPDF, tempRoot, "raw", maxRSSBytes)
	metrics.PeakRSS = max(metrics.PeakRSS, peak)
	if err != nil {
		return metrics, fmt.Errorf("inspect raw Chrome PDF with qpdf: %w", err)
	}
	update, expected, err := buildQPDFOutlineUpdate(rawDocument, plan)
	if err != nil {
		return metrics, err
	}
	updatePath := filepath.Join(tempRoot, "outline-update.json")
	if err := writePrivateJSON(updatePath, update); err != nil {
		return metrics, err
	}
	if _, err := os.Lstat(outputPDF); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return metrics, fmt.Errorf("qpdf output path already exists: %s", outputPDF)
		}
		return metrics, err
	}
	output, err := os.OpenFile(outputPDF, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return metrics, err
	}
	writer := &limitedFileWriter{file: output, limit: maxPDFBytes}
	peak, runErr := runQPDF(ctx, sidecar, writer, maxRSSBytes,
		rawPDF, "--update-from-json="+updatePath, "-")
	closeErr := errors.Join(output.Sync(), output.Close())
	metrics.PeakRSS = max(metrics.PeakRSS, peak)
	if err := errors.Join(runErr, closeErr); err != nil {
		return metrics, fmt.Errorf("replace generated PDF outline with qpdf: %w", err)
	}
	info, err := os.Lstat(outputPDF)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return metrics, errors.New("qpdf did not produce a regular private PDF")
	}
	if info.Size() < 32 || info.Size() > maxPDFBytes {
		return metrics, fmt.Errorf("qpdf output size %d is outside the allowed range", info.Size())
	}
	outputDocument, peak, err := inspectWithQPDF(ctx, sidecar, outputPDF, tempRoot, "postprocessed", maxRSSBytes)
	metrics.PeakRSS = max(metrics.PeakRSS, peak)
	if err != nil {
		return metrics, fmt.Errorf("inspect postprocessed PDF with qpdf: %w", err)
	}
	if err := validateQPDFOutline(outputDocument, expected); err != nil {
		return metrics, fmt.Errorf("validate postprocessed PDF outline: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return metrics, err
	}
	metrics.Duration = time.Since(started)
	return metrics, nil
}

func inspectWithQPDF(
	ctx context.Context,
	sidecar QPDFSidecar,
	pdfPath, tempRoot, label string,
	maxRSSBytes int64,
) (qpdfDocument, int64, error) {
	var document qpdfDocument
	jsonFile, err := os.CreateTemp(tempRoot, label+"-*.json")
	if err != nil {
		return document, 0, err
	}
	jsonPath := jsonFile.Name()
	defer os.Remove(jsonPath)
	writer := &limitedFileWriter{file: jsonFile, limit: QPDFOutlineValidationMaxBytes}
	peak, runErr := runQPDF(ctx, sidecar, writer, maxRSSBytes,
		"--json", "--json-stream-data=none", "--json-key=qpdf", "--json-key=pages", pdfPath)
	closeErr := errors.Join(jsonFile.Sync(), jsonFile.Close())
	if err := errors.Join(runErr, closeErr); err != nil {
		return document, peak, err
	}
	file, err := os.Open(jsonPath)
	if err != nil {
		return document, peak, err
	}
	var payload qpdfJSON
	decoder := json.NewDecoder(io.LimitReader(file, QPDFOutlineValidationMaxBytes+1))
	decodeErr := decoder.Decode(&payload)
	if decodeErr == nil {
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				decodeErr = errors.New("qpdf JSON contains trailing data")
			} else {
				decodeErr = err
			}
		}
	}
	closeErr = file.Close()
	if err := errors.Join(decodeErr, closeErr); err != nil {
		return document, peak, err
	}
	document, err = parseQPDFDocument(payload)
	return document, peak, err
}

func parseQPDFDocument(payload qpdfJSON) (qpdfDocument, error) {
	var document qpdfDocument
	if len(payload.QPDF) != 2 || len(payload.Pages) < 2 {
		return document, errors.New("qpdf JSON has an invalid document shape")
	}
	if err := json.Unmarshal(payload.QPDF[0], &document.Header); err != nil {
		return document, err
	}
	if document.Header.JSONVersion != 2 || document.Header.MaxObjectID < 1 {
		return document, errors.New("qpdf JSON version or object bound is unsupported")
	}
	if err := json.Unmarshal(payload.QPDF[1], &document.Objects); err != nil {
		return document, err
	}
	if len(document.Objects) < 3 || len(document.Objects) > 2_000_000 {
		return document, errors.New("qpdf JSON object count is outside the allowed range")
	}
	document.Pages = payload.Pages
	for index, page := range document.Pages {
		if page.PagePosition != index+1 {
			return document, errors.New("qpdf JSON page order is inconsistent")
		}
		if _, err := qpdfReferenceKey(page.Object); err != nil {
			return document, fmt.Errorf("invalid qpdf page reference: %w", err)
		}
	}
	return document, nil
}

func buildQPDFOutlineUpdate(
	document qpdfDocument,
	plan publication.OutlinePlan,
) (map[string]any, []*pdfOutlineNode, error) {
	catalogReference, catalog, err := qpdfCatalog(document)
	if err != nil {
		return nil, nil, err
	}
	destinations, err := qpdfNamedDestinations(document, catalog)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := expectedOutlineNodes(plan)
	if err != nil {
		return nil, nil, err
	}
	for _, node := range flattenOutline(nodes) {
		if node.Target.Name != "" {
			if _, ok := destinations["/"+node.Target.Name]; !ok {
				return nil, nil, fmt.Errorf("Chrome PDF is missing named destination %q", node.Target.Name)
			}
		}
		if node.Target.Page > len(document.Pages) {
			return nil, nil, fmt.Errorf("outline page destination %d exceeds page count", node.Target.Page)
		}
	}
	nextID := document.Header.MaxObjectID + 1
	rootID := nextID
	nextID++
	for _, node := range flattenOutline(nodes) {
		node.ID = nextID
		nextID++
	}
	rootReference := fmt.Sprintf("%d 0 R", rootID)
	catalogCopy := clonePDFDictionary(catalog)
	catalogCopy["/Outlines"] = rootReference
	objects := map[string]any{
		"obj:" + catalogReference: map[string]any{"value": catalogCopy},
	}
	all := flattenOutline(nodes)
	root := map[string]any{
		"/Type":  "/Outlines",
		"/First": referenceFor(nodes[0]),
		"/Last":  referenceFor(nodes[len(nodes)-1]),
		"/Count": len(all),
	}
	objects["obj:"+rootReference] = map[string]any{"value": root}
	for _, node := range all {
		value := map[string]any{
			"/Title":  "u:" + node.Title,
			"/Parent": rootReference,
		}
		if node.Parent != nil {
			value["/Parent"] = referenceFor(node.Parent)
		}
		if node.Previous != nil {
			value["/Prev"] = referenceFor(node.Previous)
		}
		if node.Next != nil {
			value["/Next"] = referenceFor(node.Next)
		}
		if len(node.Children) > 0 {
			value["/First"] = referenceFor(node.Children[0])
			value["/Last"] = referenceFor(node.Children[len(node.Children)-1])
			value["/Count"] = descendantCount(node)
		}
		switch {
		case node.Target.Name != "":
			value["/Dest"] = "/" + node.Target.Name
		case node.Target.URI != "":
			value["/A"] = map[string]any{"/S": "/URI", "/URI": "u:" + node.Target.URI}
		case node.Target.Page > 0:
			value["/Dest"] = []any{document.Pages[node.Target.Page-1].Object, "/Fit"}
		}
		objects["obj:"+referenceFor(node)] = map[string]any{"value": value}
	}
	update := map[string]any{
		"qpdf": []any{
			map[string]any{"jsonversion": 2},
			objects,
		},
	}
	return update, nodes, nil
}

func expectedOutlineNodes(plan publication.OutlinePlan) ([]*pdfOutlineNode, error) {
	cover := &pdfOutlineNode{Title: "Cover", Target: pdfOutlineTarget{Page: 1}}
	contents := &pdfOutlineNode{
		Title:  "Contents",
		Target: pdfOutlineTarget{Name: publication.OutlineContentsTarget},
	}
	nodes := []*pdfOutlineNode{cover, contents}
	for _, entry := range plan.Entries {
		node, err := expectedOutlineNode(entry, 1)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	linkOutlineNodes(nil, nodes)
	return nodes, nil
}

func expectedOutlineNode(entry publication.OutlineEntry, depth int) (*pdfOutlineNode, error) {
	if depth > QPDFOutlineProcessMaxDepth || !validOutlineTitle(entry.Title) {
		return nil, fmt.Errorf("invalid PDF outline title or depth at %q", entry.Title)
	}
	if entry.Target.Name != "" && entry.Target.URI != "" {
		return nil, fmt.Errorf("PDF outline entry %q has conflicting destinations", entry.Title)
	}
	node := &pdfOutlineNode{
		Title:  entry.Title,
		Target: pdfOutlineTarget{Name: entry.Target.Name, URI: entry.Target.URI},
	}
	for _, child := range entry.Children {
		nested, err := expectedOutlineNode(child, depth+1)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, nested)
	}
	if node.Target == (pdfOutlineTarget{}) {
		node.Target = firstPDFOutlineTarget(node.Children)
	}
	linkOutlineNodes(node, node.Children)
	return node, nil
}

func linkOutlineNodes(parent *pdfOutlineNode, nodes []*pdfOutlineNode) {
	for index, node := range nodes {
		node.Parent = parent
		if index > 0 {
			node.Previous = nodes[index-1]
		}
		if index+1 < len(nodes) {
			node.Next = nodes[index+1]
		}
	}
}

func validateQPDFOutline(document qpdfDocument, expected []*pdfOutlineNode) error {
	_, catalog, err := qpdfCatalog(document)
	if err != nil {
		return err
	}
	destinations, err := qpdfNamedDestinations(document, catalog)
	if err != nil {
		return err
	}
	rootReference, ok := catalog["/Outlines"].(string)
	if !ok {
		return errors.New("postprocessed PDF catalog has no outline root")
	}
	root, err := qpdfDictionary(document, rootReference)
	if err != nil {
		return err
	}
	firstReference, ok := root["/First"].(string)
	if !ok || root["/Type"] != "/Outlines" ||
		intValue(root["/Count"]) != len(flattenOutline(expected)) {
		return errors.New("postprocessed PDF outline root is inconsistent")
	}
	visited := map[string]bool{}
	lastReference, err := validateOutlineSiblings(
		document, rootReference, firstReference, expected, destinations, visited, 1,
	)
	if err != nil {
		return err
	}
	if root["/Last"] != lastReference {
		return errors.New("postprocessed PDF outline root /Last is inconsistent")
	}
	if len(visited) != len(flattenOutline(expected)) {
		return fmt.Errorf("postprocessed PDF outline visited %d of %d entries", len(visited), len(flattenOutline(expected)))
	}
	return nil
}

func validateOutlineSiblings(
	document qpdfDocument,
	parentReference string,
	firstReference string,
	expected []*pdfOutlineNode,
	destinations map[string]any,
	visited map[string]bool,
	depth int,
) (string, error) {
	if depth > QPDFOutlineProcessMaxDepth || len(visited)+len(expected) > QPDFOutlineProcessMaxNodeCount {
		return "", errors.New("postprocessed PDF outline exceeds validation bounds")
	}
	reference := firstReference
	previousReference := ""
	for index, node := range expected {
		if reference == "" {
			return "", errors.New("postprocessed PDF outline sibling chain ended early")
		}
		if visited[reference] {
			return "", fmt.Errorf("postprocessed PDF outline cycle at %s", reference)
		}
		visited[reference] = true
		actual, err := qpdfDictionary(document, reference)
		if err != nil {
			return "", err
		}
		title, ok := actual["/Title"].(string)
		if !ok || title != "u:"+node.Title || !validOutlineTitle(strings.TrimPrefix(title, "u:")) {
			return "", fmt.Errorf("postprocessed PDF outline title mismatch at %s", reference)
		}
		if actual["/Parent"] != parentReference {
			return "", fmt.Errorf(
				"postprocessed PDF outline parent mismatch for %q: got %v want %s",
				node.Title, actual["/Parent"], parentReference,
			)
		}
		if index == 0 {
			if _, ok := actual["/Prev"]; ok {
				return "", fmt.Errorf("postprocessed PDF first sibling %q has /Prev", node.Title)
			}
		} else if actual["/Prev"] != previousReference {
			return "", fmt.Errorf("postprocessed PDF outline /Prev mismatch for %q", node.Title)
		}
		if err := validateOutlineTarget(document, actual, node.Target, destinations); err != nil {
			return "", fmt.Errorf("postprocessed PDF outline target for %q: %w", node.Title, err)
		}
		if len(node.Children) == 0 {
			if _, first := actual["/First"]; first {
				return "", fmt.Errorf("postprocessed PDF leaf %q has children", node.Title)
			}
			if _, count := actual["/Count"]; count {
				return "", fmt.Errorf("postprocessed PDF leaf %q has /Count", node.Title)
			}
		} else {
			childFirst, ok := actual["/First"].(string)
			if !ok || intValue(actual["/Count"]) != descendantCount(node) {
				return "", fmt.Errorf("postprocessed PDF child links/count mismatch for %q", node.Title)
			}
			childLast, err := validateOutlineSiblings(
				document, reference, childFirst, node.Children, destinations, visited, depth+1,
			)
			if err != nil {
				return "", err
			}
			if actual["/Last"] != childLast {
				return "", fmt.Errorf("postprocessed PDF /Last mismatch for %q", node.Title)
			}
		}
		previousReference = reference
		next, hasNext := actual["/Next"].(string)
		if index+1 == len(expected) {
			if hasNext {
				return "", fmt.Errorf("postprocessed PDF last sibling %q has /Next", node.Title)
			}
			reference = ""
		} else {
			if !hasNext {
				return "", fmt.Errorf("postprocessed PDF sibling chain ends at %q", node.Title)
			}
			reference = next
		}
	}
	return previousReference, nil
}

func validateOutlineTarget(
	document qpdfDocument,
	actual map[string]any,
	expected pdfOutlineTarget,
	destinations map[string]any,
) error {
	dest, hasDest := actual["/Dest"]
	action, hasAction := actual["/A"]
	if hasDest && hasAction {
		return errors.New("entry has both /Dest and /A")
	}
	switch {
	case expected.Name != "":
		name, ok := dest.(string)
		if !ok || name != "/"+expected.Name || hasAction {
			return errors.New("named destination differs")
		}
		value, ok := destinations[name]
		if !ok {
			return errors.New("named destination is absent")
		}
		return validateDestinationArray(document, value, 0)
	case expected.URI != "":
		if hasDest {
			return errors.New("external entry uses /Dest")
		}
		dictionary, ok := action.(map[string]any)
		if !ok || len(dictionary) != 2 || dictionary["/S"] != "/URI" ||
			dictionary["/URI"] != "u:"+expected.URI {
			return errors.New("external entry is not an exact URI action")
		}
		return nil
	case expected.Page > 0:
		if hasAction {
			return errors.New("page entry has an action")
		}
		return validateDestinationArray(document, dest, expected.Page)
	default:
		if hasDest || hasAction {
			return errors.New("non-clickable entry unexpectedly has an action")
		}
		return nil
	}
}

func validateDestinationArray(document qpdfDocument, value any, expectedPage int) error {
	if reference, ok := value.(string); ok {
		resolved, err := qpdfValue(document, reference)
		if err != nil {
			return err
		}
		value = resolved
	}
	array, ok := value.([]any)
	if !ok || len(array) < 2 {
		return errors.New("destination is not an array")
	}
	pageReference, ok := array[0].(string)
	if !ok {
		return errors.New("destination has no page reference")
	}
	position := 0
	for index, page := range document.Pages {
		if page.Object == pageReference {
			position = index + 1
			break
		}
	}
	if position == 0 || (expectedPage > 0 && position != expectedPage) {
		return fmt.Errorf("destination page %d does not match expected page %d", position, expectedPage)
	}
	if mode, ok := array[1].(string); !ok || !strings.HasPrefix(mode, "/") {
		return errors.New("destination mode is invalid")
	}
	return nil
}

func qpdfCatalog(document qpdfDocument) (string, map[string]any, error) {
	trailer, err := qpdfDictionaryByKey(document, "trailer")
	if err != nil {
		return "", nil, err
	}
	reference, ok := trailer["/Root"].(string)
	if !ok {
		return "", nil, errors.New("qpdf trailer has no catalog reference")
	}
	catalog, err := qpdfDictionary(document, reference)
	return reference, catalog, err
}

func qpdfNamedDestinations(document qpdfDocument, catalog map[string]any) (map[string]any, error) {
	value, ok := catalog["/Dests"]
	if !ok {
		return nil, errors.New("Chrome PDF catalog has no named destinations")
	}
	if reference, ok := value.(string); ok {
		resolved, err := qpdfValue(document, reference)
		if err != nil {
			return nil, err
		}
		value = resolved
	}
	destinations, ok := value.(map[string]any)
	if !ok || len(destinations) == 0 {
		return nil, errors.New("Chrome PDF named destination dictionary is empty or unsupported")
	}
	return destinations, nil
}

func qpdfDictionary(document qpdfDocument, reference string) (map[string]any, error) {
	key, err := qpdfReferenceKey(reference)
	if err != nil {
		return nil, err
	}
	return qpdfDictionaryByKey(document, key)
}

func qpdfDictionaryByKey(document qpdfDocument, key string) (map[string]any, error) {
	raw, ok := document.Objects[key]
	if !ok {
		return nil, fmt.Errorf("qpdf JSON object %q is absent", key)
	}
	var object qpdfObject
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object.Value == nil {
		return nil, fmt.Errorf("qpdf JSON object %q is not a dictionary", key)
	}
	return object.Value, nil
}

func qpdfValue(document qpdfDocument, reference string) (any, error) {
	dictionary, err := qpdfDictionary(document, reference)
	if err == nil {
		return dictionary, nil
	}
	key, keyErr := qpdfReferenceKey(reference)
	if keyErr != nil {
		return nil, keyErr
	}
	raw := document.Objects[key]
	var object struct {
		Value any `json:"value"`
	}
	if unmarshalErr := json.Unmarshal(raw, &object); unmarshalErr != nil {
		return nil, unmarshalErr
	}
	if object.Value == nil {
		return nil, err
	}
	return object.Value, nil
}

func qpdfReferenceKey(reference string) (string, error) {
	fields := strings.Fields(reference)
	if len(fields) != 3 || fields[2] != "R" {
		return "", fmt.Errorf("invalid PDF indirect reference %q", reference)
	}
	object, objectErr := strconv.Atoi(fields[0])
	generation, generationErr := strconv.Atoi(fields[1])
	if objectErr != nil || generationErr != nil || object < 1 || generation < 0 {
		return "", fmt.Errorf("invalid PDF indirect reference %q", reference)
	}
	return "obj:" + reference, nil
}

func writePrivateJSON(path string, value any) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	err = errors.Join(encoder.Encode(value), file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}

func runQPDF(
	ctx context.Context,
	sidecar QPDFSidecar,
	stdout io.Writer,
	maxRSSBytes int64,
	args ...string,
) (peakRSS int64, err error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, sidecar.Path, args...)
	configureProcessGroup(command)
	logs := &boundedLog{limit: QPDFOutlineProcessMaxLogBytes, label: "qpdf"}
	command.Stderr = logs
	command.Stdout = stdout
	if err := command.Start(); err != nil {
		return 0, err
	}
	monitorCtx, stopMonitor := context.WithCancel(commandCtx)
	monitor := monitorNamedProcessGroup(monitorCtx, cancel, command.Process.Pid, maxRSSBytes, "qpdf")
	waitErr := command.Wait()
	stopMonitor()
	observation := <-monitor
	observation.PeakRSS = max(observation.PeakRSS, processPeakRSS(command.ProcessState))
	if observation.PeakRSS > maxRSSBytes {
		observation.Err = errors.Join(observation.Err, fmt.Errorf(
			"qpdf process RSS %d exceeds %d-byte limit",
			observation.PeakRSS, maxRSSBytes,
		))
	}
	if observation.Err != nil {
		err = errors.Join(err, observation.Err)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(logs.String())
		if detail != "" {
			waitErr = fmt.Errorf("%w; bounded qpdf diagnostics: %s", waitErr, detail)
		}
		err = errors.Join(err, waitErr)
	}
	if commandCtx.Err() != nil {
		err = errors.Join(err, commandCtx.Err())
	}
	return observation.PeakRSS, err
}

func outlinePlanMetrics(plan publication.OutlinePlan) (external, repeated, supplementary int) {
	targets := map[string]int{}
	var visit func([]publication.OutlineEntry)
	visit = func(entries []publication.OutlineEntry) {
		for _, entry := range entries {
			if entry.Target.URI != "" {
				external++
			}
			key := entry.Target.Name
			if key == "" {
				key = entry.Target.URI
			}
			if key != "" {
				targets[key]++
			}
			if entry.Title == "Additional archived topics" {
				supplementary++
			}
			visit(entry.Children)
		}
	}
	visit(plan.Entries)
	for _, count := range targets {
		if count > 1 {
			repeated += count - 1
		}
	}
	return external, repeated, supplementary
}

func clonePDFDictionary(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func flattenOutline(nodes []*pdfOutlineNode) []*pdfOutlineNode {
	var result []*pdfOutlineNode
	for _, node := range nodes {
		result = append(result, node)
		result = append(result, flattenOutline(node.Children)...)
	}
	return result
}

func firstPDFOutlineTarget(nodes []*pdfOutlineNode) pdfOutlineTarget {
	for _, node := range nodes {
		if node.Target != (pdfOutlineTarget{}) {
			return node.Target
		}
		if target := firstPDFOutlineTarget(node.Children); target != (pdfOutlineTarget{}) {
			return target
		}
	}
	return pdfOutlineTarget{}
}

func descendantCount(node *pdfOutlineNode) int {
	count := 0
	for _, child := range node.Children {
		count += 1 + descendantCount(child)
	}
	return count
}

func referenceFor(node *pdfOutlineNode) string {
	return fmt.Sprintf("%d 0 R", node.ID)
}

func intValue(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	default:
		return -1
	}
}

func validOutlineTitle(value string) bool {
	if value == "" || len(value) > 16<<10 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && r != '\t' {
			return false
		}
	}
	return true
}
