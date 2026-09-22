package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	maxCompletionScannedEntries = 4096
	maxDirectoryCompletions     = 256
	completionDebounce          = 40 * time.Millisecond
)

type directorySuggestion struct {
	value string
	label string
}

type directoryCompletionRequest struct {
	promptID   uint64
	generation uint64
	value      string
}

type directoryCompletionResult struct {
	promptID    uint64
	generation  uint64
	value       string
	suggestions []directorySuggestion
	truncated   bool
	omitted     bool
	err         string
}

type directoryCompletionResolver struct {
	cwd     string
	cwdErr  error
	home    string
	homeErr error
	read    func(string, int) ([]os.DirEntry, bool, error)
	stat    func(string) (os.FileInfo, error)
}

func newDirectoryCompletionResolver() directoryCompletionResolver {
	cwd, cwdErr := os.Getwd()
	home, homeErr := os.UserHomeDir()
	return directoryCompletionResolver{
		cwd: cwd, cwdErr: cwdErr, home: home, homeErr: homeErr,
		read: readDirectoryEntries, stat: os.Stat,
	}
}

func (r directoryCompletionResolver) complete(request directoryCompletionRequest) directoryCompletionResult {
	result := directoryCompletionResult{
		promptID: request.promptID, generation: request.generation, value: request.value,
	}
	parent, typedParent, prefix, err := r.lookup(request.value)
	if err != nil {
		result.err = err.Error()
		return result
	}
	read := r.read
	if read == nil {
		read = readDirectoryEntries
	}
	entries, truncated, err := read(parent, maxCompletionScannedEntries)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result
		}
		result.err = "Cannot read directory " + parent + ": " + err.Error()
		return result
	}
	result.truncated = truncated
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	showHidden := strings.HasPrefix(prefix, ".")
	stat := r.stat
	if stat == nil {
		stat = os.Stat
	}
	for _, entry := range entries {
		name := entry.Name()
		if !utf8.ValidString(name) {
			result.omitted = true
			continue
		}
		if strings.HasPrefix(name, ".") && !showHidden || !strings.HasPrefix(name, prefix) {
			continue
		}
		info, statErr := stat(filepath.Join(parent, name))
		if statErr != nil || !info.IsDir() {
			continue
		}
		if len(result.suggestions) >= maxDirectoryCompletions {
			result.truncated = true
			continue
		}
		result.suggestions = append(result.suggestions, directorySuggestion{
			value: typedParent + name + string(os.PathSeparator),
			label: name + string(os.PathSeparator),
		})
	}
	return result
}

func readDirectoryEntries(path string, limit int) ([]os.DirEntry, bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

func (r directoryCompletionResolver) lookup(value string) (parent, typedParent, prefix string, err error) {
	switch {
	case value == "~":
		if err := r.requireHome(); err != nil {
			return "", "", "", err
		}
		return r.home, "~" + string(os.PathSeparator), "", nil
	case strings.HasPrefix(value, "~"+string(os.PathSeparator)):
		if err := r.requireHome(); err != nil {
			return "", "", "", err
		}
		return r.lookupExpanded(value, r.home+value[1:])
	default:
		return r.lookupExpanded(value, value)
	}
}

func (r directoryCompletionResolver) requireHome() error {
	switch {
	case r.homeErr != nil:
		return fmt.Errorf("Cannot complete home-relative path: home directory could not be determined: %w", r.homeErr)
	case r.home == "":
		return errors.New("Cannot complete home-relative path: home directory could not be determined")
	default:
		return nil
	}
}

func (r directoryCompletionResolver) requireCWD() error {
	switch {
	case r.cwdErr != nil:
		return fmt.Errorf("Cannot complete relative path: current working directory could not be determined: %w", r.cwdErr)
	case r.cwd == "":
		return errors.New("Cannot complete relative path: current working directory could not be determined")
	default:
		return nil
	}
}

func (r directoryCompletionResolver) lookupExpanded(typed, expanded string) (parent, typedParent, prefix string, err error) {
	separator := string(os.PathSeparator)
	if strings.HasSuffix(typed, separator) {
		typedParent = typed
		prefix = ""
		parent = expanded
	} else if index := strings.LastIndex(typed, separator); index >= 0 {
		typedParent = typed[:index+1]
		prefix = typed[index+1:]
		expandedIndex := strings.LastIndex(expanded, separator)
		switch {
		case expandedIndex == 0:
			parent = separator
		case expandedIndex > 0:
			parent = expanded[:expandedIndex]
		}
	} else {
		typedParent = ""
		prefix = typed
		parent = ""
	}
	if parent == "" || !filepath.IsAbs(parent) {
		if err := r.requireCWD(); err != nil {
			return "", "", "", err
		}
		parent = filepath.Join(r.cwd, parent)
	}
	return filepath.Clean(parent), typedParent, prefix, nil
}

type directoryCompletionService struct {
	resolver directoryCompletionResolver
	requests chan directoryCompletionRequest
	stop     chan struct{}
	stopOnce sync.Once
	send     func(tea.Msg)
	nextID   atomic.Uint64
	orderMu  sync.Mutex
	lastID   uint64
	lastGen  uint64
}

func newDirectoryCompletionService(
	resolver directoryCompletionResolver,
	send func(tea.Msg),
) *directoryCompletionService {
	service := &directoryCompletionService{
		resolver: resolver, requests: make(chan directoryCompletionRequest, 1),
		stop: make(chan struct{}), send: send,
	}
	go service.run()
	return service
}

func (s *directoryCompletionService) promptID() uint64 {
	if s == nil {
		return 0
	}
	return s.nextID.Add(1)
}

func (s *directoryCompletionService) command(request directoryCompletionRequest) tea.Cmd {
	if s == nil {
		return nil
	}
	return func() tea.Msg {
		s.enqueue(request)
		return nil
	}
}

func (s *directoryCompletionService) enqueue(request directoryCompletionRequest) {
	select {
	case <-s.stop:
		return
	default:
	}
	s.orderMu.Lock()
	defer s.orderMu.Unlock()
	if request.promptID < s.lastID ||
		request.promptID == s.lastID && request.generation < s.lastGen {
		return
	}
	s.lastID, s.lastGen = request.promptID, request.generation
	select {
	case s.requests <- request:
		return
	default:
	}
	select {
	case <-s.requests:
	default:
	}
	select {
	case s.requests <- request:
	case <-s.stop:
	default:
	}
}

func (s *directoryCompletionService) run() {
	for {
		var request directoryCompletionRequest
		select {
		case <-s.stop:
			return
		case request = <-s.requests:
		}
		timer := time.NewTimer(completionDebounce)
		debouncing := true
		for debouncing {
			select {
			case <-s.stop:
				timer.Stop()
				return
			case request = <-s.requests:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(completionDebounce)
			case <-timer.C:
				debouncing = false
			}
		}
		result := s.resolver.complete(request)
		select {
		case <-s.stop:
			return
		default:
			s.send(result)
		}
	}
}

func (s *directoryCompletionService) Close() {
	if s != nil {
		s.stopOnce.Do(func() { close(s.stop) })
	}
}
