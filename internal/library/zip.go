package library

import (
	stdzip "archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"aos-cx-docs-dldr/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	maxZIPEntries   = 1_000_000
	maxZIPNameBytes = 64 << 20
)

type zipEntry struct {
	relative string
	member   string
	info     os.FileInfo
	hash     string
	size     int64
}

type zipExportOptions struct {
	beforeWrite   func(string) error
	beforeReplace func() error
	rename        func(*os.Root, string, string) error
	versionPolicy nativeVersionPolicy
}

// ExportZIP verifies and exports a published native version directory. A
// genuinely empty directory is accepted as part of the established export contract.
func ExportZIP(ctx context.Context, versionRoot string) (string, error) {
	return exportZIP(ctx, versionRoot, zipExportOptions{})
}

// ExportZIP exports this run's published version while its ownership lock is
// still held.
func (r *Run) ExportZIP(ctx context.Context) (string, error) {
	if !r.published {
		return "", errors.New("library must be published before ZIP export")
	}
	if err := r.checkOwnership(); err != nil {
		return "", err
	}
	return exportZIP(ctx, r.Target, zipExportOptions{
		beforeReplace: r.checkOwnership,
		versionPolicy: r.versionPolicy,
	})
}

func exportZIP(ctx context.Context, versionRoot string, options zipExportOptions) (targetPath string, err error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(versionRoot)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("ZIP source must be a real version directory: %s", absolute)
	}
	parentPath, base := filepath.Dir(absolute), filepath.Base(absolute)
	if base == "." || base == string(filepath.Separator) || !utf8.ValidString(base) {
		return "", fmt.Errorf("invalid ZIP source directory name %q", base)
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		return "", err
	}
	committed := false
	defer func() {
		closeErr := root.Close()
		if !committed {
			err = errors.Join(err, closeErr)
		}
	}()
	if err := storage.SafePath(base); err != nil {
		return "", err
	}
	rootInfo, err := root.Lstat(base)
	if err != nil || !os.SameFile(info, rootInfo) {
		return "", fmt.Errorf("ZIP source identity changed before export: %s", absolute)
	}
	entries, physicalEntries, err := collectZIPEntries(ctx, root, base)
	if err != nil {
		return "", err
	}
	if physicalEntries > 0 {
		if err := verifyPublishedZIPSource(root, base, options.versionPolicy); err != nil {
			return "", err
		}
	}
	if err := validateZIPMembers(entries); err != nil {
		return "", err
	}
	for index := range entries {
		hash, size, err := hashZIPSource(ctx, root, path.Join(base, entries[index].relative), entries[index].info)
		if err != nil {
			return "", err
		}
		entries[index].hash, entries[index].size = hash, size
	}

	target := base + ".zip"
	targetPath = filepath.Join(parentPath, target)
	if targetInfo, statErr := root.Lstat(target); statErr == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			return "", fmt.Errorf("ZIP target is a symlink or special entry: %s", targetPath)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	temp := "." + base + "-" + rand.Text() + ".zip.part"
	file, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	tempOwned := true
	defer func() {
		if tempOwned {
			removeErr := root.Remove(temp)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()

	writer := stdzip.NewWriter(file)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", err
		}
		if options.beforeWrite != nil {
			if err := options.beforeWrite(entry.relative); err != nil {
				_ = writer.Close()
				_ = file.Close()
				return "", err
			}
		}
		header, err := stdzip.FileInfoHeader(entry.info)
		if err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", err
		}
		header.Name = entry.member
		header.Method = stdzip.Deflate
		member, err := writer.CreateHeader(header)
		if err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", err
		}
		hash, size, err := copyZIPSource(ctx, root, path.Join(base, entry.relative), entry.info, member)
		if err != nil {
			_ = writer.Close()
			_ = file.Close()
			return "", fmt.Errorf("copy published file into ZIP %s: %w", entry.relative, err)
		}
		if hash != entry.hash || size != entry.size {
			_ = writer.Close()
			_ = file.Close()
			return "", fmt.Errorf("published file changed during ZIP export: %s", entry.relative)
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return "", err
	}
	if err := validateTemporaryZIP(ctx, root, temp, entries); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if options.beforeReplace != nil {
		if err := options.beforeReplace(); err != nil {
			return "", err
		}
	}
	rename := options.rename
	if rename == nil {
		rename = func(root *os.Root, oldName, newName string) error {
			return root.Rename(oldName, newName)
		}
	}
	if err := rename(root, temp, target); err != nil {
		return "", err
	}
	tempOwned = false
	committed = true
	return targetPath, nil
}

func collectZIPEntries(ctx context.Context, root *os.Root, base string) ([]zipEntry, int, error) {
	var entries []zipEntry
	physical := 0
	var walk func(string, string, bool) error
	walk = func(name, relative string, hidden bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
			return fmt.Errorf("unsafe symlink or special ZIP directory: %s", name)
		}
		directory, err := root.Open(name)
		if err != nil {
			return err
		}
		opened, statErr := directory.Stat()
		if statErr != nil || !os.SameFile(before, opened) {
			_ = directory.Close()
			return fmt.Errorf("ZIP directory identity changed during inspection: %s", name)
		}
		children, readErr := directory.ReadDir(-1)
		closeErr := directory.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		slices.SortFunc(children, func(a, b fs.DirEntry) int {
			return strings.Compare(a.Name(), b.Name())
		})
		for _, child := range children {
			if !utf8.ValidString(child.Name()) {
				return fmt.Errorf("invalid UTF-8 filesystem name in ZIP source: %q", child.Name())
			}
			physical++
			childRelative := child.Name()
			if relative != "" {
				childRelative = path.Join(relative, child.Name())
			}
			childName := path.Join(base, childRelative)
			childHidden := hidden || strings.HasPrefix(child.Name(), ".")
			info, err := root.Lstat(childName)
			if err != nil {
				return err
			}
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				return fmt.Errorf("symbolic link in ZIP source: %s", childRelative)
			case info.IsDir():
				if err := walk(childName, childRelative, childHidden); err != nil {
					return err
				}
			case info.Mode().IsRegular():
				if !childHidden {
					entries = append(entries, zipEntry{
						relative: childRelative,
						member:   path.Join(base, childRelative),
						info:     info,
					})
				}
			default:
				return fmt.Errorf("special file in ZIP source: %s", childRelative)
			}
		}
		return nil
	}
	if err := walk(base, "", false); err != nil {
		return nil, physical, err
	}
	return entries, physical, nil
}

func validateZIPMembers(entries []zipEntry) error {
	if len(entries) > maxZIPEntries {
		return fmt.Errorf("ZIP contains too many members: %d", len(entries))
	}
	exact, folded := map[string]bool{}, map[string]string{}
	nameBytes := 0
	fold := cases.Fold()
	for _, entry := range entries {
		name := entry.member
		nameBytes += len(name)
		if nameBytes > maxZIPNameBytes {
			return errors.New("ZIP member metadata exceeds limit")
		}
		if len(name) > 0xffff || !utf8.ValidString(name) || !fs.ValidPath(name) ||
			strings.Contains(name, "\\") || path.Clean(name) != name {
			return fmt.Errorf("unsafe ZIP member name %q", name)
		}
		for _, component := range strings.Split(name, "/") {
			if component == "." || component == ".." || component == "" {
				return fmt.Errorf("unsafe ZIP member component in %q", name)
			}
		}
		if exact[name] {
			return fmt.Errorf("duplicate ZIP member name %q", name)
		}
		exact[name] = true
		key := fold.String(norm.NFC.String(name))
		if previous, exists := folded[key]; exists {
			return fmt.Errorf("case-folding ZIP member collision between %q and %q", previous, name)
		}
		folded[key] = name
	}
	return nil
}

func verifyPublishedZIPSource(root *os.Root, base string, policy nativeVersionPolicy) error {
	var identity Manifest
	if err := storage.ReadJSON(root, path.Join(base, "manifest.json"), &identity); err != nil {
		return fmt.Errorf("ZIP source is not a recognized published native library: %w", err)
	}
	component, err := storage.SafeComponent(identity.Version)
	if err != nil || component != base {
		return fmt.Errorf("published library version does not match its directory name %q", base)
	}
	if policy.output == "" {
		policy, err = defaultNativeVersionPolicy()
		if err != nil {
			return err
		}
	}
	if err := policy.supports(identity.ApplicationVersion); err != nil {
		return fmt.Errorf("published library application version is incompatible: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, identity.UpdatedAt); err != nil {
		return fmt.Errorf("invalid published library update timestamp: %w", err)
	}
	verifier := &Run{root: root, platform: identity.Platform, version: identity.Version, versionPolicy: policy}
	if _, err := verifier.readLibraryForZIP(base); err != nil {
		return err
	}
	var history []History
	if err := storage.ReadJSON(root, path.Join(base, "history.json"), &history); err != nil {
		return fmt.Errorf("invalid published library history: %w", err)
	}
	if len(history) == 0 {
		return errors.New("published library history is empty")
	}
	for _, record := range history {
		if _, err := time.Parse(time.RFC3339Nano, record.At); err != nil ||
			strings.TrimSpace(record.ApplicationVersion) == "" || record.Changes == nil {
			return errors.New("invalid published library history record")
		}
		if err := policy.supports(record.ApplicationVersion); err != nil {
			return fmt.Errorf("invalid published library history application version: %w", err)
		}
	}
	return nil
}

func hashZIPSource(ctx context.Context, root *os.Root, name string, expected os.FileInfo) (string, int64, error) {
	return copyZIPSource(ctx, root, name, expected, io.Discard)
}

func copyZIPSource(ctx context.Context, root *os.Root, name string, expected os.FileInfo, destination io.Writer) (string, int64, error) {
	current, err := root.Lstat(name)
	if err != nil {
		return "", 0, err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(expected, current) {
		return "", 0, fmt.Errorf("published file identity changed: %s", name)
	}
	source, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	opened, statErr := source.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(current, opened) {
		_ = source.Close()
		return "", 0, fmt.Errorf("published file identity changed while opening: %s", name)
	}
	hash := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(destination, hash), contextReader{ctx: ctx, reader: source})
	closeErr := source.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", count, err
	}
	if count != expected.Size() {
		return "", count, fmt.Errorf("published file size changed: %s", name)
	}
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := r.reader.Read(buffer)
	if cancelled := r.ctx.Err(); cancelled != nil {
		return count, cancelled
	}
	return count, err
}

func validateTemporaryZIP(ctx context.Context, root *os.Root, name string, entries []zipEntry) error {
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return statErr
	}
	reader, err := stdzip.NewReader(file, info.Size())
	if err != nil {
		_ = file.Close()
		return err
	}
	if len(reader.File) != len(entries) {
		_ = file.Close()
		return errors.New("temporary ZIP central directory member count mismatch")
	}
	for index, member := range reader.File {
		if member.Name != entries[index].member || member.FileInfo().IsDir() {
			_ = file.Close()
			return errors.New("temporary ZIP central directory member mismatch")
		}
		body, err := member.Open()
		if err != nil {
			_ = file.Close()
			return err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, contextReader{ctx: ctx, reader: body})
		closeErr := body.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			_ = file.Close()
			return err
		}
		if size != entries[index].size || hex.EncodeToString(hash.Sum(nil)) != entries[index].hash {
			_ = file.Close()
			return fmt.Errorf("temporary ZIP member integrity mismatch: %s", member.Name)
		}
	}
	return file.Close()
}
