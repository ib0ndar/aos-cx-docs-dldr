package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func completionValues(result directoryCompletionResult) []string {
	values := make([]string, len(result.suggestions))
	for index, suggestion := range result.suggestions {
		values[index] = suggestion.value
	}
	return values
}

func TestDirectoryCompletionResolvesLiteralPathFormsAndDirectoriesOnly(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "Home")
	for _, path := range []string{
		filepath.Join(root, "Alpha"),
		filepath.Join(root, "A Space"),
		filepath.Join(root, "文档"),
		filepath.Join(root, ".hidden"),
		filepath.Join(root, "[literal]"),
		filepath.Join(home, "Desktop"),
		filepath.Join(root, "Parent", "Child"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "A File"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Alpha"), filepath.Join(root, "Alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, filepath.Join(root, "Shortcut")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "Broken")); err != nil {
		t.Fatal(err)
	}
	resolver := directoryCompletionResolver{
		cwd: root, home: home, read: readDirectoryEntries, stat: os.Stat,
	}
	cases := []struct {
		name  string
		value string
		want  []string
	}{
		{"relative", "A", []string{"A Space" + string(os.PathSeparator), "Alias" + string(os.PathSeparator), "Alpha" + string(os.PathSeparator)}},
		{"dot-relative", "./A", []string{"./A Space" + string(os.PathSeparator), "./Alias" + string(os.PathSeparator), "./Alpha" + string(os.PathSeparator)}},
		{"parent-relative", "Parent/../A", []string{"Parent/../A Space" + string(os.PathSeparator), "Parent/../Alias" + string(os.PathSeparator), "Parent/../Alpha" + string(os.PathSeparator)}},
		{"absolute", filepath.Join(root, "文"), []string{filepath.Join(root, "文档") + string(os.PathSeparator)}},
		{"home", "~/D", []string{"~/Desktop" + string(os.PathSeparator)}},
		{"trailing-separator", "Parent/", []string{"Parent/Child" + string(os.PathSeparator)}},
		{"symlink-parent", "Shortcut/D", []string{"Shortcut/Desktop" + string(os.PathSeparator)}},
		{"literal-glob-character", "[", []string{"[literal]" + string(os.PathSeparator)}},
		{"hidden-default", "", []string{"A Space" + string(os.PathSeparator), "Alias" + string(os.PathSeparator), "Alpha" + string(os.PathSeparator), "Home" + string(os.PathSeparator), "Parent" + string(os.PathSeparator), "Shortcut" + string(os.PathSeparator), "[literal]" + string(os.PathSeparator), "文档" + string(os.PathSeparator)}},
		{"hidden-explicit", ".", []string{".hidden" + string(os.PathSeparator)}},
		{"new-nested-parent", "Does Not Exist/Child", nil},
	}
	invalidName := string([]byte{'b', 0xff})
	if err := os.Mkdir(filepath.Join(root, invalidName), 0o700); err == nil {
		result := resolver.complete(directoryCompletionRequest{})
		if !result.omitted {
			t.Fatal("an unrepresentable directory name was silently omitted")
		}
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result := resolver.complete(directoryCompletionRequest{value: test.value})
			if result.err != "" {
				t.Fatalf("completion failed: %s", result.err)
			}
			got := completionValues(result)
			if strings.Join(got, "\x00") != strings.Join(test.want, "\x00") {
				t.Fatalf("completion mismatch\n got: %q\nwant: %q", got, test.want)
			}
		})
	}
}

func TestDirectoryCompletionReportsErrorsAndBoundsResults(t *testing.T) {
	root := t.TempDir()
	for index := range maxDirectoryCompletions + 20 {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("dir-%03d", index)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	resolver := directoryCompletionResolver{
		cwd: root,
		read: func(path string, limit int) ([]os.DirEntry, bool, error) {
			if limit != maxCompletionScannedEntries {
				t.Fatalf("unexpected scan bound: %d", limit)
			}
			return entries, true, nil
		},
		stat: os.Stat,
	}
	result := resolver.complete(directoryCompletionRequest{})
	if !result.truncated {
		t.Fatal("bounded scan was not reported as truncated")
	}
	if len(result.suggestions) != maxDirectoryCompletions {
		t.Fatalf("result bound not enforced: %d", len(result.suggestions))
	}

	resolver.read = func(string, int) ([]os.DirEntry, bool, error) {
		return nil, false, fs.ErrPermission
	}
	result = resolver.complete(directoryCompletionRequest{})
	if !strings.Contains(result.err, "permission denied") {
		t.Fatalf("unreadable parent error was hidden: %+v", result)
	}

	resolver.read = func(string, int) ([]os.DirEntry, bool, error) {
		return nil, false, fs.ErrNotExist
	}
	result = resolver.complete(directoryCompletionRequest{value: "new/child"})
	if result.err != "" || len(result.suggestions) != 0 {
		t.Fatalf("new nested path was treated as invalid: %+v", result)
	}
}

func TestDirectoryCompletionRequiresOnlyTheBaseUsedByThePath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Absolute"), 0o700); err != nil {
		t.Fatal(err)
	}
	scans := 0
	resolver := directoryCompletionResolver{
		cwdErr:  errors.New("cwd unavailable"),
		homeErr: errors.New("home unavailable"),
		read: func(path string, limit int) ([]os.DirEntry, bool, error) {
			scans++
			return readDirectoryEntries(path, limit)
		},
		stat: os.Stat,
	}
	for _, test := range []struct {
		value string
		want  string
	}{
		{"relative", "current working directory"},
		{"../relative", "current working directory"},
		{"~/Documents", "home directory"},
	} {
		before := scans
		result := resolver.complete(directoryCompletionRequest{value: test.value})
		if !strings.Contains(result.err, test.want) {
			t.Fatalf("%q did not report its unavailable base: %+v", test.value, result)
		}
		if scans != before {
			t.Fatalf("%q performed a filesystem scan without its required base", test.value)
		}
	}
	result := resolver.complete(directoryCompletionRequest{value: filepath.Join(root, "Abs")})
	if result.err != "" || strings.Join(completionValues(result), ",") != filepath.Join(root, "Absolute")+string(os.PathSeparator) {
		t.Fatalf("absolute completion incorrectly required cwd/home: %+v", result)
	}
	if scans != 1 {
		t.Fatalf("absolute completion performed %d scans", scans)
	}
}

func TestInputPromptAutocompleteKeysPreserveTypedEnterAndSafeDisplay(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{Initial: "typed/new", AllowBack: true})
	model.completionID = 7
	result := directoryCompletionResult{
		promptID: 7, generation: 0, value: "typed/new",
		suggestions: []directorySuggestion{
			{value: "typed/new\x1b[2J/"}, {value: "typed/new child/"},
		},
	}
	next, _ := model.Update(result)
	model = next.(inputPrompt)
	if strings.ContainsRune(model.View(), '\x1b') || !strings.Contains(model.View(), `\u001B`) {
		t.Fatalf("untrusted directory control byte reached the terminal: %q", model.View())
	}
	controlPath := model.suggestions[0].value
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	if string(model.value) != controlPath {
		t.Fatalf("safe display altered the actual completed path: got %q want %q", string(model.value), controlPath)
	}
	model = newInputPrompt("Destination", promptConfig{Initial: "typed/new", AllowBack: true})
	model.completionID = 7
	next, _ = model.Update(result)
	model = next.(inputPrompt)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(inputPrompt)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(inputPrompt)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	if string(model.value) != "typed/new" || model.completionIndex != -1 {
		t.Fatalf("Tab did not restore the original draft after the last sibling: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	if string(model.value) != "typed/new\x1b[2J/" {
		t.Fatalf("Tab did not continue from the restored draft: %q", string(model.value))
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(inputPrompt)
	if model.accepted || model.menuActive {
		t.Fatalf("first Enter did not only close the completion menu: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(inputPrompt)
	if model.acceptedValue != "typed/new\x1b[2J/" {
		t.Fatalf("second Enter changed the previewed path: %q", model.acceptedValue)
	}

	model = newInputPrompt("Destination", promptConfig{Initial: "brand new"})
	model.suggestions = []directorySuggestion{{value: "brand newer/"}}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := next.(inputPrompt).acceptedValue; got != "brand new" {
		t.Fatalf("Enter silently chose a suggestion: %q", got)
	}
	model = newInputPrompt("Destination", promptConfig{})
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := next.(inputPrompt).acceptedValue; got != "" {
		t.Fatalf("blank Enter no longer returns the displayed default sentinel: %q", got)
	}
}

func TestCompletionCommonPrefixCyclingSlashAndTwoStageEnter(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{Initial: "/tmp/Tar"})
	model.completionID = 12
	next, command := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	if command != nil || !model.commonPrefixPending || model.menuActive {
		t.Fatalf("inactive Tab did not request common-prefix completion: %+v", model)
	}
	result := directoryCompletionResult{
		promptID: 12, generation: model.generation, value: "/tmp/Tar",
		suggestions: []directorySuggestion{
			{value: "/tmp/Target A/", label: "Target A/"},
			{value: "/tmp/Target Root/", label: "Target Root/"},
		},
	}
	next, _ = model.Update(result)
	model = next.(inputPrompt)
	if string(model.value) != "/tmp/Target " || !model.menuActive || model.completionIndex != -1 {
		t.Fatalf("common prefix or initial completion state mismatch: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(inputPrompt)
	if string(model.value) != "/tmp/Target A/" || model.completionIndex != 0 {
		t.Fatalf("Down did not preview the first sibling: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	if string(model.value) != "/tmp/Target Root/" || model.completionIndex != 1 {
		t.Fatalf("Tab did not cycle siblings without diving: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = next.(inputPrompt)
	if string(model.value) != "/tmp/Target A/" {
		t.Fatalf("Up did not preview the prior sibling: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	model = next.(inputPrompt)
	if string(model.value) != "/tmp/Target A/" || model.menuActive ||
		model.completionBase != "/tmp/Target A/" {
		t.Fatalf("slash duplicated a separator or failed to start the child component: %+v", model)
	}
	next, _ = model.Update(directoryCompletionResult{
		promptID: 12, generation: model.generation, value: "/tmp/Target A/",
		suggestions: []directorySuggestion{{value: "/tmp/Target A/Child/", label: "Child/"}},
	})
	model = next.(inputPrompt)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(inputPrompt)
	if model.accepted || model.menuActive {
		t.Fatalf("first Enter submitted instead of closing the active menu: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = next.(inputPrompt)
	if !model.accepted || model.acceptedValue != "/tmp/Target A/" {
		t.Fatalf("second Enter did not submit the typed path: %+v", model)
	}
}

func TestCompletionBlankStartupAndExplicitTab(t *testing.T) {
	service := &directoryCompletionService{
		requests: make(chan directoryCompletionRequest, 1),
		stop:     make(chan struct{}),
	}
	defer service.Close()
	model := newInputPrompt("Destination", promptConfig{}, service)
	if command := model.Init(); command != nil {
		t.Fatal("blank fresh prompt initiated unsolicited completion")
	}
	if model.loading || len(service.requests) != 0 {
		t.Fatalf("blank fresh prompt entered a loading state: %+v", model)
	}
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	accepted := next.(inputPrompt)
	if !accepted.accepted || accepted.acceptedValue != "" {
		t.Fatalf("blank startup Enter did not immediately accept the CWD sentinel: %+v", accepted)
	}

	model = newInputPrompt("Destination", promptConfig{}, service)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	if !model.loading || !model.commonPrefixPending {
		t.Fatalf("explicit blank Tab did not start completion: %+v", model)
	}
	request := <-service.requests
	if request.value != "" || request.generation != model.generation {
		t.Fatalf("explicit blank Tab queued the wrong request: %+v", request)
	}
}

func TestCompletionCyclesThroughOriginalDraft(t *testing.T) {
	for _, test := range []struct {
		name       string
		expand     bool
		wantBase   string
		generation uint64
	}{
		{name: "typed-base", wantBase: "/fixture/Ta"},
		{name: "common-prefix-base", expand: true, wantBase: "/fixture/Target ", generation: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := newInputPrompt("Destination", promptConfig{Initial: "/fixture/Ta"})
			if test.expand {
				next, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
				model = next.(inputPrompt)
			}
			next, _ := model.Update(directoryCompletionResult{
				generation: model.generation, value: "/fixture/Ta",
				suggestions: []directorySuggestion{
					{value: "/fixture/Target A/", label: "Target A/"},
					{value: "/fixture/Target Root/", label: "Target Root/"},
				},
			})
			model = next.(inputPrompt)
			if model.completionBase != test.wantBase {
				t.Fatalf("completion base=%q want %q", model.completionBase, test.wantBase)
			}
			for range 3 {
				next, _ = model.Update(tea.KeyMsg{Type: tea.KeyTab})
				model = next.(inputPrompt)
			}
			if string(model.value) != test.wantBase || model.completionIndex != -1 {
				t.Fatalf("forward cycle did not restore %q: value=%q index=%d",
					test.wantBase, string(model.value), model.completionIndex)
			}
			for range 3 {
				next, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
				model = next.(inputPrompt)
			}
			if string(model.value) != test.wantBase || model.completionIndex != -1 {
				t.Fatalf("reverse cycle did not restore %q: value=%q index=%d",
					test.wantBase, string(model.value), model.completionIndex)
			}
		})
	}
}

func TestCompletionUniqueInactiveTabDismissesMenu(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{Initial: "/fixture/Uni"})
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = next.(inputPrompt)
	next, _ = model.Update(directoryCompletionResult{
		generation: model.generation, value: "/fixture/Uni",
		suggestions: []directorySuggestion{
			{value: "/fixture/Unique/", label: "Unique/"},
		},
	})
	model = next.(inputPrompt)
	if model.menuActive || string(model.value) != "/fixture/Unique/" ||
		model.completionIndex != -1 {
		t.Fatalf("unique inactive Tab did not insert and dismiss completion: %+v", model)
	}
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if accepted := next.(inputPrompt); !accepted.accepted ||
		accepted.acceptedValue != "/fixture/Unique/" {
		t.Fatalf("Enter after unique inactive Tab did not submit: %+v", accepted)
	}
}

func TestCompletionMenuUsesVerticalColumns(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{})
	model.width, model.height = 80, 24
	model.menuActive = true
	for index := range 6 {
		name := fmt.Sprintf("dir%d/", index)
		model.suggestions = append(model.suggestions, directorySuggestion{value: name, label: name})
	}
	grid := model.completionGrid()
	if grid.rows != 3 || grid.columns != 2 {
		t.Fatalf("six short completions use grid %dx%d, want 3x2", grid.rows, grid.columns)
	}
	lines := strings.Split(ansi.Strip(model.View()), "\n")
	var firstMenuLine string
	for _, line := range lines {
		if strings.Contains(line, "dir0/") {
			firstMenuLine = line
			break
		}
	}
	if strings.Contains(firstMenuLine, "dir1/") || !strings.Contains(firstMenuLine, "dir3/") {
		t.Fatalf("completion grid was not filled down columns: %q", firstMenuLine)
	}
	next, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(inputPrompt)
	next, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = next.(inputPrompt)
	if model.completionIndex != 1 || string(model.value) != "dir1/" {
		t.Fatalf("Down did not advance vertically within the first column: %+v", model)
	}

	model.suggestions[5] = directorySuggestion{
		value: strings.Repeat("long-name-", 12) + "/",
		label: strings.Repeat("long-name-", 12) + "/",
	}
	grid = model.completionGrid()
	if grid.columns < 2 || grid.cellWidth >= model.width {
		t.Fatalf("one long label collapsed the menu to one full-width column: %+v", grid)
	}
	model.completionIndex = 4
	model = model.cycleCompletion(1)
	if string(model.value) != model.suggestions[5].value {
		t.Fatal("truncated display changed the full selected completion value")
	}
}

func TestInputPromptDropsStaleAndBackedOutCompletionResults(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{Initial: "new", AllowBack: true})
	model.completionID, model.generation = 9, 2
	current := directoryCompletionResult{
		promptID: 9, generation: 2, value: "new",
		suggestions: []directorySuggestion{{value: "new-current/"}},
	}
	next, _ := model.Update(current)
	model = next.(inputPrompt)
	stale := directoryCompletionResult{
		promptID: 9, generation: 1, value: "old",
		suggestions: []directorySuggestion{{value: "old-stale/"}},
	}
	next, _ = model.Update(stale)
	model = next.(inputPrompt)
	if got := completionValues(directoryCompletionResult{suggestions: model.suggestions}); strings.Join(got, ",") != "new-current/" {
		t.Fatalf("reversed stale result replaced current suggestions: %v", got)
	}

	session := promptSession{}
	response := make(chan promptResult, 1)
	nextSession, _ := session.Update(promptRequest{model: model, response: response})
	session = nextSession.(promptSession)
	nextSession, _ = session.Update(tea.KeyMsg{Type: tea.KeyLeft})
	session = nextSession.(promptSession)
	if result := <-response; !errors.Is(result.err, errPromptBack) || result.input != "new" {
		t.Fatalf("Back did not retain the typed path: %+v", result)
	}
	nextSession, _ = session.Update(current)
	session = nextSession.(promptSession)
	if session.active != nil || session.View() != "" {
		t.Fatal("late completion result revived a backed-out prompt")
	}
}

func TestCompletionServiceCoalescesWithoutBlockingPromptUpdates(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	resolver := directoryCompletionResolver{
		cwd: "/fixture",
		read: func(string, int) ([]os.DirEntry, bool, error) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-release
			return nil, false, nil
		},
	}
	delivered := make(chan tea.Msg, 1)
	service := newDirectoryCompletionService(resolver, func(message tea.Msg) {
		delivered <- message
	})
	t.Cleanup(func() {
		close(release)
		service.Close()
	})
	model := newInputPrompt("Destination", promptConfig{Initial: "blocked"}, service)
	if command := model.Init(); command != nil {
		command()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("completion worker did not start")
	}
	start := time.Now()
	for index := range 1000 {
		next, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		model = next.(inputPrompt)
		if command != nil {
			t.Fatal("keystroke spawned a completion command instead of using the bounded worker queue")
		}
		if index%10 == 0 {
			next, _ = model.Update(tea.KeyMsg{Type: tea.KeyBackspace})
			model = next.(inputPrompt)
		}
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("filesystem lookup blocked the prompt event loop: %s", elapsed)
	}
	if len(service.requests) > 1 {
		t.Fatalf("completion request queue exceeded its bound: %d", len(service.requests))
	}
	service.Close()
	select {
	case message := <-delivered:
		t.Fatalf("closed completion service delivered a late result: %+v", message)
	case <-time.After(80 * time.Millisecond):
	}
}

func TestCompletionServiceRejectsOutOfOrderEnqueue(t *testing.T) {
	service := &directoryCompletionService{
		requests: make(chan directoryCompletionRequest, 1),
		stop:     make(chan struct{}),
	}
	service.enqueue(directoryCompletionRequest{promptID: 3, generation: 8, value: "new"})
	service.enqueue(directoryCompletionRequest{promptID: 3, generation: 7, value: "stale"})
	service.enqueue(directoryCompletionRequest{promptID: 2, generation: 100, value: "old prompt"})
	request := <-service.requests
	if request.value != "new" {
		t.Fatalf("out-of-order command replaced the newest request: %+v", request)
	}
	service.Close()
}

func TestCompletionServiceConcurrentProducersRetainNewestGeneration(t *testing.T) {
	const producers = 128
	for round := range 100 {
		service := &directoryCompletionService{
			requests: make(chan directoryCompletionRequest, 1),
			stop:     make(chan struct{}),
		}
		start := make(chan struct{})
		var wait sync.WaitGroup
		for generation := range producers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				service.enqueue(directoryCompletionRequest{
					promptID: 1, generation: uint64(generation), value: fmt.Sprint(generation),
				})
			}()
		}
		close(start)
		wait.Wait()
		request := <-service.requests
		if request.generation != producers-1 {
			t.Fatalf("round %d retained generation %d instead of newest %d",
				round, request.generation, producers-1)
		}
		service.Close()
	}
}

func TestInputInitialLookupCannotReplaceFastTypingRequest(t *testing.T) {
	for round := range 100 {
		service := &directoryCompletionService{
			requests: make(chan directoryCompletionRequest, 1),
			stop:     make(chan struct{}),
		}
		model := newInputPrompt("Destination", promptConfig{Initial: "i"}, service)
		initial := model.Init()
		if initial == nil {
			t.Fatal("nonempty restored input did not request completion")
		}
		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			<-start
			initial()
			close(done)
		}()
		close(start)
		next, command := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		model = next.(inputPrompt)
		if command != nil {
			t.Fatal("fast typing created an asynchronous per-key command")
		}
		<-done
		request := <-service.requests
		if request.generation != model.generation || request.value != "in" {
			t.Fatalf("round %d initial lookup replaced fast typing: %+v", round, request)
		}
		service.Close()
	}
}

func TestInputSuggestionsAdaptToTerminalHeight(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{})
	for index := range 30 {
		model.suggestions = append(model.suggestions, directorySuggestion{
			value: strings.Repeat("界", 12) + "-" + string(rune('a'+index)) + "/",
		})
	}
	model.menuActive = true
	next, _ := model.Update(tea.WindowSizeMsg{Width: 28, Height: 12})
	model = next.(inputPrompt)
	view := model.View()
	// promptViewRows includes the terminal's final cursor row in addition to
	// rendered content, so a 12-row model may report 13 here.
	if promptViewRows(view, 28) > 13 || !strings.Contains(view, "Showing ") {
		t.Fatalf("directory suggestions ignored terminal geometry:\n%s", view)
	}

	model.suggestions = model.suggestions[:8]
	next, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	model = next.(inputPrompt)
	view = model.View()
	if strings.Contains(view, "Showing ") || strings.Count(view, "/") < 8 {
		t.Fatalf("matching directories were hidden despite available terminal height:\n%s", view)
	}
	foundColumns := false
	for _, line := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Count(line, "/") >= 2 {
			foundColumns = true
			break
		}
	}
	if !foundColumns {
		t.Fatalf("wide completion menu did not use multiple columns:\n%s", view)
	}
}

func TestInputPromptShowsLookupAndTruncationState(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{Initial: "blocked"})
	model.completionID = 4
	next, _ := model.Update(directoryCompletionResult{
		promptID: 4, value: "blocked", err: "Cannot read directory blocked: permission denied",
	})
	model = next.(inputPrompt)
	if !strings.Contains(model.View(), "permission denied") {
		t.Fatalf("lookup error was not visible:\n%s", model.View())
	}
	model = newInputPrompt("Destination", promptConfig{Initial: "huge"})
	model.completionID = 5
	next, _ = model.Update(directoryCompletionResult{
		promptID: 5, value: "huge", truncated: true,
	})
	model = next.(inputPrompt)
	if !strings.Contains(model.View(), "scan limited") || strings.Contains(model.View(), "No matches") {
		t.Fatalf("truncated scan was presented as authoritative:\n%s", model.View())
	}
}

func TestInputLineShowsSafeTailOfLongPath(t *testing.T) {
	model := newInputPrompt("Destination", promptConfig{
		Initial: "/a/very/long/prefix/\x1b[2J/Target Folder",
	})
	model.width = 24
	line := model.inputValueDisplay()
	if strings.ContainsRune(line, '\x1b') || !strings.HasSuffix(line, "Folder") ||
		ansi.StringWidth(line) > model.width/2 {
		t.Fatalf("long input did not show a bounded safe tail: %q", line)
	}
}

func TestPromptSessionForwardsInputInitAndAsyncMessages(t *testing.T) {
	service := &directoryCompletionService{
		requests: make(chan directoryCompletionRequest, 1),
		stop:     make(chan struct{}),
	}
	service.nextID.Store(40)
	model := newInputPrompt("Destination", promptConfig{Initial: "a"}, service)
	response := make(chan promptResult, 1)
	session := promptSession{}
	next, command := session.Update(promptRequest{model: model, response: response})
	session = next.(promptSession)
	if command == nil {
		t.Fatal("prompt session discarded the input model Init command")
	}
	result := directoryCompletionResult{
		promptID: model.completionID, generation: 0, value: "a",
		suggestions: []directorySuggestion{{value: "alpha/"}},
	}
	next, _ = session.Update(result)
	session = next.(promptSession)
	if !strings.Contains(session.View(), "alpha/") {
		t.Fatal("prompt session discarded an asynchronous completion result")
	}
}

func TestDirectoryCompletionCancellationDoesNotCreateOutput(t *testing.T) {
	base := filepath.Join(t.TempDir(), "not-created")
	prompts := &wizardPrompts{replies: []wizardReply{
		{value: "all"},
		{value: base, err: context.Canceled},
	}}
	o := options{platform: "6300", version: "10.18"}
	_, err := completeInteractiveSelection(
		context.Background(), backCatalog(), &o, prompts, func(string) {},
		selectionFixed{Platform: true, Version: true}, true,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("destination cancellation was not retained: %v", err)
	}
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination prompt created output before confirmation: %v", err)
	}
}
