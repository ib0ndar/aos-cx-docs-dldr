//go:build reqexperiment

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aos-cx-docs-dldr/internal/fetch"
	"aos-cx-docs-dldr/internal/model"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

const wp8PDFSelectionMarker = "wp8-pdf-selection-authorization.json"

type wp8PDFSelectionPrompt struct {
	Title   string   `json:"title"`
	Choices []string `json:"choices,omitempty"`
}

type wp8PDFSelectionReport struct {
	StartedAt        string                  `json:"started_at"`
	FinishedAt       string                  `json:"finished_at"`
	Elapsed          string                  `json:"elapsed"`
	Case             string                  `json:"case"`
	Candidate        string                  `json:"candidate"`
	CandidateSHA256  string                  `json:"candidate_sha256"`
	CandidateVersion string                  `json:"candidate_version"`
	CLIArgs          []string                `json:"cli_args"`
	CLIExit          int                     `json:"cli_exit"`
	Prompts          []wp8PDFSelectionPrompt `json:"prompts"`
	SelectedLabel    string                  `json:"selected_label,omitempty"`
	PreferenceAsked  bool                    `json:"preference_asked"`
	ExistingAsked    bool                    `json:"existing_asked"`
	Stderr           string                  `json:"stderr"`
	Attempts         fetch.AttemptStats      `json:"network_attempts"`
	ValidationError  string                  `json:"validation_error,omitempty"`
}

type wp8PDFSelectionPrompts struct {
	liveCase        string
	calls           []wp8PDFSelectionPrompt
	selectedLabel   string
	preferenceAsked bool
	existingAsked   bool
}

func (p *wp8PDFSelectionPrompts) Select(
	_ context.Context,
	title string,
	choices []selectionChoice,
	_ promptConfig,
) (string, error) {
	p.record(title, choices)
	switch title {
	case "Documents to retrieve":
		return "some", nil
	case "Prefer a source-verified PDF when available?":
		p.preferenceAsked = true
		return "", context.Canceled
	case "An existing library was found":
		p.existingAsked = true
		return "", context.Canceled
	default:
		return "", fmt.Errorf("unexpected live selection prompt %q", title)
	}
}

func (p *wp8PDFSelectionPrompts) MultiSelect(
	_ context.Context,
	title string,
	choices []selectionChoice,
	_ promptConfig,
) ([]string, error) {
	p.record(title, choices)
	needle := "high availability"
	if p.liveCase == "hpe" {
		needle = "job scheduler"
	}
	for _, choice := range choices {
		if strings.Contains(strings.ToLower(choice.Label), needle) {
			p.selectedLabel = choice.Label
			return []string{choice.Value}, nil
		}
	}
	return nil, fmt.Errorf("live guide list has no %q selection", needle)
}

func (p *wp8PDFSelectionPrompts) Input(
	_ context.Context,
	title string,
	_ promptConfig,
) (string, error) {
	p.record(title, nil)
	return "", fmt.Errorf("unexpected live selection input %q", title)
}

func (p *wp8PDFSelectionPrompts) record(title string, choices []selectionChoice) {
	call := wp8PDFSelectionPrompt{Title: title}
	for _, choice := range choices {
		call.Choices = append(call.Choices, choice.Label)
	}
	p.calls = append(p.calls, call)
}

func TestLiveWP8PDFAvailabilityFirstSelection(t *testing.T) {
	root := os.Getenv("AOSCX_WP8_SELECTION_LIVE_DIR")
	liveCase := os.Getenv("AOSCX_WP8_SELECTION_LIVE_CASE")
	candidate := os.Getenv("AOSCX_WP8_SELECTION_CANDIDATE")
	if root == "" {
		t.Skip("WP8 PDF availability-first live verification requires explicit authorization")
	}
	if liveCase != "flare" && liveCase != "hpe" {
		t.Fatal("AOSCX_WP8_SELECTION_LIVE_CASE must be flare or hpe")
	}
	absolute, err := filepath.Abs(root)
	if err != nil || !strings.HasPrefix(filepath.Base(absolute), "aoscx-go-pdf-preference-selection-") {
		t.Fatal("invalid WP8 PDF selection artifact directory")
	}
	if candidate == "" {
		t.Fatal("AOSCX_WP8_SELECTION_CANDIDATE is required")
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidateBody, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidateDigest := sha256.Sum256(candidateBody)
	candidateVersion, err := exec.Command(candidate, "--app-version").CombinedOutput()
	if err != nil {
		t.Fatalf("run source candidate version check: %v: %s", err, candidateVersion)
	}
	if strings.TrimSpace(string(candidateVersion)) != model.ExecutableName+" "+model.Version {
		t.Fatalf("unexpected source candidate version %q", candidateVersion)
	}
	if err := requireWP8SelectionDiskReserve(absolute); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{wp8PDFSelectionMarker, "raw-cache", "selection-result.json"} {
		if _, err := os.Lstat(filepath.Join(absolute, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("live selection run requires a fresh path; found %s", name)
		}
	}

	const maxAttempts = 192
	const deadline = 360 * time.Second
	marker := map[string]any{
		"stage":                         "WP8 availability-first selection " + liveCase,
		"claimed_at":                    time.Now().UTC().Format(time.RFC3339Nano),
		"pid":                           os.Getpid(),
		"max_wire_attempts":             maxAttempts,
		"per_request_attempt_allowance": fetch.CompatibleWireAttemptAllowance,
		"overall_deadline_seconds":      int(deadline / time.Second),
		"source_front_and_head_only":    true,
	}
	if err := writeWP8SelectionExclusive(filepath.Join(absolute, wp8PDFSelectionMarker), marker); err != nil {
		t.Fatal(err)
	}

	platform, version := "8360", "10.16"
	if liveCase == "hpe" {
		platform, version = "6300", "10.18.xxxx"
	}
	destination := filepath.Join(absolute, "library")
	if err := os.MkdirAll(filepath.Join(destination, platform, version), 0o700); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--transport", "compatible",
		"--platform", platform,
		"--version", version,
		"--destination", destination,
		"--raw-cache", filepath.Join(absolute, "raw-cache"),
		"--workers", "4",
		"--delay", "2",
		"--timeout", "45",
		"--retries", "0",
		"--max-resource-mb", "16",
	}
	budget, err := fetch.NewAttemptBudget(maxAttempts, fetch.CompatibleWireAttemptAllowance)
	if err != nil {
		t.Fatal(err)
	}
	prompts := &wp8PDFSelectionPrompts{liveCase: liveCase}
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	var stdout, stderr bytes.Buffer
	exit := runWithPrompts(ctx, args, &stdout, &stderr,
		func(name string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
			if name != "compatible" {
				return nil, errors.New("live selection requires compatible transport")
			}
			config.Attempts = budget
			return fetch.NewCompatible(config, notify)
		},
		prompts,
	)
	cancel()

	validationErr := validateWP8SelectionLive(liveCase, exit, prompts, stderr.String(), budget.Stats())
	report := wp8PDFSelectionReport{
		StartedAt: started.UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Elapsed: time.Since(started).String(), Case: liveCase, Candidate: candidate,
		CandidateSHA256:  hex.EncodeToString(candidateDigest[:]),
		CandidateVersion: strings.TrimSpace(string(candidateVersion)), CLIArgs: args, CLIExit: exit,
		Prompts: prompts.calls, SelectedLabel: prompts.selectedLabel,
		PreferenceAsked: prompts.preferenceAsked, ExistingAsked: prompts.existingAsked,
		Stderr: stderr.String(), Attempts: budget.Stats(),
	}
	if validationErr != nil {
		report.ValidationError = validationErr.Error()
	}
	if err := writeWP8SelectionExclusive(filepath.Join(absolute, "selection-result.json"), report); err != nil {
		t.Fatal(err)
	}
	if validationErr != nil {
		t.Fatalf("single authorized live selection failed; do not restart: %v", validationErr)
	}
}

func validateWP8SelectionLive(
	liveCase string,
	exit int,
	prompts *wp8PDFSelectionPrompts,
	stderr string,
	attempts fetch.AttemptStats,
) error {
	var problems []error
	if exit != 130 {
		problems = append(problems, fmt.Errorf("selection exit=%d, want cancellation 130", exit))
	}
	if attempts.AttemptedTransmissions > 192 ||
		attempts.ObservedHeaderWrites > attempts.AttemptedTransmissions ||
		attempts.ReservedAttempts != 0 || attempts.AllowanceViolated {
		problems = append(problems, fmt.Errorf("network attempt guard failed: %+v", attempts))
	}
	if strings.Contains(stderr, "Preferred source-verified PDF was unavailable") {
		problems = append(problems, errors.New("selection-only run emitted a download fallback warning"))
	}
	for _, call := range prompts.calls {
		if call.Title != "Select guides (Space toggles; Enter confirms)" {
			continue
		}
		for _, label := range call.Choices {
			if !validWP8SelectionLiveLabel(label) {
				problems = append(problems, fmt.Errorf("guide label is not explicit: %q", label))
			}
		}
	}
	switch liveCase {
	case "flare":
		for _, line := range strings.Split(stderr, "\n") {
			if strings.Contains(line, "Could not establish mapped source availability") &&
				strings.Contains(strings.ToLower(line), "high availability") {
				problems = append(problems, errors.New("High Availability source availability was not checked"))
			}
		}
		if !strings.HasSuffix(prompts.selectedLabel, " [HTML (flare)]") {
			problems = append(problems, fmt.Errorf("High Availability label=%q", prompts.selectedLabel))
		}
		if prompts.preferenceAsked {
			problems = append(problems, errors.New("HTML-only High Availability selection offered PDF preference"))
		}
		if !prompts.existingAsked {
			problems = append(problems, errors.New("selection did not advance past the omitted preference step"))
		}
	case "hpe":
		if !strings.HasSuffix(prompts.selectedLabel, " [HTML (hpe) / PDF native]") {
			problems = append(problems, fmt.Errorf("Job Scheduler label=%q", prompts.selectedLabel))
		}
		if !prompts.preferenceAsked {
			problems = append(problems, errors.New("positive HPE export did not offer PDF preference"))
		}
	}
	return errors.Join(problems...)
}

func validWP8SelectionLiveLabel(label string) bool {
	for _, suffix := range []string{
		" [HTML (flare)]",
		" [HTML (flare) / PDF native]",
		" [HTML (hpe)]",
		" [HTML (hpe) / PDF native]",
		" [HTML (static)]",
		" [HTML (static) / PDF native]",
		" [PDF native]",
	} {
		if strings.HasSuffix(label, suffix) {
			return true
		}
	}
	return false
}

func requireWP8SelectionDiskReserve(path string) error {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return err
	}
	available := uint64(stats.Bavail) * uint64(stats.Bsize)
	total := uint64(stats.Blocks) * uint64(stats.Bsize)
	required := uint64(20 << 30)
	if fraction := total * 15 / 100; fraction > required {
		required = fraction
	}
	if available < required {
		return errors.New("live selection requires free disk space of max(20 GiB, 15%)")
	}
	return nil
}

func writeWP8SelectionExclusive(path string, value any) (err error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	if _, err := file.Write(append(body, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

const wp8ProgressPTYMarker = "wp8-progress-pty-authorization.json"

type wp8ProgressHelperReport struct {
	CLIExit  int                `json:"cli_exit"`
	Attempts fetch.AttemptStats `json:"network_attempts"`
}

type wp8ProgressPTYReport struct {
	Case                string             `json:"case"`
	StartedAt           string             `json:"started_at"`
	FinishedAt          string             `json:"finished_at"`
	Elapsed             string             `json:"elapsed"`
	Candidate           string             `json:"candidate"`
	CandidateSHA256     string             `json:"candidate_sha256"`
	CandidateVersion    string             `json:"candidate_version"`
	PortalFrames        []string           `json:"portal_frames"`
	MappingFrames       []string           `json:"mapping_frames"`
	AvailabilityFrames  []string           `json:"availability_frames"`
	DisplayedGuideCount int                `json:"displayed_guide_count"`
	SelectedLabel       string             `json:"selected_label"`
	PreferenceAsked     bool               `json:"preference_asked"`
	ExistingAsked       bool               `json:"existing_asked"`
	CLIExit             int                `json:"cli_exit"`
	Attempts            fetch.AttemptStats `json:"network_attempts"`
	ValidationError     string             `json:"validation_error,omitempty"`
}

type wp8ProgressBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *wp8ProgressBuffer) Write(body []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(body)
}

func (b *wp8ProgressBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestLiveWP8ProgressPTYHelper(t *testing.T) {
	if os.Getenv("AOSCX_WP8_PROGRESS_HELPER") != "1" {
		t.Skip("live PTY helper is subprocess-only")
	}
	root := os.Getenv("AOSCX_WP8_PROGRESS_LIVE_DIR")
	liveCase := os.Getenv("AOSCX_WP8_PROGRESS_LIVE_CASE")
	platform, version := "8360", "10.16"
	if liveCase == "hpe" {
		platform, version = "6300", "10.18.xxxx"
	}
	const maxAttempts = 192
	budget, err := fetch.NewAttemptBudget(maxAttempts, fetch.CompatibleWireAttemptAllowance)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--transport", "compatible",
		"--platform", platform,
		"--version", version,
		"--destination", filepath.Join(root, "library"),
		"--raw-cache", filepath.Join(root, "raw-cache"),
		"--workers", "4",
		"--delay", "2",
		"--timeout", "45",
		"--retries", "0",
		"--max-resource-mb", "16",
		"--json",
	}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt)
	defer stop()
	exit := runWithPrompts(
		ctx, args, os.Stdout, os.Stderr,
		func(name string, config fetch.Config, notify func(string)) (*fetch.Client, error) {
			if name != "compatible" {
				return nil, errors.New("live progress PTY requires compatible transport")
			}
			config.Attempts = budget
			return fetch.NewCompatible(config, notify)
		},
		newTerminalPrompts(os.Stdin, os.Stderr),
	)
	report := wp8ProgressHelperReport{CLIExit: exit, Attempts: budget.Stats()}
	if err := writeWP8SelectionExclusive(filepath.Join(root, "progress-helper-result.json"), report); err != nil {
		t.Fatal(err)
	}
	if exit != 130 {
		t.Fatalf("live progress helper exit=%d, want 130", exit)
	}
}

func TestLiveWP8ProgressPTY(t *testing.T) {
	root := os.Getenv("AOSCX_WP8_PROGRESS_LIVE_DIR")
	liveCase := os.Getenv("AOSCX_WP8_PROGRESS_LIVE_CASE")
	candidate := os.Getenv("AOSCX_WP8_PROGRESS_CANDIDATE")
	if root == "" {
		t.Skip("WP8 live progress PTY requires explicit authorization")
	}
	if liveCase != "flare" && liveCase != "hpe" {
		t.Fatal("AOSCX_WP8_PROGRESS_LIVE_CASE must be flare or hpe")
	}
	absolute, err := filepath.Abs(root)
	if err != nil || !strings.HasPrefix(filepath.Base(absolute), "aoscx-go-progress-live-") {
		t.Fatal("invalid live progress artifact directory")
	}
	if err := requireWP8SelectionDiskReserve(absolute); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		wp8ProgressPTYMarker, "raw-cache", "progress-helper-result.json", "progress-pty-result.json",
	} {
		if _, err := os.Lstat(filepath.Join(absolute, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("live progress run requires a fresh path; found %s", name)
		}
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidateBody, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidateDigest := sha256.Sum256(candidateBody)
	versionOutput, err := exec.Command(candidate, "--app-version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(versionOutput)) != model.ExecutableName+" "+model.Version {
		t.Fatalf("invalid frozen candidate: %v %q", err, versionOutput)
	}
	const maxAttempts = 192
	const deadline = 360 * time.Second
	if err := writeWP8SelectionExclusive(filepath.Join(absolute, wp8ProgressPTYMarker), map[string]any{
		"stage":                         "WP8 startup progress PTY " + liveCase,
		"claimed_at":                    time.Now().UTC().Format(time.RFC3339Nano),
		"pid":                           os.Getpid(),
		"max_wire_attempts":             maxAttempts,
		"per_request_attempt_allowance": fetch.CompatibleWireAttemptAllowance,
		"overall_deadline_seconds":      int(deadline / time.Second),
		"source_front_and_head_only":    true,
	}); err != nil {
		t.Fatal(err)
	}

	platform, version := "8360", "10.16"
	if liveCase == "hpe" {
		platform, version = "6300", "10.18.xxxx"
	}
	if err := os.MkdirAll(filepath.Join(absolute, "library", platform, version), 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLiveWP8ProgressPTYHelper$", "-test.v")
	command.Env = append(os.Environ(),
		"AOSCX_WP8_PROGRESS_HELPER=1",
		"AOSCX_WP8_PROGRESS_LIVE_DIR="+absolute,
		"AOSCX_WP8_PROGRESS_LIVE_CASE="+liveCase,
	)
	command.Dir = absolute
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Rows: 40, Cols: 160})
	if err != nil {
		t.Fatal(err)
	}
	output := &wp8ProgressBuffer{}
	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(output, terminal)
		close(copyDone)
	}()

	waitWP8ProgressPTY(t, output, "Refreshing Product Documentation catalogue")
	waitWP8ProgressPTY(t, output, "Refreshing guide mappings")
	waitWP8ProgressPTY(t, output, "Documents to retrieve")
	if _, err := terminal.Write([]byte("\x1b[B\r")); err != nil {
		t.Fatal(err)
	}
	waitWP8ProgressPTY(t, output, "Checking guide availability")
	waitWP8ProgressPTY(t, output, "Select guides")
	target := "High Availability Guide [HTML (flare)]"
	if liveCase == "hpe" {
		target = "Job Scheduler Guide [HTML (hpe) / PDF native]"
	}
	waitWP8ProgressPTY(t, output, target)
	choiceIndex, displayed := wp8ProgressChoiceIndex(output.String(), target)
	if choiceIndex < 0 {
		t.Fatalf("target guide was not selectable: %q", target)
	}
	keys := strings.Repeat("\x1b[B", choiceIndex) + " \r"
	if _, err := terminal.Write([]byte(keys)); err != nil {
		t.Fatal(err)
	}
	if liveCase == "hpe" {
		waitWP8ProgressPTY(t, output, "Prefer a source-verified PDF when available?")
	} else {
		waitWP8ProgressPTY(t, output, "An existing library was found")
	}
	if _, err := terminal.Write([]byte{3}); err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	_ = terminal.Close()
	<-copyDone

	var helper wp8ProgressHelperReport
	helperBody, readErr := os.ReadFile(filepath.Join(absolute, "progress-helper-result.json"))
	if readErr == nil {
		readErr = json.Unmarshal(helperBody, &helper)
	}
	portalFrames := wp8ProgressFrames(output.String(), "Refreshing Product Documentation catalogue")
	mappingFrames := wp8ProgressFrames(output.String(), "Refreshing guide mappings")
	availabilityFrames := wp8ProgressFrames(output.String(), "Checking guide availability")
	preferenceAsked := strings.Contains(ansi.Strip(output.String()), "Prefer a source-verified PDF when available?")
	existingAsked := strings.Contains(ansi.Strip(output.String()), "An existing library was found")
	validationErr := validateWP8ProgressPTY(
		liveCase, waitErr, readErr, helper, portalFrames, mappingFrames,
		availabilityFrames, displayed, target, preferenceAsked, existingAsked,
	)
	report := wp8ProgressPTYReport{
		Case: liveCase, StartedAt: started.UTC().Format(time.RFC3339Nano),
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), Elapsed: time.Since(started).String(),
		Candidate: candidate, CandidateSHA256: hex.EncodeToString(candidateDigest[:]),
		CandidateVersion: strings.TrimSpace(string(versionOutput)),
		PortalFrames:     portalFrames, MappingFrames: mappingFrames, AvailabilityFrames: availabilityFrames,
		DisplayedGuideCount: displayed, SelectedLabel: target,
		PreferenceAsked: preferenceAsked, ExistingAsked: existingAsked,
		CLIExit: helper.CLIExit, Attempts: helper.Attempts,
	}
	if validationErr != nil {
		report.ValidationError = validationErr.Error()
	}
	if err := writeWP8SelectionExclusive(filepath.Join(absolute, "progress-pty-result.json"), report); err != nil {
		t.Fatal(err)
	}
	if validationErr != nil {
		t.Fatalf("single authorized live progress PTY failed; do not reuse: %v", validationErr)
	}
}

func waitWP8ProgressPTY(t *testing.T, output *wp8ProgressBuffer, text string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(ansi.Strip(output.String()), text) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("live progress PTY did not show %q", text)
}

func wp8ProgressFrames(text, label string) []string {
	var frames []string
	for _, part := range strings.Split(text, "\r\x1b[2K") {
		plain := ansi.Strip(part)
		if newline := strings.IndexByte(plain, '\n'); newline >= 0 {
			plain = plain[:newline]
		}
		plain = strings.TrimSpace(plain)
		if strings.Contains(plain, label) {
			frames = append(frames, plain)
		}
	}
	if len(frames) > 40 {
		frames = append(frames[:20], frames[len(frames)-20:]...)
	}
	return frames
}

func wp8ProgressChoiceIndex(text, target string) (int, int) {
	plain := ansi.Strip(text)
	at := strings.LastIndex(plain, "Select guides")
	if at < 0 {
		return -1, 0
	}
	index, total := -1, 0
	for _, line := range strings.Split(plain[at:], "\n") {
		if !strings.Contains(line, "[HTML (") && !strings.Contains(line, "[PDF native]") {
			continue
		}
		if strings.Contains(line, target) {
			index = total
		}
		total++
	}
	return index, total
}

func validateWP8ProgressPTY(
	liveCase string,
	processErr, readErr error,
	helper wp8ProgressHelperReport,
	portalFrames, mappingFrames, availabilityFrames []string,
	displayed int,
	target string,
	preferenceAsked, existingAsked bool,
) error {
	var problems []error
	if processErr != nil {
		problems = append(problems, fmt.Errorf("PTY helper process: %w", processErr))
	}
	if readErr != nil {
		problems = append(problems, fmt.Errorf("read helper result: %w", readErr))
	}
	if helper.CLIExit != 130 {
		problems = append(problems, fmt.Errorf("CLI exit=%d, want 130", helper.CLIExit))
	}
	if len(portalFrames) < 2 {
		problems = append(problems, fmt.Errorf("portal animation frames=%d, want at least 2", len(portalFrames)))
	}
	for _, frame := range portalFrames {
		if strings.Contains(frame, "%") || strings.Contains(frame, "/") {
			problems = append(problems, fmt.Errorf("indeterminate frame fabricated numeric progress: %q", frame))
		}
	}
	if !wp8ProgressReached100(mappingFrames) {
		problems = append(problems, errors.New("mapping progress did not reach its typed total"))
	}
	if !wp8ProgressReached100(availabilityFrames) {
		problems = append(problems, errors.New("availability progress did not reach its typed total"))
	}
	if displayed == 0 {
		problems = append(problems, errors.New("guide prompt contained no explicit format labels"))
	}
	for _, frame := range append(append(portalFrames, mappingFrames...), availabilityFrames...) {
		if ansi.StringWidth(frame) > 159 {
			problems = append(problems, fmt.Errorf("progress frame exceeded width-minus-one: %q", frame))
		}
	}
	if helper.Attempts.AttemptedTransmissions > 192 ||
		helper.Attempts.ObservedHeaderWrites > helper.Attempts.AttemptedTransmissions ||
		helper.Attempts.ReservedAttempts != 0 || helper.Attempts.AllowanceViolated {
		problems = append(problems, fmt.Errorf("network attempt guard failed: %+v", helper.Attempts))
	}
	switch liveCase {
	case "flare":
		if target != "High Availability Guide [HTML (flare)]" || preferenceAsked || !existingAsked {
			problems = append(problems, errors.New("Flare selection labels or conditional prompt changed"))
		}
	case "hpe":
		if target != "Job Scheduler Guide [HTML (hpe) / PDF native]" || !preferenceAsked {
			problems = append(problems, errors.New("HPE selection labels or conditional prompt changed"))
		}
	}
	return errors.Join(problems...)
}

func wp8ProgressReached100(frames []string) bool {
	for _, frame := range frames {
		if strings.Contains(frame, "(100%)") && !strings.Contains(frame, ">") {
			return true
		}
	}
	return false
}
