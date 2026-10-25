package source

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"aos-cx-docs-dldr/internal/model"
	"github.com/titanous/json5"
	"golang.org/x/net/html"
)

// Oxygen WebHelp Responsive publications open on a tile index. Every topic
// page carries only the top level of the publication TOC plus the branch of
// the current topic; deeper levels are data files that the page loads on
// demand, oxygen-webhelp/app/nav-links/json/<id>.js, each assigning
// oxy_webhelp_navlinks = {"<id>": {"topics": [...]}}. A topic's children are
// either inline in "topics" or in the file named by its "next" value; the
// file's menu.hasChildren describes the top menu, not the TOC, and is ignored.
// Topics outside the TOC, such as a legal disclaimer, are reachable only
// through the rel="prev" and rel="next" topic links.

var (
	oxygenNavLinksModule = regexp.MustCompile(`(?s)^oxy_webhelp_navlinks\s*=\s*(\{.*\})\s*;?$`)
	oxygenTocID          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// maxOxygenMatter bounds how many topics are followed through prev/next links
// before and after the TOC.
const maxOxygenMatter = 50

type oxygenTopic struct {
	Title  string        `json:"title"`
	Href   string        `json:"href"`
	TocID  string        `json:"tocID"`
	Next   string        `json:"next"`
	Topics []oxygenTopic `json:"topics"`
}

type oxygenPlanner struct {
	*planner
	root    string
	chunks  []string
	visited map[string]bool
}

// oxygenFirstTile returns the first tile link of an Oxygen WebHelp main page,
// the topic the portal opens a guide on.
func oxygenFirstTile(tree *html.Node) (string, bool) {
	if len(nodes(tree, func(n *html.Node) bool { return hasClass(n, "wh_main_page") })) == 0 {
		return "", false
	}
	for _, tile := range nodes(tree, func(n *html.Node) bool { return hasClass(n, "wh_tile_text") }) {
		for _, link := range nodes(tile, func(n *html.Node) bool { return n.Data == "a" }) {
			if href, ok := attr(link, "href"); ok && label(href) {
				return href, true
			}
		}
	}
	return "", false
}

func (p *planner) oxygen(index input, tree *html.Node, first string) (model.DocumentPlan, error) {
	root, err := joinSource(index.url, "./", false)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	base, err := staticBase(tree, index.url, root)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	seedURL, err := joinSource(base, first, false)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if !inside(seedURL, root) {
		return model.DocumentPlan{}, fmt.Errorf("Oxygen guide opens outside its publication: %s", seedURL)
	}
	o := &oxygenPlanner{planner: p, root: root, visited: map[string]bool{}}
	seed, seedTree, seedBase, err := o.page(seedURL, "toc")
	if err != nil {
		return model.DocumentPlan{}, err
	}
	if p.plan.Styles, err = styles(p.ctx, seedTree, seedBase); err != nil {
		return model.DocumentPlan{}, err
	}
	toc, err := o.topLevel(seedTree, seedBase)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	pages := oxygenPages(toc, root)
	if len(pages) == 0 {
		return model.DocumentPlan{}, errors.New("Oxygen TOC contains no document topics")
	}
	firstTree, firstBase := seedTree, seedBase
	if pages[0] != canonicalPage(seed.url) {
		if _, firstTree, firstBase, err = o.page(pages[0], "topic"); err != nil {
			return model.DocumentPlan{}, err
		}
	}
	front, err := o.matter(firstTree, firstBase, "prev", pages)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	_, lastTree, lastBase, err := o.page(pages[len(pages)-1], "topic")
	if err != nil {
		return model.DocumentPlan{}, err
	}
	back, err := o.matter(lastTree, lastBase, "next", pages)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	// Topics are registered in reading order: front matter, the TOC, then
	// back matter.
	for index := len(front) - 1; index >= 0; index-- {
		if err := p.topic(front[index].URL, front[index].Title, ""); err != nil {
			return model.DocumentPlan{}, err
		}
		p.plan.TOC = append(p.plan.TOC, front[index])
	}
	if err := o.register(toc); err != nil {
		return model.DocumentPlan{}, err
	}
	p.plan.TOC = append(p.plan.TOC, toc...)
	for _, entry := range back {
		if err := p.topic(entry.URL, entry.Title, ""); err != nil {
			return model.DocumentPlan{}, err
		}
		p.plan.TOC = append(p.plan.TOC, entry)
	}
	return p.finish(root, seed.url, o.chunks)
}

// page reads one publication page inside the guide.
func (o *oxygenPlanner) page(raw, role string) (input, *html.Node, string, error) {
	in, err := o.read(raw, role, scopeInside(o.root))
	if err != nil {
		return input{}, nil, "", err
	}
	tree, err := o.html(in, false)
	if err != nil {
		return input{}, nil, "", err
	}
	base, err := staticBase(tree, in.url, o.root)
	if err != nil {
		return input{}, nil, "", err
	}
	return in, tree, base, nil
}

// topLevel reads the root entries of a topic page's publication TOC and
// expands every branch from its nav-links data.
func (o *oxygenPlanner) topLevel(tree *html.Node, base string) ([]model.TocEntry, error) {
	containers := nodes(tree, func(n *html.Node) bool {
		id, _ := attr(n, "id")
		return n.Data == "nav" && id == "wh_publication_toc"
	})
	if len(containers) != 1 {
		return nil, errors.New("Oxygen topic page must contain one publication TOC")
	}
	trees := nodes(containers[0], func(n *html.Node) bool {
		role, _ := attr(n, "role")
		return n.Data == "ul" && role == "tree"
	})
	if len(trees) != 1 {
		return nil, errors.New("Oxygen publication TOC must contain one tree")
	}
	var result []model.TocEntry
	for item := trees[0].FirstChild; item != nil; item = item.NextSibling {
		if item.Type != html.ElementNode || item.Data != "li" {
			continue
		}
		if err := o.check(0); err != nil {
			return nil, err
		}
		refs := nodes(item, func(n *html.Node) bool { return hasClass(n, "topicref") && nearest(n, "li") == item })
		if len(refs) != 1 {
			return nil, errors.New("Oxygen TOC entry must have one topicref")
		}
		links := nodes(refs[0], func(n *html.Node) bool {
			_, found := attr(n, "href")
			return n.Data == "a" && found
		})
		if len(links) != 1 {
			return nil, errors.New("Oxygen TOC entry must have one topic link")
		}
		href, _ := attr(links[0], "href")
		target, err := joinSource(base, href, false)
		if err != nil {
			return nil, err
		}
		entry := model.TocEntry{Title: text(links[0]), URL: target}
		if !label(entry.Title) {
			return nil, errors.New("Oxygen TOC has empty title")
		}
		state, _ := attr(refs[0], "data-state")
		if state != "leaf" {
			id, _ := attr(refs[0], "data-tocid")
			if entry.Children, err = o.navLinks(id, 1); err != nil {
				return nil, err
			}
		}
		result = append(result, entry)
	}
	if len(result) == 0 {
		return nil, errors.New("Oxygen publication TOC is empty")
	}
	return result, nil
}

// navLinks reads one nav-links data file and returns its entries, expanding
// deeper levels named by "next".
func (o *oxygenPlanner) navLinks(id string, depth int) ([]model.TocEntry, error) {
	if !oxygenTocID.MatchString(id) {
		return nil, fmt.Errorf("invalid Oxygen TOC identifier %q", id)
	}
	if o.visited[id] {
		return nil, fmt.Errorf("Oxygen TOC data %q is referenced more than once", id)
	}
	o.visited[id] = true
	if len(o.chunks) >= maxChunks {
		return nil, errors.New("Oxygen TOC data exceeds file limit")
	}
	chunkURL, err := joinSource(o.root, "oxygen-webhelp/app/nav-links/json/"+url.PathEscape(id)+".js", false)
	if err != nil {
		return nil, err
	}
	in, err := o.read(chunkURL, "chunk", scopeInside(o.root))
	if err != nil {
		return nil, err
	}
	o.chunks = append(o.chunks, in.url)
	topics, err := o.navLinksData(in, id)
	if err != nil {
		return nil, err
	}
	return o.entries(topics, depth)
}

func (o *oxygenPlanner) navLinksData(in input, id string) ([]oxygenTopic, error) {
	media, err := mediaType(in)
	if err != nil {
		return nil, err
	}
	if media != "" && media != "application/javascript" && media != "text/javascript" &&
		media != "application/x-javascript" && media != "text/plain" && media != "application/octet-stream" {
		return nil, fmt.Errorf("expected Oxygen JavaScript TOC data, received %s: %s", media, in.url)
	}
	body, err := inventoryText(in.body, in.headers.Get("Content-Type"))
	if err != nil {
		return nil, err
	}
	match := oxygenNavLinksModule.FindStringSubmatch(strings.TrimSpace(string(body)))
	if match == nil {
		return nil, fmt.Errorf("expected one oxy_webhelp_navlinks assignment: %s", in.url)
	}
	var value map[string]struct {
		Topics []oxygenTopic `json:"topics"`
	}
	err = json5.UnmarshalWithOptions([]byte(match[1]), &value, json5.Options{
		DisallowDuplicateKeys: true, MaxDepth: 2*maxDepth + 16, MaxValues: 16 * maxEntries, MaxBytes: maxInventoryBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("invalid Oxygen TOC data at %s: %w", in.url, err)
	}
	data, ok := value[id]
	if len(value) != 1 || !ok {
		return nil, fmt.Errorf("Oxygen TOC data at %s does not describe %q", in.url, id)
	}
	return data.Topics, o.ctx.Err()
}

func (o *oxygenPlanner) entries(topics []oxygenTopic, depth int) ([]model.TocEntry, error) {
	var result []model.TocEntry
	for _, topic := range topics {
		if err := o.check(depth); err != nil {
			return nil, err
		}
		title, err := oxygenTitle(topic.Title)
		if err != nil {
			return nil, err
		}
		target, err := joinSource(o.root, topic.Href, false)
		if err != nil {
			return nil, err
		}
		entry := model.TocEntry{Title: title, URL: target}
		switch {
		case len(topic.Topics) > 0:
			entry.Children, err = o.entries(topic.Topics, depth+1)
		case topic.Next != "":
			entry.Children, err = o.navLinks(topic.Next, depth+1)
		}
		if err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, nil
}

// oxygenTitle reduces a TOC title, which Oxygen stores as HTML, to its text.
func oxygenTitle(markup string) (string, error) {
	fragment, err := html.Parse(strings.NewReader("<!doctype html><title></title><body>" + markup))
	if err != nil {
		return "", err
	}
	bodies := nodes(fragment, func(n *html.Node) bool { return n.Data == "body" })
	if len(bodies) != 1 || !label(text(bodies[0])) {
		return "", errors.New("Oxygen TOC has empty title")
	}
	return text(bodies[0]), nil
}

// register records the TOC's topics in reading order, keeping links that
// leave the publication as retained external navigation.
func (o *oxygenPlanner) register(entries []model.TocEntry) error {
	for _, entry := range entries {
		if err := o.navigation(entry.URL, entry.Title, o.root); err != nil {
			return err
		}
		if err := o.register(entry.Children); err != nil {
			return err
		}
	}
	return nil
}

// matter follows rel="prev" or rel="next" topic links from a TOC end page and
// returns the publication topics that the TOC does not list, nearest first.
func (o *oxygenPlanner) matter(tree *html.Node, base, rel string, toc []string) ([]model.TocEntry, error) {
	listed := map[string]bool{}
	for _, page := range toc {
		listed[page] = true
	}
	var result []model.TocEntry
	for len(result) < maxOxygenMatter {
		links := nodes(tree, func(n *html.Node) bool {
			value, _ := attr(n, "rel")
			_, found := attr(n, "href")
			return n.Data == "a" && found && strings.EqualFold(value, rel)
		})
		if len(links) == 0 {
			return result, nil
		}
		href, _ := attr(links[0], "href")
		for _, link := range links[1:] {
			if other, _ := attr(link, "href"); other != href {
				return nil, fmt.Errorf("Oxygen topic has conflicting rel=%q links", rel)
			}
		}
		target, err := joinSource(base, href, false)
		if err != nil {
			return nil, err
		}
		page := canonicalPage(target)
		if !inside(page, o.root) || listed[page] {
			return result, nil
		}
		listed[page] = true
		var in input
		if in, tree, base, err = o.page(page, "topic"); err != nil {
			return nil, err
		}
		titles := nodes(tree, func(n *html.Node) bool { return n.Data == "title" })
		if len(titles) != 1 || !label(text(titles[0])) {
			return nil, fmt.Errorf("Oxygen topic outside the TOC has no title: %s", in.url)
		}
		result = append(result, model.TocEntry{Title: text(titles[0]), URL: page})
	}
	return nil, errors.New("Oxygen topics outside the TOC exceed the follow limit")
}

// oxygenPages returns the distinct publication pages of a TOC in reading
// order; entries that leave the publication are not pages of it.
func oxygenPages(entries []model.TocEntry, root string) []string {
	var result []string
	seen := map[string]bool{}
	var walk func([]model.TocEntry)
	walk = func(list []model.TocEntry) {
		for _, entry := range list {
			page := canonicalPage(entry.URL)
			if !seen[page] && inside(page, root) {
				seen[page] = true
				result = append(result, page)
			}
			walk(entry.Children)
		}
	}
	walk(entries)
	return result
}

func canonicalPage(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}
