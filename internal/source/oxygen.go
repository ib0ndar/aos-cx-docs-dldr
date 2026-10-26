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
//
// The data files can name topic files that were never published, where the
// pages themselves link the published file. So each branch's own topic page,
// which the archive needs anyway, is read with its data file, and when the
// page's rendered TOC shows the branch expanded, its children are checked
// against the data and their links taken from the page.

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
	pages   map[string]oxygenPage
}

// oxygenPage is a parsed publication page.
type oxygenPage struct {
	in   input
	tree *html.Node
	base string
}

// oxygenItem is one entry of a page's rendered TOC.
type oxygenItem struct {
	tocID, state string
	entry        model.TocEntry
}

// oxygenBranch is a TOC entry whose children are still to be read, from their
// own nav-links file or inline in the parent's data.
type oxygenBranch struct {
	entry  *model.TocEntry
	tocID  string
	file   string
	inline []oxygenTopic
	depth  int
}

// oxygenTiles returns the tile links of an Oxygen WebHelp main page in order.
// The first is the topic the portal opens a guide on.
func oxygenTiles(tree *html.Node) []string {
	if len(nodes(tree, func(n *html.Node) bool { return hasClass(n, "wh_main_page") })) == 0 {
		return nil
	}
	var tiles []string
	for _, tile := range nodes(tree, func(n *html.Node) bool { return hasClass(n, "wh_tile_text") }) {
		for _, link := range nodes(tile, func(n *html.Node) bool { return n.Data == "a" }) {
			if href, ok := attr(link, "href"); ok && label(href) {
				tiles = append(tiles, href)
				break
			}
		}
	}
	return tiles
}

// oxygen plans an Oxygen WebHelp guide. Every topic page renders the top level
// of the TOC, so the seed is the first tile's page the publisher serves; a
// tile page it answers with HTTP 404 or 410 stays in the TOC, and the archive
// records it as unpublished.
func (p *planner) oxygen(index input, tree *html.Node, tiles []string) (model.DocumentPlan, error) {
	root, err := joinSource(index.url, "./", false)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	base, err := staticBase(tree, index.url, root)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	o := &oxygenPlanner{planner: p, root: root, visited: map[string]bool{}, pages: map[string]oxygenPage{}}
	var seed oxygenPage
	for _, tile := range tiles {
		seedURL, err := joinSource(base, tile, false)
		if err != nil {
			return model.DocumentPlan{}, err
		}
		if !inside(seedURL, root) {
			return model.DocumentPlan{}, fmt.Errorf("Oxygen guide opens outside its publication: %s", seedURL)
		}
		seed, err = o.page(seedURL, "toc")
		if status, missing := permanentPublisherStatus(err); missing {
			p.warning(fmt.Sprintf("Oxygen tile page %s returned HTTP %d; the TOC is read from the next tile's page", seedURL, status))
			continue
		}
		if err != nil {
			return model.DocumentPlan{}, err
		}
		break
	}
	if seed.tree == nil {
		return model.DocumentPlan{}, errors.New("the publisher serves none of the Oxygen guide's tile pages")
	}
	if p.plan.Styles, err = styles(p.ctx, seed.tree, seed.base); err != nil {
		return model.DocumentPlan{}, err
	}
	toc, err := o.topLevel(seed)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	pages := oxygenPages(toc, root)
	if len(pages) == 0 {
		return model.DocumentPlan{}, errors.New("Oxygen TOC contains no document topics")
	}
	front, err := o.matterAround(pages[0], "prev", pages)
	if err != nil {
		return model.DocumentPlan{}, err
	}
	back, err := o.matterAround(pages[len(pages)-1], "next", pages)
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
	return p.finish(root, seed.in.url, o.chunks)
}

// page reads one publication page inside the guide, once.
func (o *oxygenPlanner) page(raw, role string) (oxygenPage, error) {
	if page, ok := o.pages[raw]; ok {
		return page, nil
	}
	in, err := o.read(raw, role, scopeInside(o.root))
	if err != nil {
		return oxygenPage{}, err
	}
	return o.parse(raw, in)
}

func (o *oxygenPlanner) parse(raw string, in input) (oxygenPage, error) {
	tree, err := o.html(in, false)
	if err != nil {
		return oxygenPage{}, err
	}
	base, err := staticBase(tree, in.url, o.root)
	if err != nil {
		return oxygenPage{}, err
	}
	page := oxygenPage{in: in, tree: tree, base: base}
	o.pages[raw] = page
	return page, nil
}

// branchPage returns the publication page a TOC entry opens, if it has one.
func (o *oxygenPlanner) branchPage(raw string) (string, bool) {
	page := canonicalPage(raw)
	u, err := url.Parse(page)
	if err != nil || !inside(page, o.root) {
		return "", false
	}
	path := strings.ToLower(u.Path)
	return page, strings.HasSuffix(path, ".htm") || strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".xhtml")
}

// oxygenTree returns the list of a page's publication TOC.
func oxygenTree(tree *html.Node) (*html.Node, error) {
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
	return trees[0], nil
}

// oxygenItems reads the entries of one rendered TOC list.
func oxygenItems(list *html.Node, base string) ([]oxygenItem, error) {
	var result []oxygenItem
	for item := list.FirstChild; item != nil; item = item.NextSibling {
		if item.Type != html.ElementNode || item.Data != "li" {
			continue
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
		entry := model.TocEntry{Title: text(links[0])}
		if !voidLink(href) {
			target, err := joinSource(base, href, false)
			if err != nil {
				return nil, err
			}
			entry.URL = target
		}
		if !label(entry.Title) {
			return nil, errors.New("Oxygen TOC has empty title")
		}
		tocID, _ := attr(refs[0], "data-tocid")
		state, _ := attr(refs[0], "data-state")
		result = append(result, oxygenItem{tocID: tocID, state: state, entry: entry})
	}
	return result, nil
}

// oxygenRendered returns the children a page's rendered TOC lists under the
// entry tocID, and whether the page shows that entry expanded.
func oxygenRendered(page oxygenPage, tocID string) ([]oxygenItem, bool, error) {
	list, err := oxygenTree(page.tree)
	if err != nil {
		return nil, false, nil
	}
	refs := nodes(list, func(n *html.Node) bool {
		id, _ := attr(n, "data-tocid")
		return hasClass(n, "topicref") && id == tocID
	})
	if len(refs) != 1 {
		return nil, false, nil
	}
	if state, _ := attr(refs[0], "data-state"); state != "expanded" {
		return nil, false, nil
	}
	item := nearest(refs[0], "li")
	if item == nil {
		return nil, false, nil
	}
	var groups []*html.Node
	for child := item.FirstChild; child != nil; child = child.NextSibling {
		if role, _ := attr(child, "role"); child.Type == html.ElementNode && child.Data == "ul" && role == "group" {
			groups = append(groups, child)
		}
	}
	if len(groups) != 1 {
		return nil, false, nil
	}
	children, err := oxygenItems(groups[0], page.base)
	if err != nil {
		return nil, false, fmt.Errorf("rendered TOC of %s: %w", page.in.url, err)
	}
	return children, true, nil
}

// topLevel reads the root entries of a topic page's publication TOC and
// expands every branch.
func (o *oxygenPlanner) topLevel(seed oxygenPage) ([]model.TocEntry, error) {
	list, err := oxygenTree(seed.tree)
	if err != nil {
		return nil, err
	}
	items, err := oxygenItems(list, seed.base)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("Oxygen publication TOC is empty")
	}
	result := make([]model.TocEntry, len(items))
	var branches []oxygenBranch
	for index, item := range items {
		if err := o.check(0); err != nil {
			return nil, err
		}
		result[index] = item.entry
		if item.state != "leaf" {
			branches = append(branches, oxygenBranch{entry: &result[index], tocID: item.tocID, file: item.tocID, depth: 1})
		}
	}
	return result, o.expand(branches)
}

// expand reads the children of the branches one TOC level at a time. Each
// level's nav-links files and branch pages are requested together.
func (o *oxygenPlanner) expand(level []oxygenBranch) error {
	for len(level) > 0 {
		var jobs []readJob
		files := make([]string, len(level))
		queued := map[string]bool{}
		for index, branch := range level {
			if branch.file != "" {
				file, err := o.navLinksURL(branch.file)
				if err != nil {
					return err
				}
				files[index] = file
				jobs = append(jobs, readJob{url: file, role: "chunk"})
			}
			// A branch page is evidence for verify. A page the publisher does
			// not serve leaves the branch with its data links, and the archive
			// records the topic as unpublished.
			if page, ok := o.branchPage(branch.entry.URL); ok && !queued[page] {
				if _, done := o.pages[page]; !done {
					queued[page] = true
					jobs = append(jobs, readJob{url: page, role: "topic", optional: true})
				}
			}
		}
		inputs, err := o.readAll(jobs, scopeInside(o.root))
		if err != nil {
			return err
		}
		read := map[string]input{}
		for index, job := range jobs {
			if job.role == "topic" {
				if inputs[index].url == "" {
					continue
				}
				if _, err := o.parse(job.url, inputs[index]); err != nil {
					return err
				}
				continue
			}
			read[job.url] = inputs[index]
		}
		var next []oxygenBranch
		for index, branch := range level {
			topics := branch.inline
			if branch.file != "" {
				in := read[files[index]]
				o.chunks = append(o.chunks, in.url)
				if topics, err = o.navLinksData(in, branch.file); err != nil {
					return err
				}
			}
			children, err := o.entries(topics, branch.depth)
			if err != nil {
				return err
			}
			if err := o.verify(branch, topics, children); err != nil {
				return err
			}
			branch.entry.Children = children
			for child, topic := range topics {
				switch {
				case len(topic.Topics) > 0:
					next = append(next, oxygenBranch{entry: &branch.entry.Children[child], tocID: topic.TocID,
						inline: topic.Topics, depth: branch.depth + 1})
				case topic.Next != "":
					next = append(next, oxygenBranch{entry: &branch.entry.Children[child], tocID: topic.TocID,
						file: topic.Next, depth: branch.depth + 1})
				}
			}
		}
		level = next
	}
	return nil
}

// navLinksURL returns the nav-links file of a TOC identifier, refusing
// identifiers already read.
func (o *oxygenPlanner) navLinksURL(id string) (string, error) {
	if !oxygenTocID.MatchString(id) {
		return "", fmt.Errorf("invalid Oxygen TOC identifier %q", id)
	}
	if o.visited[id] {
		return "", fmt.Errorf("Oxygen TOC data %q is referenced more than once", id)
	}
	if len(o.visited) >= maxChunks {
		return "", errors.New("Oxygen TOC data exceeds file limit")
	}
	o.visited[id] = true
	return joinSource(o.root, "oxygen-webhelp/app/nav-links/json/"+url.PathEscape(id)+".js", false)
}

// verify checks a branch's children against the rendered TOC of the branch's
// own page. When the page shows the branch expanded, both must list the same
// entries, and the page's links replace links in the data that differ.
func (o *oxygenPlanner) verify(branch oxygenBranch, topics []oxygenTopic, children []model.TocEntry) error {
	raw, ok := o.branchPage(branch.entry.URL)
	if !ok {
		return nil
	}
	page, ok := o.pages[raw]
	if !ok {
		return nil
	}
	rendered, expanded, err := oxygenRendered(page, branch.tocID)
	if err != nil || !expanded {
		return err
	}
	same := len(rendered) == len(topics)
	for index := 0; same && index < len(rendered); index++ {
		same = rendered[index].tocID == topics[index].TocID
	}
	if !same {
		return fmt.Errorf("Oxygen TOC data for %q lists different topics than the rendered TOC of %s", branch.tocID, page.in.url)
	}
	for index := range children {
		if published := rendered[index].entry.URL; published != children[index].URL {
			o.notice(model.NoticePublisherLinkCorrected, fmt.Sprintf("Publisher TOC data links %q to %s; using %s from the rendered TOC of %s",
				children[index].Title, children[index].URL, published, page.in.url))
			children[index].URL = published
		}
	}
	return nil
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

// entries reads one level of nav-links topics; expand reads their children.
func (o *oxygenPlanner) entries(topics []oxygenTopic, depth int) ([]model.TocEntry, error) {
	result := make([]model.TocEntry, 0, len(topics))
	for _, topic := range topics {
		if err := o.check(depth); err != nil {
			return nil, err
		}
		title, err := oxygenTitle(topic.Title)
		if err != nil {
			return nil, err
		}
		if voidLink(topic.Href) {
			o.warning(fmt.Sprintf("Publisher TOC entry %q links to nothing (%s); kept as a title without a link", title, topic.Href))
			result = append(result, model.TocEntry{Title: title})
			continue
		}
		target, err := joinSource(o.root, topic.Href, false)
		if err != nil {
			return nil, err
		}
		result = append(result, model.TocEntry{Title: title, URL: target})
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
		if entry.URL == "" {
			if err := o.register(entry.Children); err != nil {
				return err
			}
			continue
		}
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
// matterAround returns the topics outside the TOC that the TOC page raw
// links with rel. A TOC page the publisher does not serve links none; the
// archive records the page itself as unpublished.
func (o *oxygenPlanner) matterAround(raw, rel string, toc []string) ([]model.TocEntry, error) {
	page, err := o.page(raw, "topic")
	if status, missing := permanentPublisherStatus(err); missing {
		o.warning(fmt.Sprintf("Oxygen TOC page %s returned HTTP %d; topics outside the TOC that it would link with rel=%q were not looked for",
			raw, status, rel))
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return o.matter(page, rel, toc)
}

func (o *oxygenPlanner) matter(from oxygenPage, rel string, toc []string) ([]model.TocEntry, error) {
	tree, base := from.tree, from.base
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
		next, err := o.page(page, "topic")
		if err != nil {
			return nil, err
		}
		tree, base = next.tree, next.base
		titles := nodes(tree, func(n *html.Node) bool { return n.Data == "title" })
		if len(titles) != 1 || !label(text(titles[0])) {
			return nil, fmt.Errorf("Oxygen topic outside the TOC has no title: %s", next.in.url)
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

// voidLink reports a TOC link that opens nothing. For a topic reference its
// content repository could not resolve, titled "This object is not available
// in the repository.", Oxygen writes href="javascript:void(0)" in the TOC data
// and href="" in the rendered TOC; the 10.18 Diagnostics and Supportability
// Guide for 8400 has one. The link is recognised, never run.
func voidLink(href string) bool {
	href = strings.TrimSpace(href)
	scheme, _, found := strings.Cut(href, ":")
	return href == "" || found && strings.EqualFold(scheme, "javascript")
}

func canonicalPage(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}
