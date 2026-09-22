package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/testutil"
)

const planRoot = "https://arubanetworking.hpe.com/techdocs/AOS-CX/10.16/HTML/test_6300-6400/"
const planHome = planRoot + "Content/home.htm"
const planDetail = planRoot + "Content/contents.htm"
const planModule = planRoot + "Data/Tocs/Guide.js"
const planHelpSystem = planRoot + "Data/HelpSystem.xml"
const hpeAPI = "https://support.hpe.com/hpesc/public/api/document/"
const hpePublic = "https://support.hpe.com/hpesc/public/docDisplay?docId="

type planFixture struct {
	responses map[string]model.Resource
	failures  map[string]error
	calls     []string
	refreshes []bool
}

func (f *planFixture) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	f.calls = append(f.calls, raw)
	f.refreshes = append(f.refreshes, refresh)
	if err := f.failures[raw]; err != nil {
		return model.Resource{}, err
	}
	r, ok := f.responses[raw]
	if !ok {
		return model.Resource{}, errors.New("missing fixture: " + raw)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return model.Resource{}, err
	}
	r.Body.Close()
	f.responses[raw] = model.Resource{URL: r.URL, Status: r.Status, Headers: r.Headers, Body: io.NopCloser(bytes.NewReader(data))}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return r, nil
}
func (f *planFixture) set(raw, body, mime string) {
	f.responses[raw] = model.Resource{URL: raw, Status: 200, Headers: http.Header{"Content-Type": {mime}}, Body: io.NopCloser(strings.NewReader(body))}
}
func document(kind string) model.Document {
	return model.Document{ID: "fundamentals", Title: "Fundamentals Guide", Platform: "6300", Version: "10.16", Kind: kind, URL: planHome}
}
func flareFixture() *planFixture {
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(planHome, `<html data-mc-path-to-help-system="../"><head><link rel="stylesheet" href="../styles/main.css"></head><body><a href="contents.htm">Table of Contents</a><a href="shortcut.htm">Shortcut</a></body></html>`, "text/html")
	f.set(planDetail, `<html data-mc-path-to-help-system="../"><body><ul data-mc-linked-toc="Data/Tocs/Guide.js"></ul></body></html>`, "text/html")
	f.set(planModule, `define({numchunks:2,prefix:'Guide_Chunk',tree:{n:[{i:0,c:0,n:[{i:1,c:0},{i:2,c:1,n:[{i:3,c:0},{i:4,c:1,n:[{i:5,c:1}]},{i:6,c:1}]}]},{i:7,c:1}]}});`, "application/javascript")
	f.set(planRoot+"Data/Tocs/Guide_Chunk0.js", `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/contents.htm':{i:[1],t:['Detailed TOC'],b:['']},'/Content/copyright.htm':{i:[3],t:['Copyright'],b:['']}});`, "application/javascript")
	f.set(planRoot+"Data/Tocs/Guide_Chunk1.js", `define({'___':{i:[2],t:['Grouping heading'],b:['']},'/Content/chapter.htm':{i:[4,6],t:['Chapter','Repeated'],b:['','frag']},'/Content/nested/topic.htm':{i:[5],t:['Deep topic'],b:['']},'https://support.example.test/kb':{i:[7],t:['Knowledge Base'],b:['']}});`, "application/javascript")
	return f
}

func flareHelpSystemFixture() *planFixture {
	f := flareFixture()
	f.set(planHome, `<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../" data-mc-target-type="WebHelp2"><body><a href="contents.htm">Table of Contents</a><ul data-mc-toc="True"></ul></body></html>`, "text/html")
	f.set(planDetail, `<html><body><main id="mc-main-content"><h1>Cover and notices</h1></main></body></html>`, "text/html")
	f.set(planHelpSystem, `<?xml version="1.0" encoding="utf-8"?><WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"><CatapultSkin SkinType="WebHelp2"><WebHelpOptions/></CatapultSkin></WebHelpSystem>`, "application/xml")
	return f
}

func TestLoadPlanRefreshRevalidatesEveryInventoryInput(t *testing.T) {
	f := flareFixture()
	if _, err := LoadPlanWithRefresh(context.Background(), f, document("flare"), true); err != nil {
		t.Fatal(err)
	}
	if len(f.refreshes) == 0 {
		t.Fatal("planner made no requests")
	}
	for i, refresh := range f.refreshes {
		if !refresh {
			t.Fatalf("inventory request %d did not honor refresh", i)
		}
	}
}

func TestFlareHelpSystemFallbackInventoriesCompleteGuide(t *testing.T) {
	f := flareHelpSystemFixture()
	plan, err := LoadPlan(context.Background(), f, document("flare"))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Inventory.Complete || plan.Inventory.TOCURL != planModule ||
		len(plan.Inventory.ChunkURLs) != 2 || len(plan.Topics) != 5 ||
		len(plan.Inputs) != 6 {
		t.Fatalf("incomplete HelpSystem fallback plan: %+v", plan)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "index.xml") {
		t.Fatalf("advertised index.xml was fetched instead of the driver-established HelpSystem metadata: %v", f.calls)
	}
	roles := make([]string, 0, len(plan.Inputs))
	for _, sourceInput := range plan.Inputs {
		roles = append(roles, sourceInput.Role)
	}
	if !slices.Contains(roles, "help-system") {
		t.Fatalf("HelpSystem provenance missing from plan inputs: %+v", plan.Inputs)
	}
}

func TestFlareHelpSystemFallbackAfterPermanentMissingDetailedShortcut(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := flareHelpSystemFixture()
			missing := planRoot + "Content/missing-toc.htm"
			f.set(planHome, `<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../" data-mc-target-type="WebHelp2"><body><a href="missing-toc.htm">Table of Contents</a><ul data-mc-toc="True"></ul></body></html>`, "text/html")
			f.responses[missing] = model.Resource{URL: missing, Status: status, Body: http.NoBody}
			plan, err := LoadPlan(context.Background(), f, document("flare"))
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Inventory.Complete || len(plan.Topics) != 5 || len(plan.Warnings) != 1 ||
				len(plan.UnavailableNavigation) != 1 ||
				plan.UnavailableNavigation[0].URL != missing ||
				plan.UnavailableNavigation[0].HTTPStatus != status ||
				plan.UnavailableNavigation[0].Role != "optional-detailed-toc" ||
				!strings.Contains(plan.Warnings[0], missing) ||
				!strings.Contains(plan.Warnings[0], fmt.Sprintf("HTTP %d", status)) ||
				!strings.Contains(plan.Warnings[0], planHelpSystem) ||
				!strings.Contains(plan.Warnings[0], planModule) {
				t.Fatalf("permanent missing detailed shortcut was not narrowly recovered: %+v", plan)
			}
		})
	}
}

func TestFlareMissingDetailedShortcutMustBeAbsentFromAuthoritativeTOC(t *testing.T) {
	f := flareHelpSystemFixture()
	f.responses[planDetail] = model.Resource{URL: planDetail, Status: http.StatusNotFound, Body: http.NoBody}
	if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil ||
		!strings.Contains(err.Error(), "authoritative local topic") {
		t.Fatalf("authoritative missing detailed topic was accepted: %v", err)
	}
}

func TestFlareMissingDetailedShortcutDoesNotHideTransientFailure(t *testing.T) {
	f := flareHelpSystemFixture()
	f.failures = map[string]error{planDetail: context.DeadlineExceeded}
	if _, err := LoadPlan(context.Background(), f, document("flare")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transient detailed TOC failure was hidden by fallback: %v", err)
	}
	for _, call := range f.calls {
		if call == planHelpSystem {
			t.Fatal("transient failure unexpectedly entered HelpSystem fallback")
		}
	}
}

func TestFlareLinkedTOCRemainsPrimaryForWebHelp2(t *testing.T) {
	f := flareHelpSystemFixture()
	f.set(planHome, `<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../" data-mc-target-type="WebHelp2"><body><ul data-mc-toc="True" data-mc-linked-toc="Data/Tocs/Guide.js"></ul></body></html>`, "text/html")
	delete(f.responses, planHelpSystem)
	plan, err := LoadPlan(context.Background(), f, document("flare"))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Inventory.Complete || len(plan.Inputs) != 4 {
		t.Fatalf("linked TOC path changed for WebHelp2: %+v", plan)
	}
	for _, call := range f.calls {
		if call == planHelpSystem {
			t.Fatal("valid linked TOC unexpectedly fetched HelpSystem.xml")
		}
	}
}

func TestFlareHelpSystemFallbackResolvesTOCAgainstGuideRoot(t *testing.T) {
	f := flareHelpSystemFixture()
	escapedModule := planRoot + "Data/Tocs/Guide%20Book.js"
	f.set(planHelpSystem, `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide%20Book.js"/>`, "application/xml")
	f.set(escapedModule, `define({numchunks:2,prefix:'Guide_Chunk',tree:{n:[{i:0,c:0,n:[{i:1,c:0},{i:2,c:1,n:[{i:3,c:0},{i:4,c:1,n:[{i:5,c:1}]},{i:6,c:1}]}]},{i:7,c:1}]}});`, "application/javascript")
	delete(f.responses, planModule)
	f.set(planRoot+"Data/Tocs/Guide_Chunk0.js", `define({'/Content/home%20page.htm':{i:[0],t:['Home'],b:['']},'/Content/contents.htm':{i:[1],t:['Detailed TOC'],b:['']},'/Content/copyright.htm':{i:[3],t:['Copyright'],b:['']}});`, "application/javascript")
	plan, err := LoadPlan(context.Background(), f, document("flare"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Inventory.TOCURL != escapedModule || plan.Topics[2].URL != planRoot+"Content/home%20page.htm" {
		t.Fatalf("HelpSystem path was not resolved from the guide root: %+v", plan)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "/Data/Data/") {
			t.Fatalf("HelpSystem Toc was incorrectly rebased from the metadata directory: %v", f.calls)
		}
	}
}

func TestFlareHelpSystemFallbackRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name        string
		home        string
		help        string
		contentType string
	}{
		{
			name: "missing-toc",
			help: `<WebHelpSystem TargetType="WebHelp2"/>`,
		},
		{
			name: "duplicate-toc",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js" Toc="Data/Tocs/Other.js"/>`,
		},
		{
			name: "nested-duplicate-toc",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"><Child Toc="Data/Tocs/Other.js"/></WebHelpSystem>`,
		},
		{
			name: "child-only-toc",
			help: `<WebHelpSystem TargetType="WebHelp2"><Child Toc="Data/Tocs/Guide.js"/></WebHelpSystem>`,
		},
		{
			name: "wrong-root",
			help: `<HelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
		},
		{
			name: "wrong-schema",
			help: `<WebHelpSystem TargetType="WebHelp" Toc="Data/Tocs/Guide.js"/>`,
		},
		{
			name:        "wrong-mime",
			help:        `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
			contentType: "text/html",
		},
		{
			name: "doctype",
			help: `<!DOCTYPE WebHelpSystem [<!ENTITY toc "Data/Tocs/Guide.js">]><WebHelpSystem TargetType="WebHelp2" Toc="&toc;"/>`,
		},
		{
			name: "malformed-declaration",
			help: `<?xml foo?><WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
		},
		{
			name: "data-before-root",
			help: `garbage<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
		},
		{
			name: "data-after-root",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>trailing`,
		},
		{
			name: "traversal",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="../../outside.js"/>`,
		},
		{
			name: "foreign",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="https://outside.example.test/Guide.js"/>`,
		},
		{
			name: "module-query",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js?version=other"/>`,
		},
		{
			name: "module-fragment",
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js#other"/>`,
		},
		{
			name: "missing-webhelp2-home",
			home: `<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../"><body><ul data-mc-toc="True"></ul></body></html>`,
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
		},
		{
			name: "missing-toc-widget",
			home: `<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../" data-mc-target-type="WebHelp2"><body></body></html>`,
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
		},
		{
			name: "wrong-help-root",
			home: `<html data-mc-help-system-file-name="index.xml" data-mc-path-to-help-system="../../" data-mc-target-type="WebHelp2"><body><ul data-mc-toc="True"></ul></body></html>`,
			help: `<WebHelpSystem TargetType="WebHelp2" Toc="Data/Tocs/Guide.js"/>`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := flareHelpSystemFixture()
			if tc.home != "" {
				f.set(planHome, tc.home, "text/html")
			}
			contentType := tc.contentType
			if contentType == "" {
				contentType = "application/xml"
			}
			f.set(planHelpSystem, tc.help, contentType)
			if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
				t.Fatalf("invalid HelpSystem metadata accepted: %s", tc.help)
			}
		})
	}
}

func TestFlareHelpSystemFallbackRejectsUnsafeRedirectsAndBrokenInventory(t *testing.T) {
	t.Run("help-system-redirect", func(t *testing.T) {
		f := flareHelpSystemFixture()
		resource := f.responses[planHelpSystem]
		resource.URL = "https://outside.example.test/HelpSystem.xml"
		f.responses[planHelpSystem] = resource
		if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
			t.Fatal("foreign HelpSystem redirect accepted")
		}
	})
	t.Run("module-redirect", func(t *testing.T) {
		f := flareHelpSystemFixture()
		resource := f.responses[planModule]
		resource.URL = "https://outside.example.test/Guide.js"
		f.responses[planModule] = resource
		if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
			t.Fatal("foreign TOC module redirect accepted")
		}
	})
	t.Run("missing-module", func(t *testing.T) {
		f := flareHelpSystemFixture()
		delete(f.responses, planModule)
		if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
			t.Fatal("missing HelpSystem module accepted")
		}
	})
	t.Run("unresolved-tree-index", func(t *testing.T) {
		f := flareHelpSystemFixture()
		f.set(planModule, `define({numchunks:1,prefix:'Guide_Chunk',tree:{n:[{i:99,c:0}]}});`, "application/javascript")
		if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
			t.Fatal("unresolved HelpSystem tree index accepted")
		}
	})
	t.Run("missing-chunk", func(t *testing.T) {
		f := flareHelpSystemFixture()
		delete(f.responses, planRoot+"Data/Tocs/Guide_Chunk1.js")
		if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
			t.Fatal("missing HelpSystem chunk accepted")
		}
	})
}
func hpeFixture(id string) *planFixture {
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(hpeAPI+id+"?ignorePayload=true", `<main role="main" class="ditasrc"><h1>Fundamentals Guide</h1><div>Published: February 2026</div></main>`, "multiPage;charset=UTF-8")
	f.set(hpeAPI+id+"?page=content.json", `[{"topicName":"Chapter","topicLink":"GUID-ONE.html","children":[{"topicName":"Nested","topicLink":"GUID-TWO.html","children":null}]},{"topicName":"Group","topicLink":null,"children":[{"topicName":"Repeated topic","topicLink":"GUID-ONE.html#section"}]}]`, "application/json")
	return f
}
func hpeDoc(id string) model.Document { d := document("hpe"); d.URL = hpePublic + id; return d }

func TestFlarePlanPreservesAcceptedInventorySemantics(t *testing.T) {
	f := flareFixture()
	plan, err := LoadPlan(context.Background(), f, document("flare"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{planHome, planDetail, planRoot + "Content/copyright.htm", planRoot + "Content/chapter.htm", planRoot + "Content/nested/topic.htm"}
	for i, topic := range plan.Topics {
		if topic.URL != want[i] {
			t.Fatalf("topic order: %+v", plan.Topics)
		}
	}
	if len(plan.Topics) != 5 || len(f.calls) != 5 || !plan.Inventory.Complete || len(plan.Inventory.ChunkURLs) != 2 {
		t.Fatalf("incomplete plan: %+v", plan)
	}
	group := plan.TOC[2].Children[1]
	if group.URL != "" || group.Children[1].Children[0].Title != "Deep topic" || group.Children[2].URL != planRoot+"Content/chapter.htm#frag" {
		t.Fatalf("TOC positions lost: %+v", plan.TOC)
	}
	if len(plan.Warnings) != 0 || len(plan.Notices) != 1 ||
		plan.Notices[0].Kind != model.NoticeExternalLinkRetained ||
		!strings.Contains(plan.Notices[0].Message, "External navigation") ||
		len(plan.Styles) != 1 || plan.Styles[0] != planRoot+"styles/main.css" {
		t.Fatalf("metadata mismatch: %+v", plan)
	}
	for _, in := range plan.Inputs {
		if len(in.SHA256) != 64 || in.Size == 0 {
			t.Fatal("source provenance missing")
		}
	}
}

func TestFlareInvalidModulesAndChunksFailClosed(t *testing.T) {
	for _, bad := range []string{
		`define({numchunks:1,prefix:'x',tree:{n:[]}});`,
		`define({numchunks:true,prefix:'x',tree:{n:[{i:0}]}});`,
		`define({numchunks:1.0,prefix:'x',tree:{n:[{i:0}]}});`,
		`define({numchunks:1e0,prefix:'x',tree:{n:[{i:0}]}});`,
		`define({numchunks:1001,prefix:'x',tree:{n:[{i:0}]}});`,
		`define({numchunks:1,prefix:'../x',tree:{n:[{i:0}]}});`,
		`define({numchunks:1,prefix:'x',tree:{n:'broken'}});`,
		`define({numchunks:1,numchunks:2});`,
		`define({numchunks:1,"\u006eumchunks":2});`,
		`define(function(){return {numchunks:1}});`,
		`define({}); alert('never execute');`,
	} {
		t.Run(bad, func(t *testing.T) {
			f := flareFixture()
			f.set(planModule, bad, "application/javascript")
			if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	for _, bad := range []string{`define({});`, `define({'/Content/a.htm':{i:[2,4],t:['one'],b:['']}});`,
		`define({'/Content/a.htm':{i:[2,2],t:['one','two'],b:['','']}});`,
		`define({'/Content/a.htm':{i:['2'],t:['one'],b:['']}});`,
		`define({'/Content/a.htm':{i:[2],t:['one'],b:[null]}});`,
	} {
		f := flareFixture()
		f.set(planRoot+"Data/Tocs/Guide_Chunk1.js", bad, "application/javascript")
		if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
			t.Fatal("inconsistent chunk accepted")
		}
	}
	f := flareFixture()
	delete(f.responses, planRoot+"Data/Tocs/Guide_Chunk1.js")
	if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
		t.Fatal("missing chunk ignored")
	}
	f = flareFixture()
	f.set(planModule, `define({numchunks:2,prefix:'Guide_Chunk',tree:{n:[{i:0,c:1}]}});`, "application/javascript")
	if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
		t.Fatal("wrong tree chunk reference accepted")
	}
}

func TestFlareDataEscapesAndSourceSpaces(t *testing.T) {
	f := flareFixture()
	f.set(planModule, `define({numchunks:+0x2,prefix:'Guide_Chunk',tree:{n:[{i:0,c:0},{i:1,c:0},{i:2,c:1},{i:3,c:0},{i:4,c:1},{i:5,c:1},{i:6,c:1},{i:7,c:1},]}});`, "application/javascript")
	f.set(planRoot+"Data/Tocs/Guide_Chunk0.js", `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/contents.htm':{i:[1],t:['TOC'],b:['']},'/Content/logging filter%20name.htm':{i:[3],t:['\x43opyright'],b:['']}});`, "application/javascript")
	plan, err := LoadPlan(context.Background(), f, document("flare"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Topics[2].URL != planRoot+"Content/logging%20filter%20name.htm" || plan.Topics[2].Title != "Copyright" {
		t.Fatalf("source escapes lost: %+v", plan.Topics)
	}
}

func TestFlareScopeCompletenessAndBudget(t *testing.T) {
	f := flareFixture()
	f.set(planRoot+"Data/Tocs/Guide_Chunk0.js", `define({'/Content/home.htm':{i:[0],t:['Home'],b:['']},'/Content/contents.htm':{i:[1],t:['TOC'],b:['']},'/Content/copyright.htm':{i:[3,99],t:['Copyright','Unused'],b:['','']}});`, "application/javascript")
	if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
		t.Fatal("unused chunk index hidden")
	}
	f = flareFixture()
	f.set(planDetail, `<html><ul data-mc-linked-toc="../../outside.js"></ul></html>`, "text/html")
	if _, err := LoadPlan(context.Background(), f, document("flare")); err == nil {
		t.Fatal("out-of-guide module fetched")
	}
	if len(f.calls) != 2 {
		t.Fatal("unsafe module requested")
	}
	f = flareFixture()
	ctx := WithInventoryRequestBudget(context.Background(), func(count int) error { return errors.New("bounded live budget") })
	if _, err := LoadPlan(ctx, f, document("flare")); err == nil || len(f.calls) != 3 {
		t.Fatal("chunk request budget not applied before chunks")
	}
}

func TestHPEPlanPreservesAcceptedInventorySemantics(t *testing.T) {
	id := "sd00007433en_us"
	f := hpeFixture(id)
	d := hpeDoc(id)
	d.URL += "&mask=sh-rs&page=GUID-ONE.html"
	plan, err := LoadPlan(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Topics) != 3 || len(f.calls) != 2 || plan.Topics[0].URL != hpePublic+id ||
		plan.Topics[1].FetchURL != hpeAPI+id+"?page=GUID-ONE.html" || plan.TOC[2].Children[0].URL != hpePublic+id+"&page=GUID-ONE.html#section" {
		t.Fatalf("HPE identities/positions lost: %+v", plan)
	}
	if plan.PDFURL != "" || len(plan.Styles) != 1 || plan.Styles[0] != hpeStyle || !plan.Inventory.Complete {
		t.Fatalf("wrong HPE plan: %+v", plan)
	}
}

func TestHPEInvalidInventoriesNeverTriggerPDFExports(t *testing.T) {
	for _, toc := range []string{`{}`, `[]`, `"bad"`, `[null]`,
		`[{"topicName":"Leaf","topicLink":null}]`,
		`[{"topicName":"Leaf","topicLink":"../escape.html"}]`,
		`[{"topicName":"Leaf","topicLink":"GUID.html?docId=other"}]`,
		`[{"topicName":7,"topicLink":"GUID.html"}]`,
		`[{"topicName":"Leaf","topicLink":"GUID.html","children":{}}]`,
		`[{"topicName":"Leaf","topicName":"Duplicate","topicLink":"GUID.html"}]`,
	} {
		id := "sd1en_us"
		f := hpeFixture(id)
		f.set(hpeAPI+id+"?page=content.json", toc, "application/json")
		if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
			t.Fatalf("invalid HPE inventory accepted: %s", toc)
		}
		for _, raw := range f.calls {
			if strings.Contains(raw, "exportpdf") {
				t.Fatal("inventory error caused export fallback")
			}
		}
	}
}

func TestHPEIdentityMIMEAndQueries(t *testing.T) {
	id := "sd00007909en_usen_us"
	f := hpeFixture(id)
	for raw, r := range f.responses {
		delete(f.responses, raw)
		r.URL += "&revision=2"
		f.responses[raw+"&revision=2"] = r
	}

	d := hpeDoc(id)
	d.URL += "&revision=2"
	plan, err := LoadPlan(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Topics[0].URL, "revision=2") ||
		!strings.Contains(plan.Topics[1].FetchURL, "revision=2") ||
		len(plan.Warnings) != 1 || len(plan.Notices) != 1 ||
		plan.Notices[0].Kind != model.NoticeSourcePolicy {
		t.Fatalf("query/locale lost: %+v", plan)
	}
	for _, bad := range []string{"application/json", "image/png", "unknown/type"} {
		f := hpeFixture(id)
		r := f.responses[hpeAPI+id+"?ignorePayload=true"]
		r.Headers.Set("Content-Type", bad)
		f.responses[hpeAPI+id+"?ignorePayload=true"] = r
		if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
			t.Fatalf("wrong MIME accepted: %s", bad)
		}
	}
	for _, header := range []string{"Doc-Id", "Doc-Page-Name"} {
		f := hpeFixture(id)
		r := f.responses[hpeAPI+id+"?ignorePayload=true"]
		r.Headers.Set(header, "wrong")
		f.responses[hpeAPI+id+"?ignorePayload=true"] = r
		if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
			t.Fatal("wrong returned identity accepted")
		}
	}
	f = hpeFixture(id)
	r := f.responses[hpeAPI+id+"?page=content.json"]
	r.URL = hpeAPI + "other?page=content.json"
	f.responses[hpeAPI+id+"?page=content.json"] = r
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
		t.Fatal("cross-document redirect accepted")
	}
	f = hpeFixture(id)
	r = f.responses[hpeAPI+id+"?ignorePayload=true"]
	r.URL += "&page=GUID-other.html"
	f.responses[hpeAPI+id+"?ignorePayload=true"] = r
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
		t.Fatal("redirect-added HPE page identity accepted")
	}
	f = hpeFixture(id)
	r = f.responses[hpeAPI+id+"?ignorePayload=true"]
	r.URL += "&mask=sh-rs"
	f.responses[hpeAPI+id+"?ignorePayload=true"] = r
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err != nil {
		t.Fatalf("routing-only HPE mask redirect rejected: %v", err)
	}
}

func TestHPEContentJSONAcceptsObservedMultipageMIMEOnlyAtExactEndpoint(t *testing.T) {
	id := "sd1en_us"
	f := hpeFixture(id)
	r := f.responses[hpeAPI+id+"?page=content.json"]
	r.Headers.Set("Content-Type", "multiPage")
	f.responses[hpeAPI+id+"?page=content.json"] = r
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err != nil {
		t.Fatalf("observed content.json MIME rejected: %v", err)
	}
	other := input{url: "https://publisher.example.test/content.json", body: []byte(`[]`),
		headers: http.Header{"Content-Type": {"multiPage"}}}
	p := newPlanner(context.Background(), f, document("static"), false)
	if _, err := p.json(other); err == nil {
		t.Fatal("multipage MIME generalized outside exact HPE content endpoint")
	}
	f = hpeFixture(id)
	f.set(hpeAPI+id+"?page=content.json", `<main class="ditasrc"><h1>Error</h1></main>`, "multiPage")
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
		t.Fatal("multipage HTML/error body accepted as HPE TOC JSON")
	}
	f = hpeFixture(id)
	r = f.responses[hpeAPI+id+"?page=content.json"]
	r.URL = hpeAPI + "other?page=content.json"
	r.Headers.Set("Content-Type", "multiPage")
	f.responses[hpeAPI+id+"?page=content.json"] = r
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
		t.Fatal("multipage JSON from another document accepted")
	}
}

func TestHPEUnknownFrontAndPDFWrapper(t *testing.T) {
	id := "sd1en_us"
	for _, body := range []string{`<html><h1>Document not found</h1></html>`, `<html><p>Landing</p></html>`,
		`<html><input type=password></html>`, `<main class="ditasrc"></main>`} {
		f := hpeFixture(id)
		f.set(hpeAPI+id+"?ignorePayload=true", body, "multiPage;charset=UTF-8")
		if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
			t.Fatal("invalid front matter accepted")
		}
	}
	f := hpeFixture(id)
	pdf := hpeAPI + id + "/original.pdf?version=1"
	f.set(hpeAPI+id+"?ignorePayload=true", `<html><body><embed type="application/pdf" src="`+pdf+`"></body></html>`, "text/html")
	f.set(pdf, string(testutil.PDF("original")), "application/pdf")
	plan, err := LoadPlan(context.Background(), f, hpeDoc(id))
	if err != nil {
		t.Fatal(err)
	}
	if plan.PDFURL != pdf || !plan.PDFVerified || len(plan.Topics) != 0 || plan.Document.URL != hpePublic+id {
		t.Fatalf("verified wrapper PDF incorrect: %+v", plan)
	}
	f = hpeFixture(id)
	f.set(hpeAPI+id+"?ignorePayload=true", `<html><embed type="application/pdf" src="`+hpeAPI+`other/original.pdf"></html>`, "text/html")
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil || len(f.calls) != 1 {
		t.Fatal("another document's PDF was fetched")
	}
}

func TestStaticCompleteAndUnsupportedTOCs(t *testing.T) {
	home := "https://publisher.example.test/book/index.html"
	d := document("static")
	d.URL = home
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(home, `<html><head><link rel="stylesheet" href="style.css"></head><nav class="wh_publication_toc"><ul>
<li><a href="copyright.html">Copyright</a></li><li><a href="chapter.html?edition=2">Chapter</a><ul>
<li><a href="topic.html#section">Nested topic</a></li><li><a href="chapter.html?edition=3">Other</a></li></ul></li>
<li><a href="topic.html">Repeated topic</a></li><li><a href="https://external.test/support.html">Support</a></li>
</ul></nav></html>`, "text/html")
	plan, err := LoadPlan(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Topics) != 5 || len(plan.Warnings) != 0 || len(plan.Notices) != 1 ||
		plan.Notices[0].Kind != model.NoticeExternalLinkRetained ||
		plan.TOC[2].Children[0].URL != "https://publisher.example.test/book/topic.html#section" {
		t.Fatalf("static hierarchy lost: %+v", plan)
	}
	for _, bad := range []string{`<html><a href="topic.html">Shortcut</a></html>`, `<html><nav id="toc"></nav></html>`,
		`<html><nav id="toc"><ul><li data-state="not-ready"><a href="topic.html">Lazy</a></li></ul></nav></html>`} {
		f.set(home, bad, "text/html")
		if _, err := LoadPlan(context.Background(), f, d); err == nil {
			t.Fatal("unknown/lazy static source accepted")
		}
	}
	f = flareFixture()
	plan, err = LoadPlan(context.Background(), f, document("static"))
	if err != nil || plan.Document.Kind != "flare" || len(plan.Topics) != 5 {
		t.Fatalf("static Flare reclassification failed: %v", err)
	}
}

func TestSourceDataLimitsCancellationAndBinaryDetection(t *testing.T) {
	id := "sd1en_us"
	f := hpeFixture(id)
	toc := `{"topicName":"Leaf","topicLink":"GUID.html"}`
	for range 102 {
		toc = `{"topicName":"Group","children":[` + toc + `]}`
	}

	f.set(hpeAPI+id+"?page=content.json", "["+toc+"]", "application/json")
	if _, err := LoadPlan(context.Background(), f, hpeDoc(id)); err == nil {
		t.Fatal("depth guard ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadPlan(ctx, flareFixture(), document("flare")); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled planner succeeded")
	}
	for _, kind := range []string{"pdf", "static", "flare"} {
		f := &planFixture{responses: map[string]model.Resource{}}
		f.set(planHome, string(testutil.PDF("original")), "application/pdf")
		plan, err := LoadPlan(context.Background(), f, document(kind))
		if err != nil || !plan.PDFVerified || plan.Document.Kind != "pdf" {
			t.Fatalf("actual PDF not detected: %v", err)
		}
	}
}

func TestStaticFollowsOneDetailedTOCNotHomepageShortcuts(t *testing.T) {
	home := "https://publisher.example.test/book/index.html"
	toc := "https://publisher.example.test/book/toc.html"
	d := document("static")
	d.URL = home
	f := &planFixture{responses: map[string]model.Resource{}}
	f.set(home, `<html><a href="toc.html">Table of Contents</a><a href="shortcut.html">Shortcut</a></html>`, "text/html")
	f.set(toc, `<html><nav role="doc-toc"><ul><li><a href="chapter.html">Full topic</a></li></ul></nav></html>`, "text/html")
	plan, err := LoadPlan(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{home, toc, "https://publisher.example.test/book/chapter.html"}
	if len(plan.Topics) != len(want) {
		t.Fatalf("wrong topic inventory: %+v", plan.Topics)
	}
	for i, topic := range plan.Topics {
		if topic.URL != want[i] {
			t.Fatalf("homepage shortcut used: %+v", plan.Topics)
		}
	}
	if len(f.calls) != 2 || plan.Inventory.TOCURL != toc {
		t.Fatalf("detailed TOC evidence lost: %+v calls=%v", plan.Inventory, f.calls)
	}
	f.set(home, `<html><a href="a.html">Table of Contents</a><a href="b.html">Contents</a></html>`, "text/html")
	if _, err := LoadPlan(context.Background(), f, d); err == nil {
		t.Fatal("ambiguous detailed TOCs accepted")
	}
}

func TestHPEPageInputStillInventoriesWholeDocumentAndOtherDocsDiffer(t *testing.T) {
	first := hpeDoc("doc-A")
	first.URL += "&page=GUID-topic.html"
	one, err := LoadPlan(context.Background(), hpeFixture("doc-A"), first)
	if err != nil {
		t.Fatal(err)
	}
	two, err := LoadPlan(context.Background(), hpeFixture("doc-B"), hpeDoc("doc-B"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(one.Topics[0].URL, "page=") || one.Topics[1].URL == two.Topics[1].URL ||
		one.Topics[1].FetchURL == two.Topics[1].FetchURL {
		t.Fatal("whole-document or document identity lost")
	}
}

type cancellingFetcher struct {
	inner  *planFixture
	cancel context.CancelFunc
	at     string
}

func (f cancellingFetcher) Get(ctx context.Context, raw string, refresh bool) (model.Resource, error) {
	resource, err := f.inner.Get(ctx, raw, refresh)
	if raw == f.at {
		f.cancel()
	}
	return resource, err
}

func TestCancellationDuringFinalChunkCannotReturnCompletePlan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fixture := flareFixture()
	_, err := LoadPlan(ctx, cancellingFetcher{
		inner: fixture, cancel: cancel, at: planRoot + "Data/Tocs/Guide_Chunk1.js",
	}, document("flare"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("final chunk cancellation hidden: %v", err)
	}
}
