package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/archive"
	"aos-cx-docs-dldr/internal/source"
	"github.com/charmbracelet/x/ansi"
)

func TestTerminalProgressFitsWidthAndHandlesResize(t *testing.T) {
	var output bytes.Buffer
	width := 80
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return width },
	})
	now := time.Unix(1_700_000_000, 0)
	progress.started = now
	progress.now = func() time.Time { return now }
	title := "ACLs \x1b[2J and 文档 Classifiers Policy Guide with a very long suffix"

	progress.HTML(1, 1, title, archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressTopics,
		TopicsValidated: 47, PlannedTopics: 105,
	})
	assertProgressFramesFit(t, output.String(), 79)
	if plain := ansi.Strip(output.String()); !strings.Contains(plain, "47/105") ||
		!strings.Contains(plain, "44%") || strings.ContainsRune(plain, '\x1b') ||
		!strings.Contains(plain, `\u001B`) {
		t.Fatalf("wide progress lost counts or safe display: %q", plain)
	}

	width = 40
	now = now.Add(progressRepaintInterval)
	progress.HTML(1, 1, title, archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressTopics,
		TopicsValidated: 48, PlannedTopics: 105,
	})
	last := lastProgressFrame(output.String())
	if ansi.StringWidth(last) > 39 || !strings.Contains(ansi.Strip(last), "48/105") {
		t.Fatalf("narrow resized progress is not useful or bounded: %q", last)
	}
}

func TestTerminalProgressBoundsRepaintAndSeparatesStages(t *testing.T) {
	var output bytes.Buffer
	width := 80
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return width },
	})
	now := time.Unix(1_700_000_000, 0)
	progress.started = now
	progress.now = func() time.Time { return now }

	for topics := 1; topics <= 105; topics++ {
		progress.HTML(1, 1, "Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressTopics,
			TopicsValidated: topics, PlannedTopics: 105,
		})
	}
	topicFrames := progressFrameCount(output.String())
	if topicFrames != 2 {
		t.Fatalf("held-clock topic burst repainted %d times, want initial and 100%% only", topicFrames)
	}
	if strings.Contains(ansi.Strip(output.String()), "complete") {
		t.Fatal("100% topic progress falsely claimed archive completion")
	}

	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressBuilding,
		TopicsValidated: 105, PlannedTopics: 105, PagesTotal: 1000,
		AssetsDiscovered: 1, AssetsCompleted: 1,
	})
	for assets := 2; assets <= 1000; assets++ {
		progress.HTML(1, 1, "Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressBuilding,
			TopicsValidated: 105, PlannedTopics: 105,
			PagesEmitted: assets, PagesTotal: 1000,
			AssetsDiscovered: assets, AssetsCompleted: assets,
		})
	}
	if got := progressFrameCount(output.String()); got != topicFrames+2 {
		t.Fatalf("held-clock asset burst repainted %d additional times", got-topicFrames)
	}
	if plain := ansi.Strip(lastProgressFrame(output.String())); !strings.Contains(plain, "Building offline") ||
		!strings.Contains(plain, "assets") {
		t.Fatalf("topic completion did not transition to page building: %q", output.String())
	}

	now = now.Add(progressRepaintInterval)
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressBuilding,
		TopicsValidated: 105, PlannedTopics: 105,
		PagesEmitted: 1000, PagesTotal: 1000,
		AssetsDiscovered: 1001, AssetsCompleted: 1000,
	})
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressHashing,
		TopicsValidated: 105, PlannedTopics: 105,
		FilesHashed: 1, FilesTotal: 2001,
		AssetsDiscovered: 1001, AssetsCompleted: 1001,
	})
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressReferences,
		ReferenceFilesChecked: 1, ReferenceFilesTotal: 1002,
	})
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressManifest,
	})
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressFinal, Final: true,
	})
	plain := ansi.Strip(output.String())
	for _, expected := range []string{
		"Building offline guide", "Hashing generated files",
		"Checking offline references", "Writing and syncing manifest",
		"offline checks finished",
	} {
		if !strings.Contains(plain, expected) {
			t.Fatalf("progress omitted stage %q: %q", expected, output.String())
		}
	}
}

func TestTerminalProgressClearsBeforePersistentDiagnostics(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return 40 },
	}, newHumanPresentationWith(&output, true))
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressTopics,
		TopicsValidated: 1, PlannedTopics: 2,
	})
	progress.Notice("publisher notice")
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressTopics,
		TopicsValidated: 1, PlannedTopics: 2,
	})
	progress.Warning("publisher warning")
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressBuilding,
		TopicsValidated: 2, PlannedTopics: 2,
		PagesTotal: 2, AssetsDiscovered: 1,
	})
	progress.Error("archive error")
	text := output.String()
	if !strings.Contains(text, "\r\x1b[2K") ||
		!strings.Contains(ansi.Strip(text), "publisher notice\r\n") ||
		!strings.Contains(ansi.Strip(text), "publisher warning\r\n") ||
		!strings.Contains(ansi.Strip(text), "archive error\r\n") {
		t.Fatalf("persistent diagnostics did not cleanly interrupt the live line: %q", text)
	}
	assertProgressFramesFit(t, text, 39)
}

func TestTerminalPersistentLinesResetColumnAndPromptBoundary(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return 32 },
	})
	first := "Mapped source unavailable for a very long publisher URL https://example.test/one"
	second := "Mapped source unavailable for another long publisher URL https://example.test/two"
	progress.Warning(first)
	progress.Warning(second)
	text := output.String()
	want := "\r" + first + "\r\n\r" + second + "\r\n"
	if text != want {
		t.Fatalf("TTY persistent boundaries=%q want=%q", text, want)
	}
	var plain bytes.Buffer
	redirected := newProgressReporterWithTerminal(&plain, progressTerminal{})
	redirected.Warning(first)
	if plain.String() != first+"\n" || strings.Contains(plain.String(), "\r") {
		t.Fatalf("redirected warning contains control noise: %q", plain.String())
	}
}

func TestProgressCloseClearsFatalExitLineAndIsIdempotent(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return 40 },
	})
	progress.Guide(1, 1, "Guide", "accepting archive")
	progress.Close()
	progress.Close()
	output.WriteString("Error: accept archive: disk failure\n")
	text := output.String()
	if strings.Count(text, "\r\x1b[2K") != 2 ||
		!strings.HasSuffix(text, "\r\x1b[2KError: accept archive: disk failure\n") {
		t.Fatalf("fatal error was concatenated with an owned live line: %q", text)
	}
}

func TestDumbTerminalDisablesAnimationButNoColorDoesNot(t *testing.T) {
	if progressAnimationAllowed("dumb") || progressAnimationAllowed(" DUMB ") {
		t.Fatal("TERM=dumb enabled cursor-control animation")
	}
	if !progressAnimationAllowed("xterm-256color") {
		t.Fatal("ordinary terminal unexpectedly disabled animation")
	}
	t.Setenv("NO_COLOR", "1")
	if !progressAnimationAllowed("xterm-256color") {
		t.Fatal("NO_COLOR must disable SGR styling, not ordinary TTY progress animation")
	}
}

func TestNonTerminalProgressUsesSparsePlainMilestones(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporter(&output, false)
	now := time.Unix(1_700_000_000, 0)
	progress.started, progress.lastEmit = now, now
	progress.now = func() time.Time { return now }

	for topics := 1; topics <= 105; topics++ {
		progress.HTML(1, 1, "Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressTopics,
			TopicsValidated: topics, PlannedTopics: 105,
		})
	}
	for pages := 1; pages <= 1000; pages++ {
		progress.HTML(1, 1, "Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressBuilding,
			TopicsValidated: 105, PlannedTopics: 105,
			PagesEmitted: pages, PagesTotal: 1000,
			AssetsDiscovered: min(pages, 7), AssetsCompleted: min(pages, 7),
		})
	}
	for files := 0; files <= 1200; files++ {
		progress.HTML(1, 1, "Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressHashing,
			FilesHashed: files, FilesTotal: 1200,
		})
	}
	for files := 0; files <= 1002; files++ {
		progress.HTML(1, 1, "Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressReferences,
			ReferenceFilesChecked: files, ReferenceFilesTotal: 1002,
		})
	}
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressManifest,
	})
	progress.HTML(1, 1, "Guide", archive.Progress{
		SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressFinal, Final: true,
	})
	text := output.String()
	if lines := strings.Count(text, "\n"); lines > 50 {
		t.Fatalf("redirected progress emitted %d lines:\n%s", lines, text)
	}

	for _, expected := range []string{
		"topics 105/105 (100%)",
		"Building offline guide 1000/1000 (100%); assets 7/7",
		"Hashing generated files 1200/1200 (100%)",
		"Checking offline references 1002/1002 (100%)",
		"Writing and syncing manifest",
		"offline checks finished",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("redirected progress omitted %q:\n%s", expected, text)
		}
	}
	if strings.Contains(text, "\x1b[") || strings.Contains(text, "cache downloaded") {
		t.Fatalf("redirected milestones contain controls or repeated cache details: %q", text)
	}
}

func TestArchiveWorkLineKeepsPageAndDynamicAssetSemantics(t *testing.T) {
	line := progressArchiveWorkLine(
		79, 1, 1, "Quality of Service Guide", "Building offline guide", "Build",
		231, 394, 7, 7, true, 76*time.Second,
	)
	for _, expected := range []string{"Building offline guide", "231/394 (58%)", "assets 7/7", "elapsed 1m16s"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("building line omitted %q: %q", expected, line)
		}
	}
	if strings.Contains(line, "assets 7/7 (") || strings.Contains(line, "assets 100%") {
		t.Fatalf("dynamic asset counts fabricated percentage/finality: %q", line)
	}
	narrow := progressArchiveWorkLine(
		24, 1, 1, "Quality of Service Guide", "Building offline guide", "Build",
		231, 394, 7, 7, true, 76*time.Second,
	)
	if ansi.StringWidth(narrow) > 24 || !strings.Contains(narrow, "Build") ||
		!strings.Contains(narrow, "231/394") {
		t.Fatalf("narrow building line lost phase or page counts: %q", narrow)
	}
}

func TestLargeArchiveProgressDoesNotRetainPerItemHistory(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return 80 },
	})
	now := time.Unix(1_700_000_000, 0)
	progress.started = now
	progress.now = func() time.Time { return now }
	emitLargeArchiveProgress(progress)
	if frames := progressFrameCount(output.String()); frames > 6 {
		t.Fatalf("held-clock 3,000-topic progress retained/repainted %d per-item frames", frames)
	}
	if output.Len() > 1024 {
		t.Fatalf("held-clock 3,000-topic progress retained %d output bytes", output.Len())
	}
}

func BenchmarkLargeArchiveProgress(b *testing.B) {
	for b.Loop() {
		progress := newProgressReporterWithTerminal(io.Discard, progressTerminal{
			enabled: true,
			width:   func() int { return 80 },
		})
		now := time.Unix(1_700_000_000, 0)
		progress.started = now
		progress.now = func() time.Time { return now }
		emitLargeArchiveProgress(progress)
	}
}

func emitLargeArchiveProgress(progress *progressReporter) {
	const topics = 3000
	for completed := 0; completed <= topics; completed++ {
		progress.HTML(1, 1, "Large CLI Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressBuilding,
			PagesEmitted: completed, PagesTotal: topics,
			AssetsCompleted: 21, AssetsDiscovered: 21,
		})
	}
	for completed := 0; completed <= 2*topics+26; completed++ {
		progress.HTML(1, 1, "Large CLI Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressHashing,
			FilesHashed: completed, FilesTotal: 2*topics + 26,
		})
	}
	for completed := 0; completed <= topics+2; completed++ {
		progress.HTML(1, 1, "Large CLI Guide", archive.Progress{
			SchemaVersion: archive.ProgressSchemaVersion, Stage: archive.ProgressReferences,
			ReferenceFilesChecked: completed, ReferenceFilesTotal: topics + 2,
		})
	}
}

func TestGuideStagesDoNotFabricatePercentages(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return 80 },
	})
	progress.Guide(1, 1, "Publisher PDF", "downloading publisher PDF")
	if plain := ansi.Strip(lastProgressFrame(output.String())); strings.Contains(plain, "%") ||
		!strings.Contains(plain, "downloading publisher PDF") {
		t.Fatalf("indeterminate PDF stage fabricated progress: %q", plain)
	}
	progress.Guide(1, 1, "HTML Guide", "discovering complete source plan")
	if plain := ansi.Strip(lastProgressFrame(output.String())); strings.Contains(plain, "%") ||
		!strings.Contains(plain, "discovering complete source plan") {
		t.Fatalf("planning stage fabricated progress: %q", plain)
	}
}

func TestCompactStartupProgressFramesAreHonestAndBounded(t *testing.T) {
	for _, width := range []int{79, 47, 31} {
		indeterminate := progressIndeterminateLine(
			width, "Refreshing Product Documentation catalogue", 3, 3*time.Second,
		)
		if ansi.StringWidth(indeterminate) > width ||
			strings.Contains(indeterminate, "%") ||
			strings.Contains(indeterminate, "/") ||
			!strings.Contains(indeterminate, ">") {
			t.Fatalf("invalid indeterminate frame width=%d: %q", width, indeterminate)
		}
		determinate := progressDeterminateLine(
			width, "Refreshing guide mappings", 18, 28, 4*time.Second,
		)
		if ansi.StringWidth(determinate) > width ||
			!strings.Contains(determinate, "18/28 (64%)") ||
			!strings.Contains(determinate, ">") {
			t.Fatalf("invalid determinate frame width=%d: %q", width, determinate)
		}
	}
	if first, second := indeterminateProgressBar(12, 0), indeterminateProgressBar(12, 3); first == second {
		t.Fatalf("indeterminate segment did not move: %q", first)
	}
	full := determinateProgressBar(12, 1, 1)
	if full != strings.Repeat("=", 12) || strings.Contains(full, ">") {
		t.Fatalf("100%% bar retained a moving arrow: %q", full)
	}
	if zero := progressDeterminateLine(60, "Refreshing guide mappings", 0, 0, 0); strings.Contains(zero, "%") {
		t.Fatalf("zero-total progress fabricated a percentage: %q", zero)
	}
}

func TestDeterminateStartupProgressThrottlesResizesAndRejectsRegression(t *testing.T) {
	var output bytes.Buffer
	width := 80
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return width },
	})
	now := time.Unix(1_700_000_000, 0)
	progress.now = func() time.Time { return now }
	progress.Availability(availabilityProgress{Total: 100})
	for completed := 1; completed < 100; completed++ {
		progress.Availability(availabilityProgress{Completed: completed, Total: 100})
	}
	if got := progressFrameCount(output.String()); got != 1 {
		t.Fatalf("held-clock availability burst repainted %d frames", got)
	}
	progress.Availability(availabilityProgress{Completed: 100, Total: 100})
	if got := progressFrameCount(output.String()); got != 2 {
		t.Fatalf("final availability did not force one repaint: %d", got)
	}
	if frame := ansi.Strip(lastProgressFrame(output.String())); !strings.Contains(frame, "100/100 (100%)") ||
		strings.Contains(strings.Split(frame, "]")[0], ">") {
		t.Fatalf("final availability frame is misleading: %q", frame)
	}

	progress.Availability(availabilityProgress{Completed: 50, Total: 200})
	now = now.Add(progressRepaintInterval)
	progress.Availability(availabilityProgress{Completed: 40, Total: 200})
	if progress.batchCompleted != 50 {
		t.Fatalf("out-of-order completion regressed to %d", progress.batchCompleted)
	}
	width = 36
	progress.Availability(availabilityProgress{Completed: 51, Total: 200})
	last := lastProgressFrame(output.String())
	if ansi.StringWidth(last) > 35 || !strings.Contains(ansi.Strip(last), "51/200 (25%)") {
		t.Fatalf("resized availability frame lost semantics or bounds: %q", last)
	}
}

func TestCatalogueAnimationClearsAroundDiagnosticsAndStops(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporterWithTerminal(&output, progressTerminal{
		enabled: true,
		width:   func() int { return 60 },
	})
	progress.Catalog(source.CatalogProgress{Stage: source.CatalogPortalStarted})
	time.Sleep(progressAnimationTick + 25*time.Millisecond)
	progress.Warning("publisher warning")
	time.Sleep(progressAnimationTick + 25*time.Millisecond)
	progress.Clear()
	before := output.Len()
	time.Sleep(progressAnimationTick + 25*time.Millisecond)
	if output.Len() != before || progress.lineActive ||
		progress.animationCancel != nil || progress.animationDone != nil {
		t.Fatalf("owned catalogue animation survived clear: active=%v bytes=%d/%d", progress.lineActive, before, output.Len())
	}
	text := output.String()
	if !strings.Contains(ansi.Strip(text), "publisher warning\r\n") ||
		!strings.Contains(text, "\r\x1b[2K") {
		t.Fatalf("warning did not cleanly interrupt catalogue animation: %q", text)
	}
	assertProgressFramesFit(t, text, 59)

	progress.Catalog(source.CatalogProgress{Stage: source.CatalogPortalStarted})
	progress.Error("catalogue failed")
	if progress.animationCancel != nil || progress.animationDone != nil || progress.lineActive {
		t.Fatal("fatal error did not stop and clear the owned catalogue animation")
	}
}

func TestManifestAnimationStopsBeforePersistentOutput(t *testing.T) {
	for _, writePersistent := range []struct {
		name string
		run  func(*progressReporter)
	}{
		{name: "message", run: func(progress *progressReporter) {
			progress.Message("persistent message")
		}},
		{name: "warning", run: func(progress *progressReporter) {
			progress.Warning("persistent warning")
		}},
		{name: "guide result", run: func(progress *progressReporter) {
			progress.GuideResult(1, 1, "Guide", "complete", 2, 1, 0)
		}},
	} {
		t.Run(writePersistent.name, func(t *testing.T) {
			var output bytes.Buffer
			progress := newProgressReporterWithTerminal(&output, progressTerminal{
				enabled: true,
				width:   func() int { return 80 },
			})
			progress.HTML(1, 1, "Guide", archive.Progress{
				SchemaVersion: archive.ProgressSchemaVersion,
				Stage:         archive.ProgressManifest,
			})
			writePersistent.run(progress)
			before := output.String()
			time.Sleep(2 * progressAnimationTick)
			after := output.String()
			progress.Close()
			if before != after {
				t.Fatalf("manifest animation repainted after persistent output: before=%q after=%q", before, after)
			}
		})
	}
}

func TestPlainStartupProgressUsesSparseTransitionsWithoutControls(t *testing.T) {
	var output bytes.Buffer
	progress := newProgressReporter(&output, false)
	now := time.Unix(1_700_000_000, 0)
	progress.now = func() time.Time { return now }
	progress.Catalog(source.CatalogProgress{Stage: source.CatalogPortalStarted})
	progress.Catalog(source.CatalogProgress{Stage: source.CatalogPortalReady, Total: 100})
	for completed := 1; completed <= 100; completed++ {
		progress.Catalog(source.CatalogProgress{
			Stage: source.CatalogMappingCompleted, Completed: completed, Total: 100,
		})
	}
	progress.Availability(availabilityProgress{Total: 25})
	for completed := 1; completed <= 25; completed++ {
		progress.Availability(availabilityProgress{Completed: completed, Total: 25})
	}
	text := output.String()
	if strings.Contains(text, "\x1b[") || strings.Contains(text, "\r") {
		t.Fatalf("plain startup progress emitted terminal controls: %q", text)
	}
	if strings.Contains(strings.Split(text, "\n")[0], "%") ||
		!strings.Contains(text, "Refreshing guide mappings 100/100 (100%)") ||
		!strings.Contains(text, "Checking guide availability 25/25 (100%)") {
		t.Fatalf("plain startup progress lost honest transitions: %q", text)
	}
	if lines := strings.Count(text, "\n"); lines > 28 {
		t.Fatalf("plain startup progress flooded redirected output with %d lines", lines)
	}
}

func TestProgressSerializesConcurrentMessages(t *testing.T) {
	var output bytes.Buffer
	writer := &synchronizedWriter{writer: &output}
	progress := newProgressReporter(writer, false)
	done := make(chan struct{}, 20)
	for index := range 20 {
		go func() {
			progress.Message("message")
			writer.Write([]byte("warning\n"))
			_ = index
			done <- struct{}{}
		}()
	}
	for range 20 {
		<-done
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 40 {
		t.Fatalf("serialized output lost lines: %d %q", len(lines), output.String())
	}
	for _, line := range lines {
		if line != "message" && line != "warning" {
			t.Fatalf("concurrent output interleaved: %q", line)
		}
	}
}

func assertProgressFramesFit(t *testing.T, text string, width int) {
	t.Helper()
	for _, frame := range strings.Split(text, "\r\x1b[2K") {
		if frame == "" || strings.Contains(frame, "\n") {
			continue
		}
		if got := ansi.StringWidth(frame); got > width {
			t.Fatalf("progress frame width=%d exceeds %d: %q", got, width, frame)
		}
	}
}

func progressFrameCount(text string) int {
	return strings.Count(text, "\r\x1b[2K")
}

func lastProgressFrame(text string) string {
	parts := strings.Split(text, "\r\x1b[2K")
	return parts[len(parts)-1]
}
