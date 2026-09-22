package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

type promptDriver interface {
	Select(context.Context, string, []selectionChoice, promptConfig) (string, error)
	MultiSelect(context.Context, string, []selectionChoice, promptConfig) ([]string, error)
	Input(context.Context, string, promptConfig) (string, error)
}

type promptConfig struct {
	Initial      string
	Selected     []string
	AllowBack    bool
	Context      string
	presentation humanPresentation
}

var errPromptBack = errors.New("interactive prompt requested previous question")

type promptCloser interface {
	Close() error
}

func closePromptDriver(prompts promptDriver) error {
	if closer, ok := prompts.(promptCloser); ok {
		return closer.Close()
	}
	return nil
}

type terminalPrompts struct {
	input        io.Reader
	output       io.Writer
	mu           sync.Mutex
	program      *tea.Program
	completion   *directoryCompletionService
	done         chan struct{}
	runErr       error
	closed       bool
	presentation humanPresentation
	freshLine    func()
}

type promptInput struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r promptInput) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return n, err
}

func newTerminalPrompts(input *os.File, output io.Writer) promptDriver {
	outputFile, ok := output.(*os.File)
	if input == nil || !ok || !term.IsTerminal(int(input.Fd())) || !term.IsTerminal(int(outputFile.Fd())) {
		return nil
	}
	return &terminalPrompts{
		input: input, output: output, presentation: newHumanPresentation(output),
		freshLine: func() { fmt.Fprint(output, "\r") },
	}
}

type promptResult struct {
	values []string
	input  string
	err    error
}

type promptRequest struct {
	model    tea.Model
	response chan promptResult
}

type promptStop struct{}

const (
	maxPendingPromptKeys  = 256
	maxPendingPromptRunes = 4096
)

type promptSession struct {
	active       tea.Model
	response     chan promptResult
	pending      []tea.KeyMsg
	pendingRunes int
	pendingErr   error
	width        int
	height       int
}

func (m promptSession) Init() tea.Cmd { return nil }

func (m promptSession) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch value := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = value.Width, value.Height
		if m.active != nil {
			var command tea.Cmd
			m.active, command = m.active.Update(value)
			return m, command
		}
		return m, nil
	case promptStop:
		if m.response != nil {
			m.response <- promptResult{err: context.Canceled}
		}
		return m, tea.Quit
	case promptRequest:
		if m.active != nil {
			value.response <- promptResult{err: errors.New("interactive prompt already active")}
			return m, nil
		}
		if m.pendingErr != nil {
			value.response <- promptResult{err: m.pendingErr}
			return m, nil
		}
		m.active, m.response = value.model, value.response
		var sizeCommand tea.Cmd
		m.active, sizeCommand = m.active.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
		commands := []tea.Cmd{m.active.Init(), sizeCommand}
		for len(m.pending) > 0 && m.active != nil {
			key := m.pending[0]
			m.pending = m.pending[1:]
			m.pendingRunes -= promptKeySize(key)
			var command tea.Cmd
			m, command = m.applyActive(key)
			commands = append(commands, command)
		}
		return m, tea.Batch(commands...)
	case tea.KeyMsg:
		if m.active == nil {
			size := promptKeySize(value)
			if len(m.pending) >= maxPendingPromptKeys ||
				m.pendingRunes+size > maxPendingPromptRunes {
				m.pendingErr = errors.New("too much input was entered between interactive prompts")
				m.pending = nil
				m.pendingRunes = 0
				return m, nil
			}
			m.pending = append(m.pending, value)
			m.pendingRunes += size
			return m, nil
		}
		return m.applyActive(value)
	default:
		if m.active == nil {
			return m, nil
		}
		return m.applyActive(value)
	}
}

func promptKeySize(key tea.KeyMsg) int {
	if len(key.Runes) > 0 {
		return len(key.Runes)
	}
	return 1
}

func (m promptSession) applyActive(message tea.Msg) (promptSession, tea.Cmd) {
	next, command := m.active.Update(message)
	m.active = next
	var result *promptResult
	switch prompt := next.(type) {
	case selectPrompt:
		if prompt.cancelled {
			result = &promptResult{err: context.Canceled}
		} else if prompt.back {
			result = &promptResult{values: prompt.draftValues(), err: errPromptBack}
		} else if prompt.accepted {
			result = &promptResult{values: prompt.values()}
		}
	case inputPrompt:
		if prompt.cancelled {
			result = &promptResult{err: context.Canceled}
		} else if prompt.back {
			result = &promptResult{input: string(prompt.value), err: errPromptBack}
		} else if prompt.accepted {
			result = &promptResult{input: prompt.acceptedValue}
		}
	}
	if result != nil {
		m.response <- *result
		m.active, m.response = nil, nil
		command = nil
	}
	return m, command
}

func (m promptSession) View() string {
	if m.active == nil {
		return ""
	}
	return m.active.View()
}

func (p *terminalPrompts) start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("interactive prompts are closed")
	}
	if p.program != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	input := p.input
	file, terminalInput := input.(*os.File)
	if !terminalInput || !term.IsTerminal(int(file.Fd())) {
		input = promptInput{reader: input, cancel: cancel}
	}
	p.program = tea.NewProgram(
		promptSession{},
		tea.WithContext(runCtx),
		tea.WithInput(input),
		tea.WithOutput(p.output),
		tea.WithoutSignalHandler(),
	)
	p.completion = newDirectoryCompletionService(newDirectoryCompletionResolver(), p.program.Send)
	p.done = make(chan struct{})
	go func() {
		_, err := p.program.Run()
		if errors.Is(err, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
			err = context.Canceled
		}
		p.mu.Lock()
		p.runErr = err
		p.mu.Unlock()
		cancel()
		close(p.done)
	}()
	return nil
}

func (p *terminalPrompts) request(ctx context.Context, model tea.Model) (promptResult, error) {
	p.mu.Lock()
	freshLine := p.freshLine
	p.mu.Unlock()
	if freshLine != nil {
		freshLine()
	}
	if err := p.start(ctx); err != nil {
		return promptResult{}, err
	}
	response := make(chan promptResult, 1)
	p.mu.Lock()
	program, done := p.program, p.done
	p.mu.Unlock()
	program.Send(promptRequest{model: model, response: response})
	select {
	case result := <-response:
		return result, result.err
	case <-done:
		p.mu.Lock()
		err := p.runErr
		p.mu.Unlock()
		if err == nil {
			err = context.Canceled
		}
		return promptResult{}, err
	case <-ctx.Done():
		return promptResult{}, ctx.Err()
	}
}

func (p *terminalPrompts) Close() error {
	p.mu.Lock()
	if p.closed {
		err := p.runErr
		p.mu.Unlock()
		return err
	}
	p.closed = true
	program, done, completion := p.program, p.done, p.completion
	p.mu.Unlock()
	completion.Close()
	if program == nil {
		return nil
	}
	program.Send(promptStop{})
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runErr
}

func (p *terminalPrompts) Select(ctx context.Context, title string, choices []selectionChoice, config promptConfig) (string, error) {
	config.presentation = p.presentation
	model, err := newSelectPrompt(title, choices, false, config)
	if err != nil {
		return "", err
	}
	result, err := p.request(ctx, model)
	if err != nil && !errors.Is(err, errPromptBack) {
		return "", err
	}
	value := ""
	if len(result.values) > 0 {
		value = result.values[0]
	}
	return value, err
}

func (p *terminalPrompts) MultiSelect(ctx context.Context, title string, choices []selectionChoice, config promptConfig) ([]string, error) {
	config.presentation = p.presentation
	model, err := newSelectPrompt(title, choices, true, config)
	if err != nil {
		return nil, err
	}
	result, err := p.request(ctx, model)
	if err != nil && !errors.Is(err, errPromptBack) {
		return nil, err
	}
	return result.values, err
}

func (p *terminalPrompts) Input(ctx context.Context, title string, config promptConfig) (string, error) {
	if err := p.start(ctx); err != nil {
		return "", err
	}
	p.mu.Lock()
	completion := p.completion
	p.mu.Unlock()
	config.presentation = p.presentation
	result, err := p.request(ctx, newInputPrompt(title, config, completion))
	if err != nil && !errors.Is(err, errPromptBack) {
		return "", err
	}
	return result.input, err
}

type selectPrompt struct {
	title          string
	choices        []selectionChoice
	cursor         int
	selected       map[int]bool
	multiple       bool
	cancelled      bool
	back           bool
	accepted       bool
	acceptedValues []string
	allowBack      bool
	message        string
	context        string
	width          int
	height         int
	presentation   humanPresentation
}

func newSelectPrompt(title string, choices []selectionChoice, multiple bool, config promptConfig) (selectPrompt, error) {
	if len(choices) == 0 {
		return selectPrompt{}, errors.New("interactive prompt has no choices")
	}
	model := selectPrompt{
		title: safeDisplayText(title), context: safeDisplayText(config.Context),
		choices:  append([]selectionChoice{}, choices...),
		selected: map[int]bool{}, multiple: multiple, allowBack: config.AllowBack,
		presentation: config.presentation,
	}
	for index := range model.choices {
		model.choices[index].Label = safeDisplayText(model.choices[index].Label)
		model.choices[index].Disabled = safeDisplayText(model.choices[index].Disabled)
	}
	model.cursor = model.nextEnabled(-1, 1)
	if model.cursor < 0 {
		return selectPrompt{}, errors.New("interactive prompt has no selectable choices")
	}
	for index, choice := range model.choices {
		if choice.Disabled == "" && choice.Value == config.Initial {
			model.cursor = index
			break
		}
	}
	for _, selected := range config.Selected {
		for index, choice := range model.choices {
			if choice.Disabled == "" && choice.Value == selected {
				model.selected[index] = true
				break
			}
		}
	}
	return model, nil
}

func (m selectPrompt) Init() tea.Cmd { return nil }

func (m selectPrompt) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.accepted || m.cancelled || m.back {
		return m, nil
	}
	if size, ok := message.(tea.WindowSizeMsg); ok {
		m.width, m.height = size.Width, size.Height
		return m, nil
	}
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "ctrl+d", "esc":
		m.cancelled = true
		return m, tea.Quit
	case "left":
		if !m.allowBack {
			m.message = "This is the first question."
			return m, nil
		}
		m.back = true
		return m, nil
	case "up", "k":
		if next := m.nextEnabled(m.cursor, -1); next >= 0 {
			m.cursor = next
		}
	case "down", "j":
		if next := m.nextEnabled(m.cursor, 1); next >= 0 {
			m.cursor = next
		}
	case " ":
		if m.multiple {
			m.selected[m.cursor] = !m.selected[m.cursor]
			m.message = ""
		}
	case "enter":
		if m.multiple && len(m.values()) == 0 {
			m.message = "Select at least one guide."
			return m, nil
		}
		if m.multiple {
			m.acceptedValues = m.values()
		} else {
			m.acceptedValues = []string{m.choices[m.cursor].Value}
		}
		m.accepted = true
	}
	return m, nil
}

func (m selectPrompt) nextEnabled(current, direction int) int {
	if direction == 0 {
		return -1
	}
	for step := 1; step <= len(m.choices); step++ {
		index := current + direction*step
		for index < 0 {
			index += len(m.choices)
		}
		index %= len(m.choices)
		if m.choices[index].Disabled == "" {
			return index
		}
	}
	return -1
}

func (m selectPrompt) values() []string {
	if m.accepted {
		return append([]string{}, m.acceptedValues...)
	}
	var values []string
	for index, choice := range m.choices {
		if m.selected[index] {
			values = append(values, choice.Value)
		}
	}
	return values
}

func (m selectPrompt) draftValues() []string {
	if m.multiple {
		return m.values()
	}
	return []string{m.choices[m.cursor].Value}
}

func (m selectPrompt) help() string {
	back := ""
	if m.allowBack {
		back = "  Left: back"
	}
	if m.multiple {
		return "Up/Down: move  Space: toggle  Enter: continue" + back + "  Esc/Ctrl-C: cancel"
	}
	return "Up/Down: move  Enter: choose" + back + "  Esc/Ctrl-C: cancel"
}

func (m selectPrompt) choiceLine(index int) string {
	choice := m.choices[index]
	if choice.Disabled != "" {
		return m.presentation.muted(fmt.Sprintf("  - %s (not selectable: %s)", choice.Label, choice.Disabled))
	}
	cursor := " "
	if index == m.cursor {
		cursor = ">"
	}
	mark := ""
	if m.multiple {
		mark = "[ ] "
		if m.selected[index] {
			mark = "[x] "
		}
	}
	line := fmt.Sprintf("%s %s%s", cursor, mark, choice.Label)
	if choice.Emphasis != "" {
		prefix := fmt.Sprintf("%s %s", cursor, mark)
		return prefix + m.presentation.emphasized(choice.Label, choice.Emphasis, index == m.cursor)
	}
	if index == m.cursor {
		return m.presentation.answer(line)
	}
	return line
}

func promptLines(text string, width int) int {
	lines := 0
	for _, line := range strings.Split(text, "\n") {
		if width <= 0 {
			lines++
		} else {
			lines += max(1, (ansi.StringWidth(line)+width-1)/width)
		}
	}
	return lines
}

func (m selectPrompt) visibleChoices() (int, int) {
	if m.height <= 0 {
		return 0, len(m.choices)
	}
	available := m.height - promptLines(m.title, m.width) - promptLines(m.help(), m.width) - 1
	if m.message != "" {
		available -= promptLines(m.message, m.width)
	}
	offsets := make([]int, len(m.choices)+1)
	for index := range m.choices {
		offsets[index+1] = offsets[index] + promptLines(m.choiceLine(index), m.width)
	}
	if offsets[len(m.choices)] <= available {
		return 0, len(m.choices)
	}
	fits := func(start, end int) bool {
		rows := offsets[end] - offsets[start]
		if start > 0 {
			rows += promptLines(fmt.Sprintf("  ... %d earlier choices ...", start), m.width)
		}
		if end < len(m.choices) {
			rows += promptLines(fmt.Sprintf("  ... %d later choices ...", len(m.choices)-end), m.width)
		}
		return rows <= available
	}
	start, end := m.cursor, m.cursor+1
	above := true
	for {
		if above && start > 0 && fits(start-1, end) {
			start--
		} else if end < len(m.choices) && fits(start, end+1) {
			end++
		} else if start > 0 && fits(start-1, end) {
			start--
		} else {
			return start, end
		}
		above = !above
	}
}

func (m selectPrompt) View() string {
	var output strings.Builder
	if m.context != "" {
		fmt.Fprintln(&output, m.presentation.notice(m.context))
	}
	fmt.Fprintln(&output, m.presentation.question("? "+m.title))
	start, end := m.visibleChoices()
	if start > 0 {
		fmt.Fprintf(&output, "  ... %d earlier choices ...\n", start)
	}
	for index := start; index < end; index++ {
		fmt.Fprintln(&output, m.choiceLine(index))
	}
	if end < len(m.choices) {
		fmt.Fprintf(&output, "  ... %d later choices ...\n", len(m.choices)-end)
	}
	if m.message != "" {
		fmt.Fprintln(&output, m.presentation.warning(m.message))
	}
	help := m.help()
	if m.width > 0 {
		help = ansi.Wordwrap(help, m.width, " ")
	}
	fmt.Fprintln(&output, m.presentation.mutedBlock(help))
	return output.String()
}

type inputPrompt struct {
	title                  string
	value                  []rune
	cancelled              bool
	back                   bool
	accepted               bool
	acceptedValue          string
	allowBack              bool
	message                string
	width                  int
	height                 int
	presentation           humanPresentation
	completion             *directoryCompletionService
	completionID           uint64
	generation             uint64
	suggestions            []directorySuggestion
	completionBase         string
	completionIndex        int
	menuActive             bool
	commonPrefixPending    bool
	commonPrefixGeneration uint64
	loading                bool
	truncated              bool
	omitted                bool
}

func newInputPrompt(
	title string,
	config promptConfig,
	completion ...*directoryCompletionService,
) inputPrompt {
	model := inputPrompt{
		title: safeDisplayText(title), value: []rune(config.Initial),
		allowBack: config.AllowBack, presentation: config.presentation,
		completionIndex: -1,
	}
	if len(completion) > 0 {
		model.completion = completion[0]
		model.completionID = model.completion.promptID()
		model.loading = model.completion != nil && len(model.value) > 0
	}
	return model
}

func (m inputPrompt) Init() tea.Cmd {
	if m.completion == nil || len(m.value) == 0 {
		return nil
	}
	return m.completionCommand()
}

func (m inputPrompt) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.accepted || m.cancelled || m.back {
		return m, nil
	}
	if size, ok := message.(tea.WindowSizeMsg); ok {
		m.width, m.height = size.Width, size.Height
		return m, nil
	}
	if result, ok := message.(directoryCompletionResult); ok {
		if result.promptID != m.completionID || result.generation != m.generation ||
			result.value != string(m.value) {
			return m, nil
		}
		m.loading = false
		m.suggestions = append([]directorySuggestion{}, result.suggestions...)
		m.completionBase = result.value
		m.completionIndex = -1
		m.menuActive = len(m.suggestions) > 0
		m.truncated, m.omitted = result.truncated, result.omitted
		m.message = result.err
		if m.commonPrefixPending && m.commonPrefixGeneration == result.generation {
			m.commonPrefixPending = false
			if prefix := commonCompletionPrefix(m.suggestions); len(prefix) > len(result.value) {
				m.value = []rune(prefix)
				m.completionBase = prefix
				if len(m.suggestions) == 1 && !result.truncated {
					m.suggestions = nil
					m.completionIndex = -1
					m.menuActive = false
				}
			}
		}
		return m, nil
	}
	key, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c", "ctrl+d", "esc":
		m.cancelled = true
		return m, tea.Quit
	case "left":
		if !m.allowBack {
			m.message = "This is the first question."
			return m, nil
		}
		m.back = true
		return m, nil
	case "enter":
		if m.menuActive {
			m.menuActive = false
			m.suggestions = nil
			m.completionIndex = -1
			m.commonPrefixPending = false
			return m, nil
		}
		m.accepted = true
		m.acceptedValue = string(m.value)
	case "up":
		m = m.cycleCompletion(-1)
	case "down":
		m = m.cycleCompletion(1)
	case "tab":
		if m.menuActive {
			m = m.cycleCompletion(1)
			return m, nil
		}
		return m.restartCompletion(true)
	case "/":
		if !strings.HasSuffix(string(m.value), string(os.PathSeparator)) {
			m.value = append(m.value, os.PathSeparator)
		}
		return m.restartCompletion(false)
	case "backspace", "ctrl+h":
		if len(m.value) > 0 {
			m.value = m.value[:len(m.value)-1]
			return m.changed()
		}
	case " ":
		m.value = append(m.value, ' ')
		return m.changed()
	default:
		if key.Type == tea.KeyRunes {
			changed := false
			for _, value := range key.Runes {
				if value >= ' ' && value != utf8.RuneError {
					m.value = append(m.value, value)
					changed = true
				}
			}
			if changed {
				return m.changed()
			}
		}
	}
	return m, nil
}

func (m inputPrompt) changed() (tea.Model, tea.Cmd) {
	return m.restartCompletion(false)
}

func (m inputPrompt) restartCompletion(commonPrefix bool) (tea.Model, tea.Cmd) {
	m.generation++
	m.suggestions = nil
	m.completionBase = string(m.value)
	m.completionIndex = -1
	m.menuActive = false
	m.commonPrefixPending = commonPrefix
	m.commonPrefixGeneration = m.generation
	m.loading = m.completion != nil
	m.truncated, m.omitted = false, false
	m.message = ""
	m.enqueueCompletion()
	return m, nil
}

func (m inputPrompt) cycleCompletion(direction int) inputPrompt {
	if !m.menuActive || len(m.suggestions) == 0 {
		return m
	}
	if m.completionIndex < 0 {
		if direction < 0 {
			m.completionIndex = len(m.suggestions) - 1
		} else {
			m.completionIndex = 0
		}
	} else {
		next := m.completionIndex + direction
		if next < 0 || next >= len(m.suggestions) {
			m.completionIndex = -1
			m.value = []rune(m.completionBase)
			return m
		}
		m.completionIndex = next
	}
	m.value = []rune(m.suggestions[m.completionIndex].value)
	return m
}

func commonCompletionPrefix(suggestions []directorySuggestion) string {
	if len(suggestions) == 0 {
		return ""
	}
	prefix := []rune(suggestions[0].value)
	for _, suggestion := range suggestions[1:] {
		value := []rune(suggestion.value)
		length := min(len(prefix), len(value))
		index := 0
		for index < length && prefix[index] == value[index] {
			index++
		}
		prefix = prefix[:index]
		if len(prefix) == 0 {
			break
		}
	}
	return string(prefix)
}

func (m inputPrompt) completionCommand() tea.Cmd {
	if m.completion == nil {
		return nil
	}
	return m.completion.command(directoryCompletionRequest{
		promptID: m.completionID, generation: m.generation, value: string(m.value),
	})
}

func (m inputPrompt) enqueueCompletion() {
	if m.completion != nil {
		m.completion.enqueue(directoryCompletionRequest{
			promptID: m.completionID, generation: m.generation, value: string(m.value),
		})
	}
}

func (m inputPrompt) suggestionLabel(index int) string {
	label := m.suggestions[index].label
	if label == "" {
		label = m.suggestions[index].value
	}
	return safeDisplayText(label)
}

func (m inputPrompt) statusLines() []string {
	var lines []string
	switch {
	case m.message != "":
		lines = append(lines, safeDisplayText(m.message))
	case len(m.suggestions) == 0 && m.truncated:
		lines = append(lines, "Directory scan limited; type more to narrow the path.")
	}
	if m.truncated && len(m.suggestions) > 0 {
		lines = append(lines, "Results limited; type more to narrow the path.")
	}
	if m.omitted {
		lines = append(lines, "Some directory names cannot be entered safely and were omitted.")
	}
	return lines
}

type completionGrid struct {
	start, end int
	columns    int
	rows       int
	cellWidth  int
}

func (m inputPrompt) completionGrid() completionGrid {
	if !m.menuActive || len(m.suggestions) == 0 {
		return completionGrid{}
	}
	cellWidth := 4
	for index := range m.suggestions {
		cellWidth = max(cellWidth, ansi.StringWidth(m.suggestionLabel(index))+3)
	}
	width := m.width
	if width <= 0 {
		width = 80
	}
	cellWidth = min(cellWidth, width)
	if cellWidth > 30 {
		if divisor := cellWidth / 30; divisor > 1 {
			cellWidth /= divisor
		}
	}
	maxColumns := max(1, width/cellWidth)
	availableRows := 1_000_000
	if m.height > 0 {
		availableRows = m.height - promptLines(m.inlineLine(), m.width) - promptLines(m.help(), m.width)
		for _, line := range m.statusLines() {
			availableRows -= promptLines(line, m.width)
		}
		availableRows = max(1, availableRows)
	}
	targetRows := min(3, len(m.suggestions))
	columns := min(maxColumns, (len(m.suggestions)+targetRows-1)/targetRows)
	rows := (len(m.suggestions) + columns - 1) / columns
	if rows > availableRows {
		neededColumns := (len(m.suggestions) + availableRows - 1) / availableRows
		columns = min(maxColumns, max(columns, neededColumns))
		rows = (len(m.suggestions) + columns - 1) / columns
	}
	visibleRows := min(rows, availableRows)
	capacity := visibleRows * columns
	if len(m.suggestions) <= capacity {
		return completionGrid{
			start: 0, end: len(m.suggestions), columns: columns,
			rows: visibleRows, cellWidth: cellWidth,
		}
	}
	paginationRows := promptLines(fmt.Sprintf(
		"Showing %d-%d of %d directories; type more to narrow.",
		len(m.suggestions), len(m.suggestions), len(m.suggestions),
	), m.width)
	availableRows = max(1, availableRows-paginationRows)
	visibleRows = min(rows, availableRows)
	capacity = max(1, visibleRows*columns)
	selected := max(0, m.completionIndex)
	start := selected / capacity * capacity
	end := min(len(m.suggestions), start+capacity)
	return completionGrid{
		start: start, end: end, columns: columns,
		rows: min(visibleRows, end-start), cellWidth: cellWidth,
	}
}

func (m inputPrompt) inputValueDisplay() string {
	value := safeDisplayText(string(m.value))
	if value == "" {
		return "_"
	}
	limit := 40
	if m.width > 0 {
		limit = max(12, m.width/2)
	}
	if ansi.StringWidth(value) <= limit {
		return value
	}
	return ansi.TruncateLeft(value, ansi.StringWidth(value)-limit+3, "...")
}

func (m inputPrompt) inlineLine() string {
	line := m.presentation.question("? "+m.title+" ") +
		m.presentation.answer(m.inputValueDisplay())
	if m.width > 0 {
		return ansi.Wordwrap(line, m.width, " ")
	}
	return line
}

func (m inputPrompt) help() string {
	back := ""
	if m.allowBack {
		back = "  Left: back"
	}
	return "Up/Down/Tab: complete  Enter: close menu / submit" + back + "  Esc: cancel"
}

func (m inputPrompt) View() string {
	var output strings.Builder
	fmt.Fprintln(&output, m.inlineLine())
	grid := m.completionGrid()
	for row := 0; row < grid.rows; row++ {
		for column := 0; column < grid.columns; column++ {
			index := grid.start + column*grid.rows + row
			if index >= grid.end {
				break
			}
			prefix := "  "
			selected := index == m.completionIndex
			if selected {
				prefix = "> "
			}
			label := ansi.Truncate(m.suggestionLabel(index), max(1, grid.cellWidth-2), "...")
			cell := prefix + label
			padding := max(0, grid.cellWidth-ansi.StringWidth(cell))
			cell += strings.Repeat(" ", padding)
			output.WriteString(m.presentation.completion(cell, selected))
		}
		output.WriteByte('\n')
	}
	if grid.end > 0 && (grid.start > 0 || grid.end < len(m.suggestions)) {
		fmt.Fprintf(&output, "%s\n", m.presentation.muted(fmt.Sprintf(
			"Showing %d-%d of %d directories; type more to narrow.",
			grid.start+1, grid.end, len(m.suggestions),
		)))
	}
	for _, line := range m.statusLines() {
		if m.message != "" {
			fmt.Fprintln(&output, m.presentation.failure(line))
		} else {
			fmt.Fprintln(&output, m.presentation.warning(line))
		}
	}
	help := m.help()
	if m.width > 0 {
		help = ansi.Wordwrap(help, m.width, " ")
	}
	fmt.Fprintln(&output, m.presentation.mutedBlock(help))
	return output.String()
}
