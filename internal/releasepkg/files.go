package releasepkg

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type ChecksumVerification struct {
	ChecksumsSHA256 string
	FileCount       int
	Entries         map[string]string
}

func fileIdentity(root, path string) (FileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return FileIdentity{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return FileIdentity{}, fmt.Errorf("package file must be a nonsymlink regular file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return FileIdentity{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return FileIdentity{}, fmt.Errorf("file identity changed while opening: %s", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return FileIdentity{}, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return FileIdentity{}, fmt.Errorf("file identity changed while hashing: %s", path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return FileIdentity{}, err
	}
	relative, err = cleanArchivePath(relative)
	if err != nil {
		return FileIdentity{}, err
	}
	return FileIdentity{
		Path:   relative,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
		Size:   info.Size(),
		Mode:   fmt.Sprintf("%04o", info.Mode().Perm()),
	}, nil
}

func hashFile(path string) (ArtifactIdentity, error) {
	identity, err := fileIdentity(filepath.Dir(path), path)
	if err != nil {
		return ArtifactIdentity{}, err
	}
	return ArtifactIdentity{Path: filepath.Base(path), SHA256: identity.SHA256, Size: identity.Size}, nil
}

func collectFiles(root string, excludes map[string]bool) ([]FileIdentity, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var files []FileIdentity
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative, err = cleanArchivePath(relative)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in a release package: %s", relative)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("special files are not allowed in a release package: %s", relative)
		}
		if excludes[relative] {
			return nil
		}
		identity, err := fileIdentity(root, path)
		if err != nil {
			return err
		}
		files = append(files, identity)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func cleanArchivePath(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) || strings.ContainsRune(path, '\x00') ||
		strings.ContainsAny(path, "\r\n") || strings.Contains(path, `\`) {
		return "", fmt.Errorf("unsafe package path %q", path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("unsafe package path %q", path)
	}
	return clean, nil
}

func parseChecksums(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries := make(map[string]string)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		if len(text) < 67 || text[64] != ' ' || (text[65] != ' ' && text[65] != '*') {
			return nil, fmt.Errorf("invalid SHA256SUMS line %d", line)
		}
		digest := strings.ToLower(text[:64])
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("invalid SHA-256 on line %d", line)
		}
		raw := strings.TrimPrefix(text[66:], "./")
		clean, err := cleanArchivePath(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid SHA256SUMS path on line %d: %w", line, err)
		}
		if _, exists := entries[clean]; exists {
			return nil, fmt.Errorf("duplicate SHA256SUMS path %q", clean)
		}
		entries[clean] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("SHA256SUMS contains no files")
	}
	return entries, nil
}

func VerifyChecksums(root, checksums string, requiredSubtrees []string) (ChecksumVerification, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return ChecksumVerification{}, err
	}
	absoluteChecksums, err := filepath.Abs(checksums)
	if err != nil {
		return ChecksumVerification{}, err
	}
	relativeChecksums, err := filepath.Rel(absoluteRoot, absoluteChecksums)
	if err != nil {
		return ChecksumVerification{}, err
	}
	relativeChecksums, err = cleanArchivePath(relativeChecksums)
	if err != nil || strings.HasPrefix(relativeChecksums, "../") {
		return ChecksumVerification{}, fmt.Errorf("checksum file must be inside source root")
	}
	entries, err := parseChecksums(absoluteChecksums)
	if err != nil {
		return ChecksumVerification{}, err
	}
	actual, err := collectFiles(absoluteRoot, map[string]bool{relativeChecksums: true})
	if err != nil {
		return ChecksumVerification{}, err
	}
	if len(actual) != len(entries) {
		return ChecksumVerification{}, fmt.Errorf(
			"checksum file set has %d entries but source root has %d files", len(entries), len(actual),
		)
	}
	for _, file := range actual {
		expected, ok := entries[file.Path]
		if !ok {
			return ChecksumVerification{}, fmt.Errorf("source file is missing from SHA256SUMS: %s", file.Path)
		}
		if expected != file.SHA256 {
			return ChecksumVerification{}, fmt.Errorf(
				"source file SHA-256 mismatch for %s: got %s expected %s", file.Path, file.SHA256, expected,
			)
		}
	}
	for _, subtree := range requiredSubtrees {
		absoluteSubtree, err := filepath.Abs(subtree)
		if err != nil {
			return ChecksumVerification{}, err
		}
		relative, err := filepath.Rel(absoluteRoot, absoluteSubtree)
		if err != nil {
			return ChecksumVerification{}, err
		}
		relative, err = cleanArchivePath(relative)
		if err != nil {
			return ChecksumVerification{}, fmt.Errorf("required source subtree is outside checksum root: %s", subtree)
		}
		prefix := relative + "/"
		found := false
		for path := range entries {
			if path == relative || strings.HasPrefix(path, prefix) {
				found = true
				break
			}
		}
		if !found {
			return ChecksumVerification{}, fmt.Errorf("required source subtree is absent from SHA256SUMS: %s", relative)
		}
	}
	checksumIdentity, err := hashFile(absoluteChecksums)
	if err != nil {
		return ChecksumVerification{}, err
	}
	return ChecksumVerification{
		ChecksumsSHA256: checksumIdentity.SHA256,
		FileCount:       len(entries),
		Entries:         entries,
	}, nil
}

func WriteChecksums(root, destination string, excludes []string) error {
	excluded := make(map[string]bool, len(excludes)+1)
	for _, path := range excludes {
		clean, err := cleanArchivePath(path)
		if err != nil {
			return err
		}
		excluded[clean] = true
	}
	relativeDestination, err := filepath.Rel(root, destination)
	if err != nil {
		return err
	}
	relativeDestination, err = cleanArchivePath(relativeDestination)
	if err != nil {
		return err
	}
	excluded[relativeDestination] = true
	files, err := collectFiles(root, excluded)
	if err != nil {
		return err
	}
	var builder strings.Builder
	for _, file := range files {
		fmt.Fprintf(&builder, "%s  ./%s\n", file.SHA256, file.Path)
	}
	return writeFile(destination, []byte(builder.String()), 0o644)
}

func HashSourceList(repository, listPath string) (string, int, error) {
	data, err := os.ReadFile(listPath)
	if err != nil {
		return "", 0, err
	}
	rawPaths := bytes.Split(data, []byte{0})
	hash := sha256.New()
	previous := ""
	count := 0
	for _, raw := range rawPaths {
		if len(raw) == 0 {
			continue
		}
		path, err := cleanArchivePath(string(raw))
		if err != nil {
			return "", 0, err
		}
		if path <= previous {
			return "", 0, fmt.Errorf("source file list is not strictly sorted at %q", path)
		}
		previous = path
		identity, err := fileIdentity(repository, filepath.Join(repository, filepath.FromSlash(path)))
		if err != nil {
			return "", 0, fmt.Errorf("source file %s: %w", path, err)
		}
		fmt.Fprintf(hash, "%s\x00%s\x00%d\x00%s\x00", identity.Path, identity.Mode, identity.Size, identity.SHA256)
		count++
	}
	if count == 0 {
		return "", 0, fmt.Errorf("source file list contains no files")
	}
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

func writeFile(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := firstError(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func parseMode(value string) (fs.FileMode, error) {
	parsed, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, err
	}
	return fs.FileMode(parsed), nil
}
