package cli

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

type humanPresentation struct {
	enabled            bool
	questionStyle      lipgloss.Style
	answerStyle        lipgloss.Style
	headingStyle       lipgloss.Style
	mutedStyle         lipgloss.Style
	noticeStyle        lipgloss.Style
	warningStyle       lipgloss.Style
	errorStyle         lipgloss.Style
	successStyle       lipgloss.Style
	phaseStyle         lipgloss.Style
	completionStyle    lipgloss.Style
	completionSelected lipgloss.Style
	emphasisStyle      lipgloss.Style
}

func newHumanPresentation(writer io.Writer) humanPresentation {
	file, ok := writer.(*os.File)
	terminal := ok && term.IsTerminal(int(file.Fd()))
	_, noColor := os.LookupEnv("NO_COLOR")
	return makeHumanPresentation(writer, colorsAllowed(terminal, noColor, os.Getenv("TERM")))
}

func colorsAllowed(terminal, noColor bool, terminalName string) bool {
	return terminal && !noColor && !strings.EqualFold(strings.TrimSpace(terminalName), "dumb")
}

func newHumanPresentationWith(writer io.Writer, enabled bool) humanPresentation {
	return makeHumanPresentation(writer, enabled)
}

func makeHumanPresentation(writer io.Writer, enabled bool) humanPresentation {
	presentation := humanPresentation{enabled: enabled}
	if !enabled {
		return presentation
	}
	renderer := lipgloss.NewRenderer(writer)
	renderer.SetColorProfile(presentationColorProfile(os.Getenv("TERM"), os.Getenv("COLORTERM")))
	presentation.questionStyle = renderer.NewStyle().
		Foreground(lipgloss.Color("#5F819D")).Bold(true)
	presentation.answerStyle = renderer.NewStyle().
		Foreground(lipgloss.Color("#FF9D00")).Bold(true)
	presentation.headingStyle = renderer.NewStyle().Bold(true)
	presentation.mutedStyle = renderer.NewStyle().Foreground(lipgloss.Color("#808080"))
	presentation.noticeStyle = renderer.NewStyle().Foreground(lipgloss.Color("#5F819D"))
	presentation.warningStyle = renderer.NewStyle().Foreground(lipgloss.Color("#FFFF00"))
	presentation.errorStyle = renderer.NewStyle().Foreground(lipgloss.Color("#FF0000")).Bold(true)
	presentation.successStyle = renderer.NewStyle().Foreground(lipgloss.Color("#00AF00"))
	presentation.phaseStyle = renderer.NewStyle().Foreground(lipgloss.Color("#5F819D"))
	presentation.completionStyle = renderer.NewStyle().
		Foreground(lipgloss.Color("#000000")).Background(lipgloss.Color("#BBBBBB"))
	presentation.completionSelected = renderer.NewStyle().
		Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#888888")).Bold(true)
	presentation.emphasisStyle = renderer.NewStyle().
		Foreground(lipgloss.Color("#FFFFFF")).Bold(true)
	return presentation
}

func (p humanPresentation) emphasized(value, emphasis string, active bool) string {
	value = safeDisplayText(value)
	emphasis = safeDisplayText(emphasis)
	index := strings.Index(value, emphasis)
	if !p.enabled || emphasis == "" || index < 0 {
		if active {
			return p.answer(value)
		}
		return value
	}
	before, after := value[:index], value[index+len(emphasis):]
	if active {
		return p.answer(before) + p.emphasisStyle.Render(emphasis) + p.answer(after)
	}
	return before + p.emphasisStyle.Render(emphasis) + after
}

func presentationColorProfile(terminalName, colorTerminal string) termenv.Profile {
	terminalName = strings.ToLower(terminalName)
	colorTerminal = strings.ToLower(colorTerminal)
	switch {
	case strings.Contains(colorTerminal, "truecolor"), strings.Contains(colorTerminal, "24bit"):
		return termenv.TrueColor
	case strings.Contains(terminalName, "256color"):
		return termenv.ANSI256
	default:
		return termenv.ANSI
	}
}

func (p humanPresentation) render(style lipgloss.Style, value string) string {
	value = safeDisplayText(value)
	if !p.enabled {
		return value
	}
	return style.Render(value)
}

func (p humanPresentation) question(value string) string {
	return p.render(p.questionStyle, value)
}

func (p humanPresentation) plain(value string) string {
	return safeDisplayText(value)
}

func (p humanPresentation) answer(value string) string {
	return p.render(p.answerStyle, value)
}

func (p humanPresentation) heading(value string) string {
	return p.render(p.headingStyle, value)
}

func (p humanPresentation) muted(value string) string {
	return p.render(p.mutedStyle, value)
}

func (p humanPresentation) mutedBlock(value string) string {
	lines := strings.Split(value, "\n")
	for index := range lines {
		lines[index] = p.muted(lines[index])
	}
	return strings.Join(lines, "\n")
}

func (p humanPresentation) warning(value string) string {
	return p.render(p.warningStyle, value)
}

func (p humanPresentation) notice(value string) string {
	return p.render(p.noticeStyle, value)
}

func (p humanPresentation) failure(value string) string {
	return p.render(p.errorStyle, value)
}

func (p humanPresentation) success(value string) string {
	return p.render(p.successStyle, value)
}

func (p humanPresentation) phase(value string) string {
	return p.render(p.phaseStyle, value)
}

func (p humanPresentation) completion(value string, selected bool) string {
	if selected {
		return p.render(p.completionSelected, value)
	}
	return p.render(p.completionStyle, value)
}
