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
	if err := SafePath(name); err != nil {
		return err
	}
	prefix := ""
	for _, part := range strings.Split(name, "/") {
		prefix = path.Join(prefix, part)
		if err := Check(root, prefix); err != nil {
			return err
		}
		if err := root.Mkdir(prefix, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := root.Stat(prefix)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("managed path is not a directory: %s", prefix)
		}
	}
	return nil
}

func Atomic(root *os.Root, name string, data []byte) (err error) {
	if err := Check(root, name); err != nil {
		return err
	}
	temp := path.Join(path.Dir(name), "."+rand.Text()+".part")
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := root.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	_, writeErr := f.Write(data)
	syncErr, closeErr := f.Sync(), f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	return SyncDir(root, path.Dir(name))
}

func WriteJSON(root *os.Root, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return Atomic(root, name, append(data, '\n'))
}

func ReadJSON(root *os.Root, name string, value any) error {
	if err := Check(root, name); err != nil {
		return err
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
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
	info, err := root.Lstat(from)
	if err != nil {
		return err
	}
	if filter != nil {
		include, err := filter(from, info)
		if err != nil || !include {
			return err
		}
	}
	if info.IsDir() {
		if err := EnsureDir(root, to); err != nil {
			return err
		}
		dir, err := root.Open(from)
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := CopyTreeFiltered(root, path.Join(from, entry.Name()), path.Join(to, entry.Name()), filter); err != nil {
				return err
			}
		}
		return SyncDir(root, to)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular managed file: %s", from)
	}
	if err := Check(root, to); err != nil {
		return err
	}
	src, err := root.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := root.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	syncErr, closeErr := dst.Sync(), dst.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

func RemoveTree(root *os.Root, name string) error {
	if err := Check(root, name); err != nil {
		return err
	}
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		dir, err := root.Open(name)
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		for _, entry := range entries {
			if err := RemoveTree(root, path.Join(name, entry.Name())); err != nil {
				return err
			}
		}
	}
	return root.Remove(name)
}
