//go:build darwin

package pdfacceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	MuPDFManifestSchema          = 1
	MuPDFVersion                 = "1.28.4"
	MuPDFExecutableSHA256        = "5a39feb8c4c85b57eea9ea7785c8747337741baec3c367a26f611c7be3767bef"
	MuPDFLicenseSHA256           = "57c8ff33c9c0cfc3ef00e650a1cc910d7ee479a8bc509f6c9209a7c2a11399d6"
	MuPDFLibrarySHA256           = "92c04f0076ae8232c8e111229b6a19c4b478af0fd4ee00156e66fbc149ce2219"
	MuPDFFormulaLicense          = "AGPL-3.0-or-later"
	MuPDFManifestMaxBytes        = int64(1 << 20)
	MuPDFProcessMaxLogBytes      = 1 << 20
	MuPDFProcessMaxRSSBytes      = int64(4 << 30)
	MuPDFStructuredTextMaxBytes  = int64(512 << 20)
	MuPDFStructuredTextMaxDepth  = 8
	MuPDFStructuredTextMaxPages  = 20_000
	MuPDFStructuredTextMaxBlocks = 2_000_000
	MuPDFStructuredTextMaxLines  = 10_000_000
	MuPDFStructuredTextMaxSpans  = 20_000_000
	MuPDFStructuredTextMaxGlyphs = 100_000_000
	MuPDFStructuredTextMaxText   = int64(1 << 30)
	MuPDFSampleMaxBytes          = int64(256 << 20)
)

type MuPDFManifest struct {
	Dependencies       []MuPDFDependency `json:"dependencies"`
	FormulaLicense     string            `json:"formula_license"`
	GeneratedAtUTC     string            `json:"generated_at_utc"`
	GenerationTool     string            `json:"generation_tool"`
	HostOSBuild        string            `json:"host_os_build"`
	LicensePath        string            `json:"license_path"`
	LicenseSHA256      string            `json:"license_sha256"`
	MutoolPath         string            `json:"mutool_path"`
	MutoolArchitecture string            `json:"mutool_architecture"`
	MutoolSHA256       string            `json:"mutool_sha256"`
	MutoolVersion      string            `json:"mutool_version"`
	SchemaVersion      int               `json:"schema_version"`
	SystemLibraries    []string          `json:"system_libraries"`
}

type MuPDFDependency struct {
	Architecture string `json:"architecture"`
	InstallName  string `json:"install_name"`
	Path         string `json:"path"`
	SHA256       string `json:"sha256"`
}

type MuPDFTool struct {
	Manifest       MuPDFManifest
	ManifestPath   string
	ManifestSHA256 string
}

func GenerateMuPDFManifest(ctx context.Context, mutoolPath, licensePath, output string) (MuPDFManifest, error) {
	manifest, err := inspectMuPDFInstallation(mutoolPath, licensePath)
	if err != nil {
		return MuPDFManifest{}, err
	}
	manifest.GeneratedAtUTC = time.Now().UTC().Format(time.RFC3339Nano)
	manifest.GenerationTool = "aoscx-pdf-acceptance Go debug/macho recursive closure inspection"
	manifest.SchemaVersion = MuPDFManifestSchema
	tool := MuPDFTool{Manifest: manifest}
	version, err := tool.runVersion(ctx)
	if err != nil {
		return MuPDFManifest{}, err
	}
	if version != "mutool version "+MuPDFVersion {
		return MuPDFManifest{}, fmt.Errorf("mutool version output is %q, expected %q", version, "mutool version "+MuPDFVersion)
	}
	if err := writeExclusiveJSON(output, manifest); err != nil {
		return MuPDFManifest{}, err
	}
	return manifest, nil
}

func LoadMuPDFTool(ctx context.Context, manifestPath string) (MuPDFTool, error) {
	absolute, err := requireUnsymPath(manifestPath, false)
	if err != nil {
		return MuPDFTool{}, err
	}
	data, info, err := readNonsymlinkFile(absolute, MuPDFManifestMaxBytes)
	if err != nil {
		return MuPDFTool{}, fmt.Errorf("read mutool identity manifest: %w", err)
	}
	var manifest MuPDFManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return MuPDFTool{}, fmt.Errorf("parse mutool identity manifest: %w", err)
	}
	if err := decoder.Decode(&manifest); err != nil {
		return MuPDFTool{}, fmt.Errorf("parse mutool identity manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return MuPDFTool{}, errors.New("mutool identity manifest has trailing JSON")
		}
		return MuPDFTool{}, fmt.Errorf("parse mutool identity manifest trailing data: %w", err)
	}
	if manifest.SchemaVersion != MuPDFManifestSchema ||
		manifest.GenerationTool == "" || manifest.GeneratedAtUTC == "" ||
		manifest.MutoolVersion != MuPDFVersion ||
		manifest.MutoolArchitecture != "arm64" ||
		manifest.MutoolSHA256 != MuPDFExecutableSHA256 ||
		manifest.LicenseSHA256 != MuPDFLicenseSHA256 ||
		manifest.FormulaLicense != MuPDFFormulaLicense ||
		manifest.HostOSBuild == "" || len(manifest.Dependencies) == 0 ||
		len(manifest.Dependencies) > 512 || len(manifest.SystemLibraries) > 512 {
		return MuPDFTool{}, errors.New("mutool identity manifest is incomplete or unsupported")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.GeneratedAtUTC); err != nil ||
		manifest.GenerationTool != "aoscx-pdf-acceptance Go debug/macho recursive closure inspection" {
		return MuPDFTool{}, errors.New("mutool identity manifest generation metadata is invalid")
	}
	if err := validateManifestClosure(manifest); err != nil {
		return MuPDFTool{}, err
	}
	if manifest.MutoolPath == "" || manifest.LicensePath == "" ||
		!filepath.IsAbs(manifest.MutoolPath) || !filepath.IsAbs(manifest.LicensePath) {
		return MuPDFTool{}, errors.New("mutool identity manifest paths must be absolute")
	}
	hash := sha256.Sum256(data)
	tool := MuPDFTool{
		Manifest: manifest, ManifestPath: absolute,
		ManifestSHA256: hex.EncodeToString(hash[:]),
	}
	current, err := inspectMuPDFInstallation(manifest.MutoolPath, manifest.LicensePath)
	if err != nil {
		return MuPDFTool{}, err
	}
	if err := compareMuPDFManifest(manifest, current); err != nil {
		return MuPDFTool{}, err
	}
	version, err := tool.runVersion(ctx)
	if err != nil {
		return MuPDFTool{}, err
	}
	if version != "mutool version "+MuPDFVersion {
		return MuPDFTool{}, fmt.Errorf("mutool version output changed to %q", version)
	}
	currentManifest, err := os.Lstat(absolute)
	if err != nil || !os.SameFile(info, currentManifest) {
		return MuPDFTool{}, errors.New("mutool identity manifest changed during validation")
	}
	return tool, nil
}

func (tool MuPDFTool) Revalidate() error {
	current, err := inspectMuPDFInstallation(tool.Manifest.MutoolPath, tool.Manifest.LicensePath)
	if err != nil {
		return err
	}
	if err := compareMuPDFManifest(tool.Manifest, current); err != nil {
		return err
	}
	if tool.ManifestPath != "" {
		data, _, err := readNonsymlinkFile(tool.ManifestPath, MuPDFManifestMaxBytes)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != tool.ManifestSHA256 {
			return errors.New("mutool identity manifest changed after validation")
		}
	}
	return nil
}

func (tool MuPDFTool) ExtractLayout(ctx context.Context, pdfPath string) (AcceptanceDocument, error) {
	extractCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	runResult := make(chan error, 1)
	go func() {
		diagnostics, err := tool.run(
			extractCtx,
			&boundedWriter{writer: writer, limit: MuPDFStructuredTextMaxBytes},
			5*time.Minute, "", 0, "draw", "-q", "-F", "stext", pdfPath,
		)
		if err == nil && strings.TrimSpace(diagnostics) != "" {
			err = fmt.Errorf("mutool emitted unexpected structured-text diagnostics: %s", strings.TrimSpace(diagnostics))
		}
		_ = writer.CloseWithError(err)
		runResult <- err
	}()
	document, parseErr := parseStructuredText(reader)
	if parseErr != nil {
		cancel()
	}
	closeErr := reader.Close()
	runErr := <-runResult
	if err := errors.Join(parseErr, runErr, closeErr); err != nil {
		return AcceptanceDocument{}, fmt.Errorf("extract generated PDF structured text with mutool: %w", err)
	}
	return document, nil
}

func (tool MuPDFTool) RenderPNG(ctx context.Context, pdfPath, outputPath string, physicalPage int) error {
	if physicalPage < 1 {
		return errors.New("physical sample page must be positive")
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("sample output already exists: %s", outputPath)
		}
		return err
	}
	diagnostics, err := tool.run(
		ctx, io.Discard, 2*time.Minute, outputPath, MuPDFSampleMaxBytes,
		"draw", "-q", "-F", "png", "-r", "108", "-o", outputPath, pdfPath, strconv.Itoa(physicalPage),
	)
	if err == nil && strings.TrimSpace(diagnostics) != "" {
		return fmt.Errorf("mutool emitted unexpected raster diagnostics: %s", strings.TrimSpace(diagnostics))
	}
	return err
}

func (tool MuPDFTool) runVersion(ctx context.Context) (string, error) {
	diagnostics, err := tool.run(ctx, io.Discard, 30*time.Second, "", 0, "-v")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(diagnostics), nil
}

func (tool MuPDFTool) run(
	ctx context.Context,
	stdout io.Writer,
	timeout time.Duration,
	watchedPath string,
	watchedLimit int64,
	args ...string,
) (diagnostics string, err error) {
	if ctx == nil {
		return "", errors.New("mutool process requires a context")
	}
	if err := tool.Revalidate(); err != nil {
		return "", fmt.Errorf("revalidate mutool identity immediately before process: %w", err)
	}
	executableInfo, err := os.Lstat(tool.Manifest.MutoolPath)
	if err != nil {
		return "", err
	}
	executable, err := os.Open(tool.Manifest.MutoolPath)
	if err != nil {
		return "", err
	}
	defer executable.Close()
	opened, err := executable.Stat()
	if err != nil || !os.SameFile(executableInfo, opened) {
		return "", errors.New("mutool executable identity changed while opening for launch")
	}
	processCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(processCtx, tool.Manifest.MutoolPath, args...)
	command.Env = sanitizedMuPDFEnvironment(os.Environ())
	command.Stdout = stdout
	logs := &boundedLogWriter{limit: MuPDFProcessMaxLogBytes}
	command.Stderr = logs
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
	if err := command.Start(); err != nil {
		return "", err
	}
	launched, launchErr := os.Lstat(tool.Manifest.MutoolPath)
	if launchErr != nil || !os.SameFile(executableInfo, launched) {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		return "", errors.New("mutool executable identity changed around launch")
	}
	monitorDone := make(chan error, 1)
	go func() {
		monitorDone <- monitorMutool(processCtx, cancel, command.Process.Pid, watchedPath, watchedLimit)
	}()
	waitErr := command.Wait()
	processErr := processCtx.Err()
	cancel()
	monitorErr := <-monitorDone
	if processErr != nil {
		err = errors.Join(err, processErr)
	}
	if monitorErr != nil && !errors.Is(monitorErr, context.Canceled) {
		err = errors.Join(err, monitorErr)
	}
	if waitErr != nil {
		detail := strings.TrimSpace(logs.String())
		if detail != "" {
			waitErr = fmt.Errorf("%w; bounded mutool diagnostics: %s", waitErr, detail)
		}
		err = errors.Join(err, waitErr)
	}
	current, currentErr := os.Lstat(tool.Manifest.MutoolPath)
	if currentErr != nil || !os.SameFile(executableInfo, current) {
		err = errors.Join(err, errors.New("mutool executable identity changed during process"))
	}
	return logs.String(), err
}

func monitorMutool(
	ctx context.Context,
	cancel context.CancelFunc,
	processGroup int,
	watchedPath string,
	watchedLimit int64,
) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		rss, err := mutoolProcessGroupRSS(ctx, processGroup)
		if err != nil && !errors.Is(err, context.Canceled) {
			cancel()
			return err
		}
		if rss > MuPDFProcessMaxRSSBytes {
			cancel()
			return fmt.Errorf("mutool process-group RSS %d exceeds %d-byte limit", rss, MuPDFProcessMaxRSSBytes)
		}
		if watchedPath != "" {
			info, statErr := os.Lstat(watchedPath)
			if statErr == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					cancel()
					return errors.New("mutool sample output is not a regular file")
				}
				if info.Size() > watchedLimit {
					cancel()
					return fmt.Errorf("mutool sample output exceeds %d-byte limit", watchedLimit)
				}
			} else if !errors.Is(statErr, os.ErrNotExist) {
				cancel()
				return statErr
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func mutoolProcessGroupRSS(ctx context.Context, processGroup int) (int64, error) {
	sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	command := exec.CommandContext(sampleCtx, "/bin/ps", "-axo", "pgid=,rss=")
	command.Env = sanitizedMuPDFEnvironment(os.Environ())
	var output bytes.Buffer
	command.Stdout = &boundedWriter{writer: &output, limit: 16 << 20}
	command.Stderr = &boundedWriter{writer: io.Discard, limit: 64 << 10}
	if err := command.Run(); err != nil {
		if sampleCtx.Err() != nil {
			return 0, sampleCtx.Err()
		}
		return 0, err
	}
	var total int64
	for _, line := range strings.Split(output.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		group, groupErr := strconv.Atoi(fields[0])
		rssKB, rssErr := strconv.ParseInt(fields[1], 10, 64)
		if groupErr != nil || rssErr != nil {
			return 0, errors.New("unexpected /bin/ps mutool accounting output")
		}
		if group == processGroup {
			if rssKB > math.MaxInt64/1024 || total > math.MaxInt64-rssKB*1024 {
				return 0, errors.New("mutool RSS accounting overflow")
			}
			total += rssKB * 1024
		}
	}
	return total, nil
}

func inspectMuPDFInstallation(mutoolPath, licensePath string) (MuPDFManifest, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return MuPDFManifest{}, fmt.Errorf("mutool acceptance is supported only on macOS arm64, not %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	mutool, err := requireUnsymPath(mutoolPath, true)
	if err != nil {
		return MuPDFManifest{}, fmt.Errorf("verify mutool path: %w", err)
	}
	license, err := requireUnsymPath(licensePath, false)
	if err != nil {
		return MuPDFManifest{}, fmt.Errorf("verify mutool license path: %w", err)
	}
	mutoolIdentity, _, _, err := inspectMachOFile(mutool)
	if err != nil {
		return MuPDFManifest{}, err
	}
	if mutoolIdentity.SHA256 != MuPDFExecutableSHA256 {
		return MuPDFManifest{}, fmt.Errorf("mutool SHA-256 is %s, expected %s", mutoolIdentity.SHA256, MuPDFExecutableSHA256)
	}
	licenseIdentity, licenseFile, err := hashStableFile(license)
	if err != nil {
		return MuPDFManifest{}, err
	}
	if err := licenseFile.Close(); err != nil {
		return MuPDFManifest{}, err
	}
	if licenseIdentity.SHA256 != MuPDFLicenseSHA256 {
		return MuPDFManifest{}, fmt.Errorf("mutool COPYING SHA-256 is %s, expected %s", licenseIdentity.SHA256, MuPDFLicenseSHA256)
	}
	dependencies, systemLibraries, err := inspectMachOClosure(mutool)
	if err != nil {
		return MuPDFManifest{}, err
	}
	if !containsDependencyHash(dependencies, MuPDFLibrarySHA256) {
		return MuPDFManifest{}, errors.New("mutool dependency closure does not contain the approved libmupdf identity")
	}
	hostBuild, err := unix.Sysctl("kern.osversion")
	if err != nil || strings.TrimSpace(hostBuild) == "" {
		return MuPDFManifest{}, fmt.Errorf("read host macOS build identity: %w", err)
	}
	return MuPDFManifest{
		Dependencies: dependencies, FormulaLicense: MuPDFFormulaLicense,
		HostOSBuild: strings.TrimSpace(hostBuild), LicensePath: license,
		LicenseSHA256: licenseIdentity.SHA256, MutoolPath: mutool, MutoolArchitecture: "arm64",
		MutoolSHA256: mutoolIdentity.SHA256, MutoolVersion: MuPDFVersion,
		SystemLibraries: systemLibraries,
	}, nil
}

func inspectMachOClosure(root string) ([]MuPDFDependency, []string, error) {
	type pendingDependency struct {
		installName string
		path        string
	}
	queue := []pendingDependency{{path: root}}
	seen := map[string]bool{root: true}
	system := map[string]struct{}{}
	var dependencies []MuPDFDependency
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		_, imports, rpaths, err := inspectMachOFile(current.path)
		if err != nil {
			return nil, nil, err
		}
		for _, installName := range imports {
			if systemInstallName(installName) {
				system[installName] = struct{}{}
				continue
			}
			resolved, err := resolveMachOImport(root, current.path, installName, rpaths)
			if err != nil {
				return nil, nil, err
			}
			identity, _, _, err := inspectMachOFile(resolved)
			if err != nil {
				return nil, nil, err
			}
			if !strings.HasPrefix(resolved, "/opt/homebrew/Cellar/") {
				return nil, nil, fmt.Errorf("mutool nonsystem dependency is outside the approved Homebrew Cellar: %s", resolved)
			}
			if !seen[resolved] {
				seen[resolved] = true
				dependencies = append(dependencies, MuPDFDependency{
					Architecture: "arm64", InstallName: installName,
					Path: resolved, SHA256: identity.SHA256,
				})
				queue = append(queue, pendingDependency{installName: installName, path: resolved})
			}
		}
	}
	sort.Slice(dependencies, func(i, j int) bool { return dependencies[i].Path < dependencies[j].Path })
	systemLibraries := sortedKeys(system)
	return dependencies, systemLibraries, nil
}

func inspectMachOFile(path string) (ToolFile, []string, []string, error) {
	identity, file, err := hashStableFile(path)
	if err != nil {
		return ToolFile{}, nil, nil, err
	}
	defer file.Close()
	mach, err := macho.NewFile(file)
	if err != nil {
		return ToolFile{}, nil, nil, fmt.Errorf("acceptance tool component is not Mach-O: %s: %w", path, err)
	}
	if mach.Cpu != macho.CpuArm64 {
		_ = mach.Close()
		return ToolFile{}, nil, nil, fmt.Errorf("acceptance tool component architecture is %s, expected arm64: %s", mach.Cpu, path)
	}
	imports, importErr := mach.ImportedLibraries()
	var rpaths []string
	for _, load := range mach.Loads {
		if rpath, ok := load.(*macho.Rpath); ok {
			rpaths = append(rpaths, rpath.Path)
		}
	}
	closeErr := mach.Close()
	if err := errors.Join(importErr, closeErr); err != nil {
		return ToolFile{}, nil, nil, err
	}
	sort.Strings(imports)
	return identity, imports, rpaths, nil
}

func resolveMachOImport(executablePath, loaderPath, installName string, rpaths []string) (string, error) {
	raw := installName
	switch {
	case strings.HasPrefix(raw, "@loader_path/"):
		raw = filepath.Join(filepath.Dir(loaderPath), strings.TrimPrefix(raw, "@loader_path/"))
	case strings.HasPrefix(raw, "@executable_path/"):
		raw = filepath.Join(filepath.Dir(executablePath), strings.TrimPrefix(raw, "@executable_path/"))
	case strings.HasPrefix(raw, "@rpath/"):
		suffix := strings.TrimPrefix(raw, "@rpath/")
		for _, rpath := range rpaths {
			switch {
			case strings.HasPrefix(rpath, "@loader_path/"):
				rpath = filepath.Join(filepath.Dir(loaderPath), strings.TrimPrefix(rpath, "@loader_path/"))
			case strings.HasPrefix(rpath, "@executable_path/"):
				rpath = filepath.Join(filepath.Dir(executablePath), strings.TrimPrefix(rpath, "@executable_path/"))
			case !filepath.IsAbs(rpath):
				continue
			}
			candidate := filepath.Join(rpath, suffix)
			if _, err := os.Stat(candidate); err == nil {
				raw = candidate
				break
			}
		}
		if strings.HasPrefix(raw, "@rpath/") {
			return "", fmt.Errorf("unresolved @rpath dependency in acceptance closure: %s", installName)
		}
	case !filepath.IsAbs(raw):
		return "", fmt.Errorf("relative dependency in acceptance closure: %s", installName)
	}

	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("resolve acceptance dependency %s: %w", installName, err)
	}
	return filepath.Clean(resolved), nil
}

func sanitizedMuPDFEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(name, "DYLD_") {
			continue
		}
		result = append(result, value)
	}
	return result
}

func systemInstallName(value string) bool {
	return strings.HasPrefix(value, "/usr/lib/") || strings.HasPrefix(value, "/System/Library/")
}

func requireUnsymPath(raw string, executable bool) (string, error) {
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	if resolved != absolute {
		return "", fmt.Errorf("path must not contain symbolic links: %s", absolute)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("path must identify a nonsymlink regular file: %s", absolute)
	}
	if executable && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("path is not executable: %s", absolute)
	}
	return absolute, nil
}

func hashStableFile(path string) (ToolFile, *os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ToolFile{}, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ToolFile{}, nil, fmt.Errorf("acceptance component must be a nonsymlink regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return ToolFile{}, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return ToolFile{}, nil, fmt.Errorf("acceptance component identity changed while opening: %s", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		_ = file.Close()
		return ToolFile{}, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		_ = file.Close()
		return ToolFile{}, nil, fmt.Errorf("acceptance component identity changed while hashing: %s", path)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return ToolFile{}, nil, err
	}
	identity := ToolFile{Path: path, SHA256: hex.EncodeToString(hash.Sum(nil))}
	return identity, file, nil
}

func compareMuPDFManifest(expected, current MuPDFManifest) error {
	current.GeneratedAtUTC = expected.GeneratedAtUTC
	current.GenerationTool = expected.GenerationTool
	current.SchemaVersion = expected.SchemaVersion
	if !reflect.DeepEqual(expected, current) {
		return errors.New("current mutool installation or dependency closure differs from the supplied identity manifest")
	}
	return nil
}

func validateManifestClosure(manifest MuPDFManifest) error {
	previous := ""
	seenInstallNames := map[string]bool{}
	for _, dependency := range manifest.Dependencies {
		if dependency.Architecture != "arm64" || !filepath.IsAbs(dependency.Path) ||
			!strings.HasPrefix(dependency.Path, "/opt/homebrew/Cellar/") ||
			len(dependency.SHA256) != sha256.Size*2 || dependency.InstallName == "" ||
			dependency.Path <= previous {
			return errors.New("mutool identity manifest dependency closure is invalid or unsorted")
		}
		if seenInstallNames[dependency.Path+"\x00"+dependency.InstallName] {
			return errors.New("mutool identity manifest dependency closure contains duplicates")
		}
		seenInstallNames[dependency.Path+"\x00"+dependency.InstallName] = true
		previous = dependency.Path
	}
	previous = ""
	for _, library := range manifest.SystemLibraries {
		if !systemInstallName(library) || library <= previous {
			return errors.New("mutool identity manifest system library closure is invalid or unsorted")
		}
		previous = library
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var readValue func(json.Token) error
	readValue = func(token json.Token) error {
		delim, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				rawKey, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := rawKey.(string)
				if !ok || seen[key] {
					return fmt.Errorf("duplicate or invalid JSON object key %q", key)
				}
				seen[key] = true
				value, err := decoder.Token()
				if err != nil {
					return err
				}
				if err := readValue(value); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				value, err := decoder.Token()
				if err != nil {
					return err
				}
				if err := readValue(value); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		return nil
	}
	first, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := readValue(first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains trailing data")
		}
		return err
	}
	return nil
}

func containsDependencyHash(values []MuPDFDependency, hash string) bool {
	for _, value := range values {
		if value.SHA256 == hash {
			return true
		}
	}
	return false
}

func readNonsymlinkFile(path string, limit int64) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, nil, fmt.Errorf("file is not a bounded nonsymlink regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil, fmt.Errorf("file identity changed while opening: %s", path)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, limit+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, nil, err
	}
	if int64(len(data)) != info.Size() {
		return nil, nil, fmt.Errorf("file size changed while reading: %s", path)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, nil, fmt.Errorf("file identity changed while reading: %s", path)
	}
	return data, info, nil
}

func writeExclusiveJSON(path string, value any) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, err := file.Write(data); err != nil {
		writeErr = err
	}
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		_ = os.Remove(absolute)
		return err
	}
	return nil
}

type boundedWriter struct {
	writer io.Writer
	limit  int64
	wrote  int64
}

func (writer *boundedWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > writer.limit-writer.wrote {
		return 0, fmt.Errorf("process output exceeds %d-byte limit", writer.limit)
	}
	count, err := writer.writer.Write(value)
	writer.wrote += int64(count)
	return count, err
}

type boundedLogWriter struct {
	lock      sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (writer *boundedLogWriter) Write(value []byte) (int, error) {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	remaining := writer.limit - writer.buffer.Len()
	if remaining > 0 {
		count := min(remaining, len(value))
		_, _ = writer.buffer.Write(value[:count])
	}
	if len(value) > remaining {
		writer.truncated = true
	}
	return len(value), nil
}

func (writer *boundedLogWriter) String() string {
	writer.lock.Lock()
	defer writer.lock.Unlock()
	value := writer.buffer.String()
	if writer.truncated {
		value += " [truncated]"
	}
	return value
}

func parseStructuredText(reader io.Reader) (AcceptanceDocument, error) {
	decoder := xml.NewDecoder(io.LimitReader(reader, MuPDFStructuredTextMaxBytes+1))
	decoder.Strict = true
	var document AcceptanceDocument
	stack := []string{}
	rootSeen, rootClosed := false, false
	blockIndex := -1
	var currentLine *AcceptanceLine
	var textBytes int64
	blocks, lines, spans, glyphs, images := 0, 0, 0, 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return AcceptanceDocument{}, fmt.Errorf("parse mutool structured text XML: %w", err)
		}
		switch token := token.(type) {
		case xml.Directive:
			return AcceptanceDocument{}, errors.New("mutool structured text XML directives are forbidden")
		case xml.ProcInst:
			if token.Target != "xml" || rootSeen {
				return AcceptanceDocument{}, errors.New("mutool structured text XML has an unsupported processing instruction")
			}
		case xml.StartElement:
			if rootClosed {
				return AcceptanceDocument{}, errors.New("mutool structured text XML has trailing roots")
			}
			if len(stack) >= MuPDFStructuredTextMaxDepth {
				return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds nesting limit")
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			name := token.Name.Local
			switch name {
			case "document":
				if rootSeen || parent != "" {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has an invalid document root")
				}
				rootSeen = true
			case "page":
				if parent != "document" || len(document.Pages) >= MuPDFStructuredTextMaxPages {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has invalid or excessive pages")
				}
				width, err := finiteXMLFloat(attribute(token, "width"))
				if err != nil {
					return AcceptanceDocument{}, fmt.Errorf("invalid structured-text page width: %w", err)
				}
				height, err := finiteXMLFloat(attribute(token, "height"))
				if err != nil || width <= 0 || height <= 0 {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has invalid page geometry")
				}
				document.Pages = append(document.Pages, AcceptancePage{Width: width, Height: height})
				blockIndex = -1
			case "block":
				if parent != "page" {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has an invalid block hierarchy")
				}
				blocks++
				blockIndex++
				if blocks > MuPDFStructuredTextMaxBlocks {
					return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds block limit")
				}
			case "line":
				if parent != "block" || len(document.Pages) == 0 {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has an invalid line hierarchy")
				}
				lines++
				if lines > MuPDFStructuredTextMaxLines {
					return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds line limit")
				}
				box, err := finiteXMLBox(attribute(token, "bbox"))
				if err != nil {
					return AcceptanceDocument{}, err
				}
				text := NormalizeAcceptanceText(attribute(token, "text"))
				textBytes += int64(len(text))
				if textBytes > MuPDFStructuredTextMaxText {
					return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds text limit")
				}
				currentLine = &AcceptanceLine{
					BBox: box, Block: blockIndex, Page: len(document.Pages), Text: text,
				}
			case "font":
				if parent != "line" || currentLine == nil {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has an invalid font hierarchy")
				}
				spans++
				if spans > MuPDFStructuredTextMaxSpans {
					return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds span limit")
				}
				size, err := finiteXMLFloat(attribute(token, "size"))
				if err != nil || size < 0 {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has invalid font size")
				}
				size = float64(float32(size))
				currentLine.MaxFontPoints = math.Max(currentLine.MaxFontPoints, size)
			case "char":
				if parent != "font" || currentLine == nil {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has an invalid glyph hierarchy")
				}
				glyphs++
				if glyphs > MuPDFStructuredTextMaxGlyphs {
					return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds glyph limit")
				}
				value := attribute(token, "c")
				if !utf8.ValidString(value) {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has invalid glyph text")
				}
				if _, err := finiteXMLNumbers(attribute(token, "quad"), 8); err != nil {
					return AcceptanceDocument{}, fmt.Errorf("mutool structured text XML has invalid glyph geometry: %w", err)
				}
			case "image":
				if parent != "block" {
					return AcceptanceDocument{}, errors.New("mutool structured text XML has an invalid image hierarchy")
				}
				images++
				if images > MuPDFStructuredTextMaxBlocks {
					return AcceptanceDocument{}, errors.New("mutool structured text XML exceeds image limit")
				}
			default:
				return AcceptanceDocument{}, fmt.Errorf("mutool structured text XML has unsupported element %q", name)
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != token.Name.Local {
				return AcceptanceDocument{}, errors.New("mutool structured text XML has inconsistent closing elements")
			}
			name := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if name == "line" {
				if currentLine == nil {
					return AcceptanceDocument{}, errors.New("mutool structured text XML line state is inconsistent")
				}
				if currentLine.Text != "" {
					page := &document.Pages[len(document.Pages)-1]
					page.Lines = append(page.Lines, *currentLine)
				}
				currentLine = nil
			}
			if name == "document" {
				rootClosed = true
			}
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return AcceptanceDocument{}, errors.New("mutool structured text XML has unexpected character data")
			}
		case xml.Comment:
			return AcceptanceDocument{}, errors.New("mutool structured text XML comments are forbidden")
		}
	}
	if !rootSeen || !rootClosed || len(stack) != 0 || len(document.Pages) == 0 {
		return AcceptanceDocument{}, errors.New("mutool structured text XML is incomplete")
	}
	return document, nil
}

func attribute(element xml.StartElement, name string) string {
	for _, attribute := range element.Attr {
		if attribute.Name.Local == name {
			return attribute.Value
		}
	}
	return ""
}

func finiteXMLFloat(value string) (float64, error) {
	if value == "" {
		return 0, errors.New("missing numeric attribute")
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
		return 0, errors.New("nonfinite or invalid numeric attribute")
	}
	return number, nil
}

func finiteXMLBox(value string) ([4]float64, error) {
	var result [4]float64
	values, err := finiteXMLNumbers(value, len(result))
	if err != nil {
		return result, err
	}
	copy(result[:], values)
	if result[2] < result[0] || result[3] < result[1] {
		return result, errors.New("bounding box is inverted")
	}
	return result, nil
}

func finiteXMLNumbers(value string, count int) ([]float64, error) {
	fields := strings.Fields(value)
	if len(fields) != count {
		return nil, fmt.Errorf("numeric geometry must contain %d coordinates", count)
	}
	result := make([]float64, count)
	for index, field := range fields {
		number, err := finiteXMLFloat(field)
		if err != nil {
			return nil, err
		}
		result[index] = number
	}
	return result, nil
}
