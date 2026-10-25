package pdfgen

import (
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
	"sync"
)

// pinnedFileKind says how a pinned sidecar file may be used and therefore
// which permission bits it must carry.
type pinnedFileKind uint8

const (
	// pinnedData is a regular file without execute permission.
	pinnedData pinnedFileKind = iota
	// pinnedExecutable is a regular file with execute permission: the
	// executable itself and the shared libraries it loads.
	pinnedExecutable
	// pinnedOwnerOnlyMetadata is packaging metadata the sidecar never opens
	// and that upstream ships readable by its owner only (Chrome's Linux
	// deb.deps and rpm.deps). The container stages it root-owned while the
	// application runs unprivileged. A readable copy is hashed like any other
	// file; a copy the running user cannot open is accepted only with its
	// pinned size and an owner-only mode, because Chrome runs under the same
	// credentials and cannot read it either.
	pinnedOwnerOnlyMetadata
)

// sidecarFile is the pinned identity of one regular file in a sidecar
// directory.
type sidecarFile struct {
	// relative is the slash-separated path below the sidecar directory.
	relative string
	size     int64
	sha256   string
	kind     pinnedFileKind
}

// finderMetadataMaxBytes bounds the only unpinned file a sidecar directory may
// hold on macOS: a regular .DS_Store, which Finder writes into any directory a
// user browses and which no sidecar opens. It mirrors the library's bounded
// Finder metadata exception.
const finderMetadataMaxBytes = 1 << 20

// sidecarManifestSHA256 returns the canonical digest of a pinned directory
// table: SHA-256 over one "<sha256> <size> <kind> <path>\n" line per file in
// byte order of path, where kind is "x" (executable), "-" (data) or "o"
// (owner-only metadata). The digest names a table in documentation and tool
// output; verification always checks every file individually.
func sidecarManifestSHA256(files []sidecarFile) string {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b sidecarFile) int { return strings.Compare(a.relative, b.relative) })
	hash := sha256.New()
	for _, file := range sorted {
		kind := "-"
		switch file.kind {
		case pinnedExecutable:
			kind = "x"
		case pinnedOwnerOnlyMetadata:
			kind = "o"
		}
		fmt.Fprintf(hash, "%s %d %s %s\n", file.sha256, file.size, kind, file.relative)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// verifySidecarTree confirms that root holds exactly the pinned files. Every
// pinned path must be a regular file with its pinned size, execute permission
// and SHA-256; every directory must lead to a pinned file; nothing else may be
// present apart from bounded Finder metadata where the platform allows it.
// Symbolic links and special files are refused anywhere in the tree. Errors
// never wrap fs.ErrNotExist, so an incomplete directory is reported as such
// rather than as a missing sidecar.
func verifySidecarTree(label, root string, files []sidecarFile, finderMetadata bool) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("%s directory is unavailable: %s: %v", label, root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s directory must be a real directory: %s", label, root)
	}
	expected := make(map[string]sidecarFile, len(files))
	directories := map[string]bool{".": true}
	for _, file := range files {
		expected[file.relative] = file
		for dir := path.Dir(file.relative); dir != "."; dir = path.Dir(dir) {
			directories[dir] = true
		}
	}
	present := make(map[string]bool, len(files))
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%s directory cannot be listed: %v", label, walkErr)
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		entryInfo, err := entry.Info()
		if err != nil {
			return fmt.Errorf("%s directory entry is unavailable: %s: %v", label, relative, err)
		}
		mode := entryInfo.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			return fmt.Errorf("%s directory must not contain symbolic links: %s", label, relative)
		case mode.IsDir():
			if !directories[relative] {
				return fmt.Errorf("%s directory contains an unexpected directory: %s", label, relative)
			}
			return nil
		case !mode.IsRegular():
			return fmt.Errorf("%s directory contains a special file: %s", label, relative)
		}
		if _, ok := expected[relative]; ok {
			present[relative] = true
			return nil
		}
		if finderMetadata && entry.Name() == ".DS_Store" && entryInfo.Size() <= finderMetadataMaxBytes {
			return nil
		}
		return fmt.Errorf("%s directory contains an unexpected file: %s", label, relative)
	})
	if err != nil {
		return err
	}
	for _, file := range files {
		if !present[file.relative] {
			return fmt.Errorf("%s directory is incomplete; missing pinned file %s in %s", label, file.relative, root)
		}
	}
	for _, file := range files {
		if err := verifyPinnedFile(label, filepath.Join(root, filepath.FromSlash(file.relative)), file); err != nil {
			return err
		}
	}
	return nil
}

// verifyPinnedFile checks one pinned file's type, size, permissions and bytes.
func verifyPinnedFile(label, path string, pin sidecarFile) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s pinned file is unavailable: %s: %v", label, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s pinned file is not a regular file: %s", label, path)
	}
	if info.Size() != pin.size {
		return fmt.Errorf("%s pinned file size is %d, expected %d: %s", label, info.Size(), pin.size, path)
	}
	executable := info.Mode().Perm()&0o111 != 0
	if executable != (pin.kind == pinnedExecutable) {
		if executable {
			return fmt.Errorf("%s pinned data file must not be executable: %s", label, path)
		}
		return fmt.Errorf("%s pinned executable file is not executable: %s", label, path)
	}
	if pinnedContentCurrent(path, info, pin.sha256) {
		return nil
	}
	digest, stable, err := hashPinnedFile(label, path, info, nil)
	if err != nil {
		if pin.kind == pinnedOwnerOnlyMetadata && errors.Is(err, fs.ErrPermission) &&
			info.Mode().Perm()&0o077 == 0 {
			return nil
		}
		return err
	}
	if digest != pin.sha256 {
		return fmt.Errorf("%s pinned file SHA-256 is %s, expected %s: %s", label, digest, pin.sha256, path)
	}
	rememberPinnedContent(path, stable, digest)
	return nil
}

// maxPinnedFileReads bounds how often a file whose metadata keeps changing
// is read again.
const maxPinnedFileReads = 3

// hashPinnedFile hashes the regular file at path, which must still be the file
// described by info. inspect, when set, examines the opened file first. The
// digest is accepted only from a read that the file was unchanged across.
// Sync clients such as OneDrive and iCloud Drive write extended attributes of
// the files they upload, which moves only ctime; a file whose only change is
// its ctime is read again, up to maxPinnedFileReads times. The returned
// observation is the one the accepted read was stable against. An open
// failure is returned unwrapped so callers can recognise fs.ErrPermission and
// fs.ErrNotExist.
func hashPinnedFile(label, path string, info fs.FileInfo, inspect func(*os.File) error) (string, fs.FileInfo, error) {
	forgetPinnedContent(path)
	for reads := 1; ; reads++ {
		digest, current, err := readPinnedFile(label, path, info, inspect)
		if err != nil {
			return "", nil, err
		}
		if sameFileIdentity(info, current) {
			return digest, info, nil
		}
		if reads == maxPinnedFileReads || !onlyChangeTimeDiffers(info, current) {
			return "", nil, fmt.Errorf("%s identity changed during verification: %s", label, path)
		}
		info = current
	}
}

// readPinnedFile reads and hashes the file once and returns the digest with
// the file's observation after the read.
func readPinnedFile(label, path string, info fs.FileInfo, inspect func(*os.File) error) (string, fs.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", nil, fmt.Errorf("%s identity changed while opening: %s", label, path)
	}
	if inspect != nil {
		if err := inspect(file); err != nil {
			return "", nil, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return "", nil, err
		}
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", nil, fmt.Errorf("%s identity changed during verification: %s", label, path)
	}
	return hex.EncodeToString(hash.Sum(nil)), current, nil
}

// onlyChangeTimeDiffers reports whether two observations of the same file
// differ in nothing but ctime.
func onlyChangeTimeDiffers(a, b fs.FileInfo) bool {
	first, firstOK := fileSnapshotOf(a)
	second, secondOK := fileSnapshotOf(b)
	if !firstOK || !secondOK || !os.SameFile(a, b) {
		return false
	}
	second.changed = first.changed
	return first == second
}

// fileSnapshot is the cheap identity of a regular file. Writing a file changes
// its mtime and ctime, replacing it changes its inode, and an unprivileged
// user cannot set ctime back, so an identical snapshot means unchanged bytes
// for the purpose of re-verification between full hashes.
type fileSnapshot struct {
	device, inode uint64
	size          int64
	mode          fs.FileMode
	uid, gid      uint32
	modified      int64
	changed       int64
}

type verifiedContent struct {
	snapshot fileSnapshot
	sha256   string
}

// verifiedSidecarContent records, per absolute path, the snapshot of each
// sidecar file whose bytes matched their pin in this process. Each sidecar is
// therefore hashed in full once per process; later checks re-hash only files
// whose snapshot differs. Hosts without snapshot support always re-hash.
var verifiedSidecarContent = struct {
	sync.Mutex
	files map[string]verifiedContent
}{files: map[string]verifiedContent{}}

// pinnedContentCurrent reports whether the file described by info was already
// verified to hold expectedSHA256 and is unchanged since.
func pinnedContentCurrent(path string, info fs.FileInfo, expectedSHA256 string) bool {
	snapshot, ok := fileSnapshotOf(info)
	if !ok {
		return false
	}
	verifiedSidecarContent.Lock()
	defer verifiedSidecarContent.Unlock()
	cached, found := verifiedSidecarContent.files[path]
	return found && cached.sha256 == expectedSHA256 && cached.snapshot == snapshot
}

// rememberPinnedContent records that the file described by info, unchanged
// across hashing, holds digest.
func rememberPinnedContent(path string, info fs.FileInfo, digest string) {
	snapshot, ok := fileSnapshotOf(info)
	if !ok {
		return
	}
	verifiedSidecarContent.Lock()
	defer verifiedSidecarContent.Unlock()
	verifiedSidecarContent.files[path] = verifiedContent{snapshot: snapshot, sha256: digest}
}

func forgetPinnedContent(path string) {
	verifiedSidecarContent.Lock()
	defer verifiedSidecarContent.Unlock()
	delete(verifiedSidecarContent.files, path)
}

// sameFileIdentity reports whether two observations describe the same,
// unmodified file.
func sameFileIdentity(a, b fs.FileInfo) bool {
	if !os.SameFile(a, b) {
		return false
	}
	first, firstOK := fileSnapshotOf(a)
	second, secondOK := fileSnapshotOf(b)
	if firstOK && secondOK {
		return first == second
	}
	return a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}
