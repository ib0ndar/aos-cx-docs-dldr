package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/source"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

const (
	progressRepaintInterval = 100 * time.Millisecond
	progressAnimationTick   = 125 * time.Millisecond
	progressFallbackWidth   = 80
)

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(body []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(body)
}

type progressTerminal struct {
	enabled bool
	width   func() int
}

func inspectProgressTerminal(writer io.Writer) progressTerminal {
	file, ok := writer.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) || !progressAnimationAllowed(os.Getenv("TERM")) {
		return progressTerminal{}
	}
	return progressTerminal{
		enabled: true,
		width: func() int {
			width, _, err := term.GetSize(int(file.Fd()))
			if err != nil || width <= 1 {
				return progressFallbackWidth
			}
			return width
		},
	}
}

func progressAnimationAllowed(termName string) bool {
	return !strings.EqualFold(strings.TrimSpace(termName), "dumb")
}

type progressReporter struct {
	writer             io.Writer
	terminal           progressTerminal
	started            time.Time
	now                func() time.Time
	mu                 sync.Mutex
	lineActive         bool
	lastGuide          int
	lastTopics         int
	lastAssets         int
	lastCompleted      int
	lastTotal          int
	lastPlainCompleted int
	lastEmit           time.Time
	lastPaint          time.Time
	lastWidth          int
	lastStage          archive.ProgressStage
	topicCompleteShown bool
	stageCompleteShown bool
	finalShown         bool
	presentation       humanPresentation
	animationCancel    context.CancelFunc
	animationDone      chan struct{}
	batchStarted       time.Time
	batchStage         string
	batchCompleted     int
	batchTotal         int
	batchFrame         int
	batchLastPlain     int
}

func newProgressReporter(writer io.Writer, interactive bool, styles ...humanPresentation) *progressReporter {
	terminal := progressTerminal{enabled: interactive}
	if interactive {
		terminal.width = func() int { return progressFallbackWidth }
	}
	return newProgressReporterWithTerminal(writer, terminal, styles...)
}

func newProgressReporterWithTerminal(
	writer io.Writer,
	terminal progressTerminal,
	styles ...humanPresentation,
) *progressReporter {
	now := time.Now()
	presentation := newHumanPresentation(writer)
	if len(styles) > 0 {
		presentation = styles[0]
	}
	return &progressReporter{
		writer: writer, terminal: terminal, started: now, now: time.Now,
		lastEmit: now, presentation: presentation,
	}
}

func (p *progressReporter) elapsedAt(now time.Time) time.Duration {
	elapsed := now.Sub(p.started)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Round(time.Second)
}

func (p *progressReporter) elapsed() time.Duration {
	return p.elapsedAt(p.now())
}

func (p *progressReporter) batchElapsedAt(now time.Time) time.Duration {
	started := p.batchStarted
	if started.IsZero() {
		started = p.started
	}
	elapsed := now.Sub(started)
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Round(time.Second)
}

func (p *progressReporter) widthLocked() int {
	width := progressFallbackWidth
	if p.terminal.width != nil {
		width = p.terminal.width()
	}
	return max(1, width-1)
}

func (p *progressReporter) clearLocked() {
	if p.terminal.enabled && p.lineActive {
		fmt.Fprint(p.writer, "\r\x1b[2K")
		p.lineActive = false
	}
}

func (p *progressReporter) persistentLocked(value string) {
	if p.terminal.enabled {
		fmt.Fprintf(p.writer, "\r%s\r\n", value)
		return
	}
	fmt.Fprintln(p.writer, value)
}

func (p *progressReporter) Close() {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
}

func (p *progressReporter) Clear() {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
}

func (p *progressReporter) Message(message string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.persistentLocked(safeDisplayText(message))
}

func (p *progressReporter) Warning(message string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.persistentLocked(p.presentation.warning(message))
}

func (p *progressReporter) Notice(message string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.persistentLocked(p.presentation.notice(message))
}

func (p *progressReporter) Error(message string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.persistentLocked(p.presentation.failure(message))
}

func (p *progressReporter) Phase(message string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	p.persistentLocked(p.presentation.phase(fmt.Sprintf("%s (elapsed %s)", message, p.elapsed())))
}

func (p *progressReporter) Guide(number, total int, title, phase string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.terminal.enabled {
		fmt.Fprintln(p.writer, p.presentation.phase(fmt.Sprintf(
			"[%d/%d] %s: %s (elapsed %s)", number, total,
			safeDisplayText(title), safeDisplayText(phase), p.elapsed(),
		)))
		return
	}
	p.drawLiveLocked(progressStageLine(
		p.widthLocked(), number, total, title, phase, p.elapsed(),
	))
}

func (p *progressReporter) GuideResult(number, total int, title, status string, topics, assets, placeholders int) {
	p.stopAnimation()
	if status == "" {
		status = "failed"
	}
	message := fmt.Sprintf("[%d/%d] %s: %s (%d topics, %d real assets", number, total, title, status, topics, assets)
	if placeholders > 0 {
		label := "image placeholders"
		if placeholders == 1 {
			label = "image placeholder"
		}
		message += fmt.Sprintf(", %d %s", placeholders, label)
	}
	message += ")"
	switch status {
	case "complete":
		p.mu.Lock()
		defer p.mu.Unlock()
		p.clearLocked()
		fmt.Fprintln(p.writer, p.presentation.success(message))
	case "degraded", "incomplete":
		p.Warning(message)
	default:
		p.Error(message)
	}
}

func (p *progressReporter) drawLiveLocked(line string) {
	width := p.widthLocked()
	line = ansi.Truncate(safeDisplayText(line), width, "...")
	line = p.presentation.phase(line)
	fmt.Fprintf(p.writer, "\r\x1b[2K%s", line)
	p.lineActive = true
	p.lastWidth = width
}

func (p *progressReporter) HTML(number, total int, title string, event archive.Progress) {
	if event.Stage == archive.ProgressManifest {
		p.startArchiveIndeterminate(number, total, title)
		return
	}
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.lastGuide != number {
		p.lastGuide, p.lastTopics, p.lastAssets = number, 0, 0
		p.lastCompleted, p.lastTotal, p.lastPlainCompleted = 0, 0, 0
		p.lastEmit, p.lastPaint = now, time.Time{}
		p.lastStage, p.topicCompleteShown = "", false
		p.stageCompleteShown, p.finalShown = false, false
	}
	stage := archiveProgressStage(event, p.topicCompleteShown)
	completed, workTotal := archiveProgressCounts(event, stage)
	if stage != p.lastStage {
		p.lastCompleted, p.lastTotal, p.lastPlainCompleted = 0, workTotal, 0
		p.stageCompleteShown = false
	}
	if !p.terminal.enabled {
		p.emitPlainHTMLLocked(number, total, title, event, stage, now)
		return
	}
	width := p.widthLocked()
	atCompletion := workTotal > 0 && completed >= workTotal
	force := p.lastPaint.IsZero() || stage != p.lastStage || width != p.lastWidth ||
		atCompletion && !p.stageCompleteShown || event.Final && !p.finalShown
	if !force && now.Sub(p.lastPaint) < progressRepaintInterval {
		p.lastTopics, p.lastAssets = event.TopicsValidated, event.AssetsCompleted
		p.lastCompleted, p.lastTotal = completed, workTotal
		return
	}
	var line string
	switch stage {
	case archive.ProgressTopics:
		line = progressTopicLine(
			width, number, total, title, event.TopicsValidated,
			event.PlannedTopics, p.elapsedAt(now),
		)
		if atCompletion {
			p.topicCompleteShown = true
		}
	case archive.ProgressBuilding:
		line = progressArchiveWorkLine(
			width, number, total, title, "Building offline guide", "Build",
			event.PagesEmitted, event.PagesTotal, event.AssetsCompleted,
			event.AssetsDiscovered, true, p.elapsedAt(now),
		)
	case archive.ProgressHashing:
		line = progressArchiveWorkLine(
			width, number, total, title, "Hashing generated files", "Hash",
			event.FilesHashed, event.FilesTotal, 0, 0, false, p.elapsedAt(now),
		)
	case archive.ProgressReferences:
		line = progressArchiveWorkLine(
			width, number, total, title, "Checking offline references", "Check",
			event.ReferenceFilesChecked, event.ReferenceFilesTotal,
			0, 0, false, p.elapsedAt(now),
		)
	case archive.ProgressFinal:
		line = progressStageLine(width, number, total, title, "offline checks finished", p.elapsedAt(now))
		p.finalShown = true
	default:
		line = progressStageLine(width, number, total, title, string(stage), p.elapsedAt(now))
	}
	p.drawLiveLocked(line)
	if atCompletion {
		p.stageCompleteShown = true
	}
	p.lastPaint, p.lastStage = now, stage
	p.lastTopics, p.lastAssets = event.TopicsValidated, event.AssetsCompleted
	p.lastCompleted, p.lastTotal = completed, workTotal
}

func archiveProgressStage(event archive.Progress, topicCompleteShown bool) archive.ProgressStage {
	if event.Stage != "" {
		return event.Stage
	}
	switch {
	case event.Final:
		return archive.ProgressFinal
	case event.PlannedTopics > 0 &&
		(event.TopicsValidated < event.PlannedTopics || !topicCompleteShown):
		return archive.ProgressTopics
	default:
		return archive.ProgressBuilding
	}
}

func archiveProgressCounts(event archive.Progress, stage archive.ProgressStage) (int, int) {
	switch stage {
	case archive.ProgressTopics:
		return event.TopicsValidated, event.PlannedTopics
	case archive.ProgressBuilding:
		return event.PagesEmitted, event.PagesTotal
	case archive.ProgressHashing:
		return event.FilesHashed, event.FilesTotal
	case archive.ProgressReferences:
		return event.ReferenceFilesChecked, event.ReferenceFilesTotal
	default:
		return 0, 0
	}
}

func (p *progressReporter) startArchiveIndeterminate(number, total int, title string) {
	p.stopAnimation()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	now := p.now()
	if p.lastGuide != number {
		p.lastGuide = number
	}
	p.animationCancel, p.animationDone = cancel, done
	p.lastStage = archive.ProgressManifest
	p.stageCompleteShown = false
	label := fmt.Sprintf("Writing and syncing manifest [%d/%d] %s",
		number, total, safeDisplayText(title))
	if p.terminal.enabled {
		p.drawLiveLocked(progressIndeterminateLine(
			p.widthLocked(), label, 0, p.elapsedAt(now),
		))
		p.lastPaint = now
	} else {
		fmt.Fprintln(p.writer, p.presentation.phase(fmt.Sprintf(
			"[%d/%d] %s: Writing and syncing manifest; elapsed %s",
			number, total, safeDisplayText(title), p.elapsedAt(now),
		)))
	}
	p.mu.Unlock()
	if !p.terminal.enabled {
		close(done)
		p.mu.Lock()
		if p.animationDone == done {
			p.animationCancel, p.animationDone = nil, nil
		}
		p.mu.Unlock()
		return
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(progressAnimationTick)
		defer ticker.Stop()
		frame := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.mu.Lock()
				if p.animationDone != done {
					p.mu.Unlock()
					return
				}
				frame++
				now := p.now()
				p.drawLiveLocked(progressIndeterminateLine(
					p.widthLocked(), label, frame, p.elapsedAt(now),
				))
				p.lastPaint = now
				p.mu.Unlock()
			}
		}
	}()
}

func (p *progressReporter) Catalog(event source.CatalogProgress) {
	switch event.Stage {
	case source.CatalogPortalStarted:
		p.startIndeterminate("Refreshing Product Documentation catalogue")
	case source.CatalogPortalReady:
		p.stopAnimation()
		p.drawDeterminate("catalog-mappings", "Refreshing guide mappings", event.Completed, event.Total)
	case source.CatalogMappingCompleted:
		p.drawDeterminate("catalog-mappings", "Refreshing guide mappings", event.Completed, event.Total)
	}
}

func (p *progressReporter) Availability(event availabilityProgress) {
	p.drawDeterminate("guide-availability", "Checking guide availability", event.Completed, event.Total)
}

func (p *progressReporter) startIndeterminate(label string) {
	p.stopAnimation()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.mu.Lock()
	now := p.now()
	p.animationCancel, p.animationDone = cancel, done
	p.batchStarted, p.batchStage = now, "catalog-portal"
	p.batchCompleted, p.batchTotal, p.batchFrame, p.batchLastPlain = 0, 0, 0, 0
	if p.terminal.enabled {
		p.drawLiveLocked(progressIndeterminateLine(
			p.widthLocked(), label, p.batchFrame, p.batchElapsedAt(now),
		))
		p.lastPaint = now
	} else {
		fmt.Fprintln(p.writer, p.presentation.phase(fmt.Sprintf("%s; elapsed %s", label, p.batchElapsedAt(now))))
	}
	p.mu.Unlock()
	if !p.terminal.enabled {
		close(done)
		p.mu.Lock()
		if p.animationDone == done {
			p.animationCancel, p.animationDone = nil, nil
		}
		p.mu.Unlock()
		return
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(progressAnimationTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.mu.Lock()
				if p.animationDone != done {
					p.mu.Unlock()
					return
				}
				now := p.now()
				p.batchFrame++
				p.drawLiveLocked(progressIndeterminateLine(
					p.widthLocked(), label, p.batchFrame, p.batchElapsedAt(now),
				))
				p.lastPaint = now
				p.mu.Unlock()
			}
		}
	}()
}

func (p *progressReporter) stopAnimation() {
	p.mu.Lock()
	cancel, done := p.animationCancel, p.animationDone
	p.animationCancel, p.animationDone = nil, nil
	p.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
}

func (p *progressReporter) drawDeterminate(stage, label string, completed, total int) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if stage != p.batchStage || total != p.batchTotal {
		p.batchStarted, p.batchStage = now, stage
		p.batchCompleted, p.batchTotal, p.batchLastPlain = 0, total, 0
		p.lastPaint = time.Time{}
	}
	if completed < p.batchCompleted {
		return
	}
	completed = min(max(0, completed), max(0, total))
	if !p.terminal.enabled {
		step := max(1, total/10)
		emit := completed == 0 && p.batchLastPlain == 0 ||
			total > 0 && completed == total ||
			completed > p.batchLastPlain && completed-p.batchLastPlain >= step
		if !emit {
			p.batchCompleted = completed
			return
		}
		fmt.Fprintln(p.writer, p.presentation.phase(progressPlainDeterminateLine(
			label, completed, total, p.batchElapsedAt(now),
		)))
		p.batchCompleted, p.batchLastPlain = completed, completed
		p.lastEmit = now
		return
	}
	force := p.lastPaint.IsZero() || p.widthLocked() != p.lastWidth ||
		total > 0 && completed == total
	p.batchCompleted = completed
	if !force && now.Sub(p.lastPaint) < progressRepaintInterval {
		return
	}
	p.drawLiveLocked(progressDeterminateLine(
		p.widthLocked(), label, completed, total, p.batchElapsedAt(now),
	))
	p.lastPaint = now
}

func progressIndeterminateLine(width int, label string, frame int, elapsed time.Duration) string {
	tail := " elapsed " + elapsed.String()
	return progressCompactLine(width, label, tail, func(barWidth int) string {
		return indeterminateProgressBar(barWidth, frame)
	})
}

func progressDeterminateLine(width int, label string, completed, total int, elapsed time.Duration) string {
	percent := progressPercent(completed, total)
	tail := fmt.Sprintf(" %d/%d (%d%%) elapsed %s", completed, total, percent, elapsed)
	if total <= 0 {
		tail = fmt.Sprintf(" %d/%d elapsed %s", completed, total, elapsed)
	}
	return progressCompactLine(width, label, tail, func(barWidth int) string {
		return determinateProgressBar(barWidth, completed, total)
	})
}

func progressPlainDeterminateLine(label string, completed, total int, elapsed time.Duration) string {
	if total <= 0 {
		return fmt.Sprintf("%s %d/%d; elapsed %s", label, completed, total, elapsed)
	}
	return fmt.Sprintf(
		"%s %d/%d (%d%%); elapsed %s",
		label, completed, total, progressPercent(completed, total), elapsed,
	)
}

func progressCompactLine(width int, label, tail string, bar func(int) string) string {
	width = max(1, width)
	label = safeDisplayText(label)
	tailWidth := ansi.StringWidth(tail)
	if width <= tailWidth+3 {
		return ansi.Truncate(strings.TrimSpace(tail), width, "...")
	}
	available := width - tailWidth - 3
	labelWidth := ansi.StringWidth(label)
	barWidth := 1
	if available-labelWidth >= 3 {
		barWidth = min(24, available-labelWidth)
	} else {
		barWidth = min(6, max(1, available-1))
		labelWidth = max(1, available-barWidth)
	}
	label = ansi.Truncate(label, labelWidth, "...")
	return label + " [" + bar(barWidth) + "]" + tail
}

func determinateProgressBar(width, completed, total int) string {
	width = max(1, width)
	if total > 0 && completed >= total {
		return strings.Repeat("=", width)
	}
	if total <= 0 || completed <= 0 {
		return ">" + strings.Repeat("-", width-1)
	}
	filled := min(width-1, max(0, completed*(width-1)/total))
	return strings.Repeat("=", filled) + ">" + strings.Repeat("-", width-filled-1)
}

func indeterminateProgressBar(width, frame int) string {
	width = max(1, width)
	if width == 1 {
		return ">"
	}
	segment := min(4, width-1)
	maximum := width - segment - 1
	position := 0
	if maximum > 0 {
		cycle := maximum * 2
		position = frame % cycle
		if position > maximum {
			position = cycle - position
		}
	}
	return strings.Repeat("-", position) +
		strings.Repeat("=", segment) + ">" +
		strings.Repeat("-", width-position-segment-1)
}

func (p *progressReporter) emitPlainHTMLLocked(
	number, total int,
	title string,
	event archive.Progress,
	stage archive.ProgressStage,
	now time.Time,
) {
	completed, workTotal := archiveProgressCounts(event, stage)
	stageBoundary := stage != p.lastStage
	step := max(1, workTotal/4)
	workBoundary := workTotal > 0 && completed > p.lastPlainCompleted &&
		(completed >= workTotal || completed-p.lastPlainCompleted >= step)
	assetBoundary := stage == archive.ProgressBuilding &&
		event.AssetsCompleted > p.lastAssets &&
		(event.AssetsCompleted-p.lastAssets >= 250 || now.Sub(p.lastEmit) >= 2*time.Second)
	finalBoundary := stage == archive.ProgressFinal && event.Final && !p.finalShown
	if !workBoundary && !assetBoundary && !finalBoundary && !stageBoundary {
		p.lastTopics, p.lastAssets = event.TopicsValidated, event.AssetsCompleted
		p.lastCompleted, p.lastTotal = completed, workTotal
		return
	}
	var line string
	switch stage {
	case archive.ProgressTopics:
		completedTopics := event.PlannedTopics > 0 &&
			event.TopicsValidated >= event.PlannedTopics
		percent := progressPercent(event.TopicsValidated, event.PlannedTopics)
		line = fmt.Sprintf(
			"[%d/%d] %s: topics %d/%d (%d%%); elapsed %s",
			number, total, safeDisplayText(title), event.TopicsValidated,
			event.PlannedTopics, percent, p.elapsedAt(now),
		)
		if completedTopics {
			p.topicCompleteShown = true
		}
	case archive.ProgressBuilding:
		line = fmt.Sprintf(
			"[%d/%d] %s: Building offline guide %d/%d (%d%%); assets %d/%d; elapsed %s",
			number, total, safeDisplayText(title), event.PagesEmitted,
			event.PagesTotal, progressPercent(event.PagesEmitted, event.PagesTotal),
			event.AssetsCompleted, event.AssetsDiscovered, p.elapsedAt(now),
		)
	case archive.ProgressHashing:
		line = fmt.Sprintf(
			"[%d/%d] %s: Hashing generated files %d/%d (%d%%); elapsed %s",
			number, total, safeDisplayText(title), event.FilesHashed,
			event.FilesTotal, progressPercent(event.FilesHashed, event.FilesTotal),
			p.elapsedAt(now),
		)
	case archive.ProgressReferences:
		line = fmt.Sprintf(
			"[%d/%d] %s: Checking offline references %d/%d (%d%%); elapsed %s",
			number, total, safeDisplayText(title), event.ReferenceFilesChecked,
			event.ReferenceFilesTotal,
			progressPercent(event.ReferenceFilesChecked, event.ReferenceFilesTotal),
			p.elapsedAt(now),
		)
	case archive.ProgressFinal:
		line = fmt.Sprintf(
			"[%d/%d] %s: offline checks finished; elapsed %s",
			number, total, safeDisplayText(title), p.elapsedAt(now),
		)
		p.finalShown = true
	}
	fmt.Fprintln(p.writer, p.presentation.phase(line))
	p.lastEmit, p.lastStage = now, stage
	p.lastTopics, p.lastAssets = event.TopicsValidated, event.AssetsCompleted
	p.lastCompleted, p.lastTotal, p.lastPlainCompleted = completed, workTotal, completed
}

func progressPercent(completed, total int) int {
	if total <= 0 {
		return 0
	}
	return min(100, max(0, completed*100/total))
}

func progressTopicLine(width, number, total int, title string, completed, planned int, elapsed time.Duration) string {
	percent := progressPercent(completed, planned)
	tail := fmt.Sprintf(" %d/%d %d%% %s", completed, planned, percent, elapsed)
	base := fmt.Sprintf("[%d/%d] ", number, total)
	stage := "topics "
	safeTitle := safeDisplayText(title)
	minBar := 6
	titleBudget := width - ansi.StringWidth(base+stage+tail) - minBar - 2
	prefix := base
	if titleBudget >= 4 {
		title = ansi.Truncate(safeTitle, min(24, titleBudget), "...")
		prefix += title + " - " + stage
	} else {
		prefix += stage
	}
	barWidth := width - ansi.StringWidth(prefix+tail) - 2
	if barWidth < 3 {
		return ansi.Truncate(
			fmt.Sprintf("%stopics %d/%d %d%% %s", base, completed, planned, percent, elapsed),
			width, "...",
		)
	}
	filled := 0
	if planned > 0 {
		filled = min(barWidth, max(0, completed*barWidth/planned))
	}
	bar := "[" + strings.Repeat("=", filled) + strings.Repeat("-", barWidth-filled) + "]"
	return prefix + bar + tail
}

func progressArchiveWorkLine(
	width, number, guideTotal int,
	title, label, shortLabel string,
	completed, total, assetsCompleted, assetsDiscovered int,
	showAssets bool,
	elapsed time.Duration,
) string {
	counts := fmt.Sprintf("%d/%d (%d%%)", completed, total, progressPercent(completed, total))
	compact := shortLabel + " " + counts
	if showAssets {
		compact += fmt.Sprintf(" | assets %d/%d", assetsCompleted, assetsDiscovered)
	}
	if ansi.StringWidth(compact) >= width {
		return ansi.Truncate(compact, width, "...")
	}
	tail := " " + counts
	if showAssets {
		tail += fmt.Sprintf(" | assets %d/%d", assetsCompleted, assetsDiscovered)
	}
	tail += " | elapsed " + elapsed.String()
	fullLabel := fmt.Sprintf("%s [%d/%d] %s", label, number, guideTotal, safeDisplayText(title))
	if width <= ansi.StringWidth(tail)+ansi.StringWidth(shortLabel)+4 {
		return ansi.Truncate(compact, width, "...")
	}
	return progressCompactLine(width, fullLabel, tail, func(barWidth int) string {
		return determinateProgressBar(barWidth, completed, total)
	})
}

func progressStageLine(width, number, total int, title, stage string, elapsed time.Duration) string {
	base := fmt.Sprintf("[%d/%d] ", number, total)
	suffix := fmt.Sprintf(" - %s - %s", safeDisplayText(stage), elapsed)
	safeTitle := safeDisplayText(title)
	titleBudget := width - ansi.StringWidth(base+suffix)
	if titleBudget < 4 {
		return ansi.Truncate(base+safeDisplayText(stage)+" - "+elapsed.String(), width, "...")
	}
	return base + ansi.Truncate(safeTitle, min(28, titleBudget), "...") + suffix
}

func (p *progressReporter) Finish(status, message string) {
	p.stopAnimation()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearLocked()
	message = fmt.Sprintf("%s (elapsed %s)", message, p.elapsed())
	switch status {
	case "complete":
		fmt.Fprintln(p.writer, p.presentation.success(message))
	case "incomplete":
		fmt.Fprintln(p.writer, p.presentation.warning(message))
	default:
		fmt.Fprintln(p.writer, p.presentation.failure(message))
	}
}

func (p *progressReporter) Done(message string) {
	p.Finish("complete", message)
}
