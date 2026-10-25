package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"slices"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func SafePath(name string) error {
	if name == "." || !fs.ValidPath(name) || strings.Contains(name, "\\") {
		return fmt.Errorf("unsafe managed path %q", name)
	}
	return nil
}

func Check(root *os.Root, name string) error {
	if err := SafePath(name); err != nil {
		return err
	}
	prefix := ""
	for _, part := range strings.Split(name, "/") {
		prefix = path.Join(prefix, part)
		info, err := root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe symlink or special managed entry: %s", prefix)
		}
	}
	return nil
}

func EnsureDir(root *os.Root, name string) error {
	dir, err := EnsureDirRoot(root, name)
	if err != nil {
		return err
	}
	return dir.Close()
}

// EnsureDirRoot creates the managed directory name as needed and returns it
// opened as in OpenDir. The caller closes the result.
func EnsureDirRoot(root *os.Root, name string) (*os.Root, error) {
	if err := SafePath(name); err != nil {
		return nil, err
	}
	return openManagedDir(root, name, true)
}

// OpenDir opens the managed directory name, or "." for root itself, as a root
// of its own. Each component is opened singly and must keep the identity that
// Lstat reported for it, so a symlink at any component is refused even where
// os.Root would follow it because it resolves inside root. Work through the
// result is anchored to the opened directory: a later rename or symlink swap
// of any component cannot redirect it. The caller closes the result.
func OpenDir(root *os.Root, name string) (*os.Root, error) {
	return openManagedDir(root, name, false)
}

func openManagedDir(root *os.Root, name string, create bool) (*os.Root, error) {
	if name != "." {
		if err := SafePath(name); err != nil {
			return nil, err
		}
	}
	current, err := root.OpenRoot(".")
	if err != nil || name == "." {
		return current, err
	}
	prefix := ""
	for _, part := range strings.Split(name, "/") {
		prefix = path.Join(prefix, part)
		next, err := openChildDir(current, part, prefix, create)
		if closeErr := current.Close(); closeErr != nil && err == nil {
			err = errors.Join(closeErr, next.Close())
		}
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openChildDir(parent *os.Root, name, display string, create bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if create && errors.Is(err, os.ErrNotExist) {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	return openVerifiedDir(parent, name, display, info)
}

// openVerifiedDir opens the single entry name of parent, which Lstat reported
// as info, and refuses it unless the opened directory is that same node.
func openVerifiedDir(parent *os.Root, name, display string, info os.FileInfo) (*os.Root, error) {
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return nil, fmt.Errorf("unsafe symlink or special managed entry: %s", display)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("managed path is not a directory: %s", display)
	}
	dir, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := dir.Stat(".")
	if err != nil || !sameManagedNode(info, opened) {
		return nil, errors.Join(fmt.Errorf("managed directory identity changed while opening: %s", display), err, dir.Close())
	}
	return dir, nil
}

func readManagedDir(dir *os.Root) ([]os.DirEntry, error) {
	f, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	entries, readErr := f.ReadDir(-1)
	return entries, errors.Join(readErr, f.Close())
}

func Atomic(root *os.Root, name string, data []byte) (err error) {
	if err := Check(root, name); err != nil {
		return err
	}
	dir, err := OpenDir(root, path.Dir(name))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	temp := "." + rand.Text() + ".part"
	f, err := dir.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := dir.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, writeErr := f.Write(data)
	syncErr, closeErr := f.Sync(), f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := dir.Rename(temp, path.Base(name)); err != nil {
		return err
	}
	return SyncDir(dir, ".")
}

func WriteJSON(root *os.Root, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return Atomic(root, name, append(data, '\n'))
}

// MaxManagedJSON bounds a manifest or index the application reads back. A
// release manifest carries every HTML guide's archive record, so it grows with
// the release's total topics: the 10.18 Command Line Interface Guide alone
// needs about 14 MiB.
const MaxManagedJSON = 128 << 20

func ReadJSON(root *os.Root, name string, value any) error {
	if err := Check(root, name); err != nil {
		return err
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxManagedJSON+1))
	if err != nil {
		return err
	}
	if len(data) > MaxManagedJSON {
		return errors.New("managed JSON exceeds limit")
	}
	return json.Unmarshal(data, value)
}

func SyncDir(root *os.Root, name string) error {
	if runtime.GOOS == "windows" {
		return nil // Windows directory handles do not support File.Sync.
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func Hash(root *os.Root, name string, limit int64) (string, int64, error) {
	if err := Check(root, name); err != nil {
		return "", 0, err
	}
	f, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err == nil && n > limit {
		err = errors.New("managed file exceeds declared size")
	}
	return hex.EncodeToString(h.Sum(nil)), n, err
}

type CopyFilter func(name string, info fs.FileInfo) (bool, error)
type treeIdentityHook func(point, name string) error

func CopyTree(root *os.Root, from, to string) error {
	return CopyTreeFiltered(root, from, to, nil)
}

func TreeDigest(ctx context.Context, root *os.Root, name string) (string, error) {
	return treeDigestWithHook(ctx, root, name, nil)
}

func treeDigestWithHook(ctx context.Context, root *os.Root, name string, hook treeIdentityHook) (string, error) {
	if err := Check(root, name); err != nil {
		return "", err
	}
	h := sha256.New()
	if err := writeDigestFrame(h, []byte("aos-cx-docs-dldr-managed-tree-v1")); err != nil {
		return "", err
	}
	folded := map[string]string{}
	if err := digestTree(ctx, root, name, ".", folded, h, hook); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func digestTree(
	ctx context.Context,
	root *os.Root,
	name, relative string,
	folded map[string]string,
	h io.Writer,
	hook treeIdentityHook,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	entryType := "file"
	size := info.Size()
	if info.IsDir() {
		entryType = "directory"
		size = 0
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular managed file or directory: %s", name)
	}
	if relative != "." {
		key := foldedManagedPath(relative)
		if previous, exists := folded[key]; exists {
			return fmt.Errorf("case-fold or normalization collision between %q and %q", previous, relative)
		}
		folded[key] = relative
	}
	for _, field := range []string{relative, entryType, fmt.Sprintf("%#x", digestMode(info.Mode())), fmt.Sprintf("%d", size)} {
		if err := writeDigestFrame(h, []byte(field)); err != nil {
			return err
		}
	}
	if info.IsDir() {
		if err := callTreeIdentityHook(hook, "digest-directory-before-open", name); err != nil {
			return err
		}
		dir, err := root.Open(name)
		if err != nil {
			return err
		}
		opened, statErr := dir.Stat()
		if statErr != nil || !sameManagedIdentity(info, opened) {
			return errors.Join(
				fmt.Errorf("managed directory identity changed before read: %s", name),
				statErr,
				dir.Close(),
			)
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if err := callTreeIdentityHook(hook, "digest-directory-after-read", name); err != nil {
			return err
		}
		if err := verifyManagedPathIdentity(root, name, info); err != nil {
			return err
		}
		slices.SortFunc(entries, func(left, right os.DirEntry) int {
			return strings.Compare(left.Name(), right.Name())
		})
		for _, entry := range entries {
			childRelative := entry.Name()
			if relative != "." {
				childRelative = path.Join(relative, entry.Name())
			}
			if err := SafePath(childRelative); err != nil {
				return err
			}
			if err := digestTree(ctx, root, path.Join(name, entry.Name()), childRelative, folded, h, hook); err != nil {
				return err
			}
		}
		return verifyManagedPathIdentity(root, name, info)
	}
	if err := callTreeIdentityHook(hook, "digest-file-before-open", name); err != nil {
		return err
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameManagedIdentity(info, opened) {
		return errors.Join(fmt.Errorf("managed file identity changed before read: %s", name), err)
	}
	buffer := make([]byte, 64<<10)
	var copied int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if _, err := h.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if copied != size {
		return fmt.Errorf("managed file size changed while hashing: %s", name)
	}
	if err := callTreeIdentityHook(hook, "digest-file-after-read", name); err != nil {
		return err
	}
	openedAfter, err := file.Stat()
	if err != nil || !sameManagedIdentity(info, openedAfter) {
		return errors.Join(fmt.Errorf("managed file identity changed while hashing: %s", name), err)
	}
	return verifyManagedPathIdentity(root, name, info)
}

func writeDigestFrame(writer io.Writer, value []byte) error {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	if _, err := writer.Write(length[:]); err != nil {
		return err
	}
	_, err := writer.Write(value)
	return err
}

func digestMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func foldedManagedPath(name string) string {
	return cases.Fold().String(norm.NFC.String(name))
}

func sameManagedNode(expected, current os.FileInfo) bool {
	return expected != nil && current != nil &&
		os.SameFile(expected, current) &&
		expected.Mode().Type() == current.Mode().Type()
}

func sameManagedIdentity(expected, current os.FileInfo) bool {
	return sameManagedNode(expected, current) &&
		expected.Mode() == current.Mode() &&
		expected.Size() == current.Size()
}

func verifyManagedPathIdentity(root *os.Root, name string, expected os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil || !sameManagedIdentity(expected, current) {
		return errors.Join(fmt.Errorf("managed path identity, mode, or size changed: %s", name), err)
	}
	return nil
}

func callTreeIdentityHook(hook treeIdentityHook, point, name string) error {
	if hook == nil {
		return nil
	}
	return hook(point, name)
}

func CopyTreeFiltered(root *os.Root, from, to string, filter CopyFilter) error {
	if err := Check(root, from); err != nil {
		return err
	}
	if err := SafePath(to); err != nil {
		return err
	}
	source, err := OpenDir(root, path.Dir(from))
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Lstat(path.Base(from))
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe symlink or special managed entry: %s", from)
	}
	if filter != nil {
		include, err := filter(from, info)
		if err != nil || !include {
			return err
		}
	}
	if info.IsDir() {
		sourceDir, err := openVerifiedDir(source, path.Base(from), from, info)
		if err != nil {
			return err
		}
		defer sourceDir.Close()
		target, err := EnsureDirRoot(root, to)
		if err != nil {
			return err
		}
		defer target.Close()
		return copyTreeEntries(sourceDir, target, from, filter)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular managed file: %s", from)
	}
	if err := Check(root, to); err != nil {
		return err
	}
	target, err := OpenDir(root, path.Dir(to))
	if err != nil {
		return err
	}
	defer target.Close()
	return copyManagedFile(source, path.Base(from), from, info, target, path.Base(to))
}

// copyTreeEntries copies the contents of source into target. Both are opened
// directories and every child is reached by its single name within them, so
// no path is resolved again from the shared root while copying.
func copyTreeEntries(source, target *os.Root, name string, filter CopyFilter) error {
	entries, err := readManagedDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := path.Join(name, entry.Name())
		info, err := source.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("unsafe symlink or special managed entry: %s", child)
		}
		if filter != nil {
			include, err := filter(child, info)
			if err != nil {
				return err
			}
			if !include {
				continue
			}
		}
		if !info.IsDir() {
			if err := copyManagedFile(source, entry.Name(), child, info, target, entry.Name()); err != nil {
				return err
			}
			continue
		}
		sourceDir, err := openVerifiedDir(source, entry.Name(), child, info)
		if err != nil {
			return err
		}
		targetDir, err := openChildDir(target, entry.Name(), child, true)
		if err != nil {
			return errors.Join(err, sourceDir.Close())
		}
		copyErr := copyTreeEntries(sourceDir, targetDir, child, filter)
		if err := errors.Join(copyErr, sourceDir.Close(), targetDir.Close()); err != nil {
			return err
		}
	}
	return SyncDir(target, ".")
}

func copyManagedFile(source *os.Root, name, display string, info os.FileInfo, target *os.Root, targetName string) error {
	src, err := source.Open(name)
	if err != nil {
		return err
	}
	defer src.Close()
	opened, err := src.Stat()
	if err != nil || !sameManagedNode(info, opened) {
		return errors.Join(fmt.Errorf("managed file identity changed before copy: %s", display), err)
	}
	dst, err := target.OpenFile(targetName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	syncErr, closeErr := dst.Sync(), dst.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

// RemoveTree removes name and everything below it. It descends through opened
// directories and removes each entry by its single name in a verified parent,
// so a symlink swapped into the tree, even one resolving inside root, cannot
// redirect the removal.
func RemoveTree(root *os.Root, name string) error {
	if err := Check(root, name); err != nil {
		return err
	}
	parent, err := OpenDir(root, path.Dir(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.Join(removeTreeEntry(parent, path.Base(name), name), parent.Close())
}

// Remove removes the single entry name, an empty directory or a non-directory,
// from its parent opened as in OpenDir, so a symlink swapped into a parent
// component cannot redirect the removal. A symlink at name itself is removed,
// never followed.
func Remove(root *os.Root, name string) error {
	if err := SafePath(name); err != nil {
		return err
	}
	parent, err := OpenDir(root, path.Dir(name))
	if err != nil {
		return err
	}
	return errors.Join(parent.Remove(path.Base(name)), parent.Close())
}

func removeTreeEntry(parent *os.Root, name, display string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
		return fmt.Errorf("unsafe symlink or special managed entry: %s", display)
	}
	if info.IsDir() {
		dir, err := openVerifiedDir(parent, name, display, info)
		if err != nil {
			return err
		}
		entries, err := readManagedDir(dir)
		for _, entry := range entries {
			if err != nil {
				break
			}
			err = removeTreeEntry(dir, entry.Name(), path.Join(display, entry.Name()))
		}
		if err := errors.Join(err, dir.Close()); err != nil {
			return err
		}
	}
	current, err := parent.Lstat(name)
	if err != nil || !sameManagedNode(info, current) {
		return errors.Join(fmt.Errorf("managed path identity changed before removal: %s", display), err)
	}
	return parent.Remove(name)
}
