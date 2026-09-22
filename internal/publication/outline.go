package publication

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strings"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
)

const (
	OutlineSchemaVersion      = 2
	FooterSourceSchemaVersion = 2
	OutlineContentsTarget     = "aos-cx-docs-dldr-print-contents"
	OutlineProvenanceTarget   = "aos-cx-docs-dldr-print-provenance"
	maxOutlineEntries         = 100_000
	maxOutlineDepth           = 1024
)

type OutlineTarget struct {
	Name string `json:"name,omitempty"`
	URI  string `json:"uri,omitempty"`
}

type OutlineEntry struct {
	Title    string         `json:"title"`
	Target   OutlineTarget  `json:"target,omitempty"`
	Children []OutlineEntry `json:"children,omitempty"`
}

type OutlinePlan struct {
	SchemaVersion int            `json:"schema_version"`
	GuideTitle    string         `json:"guide_title"`
	Entries       []OutlineEntry `json:"entries"`
	EntryCount    int            `json:"entry_count"`
	MaxDepth      int            `json:"max_depth"`
}

type FooterSourceTopic struct {
	Target string `json:"target"`
	Label  string `json:"label"`
}

type FooterSourcePlan struct {
	SchemaVersion    int                 `json:"schema_version"`
	GuideTitle       string              `json:"guide_title"`
	ContentsTarget   string              `json:"contents_target"`
	ProvenanceTarget string              `json:"provenance_target"`
	Topics           []FooterSourceTopic `json:"topics"`
}

func (a *assembler) footerSourcePlan() (FooterSourcePlan, error) {
	result := FooterSourcePlan{
		SchemaVersion:    FooterSourceSchemaVersion,
		GuideTitle:       normalizeOutlineTitle(a.result.Document.Title),
		ContentsTarget:   OutlineContentsTarget,
		ProvenanceTarget: OutlineProvenanceTarget,
	}
	if result.GuideTitle == "" {
		return result, errors.New("PDF footer guide title is empty")
	}
	labels := map[string]string{}
	var visit func([]model.TocEntry, string, bool) error
	visit = func(entries []model.TocEntry, parent string, root bool) error {
		for _, entry := range entries {
			title := normalizeOutlineTitle(entry.Title)
			if title == "" {
				return errors.New("PDF footer category title is empty")
			}
			if topic := a.topicForURL(entry.URL); topic != nil {
				if _, exists := labels[topic.full]; !exists {
					label := parent
					if root {
						label = title
					}
					labels[topic.full] = label
				}
			}
			if err := visit(entry.Children, title, false); err != nil {
				return err
			}
		}
		return nil
	}
	entries, err := normalizeOutlineWrappers(a.plan)
	if err != nil {
		return result, err
	}
	if err := visit(entries, result.GuideTitle, true); err != nil {
		return result, err
	}
	for _, topic := range a.topics {
		label := labels[topic.full]
		if label == "" {
			if topic.record.Supplementary {
				label = "Additional archived topics"
			} else {
				label = result.GuideTitle
			}
		}
		result.Topics = append(result.Topics, FooterSourceTopic{Target: topic.anchor, Label: label})
	}
	if len(result.Topics) == 0 {
		return result, errors.New("PDF footer source plan has no topics")
	}
	return result, nil
}

func (a *assembler) outlinePlan() (OutlinePlan, error) {
	entries, err := normalizeOutlineWrappers(a.plan)
	if err != nil {
		return OutlinePlan{}, err
	}
	result := OutlinePlan{
		SchemaVersion: OutlineSchemaVersion,
		GuideTitle:    normalizeOutlineTitle(a.result.Document.Title),
	}
	if result.GuideTitle == "" {
		return result, errors.New("PDF outline guide title is empty")
	}
	for _, entry := range entries {
		item, err := a.outlineEntry(entry, 1)
		if err != nil {
			return result, err
		}
		result.Entries = append(result.Entries, item)
	}
	var additional []OutlineEntry
	for _, topic := range a.topics {
		if topic.record.Supplementary && !a.included[topic.full] {
			additional = append(additional, OutlineEntry{
				Title: topicTitle(topic),
				Target: OutlineTarget{
					Name: topic.anchor,
				},
			})
		}
	}
	if len(additional) > 0 {
		result.Entries = append(result.Entries, OutlineEntry{
			Title:    "Additional archived topics",
			Target:   firstOutlineTarget(additional),
			Children: additional,
		})
	}
	result.EntryCount, result.MaxDepth = outlineMetrics(result.Entries, 1)
	if result.EntryCount < 1 || result.EntryCount > maxOutlineEntries {
		return result, fmt.Errorf("PDF outline entry count %d is outside the allowed range", result.EntryCount)
	}
	if result.MaxDepth > maxOutlineDepth {
		return result, fmt.Errorf("PDF outline depth %d exceeds limit %d", result.MaxDepth, maxOutlineDepth)
	}
	return result, nil
}

func normalizeOutlineWrappers(plan model.DocumentPlan) ([]model.TocEntry, error) {
	entries := slices.Clone(plan.TOC)
	if len(entries) == 0 {
		return entries, nil
	}

	prefix := 0
	if isDocumentNavigationLeaf(entries[0], plan) {
		prefix++
		if len(entries) > prefix && isContentsNavigationLeaf(entries[prefix], plan) {
			prefix++
		}
	}
	if prefix > 0 {
		remaining := entries[prefix:]
		if len(remaining) == 0 {
			return nil, errors.New("PDF outline source navigation prefix has no authoritative root content")
		}
		if !isValidatedRootWrapper(remaining[0], plan) {
			for _, entry := range remaining[1:] {
				if isValidatedRootWrapper(entry, plan) {
					return nil, errors.New("PDF outline source navigation prefix has ambiguous root content")
				}
			}
			return slices.Clone(remaining), nil
		}
		var trailing []model.TocEntry
		titlePages := 0
		for _, entry := range remaining[1:] {
			switch {
			case isExternalOutlineRoot(entry, plan):
				trailing = append(trailing, entry)
			case isSourceTitlePageNavigationLeaf(entry, plan):
				titlePages++
				if titlePages > 1 {
					return nil, errors.New("PDF outline source navigation prefix has duplicate title-page leaves")
				}
			default:
				return nil, errors.New("PDF outline source navigation prefix has ambiguous root content")
			}
		}
		promoted, removed, err := unwrapOutlineRootChain(remaining[0], plan)
		if err != nil {
			return nil, err
		}
		if removed == 0 {
			return nil, errors.New("PDF outline source navigation prefix has ambiguous or missing wrapper chain")
		}
		promoted = append(promoted, slices.Clone(trailing)...)
		return promoted, nil
	}

	if len(entries) == 1 {
		promoted, removed, err := unwrapOutlineRootChain(entries[0], plan)
		if err != nil {
			return nil, err
		}
		if removed > 0 {
			return promoted, nil
		}
	}
	for _, entry := range entries {
		if isValidatedRootWrapper(entry, plan) {
			return nil, errors.New("PDF outline source navigation prefix has ambiguous root content")
		}
	}
	return entries, nil
}

func unwrapOutlineRootChain(entry model.TocEntry, plan model.DocumentPlan) ([]model.TocEntry, int, error) {
	removed := 0
	seen := map[string]bool{}
	for {
		kind := outlineRootWrapperKind(entry, plan)
		if kind == "" {
			return []model.TocEntry{entry}, removed, nil
		}
		if seen[kind] {
			return nil, 0, fmt.Errorf("PDF outline source wrapper %q is duplicated", kind)
		}
		seen[kind] = true
		if len(entry.Children) == 0 {
			return nil, 0, fmt.Errorf("PDF outline source wrapper %q has no children", kind)
		}
		removed++
		children := slices.Clone(entry.Children)
		if len(children) != 1 {
			return children, removed, nil
		}
		entry = children[0]
	}
}

func isValidatedRootWrapper(entry model.TocEntry, plan model.DocumentPlan) bool {
	return outlineRootWrapperKind(entry, plan) != ""
}

func outlineRootWrapperKind(entry model.TocEntry, plan model.DocumentPlan) string {
	if len(entry.Children) == 0 {
		return ""
	}
	title := normalizeOutlineTitle(entry.Title)
	switch {
	case strings.EqualFold(title, "home") && sameOutlineTopic(entry.URL, plan.Document.URL):
		return "home"
	case strings.EqualFold(title, "table of contents") &&
		(entry.URL == "" || planHasOutlineTopic(plan, entry.URL)):
		return "contents"
	case guideWrapperTitle(title, plan.Document) &&
		(entry.URL == "" || sameOutlineTopic(entry.URL, plan.Document.URL) ||
			planHasOutlineTopic(plan, entry.URL)):
		return "guide-title"
	case isSourceGuideTitleWrapper(entry, plan):
		return "source-guide-title"
	default:
		return ""
	}
}

func isSourceGuideTitleWrapper(entry model.TocEntry, plan model.DocumentPlan) bool {
	if len(entry.Children) == 0 || entry.URL == "" ||
		strings.EqualFold(normalizeOutlineTitle(entry.Title), "table of contents") {
		return false
	}
	matches := 0
	for _, root := range plan.TOC {
		if isContentsNavigationLeaf(root, plan) && sameOutlineTopic(entry.URL, root.URL) {
			matches++
		}
	}
	return matches == 1
}

func isDocumentNavigationLeaf(entry model.TocEntry, plan model.DocumentPlan) bool {
	return len(entry.Children) == 0 &&
		strings.EqualFold(normalizeOutlineTitle(entry.Title), normalizeOutlineTitle(plan.Document.Title)) &&
		sameOutlineTopic(entry.URL, plan.Document.URL)
}

func isContentsNavigationLeaf(entry model.TocEntry, plan model.DocumentPlan) bool {
	return len(entry.Children) == 0 &&
		strings.EqualFold(normalizeOutlineTitle(entry.Title), "table of contents") &&
		planHasOutlineTopic(plan, entry.URL)
}

func isExternalOutlineRoot(entry model.TocEntry, plan model.DocumentPlan) bool {
	if entry.URL == "" || isValidatedRootWrapper(entry, plan) || planHasOutlineTopic(plan, entry.URL) {
		return false
	}
	base, baseErr := url.Parse(plan.Document.URL)
	ref, refErr := url.Parse(entry.URL)
	if baseErr != nil || refErr != nil {
		return false
	}
	resolved := base.ResolveReference(ref)
	return (resolved.Scheme == "https" || resolved.Scheme == "http") &&
		resolved.Host != "" && resolved.User == nil &&
		!strings.EqualFold(resolved.Host, base.Host)
}

func isSourceTitlePageNavigationLeaf(entry model.TocEntry, plan model.DocumentPlan) bool {
	if plan.Document.Kind != "flare" || len(entry.Children) != 0 ||
		normalizeOutlineTitle(entry.Title) != "Title" ||
		entry.URL == "" || !planHasOutlineTopic(plan, entry.URL) {
		return false
	}
	ref, refErr := url.Parse(entry.URL)
	home, homeErr := url.Parse(plan.Document.URL)
	if refErr != nil || homeErr != nil || ref.Scheme == "" || ref.Host == "" ||
		ref.User != nil || ref.RawQuery != "" || ref.ForceQuery || ref.Fragment != "" ||
		!strings.EqualFold(ref.Scheme, home.Scheme) ||
		!strings.EqualFold(ref.Host, home.Host) {
		return false
	}
	return path.Base(ref.Path) == "tit.htm"
}

func planHasOutlineTopic(plan model.DocumentPlan, raw string) bool {
	for _, topic := range plan.Topics {
		if sameOutlineTopic(raw, topic.URL) || sameOutlineTopic(raw, topic.FetchURL) {
			return true
		}
	}
	return false
}

func guideWrapperTitle(title string, document model.Document) bool {
	guide := normalizeOutlineTitle(document.Title)
	if guide == "" {
		return false
	}
	if strings.EqualFold(title, guide) {
		return true
	}
	lowerTitle, lowerGuide := strings.ToLower(title), strings.ToLower(guide)
	if !strings.HasSuffix(lowerTitle, " "+lowerGuide) {
		return false
	}
	prefix := strings.TrimSpace(title[:len(title)-len(guide)])
	if prefix == "" {
		return false
	}
	allowed := map[string]bool{
		outlineIdentityToken(document.Platform): true,
		outlineIdentityToken(document.Version):  true,
	}
	matchedIdentity := false
	product := false
	release := false
	for _, field := range strings.Fields(prefix) {
		token := outlineIdentityToken(field)
		if token == "" {
			continue
		}
		switch {
		case token == "aoscx":
			product = true
		case allowed[token]:
			matchedIdentity = true
		case outlineReleaseToken(field):
			release = true
		default:
			return false
		}
	}
	return matchedIdentity || product && release
}

func outlineIdentityToken(value string) string {
	var out strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			out.WriteRune(character)
		}
	}
	return out.String()
}

func outlineReleaseToken(value string) bool {
	hasDigit := false
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9':
			hasDigit = true
		case character == '.' || character == '-' || character == 'x' || character == 'X':
		default:
			return false
		}
	}
	return hasDigit
}

func sameOutlineTopic(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	leftURL, leftErr := url.Parse(left)
	rightURL, rightErr := url.Parse(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftURL.Fragment = ""
	rightURL.Fragment = ""
	leftKey, leftErr := fetch.CanonicalURL(leftURL.String())
	rightKey, rightErr := fetch.CanonicalURL(rightURL.String())
	return leftErr == nil && rightErr == nil && leftKey == rightKey
}

func normalizeOutlineTitle(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func (a *assembler) outlineEntry(entry model.TocEntry, depth int) (OutlineEntry, error) {
	if depth > maxOutlineDepth {
		return OutlineEntry{}, errors.New("PDF outline nesting exceeds limit")
	}
	title := normalizeOutlineTitle(entry.Title)
	if title == "" {
		return OutlineEntry{}, errors.New("PDF outline entry title is empty")
	}

	item := OutlineEntry{Title: title}
	if topic := a.topicForURL(entry.URL); topic != nil {
		fragment := ""
		if parsed, err := url.Parse(entry.URL); err == nil {
			fragment = parsed.Fragment
		}
		target, err := a.topicDestinationWithoutEvidence(topic, fragment)
		if err != nil {
			return OutlineEntry{}, err
		}
		item.Target.Name = target
		a.included[topic.full] = true
	} else if entry.URL != "" {
		uri, err := a.externalOutlineURI(entry.URL)
		if err != nil {
			return OutlineEntry{}, err
		}
		item.Target.URI = uri
	}
	for _, child := range entry.Children {
		nested, err := a.outlineEntry(child, depth+1)
		if err != nil {
			return OutlineEntry{}, err
		}
		item.Children = append(item.Children, nested)
	}
	if item.Target == (OutlineTarget{}) {
		item.Target = firstOutlineTarget(item.Children)
	}
	return item, nil
}

func (a *assembler) topicDestinationWithoutEvidence(topic *topicInfo, fragment string) (string, error) {
	if fragment == "" {
		return topic.anchor, nil
	}
	if target := topic.ids[fragment]; target != "" {
		return target, nil
	}
	message := fmt.Sprintf("Publisher bookmark #%s is absent from source topic %s", fragment, topic.record.URL)
	if topic.record.SourceBookmarksComplete && !slices.Contains(topic.record.SourceBookmarks, fragment) {
		return topic.anchor, nil
	}
	if slices.Contains(a.archive.Warnings, message) {
		return topic.anchor, nil
	}
	return "", fmt.Errorf("missing local PDF outline bookmark #%s in %s", fragment, topic.record.Path)
}

func (a *assembler) externalOutlineURI(raw string) (string, error) {
	base, err := url.Parse(a.result.Document.URL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid PDF outline URL %q: %w", raw, err)
	}
	resolved := base.ResolveReference(ref)
	if (resolved.Scheme != "https" && resolved.Scheme != "http") ||
		resolved.Host == "" || resolved.User != nil {
		return "", fmt.Errorf("unsafe external PDF outline URL %q", raw)
	}
	return resolved.String(), nil
}

func firstOutlineTarget(entries []OutlineEntry) OutlineTarget {
	for _, entry := range entries {
		if entry.Target != (OutlineTarget{}) {
			return entry.Target
		}
		if target := firstOutlineTarget(entry.Children); target != (OutlineTarget{}) {
			return target
		}
	}
	return OutlineTarget{}
}

func outlineMetrics(entries []OutlineEntry, depth int) (count, maximum int) {
	for _, entry := range entries {
		count++
		maximum = max(maximum, depth)
		childCount, childDepth := outlineMetrics(entry.Children, depth+1)
		count += childCount
		maximum = max(maximum, childDepth)
	}
	return count, maximum
}
