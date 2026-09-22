package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/source"
	"aos-cx-docs-dldr/internal/storage"
	"github.com/tdewolff/parse/v2/css"
	"golang.org/x/net/html"
)

type htmlArchiver struct {
	ctx                      context.Context
	plan                     model.DocumentPlan
	fetcher                  model.Fetcher
	root                     *os.Root
	refresh                  bool
	workers                  int
	maxBytes                 int64
	budget                   *sourceBudget
	guideRoots               []string
	pages                    []*pageInput
	pageByURL                map[string]*pageInput
	hpeDocument              string
	hpeAliases               map[string]*pageInput
	hpeAmbiguous             map[string]bool
	hpeHosts                 map[string]bool
	notices                  []model.Notice
	warnings                 []string
	errors                   []string
	required                 map[string]map[string]bool
	requiredRefs             []sourceBookmarkReference
	sourceAbsent             map[localBookmarkReference]bool
	localIDs                 map[string]map[string]bool
	usedClasses              map[string]bool
	usedIDs                  map[string]bool
	assets                   *assetManager
	search                   []searchRecord
	progress                 Progress
	observe                  ProgressObserver
	bookmarkCount            int
	bookmarkBytes            int
	tableStyleCandidates     map[string][]tableStyleCandidate
	tableStyleResolutions    map[string]tableStyleResolution
	ordinaryStyleResolutions map[string]ordinaryStyleResolution
	ordinaryStyleProbes      map[string]ordinaryStyleProbe
	stylesheetRecoveries     []model.StylesheetRecovery
	missingResources         []model.MissingResource
}

type searchRecord struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Text      string `json:"text"`
	SourceURL string `json:"source_url"`
}

type ProgressStage string

const (
	ProgressSchemaVersion               = 1
	ProgressTopics        ProgressStage = "topics"
	ProgressBuilding      ProgressStage = "building"
	ProgressHashing       ProgressStage = "hashing"
	ProgressReferences    ProgressStage = "references"
	ProgressManifest      ProgressStage = "manifest"
	ProgressFinal         ProgressStage = "final"
)

// Progress reports completed work at real archive operation boundaries.
type Progress struct {
	SchemaVersion         int
	Stage                 ProgressStage
	TopicsValidated       int
	PlannedTopics         int
	PagesEmitted          int
	PagesTotal            int
	FilesHashed           int
	FilesTotal            int
	ReferenceFilesChecked int
	ReferenceFilesTotal   int
	AssetsDiscovered      int
	AssetsCompleted       int
	Final                 bool
}

type ProgressObserver func(Progress)

const (
	maxRecordedBookmarks      = 65_536
	maxRecordedBookmarksPage  = 4_096
	maxRecordedBookmarkBytes  = 2 << 20
	maxRecordedBookmarkLength = 1_024
)

// HTML creates a complete offline archive from a complete supported source plan.
func HTML(ctx context.Context, plan model.DocumentPlan, fetcher model.Fetcher, output string, refresh bool, maxResource, maxTotal int64) (result model.ArchiveResult, err error) {
	return HTMLWithWorkers(ctx, plan, fetcher, output, refresh, maxResource, maxTotal, 1)
}

// HTMLWithWorkers prefetches planned topic bodies into a disk-backed fetcher
// with bounded concurrency. Parsing, discovery, rewriting, and output remain
// ordered and serial.
func HTMLWithWorkers(ctx context.Context, plan model.DocumentPlan, fetcher model.Fetcher, output string, refresh bool, maxResource, maxTotal int64, workers int) (result model.ArchiveResult, err error) {
	return HTMLWithWorkersObserved(ctx, plan, fetcher, output, refresh, maxResource, maxTotal, workers, nil)
}

// HTMLWithWorkersObserved is HTMLWithWorkers with bounded progress snapshots.
func HTMLWithWorkersObserved(ctx context.Context, plan model.DocumentPlan, fetcher model.Fetcher, output string, refresh bool, maxResource, maxTotal int64, workers int, observe ProgressObserver) (result model.ArchiveResult, err error) {
	result = model.ArchiveResult{Document: plan.Document, OutputDir: output, Status: "failed", Format: "html",
		Errors: []string{}, Notices: append([]model.Notice{}, plan.Notices...),
		Warnings: append([]string{}, plan.Warnings...)}
	defer func() {
		if err != nil {
			result.Status = "failed"
			result.Errors = append(result.Errors, err.Error())
		}
	}()
	if plan.Document.Kind != "flare" && plan.Document.Kind != "hpe" && plan.Document.Kind != "static" {
		return result, fmt.Errorf("HTML archiving is not yet implemented for source kind %q", plan.Document.Kind)
	}
	if plan.Inventory == nil || !plan.Inventory.Complete || len(plan.Topics) == 0 {
		return result, errors.New("HTML archiving requires a complete non-empty source inventory")
	}
	if maxResource < 1 || maxTotal < 1 {
		return result, errors.New("invalid HTML archive byte limits")
	}
	if workers < 1 {
		return result, errors.New("HTML archive workers must be at least 1")
	}
	info, statErr := os.Lstat(output)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return result, errors.New("HTML output must be an owned real staging directory")
	}
	root, err := os.OpenRoot(output)
	if err != nil {
		return result, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	for _, managed := range []string{"pages", "assets", "index.html", "toc.html", "manifest.json", "search.json", "search-index.js", "search.js"} {
		if err := storage.Check(root, managed); err != nil {
			return result, err
		}
		if existing, statErr := root.Lstat(managed); statErr == nil {
			expectDirectory := managed == "pages" || managed == "assets"
			if existing.IsDir() != expectDirectory {
				return result, fmt.Errorf("unexpected managed output file type: %s", managed)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return result, statErr
		}
		if err := storage.RemoveTree(root, managed); err != nil {
			return result, err
		}
	}
	if err := storage.EnsureDir(root, "pages"); err != nil {
		return result, err
	}
	if err := storage.EnsureDir(root, "assets"); err != nil {
		return result, err
	}
	budget := &sourceBudget{limit: maxTotal}
	a := &htmlArchiver{ctx: ctx, plan: plan, fetcher: fetcher, root: root, refresh: refresh, workers: workers,
		maxBytes: min(maxResource, maxTotal), budget: budget,
		pageByURL: map[string]*pageInput{}, required: map[string]map[string]bool{}, localIDs: map[string]map[string]bool{},
		sourceAbsent: map[localBookmarkReference]bool{},
		usedClasses:  map[string]bool{}, usedIDs: map[string]bool{},
		tableStyleCandidates:     map[string][]tableStyleCandidate{},
		tableStyleResolutions:    map[string]tableStyleResolution{},
		ordinaryStyleResolutions: map[string]ordinaryStyleResolution{},
		ordinaryStyleProbes:      map[string]ordinaryStyleProbe{},
		notices:                  append([]model.Notice{}, plan.Notices...),
		warnings:                 append([]string{}, plan.Warnings...),
		progress:                 Progress{SchemaVersion: ProgressSchemaVersion, Stage: ProgressTopics}}
	a.observe = observe
	a.assets = newAssetManager(ctx, fetcher, root, refresh, min(maxResource, maxTotal), budget,
		&a.notices, &a.warnings, &a.errors,
		func(discovered, completed int) {
			a.progress.AssetsDiscovered, a.progress.AssetsCompleted = discovered, completed
			a.reportProgress()
		})
	if err := a.inventory(); err != nil {
		return result, err
	}
	a.assets.relevant = a.selectorRelevant
	a.progress.Stage = ProgressBuilding
	a.progress.PagesTotal = len(a.pages)
	a.reportProgress()
	if err := a.prepareTableStyleCandidates(); err != nil {
		return result, err
	}
	if err := a.prepareOrdinaryStyleRecoveries(); err != nil {
		return result, err
	}
	if err := a.emit(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	assetRecords := a.assets.records()
	emittedPages, completeAssets := 0, 0
	for _, page := range a.pages {
		if page.emitted {
			emittedPages++
		}
	}
	for _, record := range assetRecords {
		if record.Status == "complete" {
			completeAssets++
		}
	}
	a.progress.Stage = ProgressHashing
	a.progress.FilesTotal = len(a.pages) + 5 + emittedPages + completeAssets
	a.reportProgress()
	archive := &model.HTMLArchive{SchemaVersion: 1, Status: "complete", SourceURL: plan.Document.URL,
		Inputs: append([]model.SourceInput{}, plan.Inputs...), Inventory: plan.Inventory,
		Topics: make([]model.FileRecord, 0, len(a.pages)), Assets: assetRecords,
		Notices: uniqueNotices(a.notices), Warnings: uniqueStrings(a.warnings),
		Errors: uniqueStrings(a.errors), StylesheetRecoveries: cloneStylesheetRecoveries(a.stylesheetRecoveries),
		MissingResources: cloneMissingResources(a.missingResources),
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339Nano)}
	for _, page := range a.pages {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		hash, size, hashErr := storage.Hash(root, page.path, maxResource)
		bookmarks, bookmarksComplete := a.recordedBookmarks(page)
		record := model.FileRecord{URL: page.topic.URL, FetchURL: page.requestURL, FinalURL: page.finalURL, Path: page.path, Status: "complete",
			ContentType: page.contentType, Size: size, SourceSize: page.sourceSize, SourceSHA256: page.sourceSHA256, SHA256: hash,
			Supplementary: page.supplementary, SourceBookmarks: bookmarks, SourceBookmarksComplete: bookmarksComplete,
			Metadata: page.metadata, Copyright: page.copyright}
		if hashErr != nil {
			record.Status, record.Error = "failed", hashErr.Error()
			a.errors = append(a.errors, page.topic.URL+": "+hashErr.Error())
		} else {
			a.progress.FilesHashed++
			a.reportProgress()
		}
		archive.Topics = append(archive.Topics, record)
		for index := range archive.MissingResources {
			if archive.MissingResources[index].GeneratedPagePath == page.path {
				archive.MissingResources[index].GeneratedPageSHA256 = hash
			}
		}
	}
	archive.Warnings, archive.Errors = uniqueStrings(a.warnings), uniqueStrings(a.errors)
	if len(archive.Errors) > 0 || slices.ContainsFunc(archive.Assets, func(record model.FileRecord) bool { return record.Status != "complete" }) {
		archive.Status = "incomplete"
	} else if len(archive.MissingResources) > 0 {
		archive.Status = "degraded"
	}
	hashGenerated := func(name string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		hash, _, hashErr := storage.Hash(root, name, maxResource)
		if hashErr == nil {
			a.progress.FilesHashed++
			a.reportProgress()
		}
		return hash, hashErr
	}
	indexHash, hashErr := hashGenerated("index.html")
	if hashErr != nil {
		return result, hashErr
	}
	tocHash, hashErr := hashGenerated("toc.html")
	if hashErr != nil {
		return result, hashErr
	}
	searchHash, hashErr := hashGenerated("search.json")
	if hashErr != nil {
		return result, hashErr
	}
	searchIndexHash, hashErr := hashGenerated("search-index.js")
	if hashErr != nil {
		return result, hashErr
	}
	searchJSHash, hashErr := hashGenerated("search.js")
	if hashErr != nil {
		return result, hashErr
	}
	hashErrs, validationErr := validateOutputHashes(ctx, root, archive, func() {
		a.progress.FilesHashed++
		a.reportProgress()
	})
	if validationErr != nil {
		return result, validationErr
	}
	var links int
	linkErrs := hashErrs
	if len(hashErrs) == 0 {
		a.progress.Stage = ProgressReferences
		a.progress.ReferenceFilesTotal = outputReferenceFileCount(archive)
		a.reportProgress()
		links, linkErrs, validationErr = validateOutputReferences(ctx, root, archive, a.sourceAbsent, func() {
			a.progress.ReferenceFilesChecked++
			a.reportProgress()
		})
		if validationErr != nil {
			return result, validationErr
		}
	}
	archive.Integrity = model.HTMLIntegrity{IndexSHA256: indexHash, TOCSHA256: tocHash, SearchSHA256: searchHash,
		SearchIndexSHA256: searchIndexHash, SearchJSSHA256: searchJSHash, CheckedLinks: links}
	if len(linkErrs) > 0 {
		archive.Errors = append(archive.Errors, linkErrs...)
		archive.Status = "incomplete"
	}
	a.progress.Stage = ProgressManifest
	a.reportProgress()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := storage.WriteJSON(root, "manifest.json", archive); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := storage.SyncDir(root, "."); err != nil {
		return result, err
	}
	result.HTML, result.Status = archive, archive.Status
	result.Errors = append([]string{}, archive.Errors...)
	result.Notices = append([]model.Notice{}, archive.Notices...)
	result.Warnings = append([]string{}, archive.Warnings...)
	result.StylesheetRecoveries = cloneStylesheetRecoveries(archive.StylesheetRecoveries)
	result.MissingResources = cloneMissingResources(archive.MissingResources)
	a.progress.Stage = ProgressFinal
	a.progress.Final = true
	a.reportProgress()
	return result, nil
}

func (a *htmlArchiver) reportProgress() {
	if a.observe != nil {
		a.observe(a.progress)
	}
}

func (a *htmlArchiver) recordedBookmarks(page *pageInput) ([]string, bool) {
	bookmarks := slices.Sorted(maps.Keys(page.sourceIDs))
	bytes := 0
	for _, bookmark := range bookmarks {
		bytes += len(bookmark)
		if len(bookmark) > maxRecordedBookmarkLength {
			a.warnings = append(a.warnings, page.topic.URL+": source bookmark evidence exceeds the per-value limit; cross-guide missing-fragment localization will fail closed")
			return nil, false
		}
	}
	if len(bookmarks) > maxRecordedBookmarksPage ||
		a.bookmarkCount > maxRecordedBookmarks-len(bookmarks) ||
		a.bookmarkBytes > maxRecordedBookmarkBytes-bytes {
		a.warnings = append(a.warnings, page.topic.URL+": source bookmark evidence exceeds archive limits; cross-guide missing-fragment localization will fail closed")
		return nil, false
	}
	a.bookmarkCount += len(bookmarks)
	a.bookmarkBytes += bytes
	return bookmarks, true
}

type diskPrefetcher interface {
	Prefetch(context.Context, string, bool) error
}

type prefetchResult struct {
	page *pageInput
	err  error
}

type topicPrefetch struct {
	ctx     context.Context
	cancel  context.CancelFunc
	jobs    chan *pageInput
	results chan prefetchResult
	group   sync.WaitGroup
	pending map[*pageInput]error
	pages   []*pageInput
	next    int
	closed  bool
}

func (a *htmlArchiver) startPrefetch(pages []*pageInput) *topicPrefetch {
	fetcher, ok := a.fetcher.(diskPrefetcher)
	if !ok || len(pages) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(a.ctx)
	window := min(len(pages), 2*a.workers)
	prefetch := &topicPrefetch{
		ctx: ctx, cancel: cancel, jobs: make(chan *pageInput, a.workers),
		results: make(chan prefetchResult, window), pending: map[*pageInput]error{},
		pages: pages, next: window,
	}
	for range min(a.workers, len(pages)) {
		prefetch.group.Add(1)
		go func() {
			defer prefetch.group.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case page, open := <-prefetch.jobs:
					if !open {
						return
					}
					requestCtx := ctx
					if a.plan.Document.Kind == "hpe" {
						requestCtx = fetch.WithRequestScope(requestCtx, hpeTopicScope(page.requestURL))
					}
					err := fetcher.Prefetch(requestCtx, page.requestURL, a.refresh)
					prefetch.results <- prefetchResult{page: page, err: err}
					if err != nil {
						return
					}
				}
			}
		}()
	}
	for _, page := range pages[:window] {
		prefetch.jobs <- page
	}
	return prefetch
}

func (p *topicPrefetch) wait(page *pageInput) error {
	if err, ok := p.pending[page]; ok {
		delete(p.pending, page)
		return err
	}
	for {
		select {
		case result := <-p.results:
			if result.err != nil {
				return fmt.Errorf("prefetch topic %s: %w", result.page.topic.URL, result.err)
			}
			if result.page == page {
				return nil
			}
			p.pending[result.page] = nil
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
}

func (p *topicPrefetch) advance() {
	if p.next < len(p.pages) {
		p.jobs <- p.pages[p.next]
		p.next++
	}
}

func (p *topicPrefetch) close() {
	if p == nil || p.closed {
		return
	}
	p.closed = true
	p.cancel()
	close(p.jobs)
	p.group.Wait()
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func uniqueNotices(values []model.Notice) []model.Notice {
	result := make([]model.Notice, 0, len(values))
	seen := make(map[model.Notice]bool, len(values))
	for _, value := range values {
		if value.Kind != "" && value.Message != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (a *htmlArchiver) inventory() error {
	titles := map[string]string{}
	fetchURLs := map[string]string{}
	add := func(topic model.Topic, supplementary bool) error {
		public, err := fetch.NormalizeURL(topic.URL)
		if err != nil {
			return err
		}
		key := canonicalNoFragment(public)
		if key == "" {
			return fmt.Errorf("invalid topic URL %q", topic.URL)
		}
		if existing := a.pageByURL[key]; existing != nil {
			if existing.title == "" && topic.Title != "" {
				existing.title = topic.Title
			}
			return nil
		}
		if len(a.pages) >= maxHTMLTopics {
			return errors.New("topic inventory exceeds limit")
		}
		topic.URL = public
		if topic.FetchURL == "" {
			topic.FetchURL = key
		}
		request, err := fetch.CanonicalURL(topic.FetchURL)
		if err != nil {
			return err
		}
		page := &pageInput{topic: topic, requestURL: request, title: topic.Title,
			path: path.Join("pages", fileName(key, topic.Title, ".html")), supplementary: supplementary}
		a.pageByURL[key], titles[key], fetchURLs[key] = page, topic.Title, request
		a.pages = append(a.pages, page)
		return nil
	}
	for _, topic := range a.plan.Topics {
		if err := add(topic, false); err != nil {
			return err
		}
	}
	if err := a.initializeSource(); err != nil {
		return err
	}
	var addTOC func([]model.TocEntry) error
	addTOC = func(entries []model.TocEntry) error {
		for _, entry := range entries {
			if entry.URL != "" {
				key := canonicalNoFragment(entry.URL)
				target := a.topicForReference(entry.URL, nil)
				if parsed, err := url.Parse(entry.URL); err == nil && parsed.Fragment != "" {
					if target != nil {
						key = canonicalNoFragment(target.topic.URL)
					}
					a.requireFragmentFrom("toc.html", key, parsed.Fragment)
				}
				if target == nil && a.plan.Document.Kind != "flare" {
					return fmt.Errorf("%s TOC references a topic absent from its authoritative plan: %s",
						a.plan.Document.Kind, entry.URL)
				}
				if target == nil && a.flareTopic(key) {
					topic := model.Topic{URL: entry.URL, Title: entry.Title, FetchURL: fetchURLs[key]}
					if err := add(topic, false); err != nil {
						return err
					}
				}
			}
			if err := addTOC(entry.Children); err != nil {
				return err
			}
		}
		return nil
	}
	if err := addTOC(a.plan.TOC); err != nil {
		return err
	}
	planned := len(a.pages)
	a.progress.PlannedTopics = planned
	a.reportProgress()
	prefetch := a.startPrefetch(a.pages[:planned])
	defer prefetch.close()
	for index := 0; index < len(a.pages); index++ {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		page := a.pages[index]
		refresh := a.refresh
		if prefetch != nil && index < planned {
			if err := prefetch.wait(page); err != nil {
				return err
			}
			prefetch.advance()
			refresh = false
		}
		resource, body, err := a.readPage(page, refresh)
		if err != nil {
			return fmt.Errorf("retrieve topic %s: %w", page.topic.URL, err)
		}
		if err := a.budget.add(int64(len(body))); err != nil {
			return err
		}
		doc, err := parseTopic(a.ctx, body, resource, a.plan.Document.Kind)
		if err != nil {
			return err
		}
		if a.plan.Document.Kind == "hpe" {
			if err := validateHPEPage(resource, page.topic, doc); err != nil {
				return err
			}
		} else if a.plan.Document.Kind == "static" {
			if err := validateStaticPage(doc); err != nil {
				return err
			}
		}
		content := findContent(doc, a.plan.Document.Kind)
		if content == nil {
			return fmt.Errorf("publisher topic has no supported content container: %s", page.topic.URL)
		}
		if err := validateTopicContent(doc, content, resource.URL); err != nil {
			return err
		}
		page.finalURL, page.contentType, page.sourceSHA256, page.sourceSize = resource.URL, resource.Headers.Get("Content-Type"), sourceHash(body), int64(len(body))
		for _, alias := range []string{page.requestURL, resource.URL} {
			if key := canonicalNoFragment(alias); key != "" && a.pageByURL[key] == nil {
				a.pageByURL[key] = page
			}
		}
		if a.plan.Document.Kind == "static" {
			page.baseURL, err = staticBaseURL(doc, resource.URL, a.guideRoots)
		} else {
			page.baseURL, err = baseURL(doc, resource.URL)
		}
		if err != nil {
			return err
		}
		page.sourceIDs = sourceIDs(content)
		for id := range page.sourceIDs {
			a.usedIDs[id] = true
		}
		walk(content, func(node *html.Node) {
			if node.Type != html.ElementNode {
				return
			}
			for _, class := range strings.Fields(attr(node, "class")) {
				a.usedClasses[class] = true
			}
		})
		if a.plan.Document.Kind == "flare" {
			if finalRoot, err := source.GuideRoot(resource.URL); err == nil && !slices.Contains(a.guideRoots, finalRoot) {
				a.guideRoots = append(a.guideRoots, finalRoot)
			}
			if err := a.collectTableStyleEvidence(doc, content, page); err != nil {
				return err
			}
			a.collectOrdinaryStyleEvidence(doc, page)
		}
		page.title = contentTitle(content, page.title)
		if label, mismatch := publisherReleaseLabel(doc, a.plan.Document.Version); mismatch {
			a.warnings = append(a.warnings, fmt.Sprintf(
				"Publisher document label %q at %s does not match selected release %s; exact mapped source content was retained without relabeling.",
				label, resource.URL, a.plan.Document.Version))
		}
		page.metadata, page.copyright = pageMetadata(doc)
		page.sourceDate = page.metadata["dc.date"]
		if index < planned {
			a.progress.TopicsValidated++
			a.reportProgress()
		}
		var discovered []model.Topic
		walk(content, func(node *html.Node) {
			if node.Type != html.ElementNode || node.Data != "a" || attr(node, "href") == "" {
				return
			}
			raw := strings.TrimSpace(attr(node, "href"))
			if a.plan.Document.Kind == "hpe" {
				if target := a.topicForReference(raw, page); target != nil {
					if fragment := linkFragment(raw); fragment != "" {
						a.requireFragmentFrom(
							page.path,
							canonicalNoFragment(target.topic.URL),
							fragment,
						)
					}
				}
				return
			}
			resolved, err := resolveReference(page.baseURL, raw)
			if err != nil {
				return
			}
			parsed, _ := url.Parse(resolved)
			key := canonicalNoFragment(resolved)
			if parsed.Fragment != "" {
				a.requireFragmentFrom(page.path, key, parsed.Fragment)
			}
			if a.plan.Document.Kind == "flare" && a.flareTopic(key) && a.pageByURL[key] == nil {
				discovered = append(discovered, model.Topic{URL: key, Title: strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(node), " "))})
			}
		})
		for _, topic := range discovered {
			if err := add(topic, true); err != nil {
				return err
			}
		}
	}
	for key, fragments := range a.required {
		page := a.pageByURL[key]
		if page == nil {
			continue
		}
		for fragment := range fragments {
			if !hasBookmark(page.sourceIDs, fragment) {
				a.warnings = append(a.warnings, fmt.Sprintf("Publisher bookmark #%s is absent from source topic %s", fragment, page.topic.URL))
				for _, reference := range a.requiredRefs {
					if reference.target != key ||
						normalizeBookmarkFragment(reference.fragment) != normalizeBookmarkFragment(fragment) {
						continue
					}
					a.sourceAbsent[localBookmarkReference{
						source: reference.referrer, target: page.path,
						fragment: normalizeBookmarkFragment(fragment),
					}] = true
				}
			}
		}
	}
	return a.ctx.Err()
}

func (a *htmlArchiver) requireFragmentFrom(referrer, key, fragment string) {
	if key == "" || fragment == "" {
		return
	}
	if a.required[key] == nil {
		a.required[key] = map[string]bool{}
	}
	a.required[key][fragment] = true
	if referrer != "" {
		a.requiredRefs = append(a.requiredRefs, sourceBookmarkReference{
			referrer: referrer, target: key, fragment: fragment,
		})
	}
}

func (a *htmlArchiver) flareTopic(raw string) bool {
	key := canonicalNoFragment(raw)
	for _, unavailable := range a.plan.UnavailableNavigation {
		if unavailable.Role == "optional-detailed-toc" &&
			canonicalNoFragment(unavailable.URL) == key {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	ext := strings.ToLower(path.Ext(parsed.Path))
	if ext != ".htm" && ext != ".html" {
		return false
	}
	for _, root := range a.guideRoots {
		base, _ := url.Parse(root)
		if parsed.Scheme == base.Scheme && parsed.Host == base.Host && strings.HasPrefix(parsed.Path, base.Path+"Content/") {
			return true
		}
	}
	return false
}

func (a *htmlArchiver) emit() error {
	toc := a.renderTOC()
	if err := storage.Atomic(a.root, "toc.html", []byte(renderTOCPage(a.plan.Document.Title, toc))); err != nil {
		return err
	}
	for i, page := range a.pages {
		if err := a.ctx.Err(); err != nil {
			return err
		}
		resource, body, err := a.readPage(page, false)
		if err != nil {
			a.errors = append(a.errors, page.topic.URL+": "+err.Error())
			continue
		}
		if sourceHash(body) != page.sourceSHA256 {
			return fmt.Errorf("cached topic bytes changed during two-pass archive: %s", page.topic.URL)
		}
		doc, err := parseTopic(a.ctx, body, resource, a.plan.Document.Kind)
		if err != nil {
			a.errors = append(a.errors, page.topic.URL+": "+err.Error())
			continue
		}
		if a.plan.Document.Kind == "hpe" {
			if err := validateHPEPage(resource, page.topic, doc); err != nil {
				a.errors = append(a.errors, page.topic.URL+": "+err.Error())
				continue
			}
		} else if a.plan.Document.Kind == "static" {
			if err := validateStaticPage(doc); err != nil {
				a.errors = append(a.errors, page.topic.URL+": "+err.Error())
				continue
			}
		}
		content := findContent(doc, a.plan.Document.Kind)
		if content == nil {
			a.errors = append(a.errors, page.topic.URL+": content container disappeared")
			continue
		}
		styles := a.pageStyles(doc, page)
		inlineStyles := a.pageInlineStyles(doc, page)
		if a.plan.Document.Kind == "hpe" {
			inlineStyles = append(a.hpeFontStyles(page), inlineStyles...)
		}
		if err := a.rewriteContent(content, page); err != nil {
			a.errors = append(a.errors, page.topic.URL+": "+err.Error())
		}
		localIDs := sourceIDs(content)
		a.localIDs[canonicalNoFragment(page.topic.URL)] = localIDs
		for fragment := range a.required[canonicalNoFragment(page.topic.URL)] {
			sourceHad := hasBookmark(page.sourceIDs, fragment)
			localHas := hasBookmark(localIDs, fragment)
			if sourceHad && !localHas {
				a.errors = append(a.errors, fmt.Sprintf("%s: source bookmark #%s was lost during archival", page.topic.URL, fragment))
			}
		}
		styleLinks := make([]string, 0, len(styles))
		for _, style := range styles {
			local, err := a.localPageStyle(style, page)
			if err != nil {
				a.errors = append(a.errors, page.topic.URL+": stylesheet "+style+": "+err.Error())
				continue
			}
			if !slices.Contains(styleLinks, local) {
				styleLinks = append(styleLinks, local)
			}
		}
		contentHTML, err := renderNode(content)
		if err != nil {
			return err
		}
		var previous, next *pageInput
		if i > 0 {
			previous = a.pages[i-1]
		}
		if i+1 < len(a.pages) {
			next = a.pages[i+1]
		}
		pageBytes := renderTopicPage(a.plan.Document, page, previous, next, styleLinks, inlineStyles, string(contentHTML))
		if err := storage.Atomic(a.root, page.path, pageBytes); err != nil {
			return err
		}
		page.emitted = true
		a.progress.PagesEmitted++
		a.reportProgress()
		a.search = append(a.search, searchRecord{Title: page.title, URL: page.path,
			Text: strings.TrimSpace(spacePattern.ReplaceAllString(nodeText(content), " ")), SourceURL: page.topic.URL})
	}
	if err := a.ctx.Err(); err != nil {
		return err
	}
	searchData, err := json.MarshalIndent(a.search, "", "  ")
	if err != nil {
		return err
	}
	if err := storage.Atomic(a.root, "search.json", append(searchData, '\n')); err != nil {
		return err
	}
	searchIndex := append([]byte("window.AOSCX_GUIDE_SEARCH = "), searchData...)
	searchIndex = append(searchIndex, ';', '\n')
	if err := storage.Atomic(a.root, "search-index.js", searchIndex); err != nil {
		return err
	}
	if err := storage.Atomic(a.root, "search.js", []byte(guideSearchJS)); err != nil {
		return err
	}
	if err := storage.Atomic(a.root, "index.html", []byte(renderGuideIndex(a.plan.Document, toc))); err != nil {
		return err
	}
	return nil
}

func (a *htmlArchiver) pageStyles(doc *html.Node, page *pageInput) []string {
	var styles []string
	walk(doc, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "link" || attr(node, "disabled") != "" {
			return
		}
		if !slices.Contains(strings.Fields(strings.ToLower(attr(node, "rel"))), "stylesheet") {
			return
		}
		resolved, err := resolveReference(page.baseURL, attr(node, "href"))
		if err != nil {
			a.warnings = append(a.warnings, page.topic.URL+": invalid stylesheet reference: "+err.Error())
			return
		}
		if a.plan.Document.Kind == "flare" && strings.EqualFold(attr(node, "data-mc-generated"), "true") &&
			strings.Contains(strings.ToLower(mustURL(resolved).Path), "/skins/") {
			a.notices = append(a.notices, model.Notice{
				Kind:    model.NoticeNavigationStyleOmitted,
				Message: "Publisher navigation/toolbar skin stylesheet omitted: " + resolved,
			})
			return
		}
		if !slices.Contains(styles, resolved) {
			styles = append(styles, resolved)
		}
	})
	if a.plan.Document.Kind == "flare" {
		keys := make([]string, 0, len(page.tableStyles))
		for key := range page.tableStyles {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			reference := page.tableStyles[key]
			if !a.guideLocalURL(reference.URL) && !slices.Contains(styles, reference.URL) {
				styles = append(styles, reference.URL)
			}
		}
	}
	if a.plan.Document.Kind != "flare" {
		for _, style := range a.plan.Styles {
			if !slices.Contains(styles, style) {
				styles = append(styles, style)
			}
		}
	}
	return styles
}

func (a *htmlArchiver) pageInlineStyles(doc *html.Node, page *pageInput) []string {
	var styles []string
	head := findNode(doc, func(node *html.Node) bool { return node.Type == html.ElementNode && node.Data == "head" })
	if head == nil {
		return styles
	}
	walk(head, func(node *html.Node) {
		if node.Type != html.ElementNode || node.Data != "style" {
			return
		}
		rewritten, diagnostics, err := rewriteCSSFiltered([]byte(nodeText(node)), false, func(reference string, kind dependencyKind) (string, error) {
			return a.assets.local(reference, page.baseURL, page.path, kind)
		}, a.selectorRelevant)
		a.addCSSDiagnostics(diagnostics)
		if err != nil {
			a.errors = append(a.errors, page.topic.URL+": inline stylesheet: "+err.Error())
			return
		}
		styles = append(styles, string(rewritten))
	})
	return styles
}

func (a *htmlArchiver) addCSSDiagnostics(diagnostics cssDiagnostics) {
	a.notices = append(a.notices, diagnostics.Notices...)
	a.warnings = append(a.warnings, diagnostics.Warnings...)
}

func (a *htmlArchiver) selectorRelevant(tokens []css.Token) bool {
	return selectorGroupRelevant(tokens, a.usedClasses, a.usedIDs)
}

func selectorGroupRelevant(tokens []css.Token, classes, ids map[string]bool) bool {
	for _, token := range tokens {
		function := strings.TrimSuffix(strings.ToLower(string(token.Data)), "(")
		if token.TokenType == css.LeftBracketToken ||
			token.TokenType == css.FunctionToken && (function == "not" || function == "is" || function == "where" || function == "has") {
			return true
		}
	}
	for i := 0; i < len(tokens); i++ {
		if tokens[i].TokenType == css.HashToken {
			name, err := cssUnescape(strings.TrimPrefix(string(tokens[i].Data), "#"))
			if err != nil {
				return true
			}
			if !ids[name] {
				return false
			}
			continue
		}
		if tokens[i].TokenType == css.DelimToken && string(tokens[i].Data) == "." && i+1 < len(tokens) &&
			tokens[i+1].TokenType == css.IdentToken {
			name, err := cssUnescape(string(tokens[i+1].Data))
			if err != nil {
				return true
			}
			if !classes[name] {
				return false
			}
			i++
		}
	}
	return true
}
