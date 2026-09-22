package releasepkg

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func CreateZIP(bundle, destination, rootName string, epoch int64) error {
	if _, err := cleanArchivePath(rootName); err != nil || strings.Contains(rootName, "/") {
		return fmt.Errorf("invalid ZIP root name %q", rootName)
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("refusing to replace existing ZIP: %s", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	stamp := deterministicTime(epoch)
	if stamp.Before(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)) {
		return fmt.Errorf("ZIP timestamp predates 1980")
	}
	files, err := collectFiles(bundle, nil)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	writer := zip.NewWriter(output)
	var writeErr error
	for _, file := range files {
		mode, err := parseMode(file.Mode)
		if err != nil {
			writeErr = err
			break
		}
		header := &zip.FileHeader{
			Name:     rootName + "/" + file.Path,
			Method:   zip.Deflate,
			Modified: stamp,
		}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			writeErr = err
			break
		}
		input, err := os.Open(filepath.Join(bundle, filepath.FromSlash(file.Path)))
		if err != nil {
			writeErr = err
			break
		}
		_, copyErr := io.Copy(entry, input)
		closeErr := input.Close()
		if err := firstError(copyErr, closeErr); err != nil {
			writeErr = err
			break
		}
	}
	closeZipErr := writer.Close()
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := firstError(writeErr, closeZipErr, syncErr, closeErr); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func VerifyAndExtractZIP(zipPath, extractRoot, rootName string, epoch int64) (string, error) {
	if _, err := cleanArchivePath(rootName); err != nil || strings.Contains(rootName, "/") {
		return "", fmt.Errorf("invalid ZIP root name %q", rootName)
	}
	if _, err := os.Lstat(extractRoot); err == nil {
		return "", fmt.Errorf("refusing to use existing ZIP extraction root: %s", extractRoot)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if len(reader.File) == 0 {
		return "", fmt.Errorf("release ZIP contains no files")
	}
	if len(reader.File) > 100_000 {
		return "", fmt.Errorf("release ZIP has too many files: %d", len(reader.File))
	}
	stamp := deterministicTime(epoch)
	seen := make(map[string]bool, len(reader.File))
	previous := ""
	prefix := rootName + "/"
	var totalSize uint64
	for _, file := range reader.File {
		if file.Name <= previous {
			return "", fmt.Errorf("ZIP entries are not strictly sorted: %q after %q", file.Name, previous)
		}
		previous = file.Name
		if seen[file.Name] {
			return "", fmt.Errorf("duplicate ZIP entry: %s", file.Name)
		}
		seen[file.Name] = true
		if !strings.HasPrefix(file.Name, prefix) {
			return "", fmt.Errorf("ZIP entry is outside package root: %s", file.Name)
		}
		relative, err := cleanArchivePath(strings.TrimPrefix(file.Name, prefix))
		if err != nil {
			return "", err
		}
		if relative != strings.TrimPrefix(file.Name, prefix) {
			return "", fmt.Errorf("noncanonical ZIP entry path: %s", file.Name)
		}
		if !file.Mode().IsRegular() || file.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("ZIP entry is not a regular file: %s", file.Name)
		}
		if file.UncompressedSize64 > 4<<30 || totalSize > (4<<30)-file.UncompressedSize64 {
			return "", fmt.Errorf("release ZIP exceeds the 4 GiB extraction limit")
		}
		totalSize += file.UncompressedSize64
		if !file.Modified.UTC().Equal(stamp) {
			return "", fmt.Errorf("ZIP entry timestamp differs from deterministic policy: %s", file.Name)
		}
	}
	if err := os.Mkdir(extractRoot, 0o700); err != nil {
		return "", err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(extractRoot)
		}
	}()
	bundle := filepath.Join(extractRoot, rootName)
	if err := os.Mkdir(bundle, 0o755); err != nil {
		return "", err
	}
	for _, file := range reader.File {
		relative := strings.TrimPrefix(file.Name, prefix)
		destination := filepath.Join(bundle, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return "", err
		}
		input, err := file.Open()
		if err != nil {
			return "", err
		}
		output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return "", err
		}
		_, copyErr := io.Copy(output, input)
		syncErr := output.Sync()
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		if err := firstError(copyErr, syncErr, closeOutputErr, closeInputErr); err != nil {
			return "", err
		}
	}
	if err := VerifyBundle(bundle); err != nil {
		return "", err
	}
	success = true
	return bundle, nil
}

func WriteDetachedProvenance(
	bundle, zipPath, provenancePath, shaPath string,
	extractedSmoke bool,
) (DetachedProvenance, error) {
	if err := VerifyBundle(bundle); err != nil {
		return DetachedProvenance{}, err
	}
	manifest, err := loadManifest(bundle)
	if err != nil {
		return DetachedProvenance{}, err
	}
	zipIdentity, err := hashFile(zipPath)
	if err != nil {
		return DetachedProvenance{}, err
	}
	manifestIdentity, err := hashFile(filepath.Join(bundle, ManifestName))
	if err != nil {
		return DetachedProvenance{}, err
	}
	checksumsIdentity, err := hashFile(filepath.Join(bundle, ChecksumsName))
	if err != nil {
		return DetachedProvenance{}, err
	}
	sbomIdentity, err := hashFile(filepath.Join(bundle, SBOMName))
	if err != nil {
		return DetachedProvenance{}, err
	}
	noticesIdentity, err := hashFile(filepath.Join(bundle, NoticesName))
	if err != nil {
		return DetachedProvenance{}, err
	}
	shaLine := fmt.Sprintf("%s  %s\n", zipIdentity.SHA256, filepath.Base(zipPath))
	if err := writeFile(shaPath, []byte(shaLine), 0o644); err != nil {
		return DetachedProvenance{}, err
	}
	provenance := DetachedProvenance{
		SchemaVersion: ProvenanceSchema,
		Application:   "aos-cx-docs-dldr", ApplicationVersion: manifest.ApplicationVersion,
		Source: manifest.Source, TrustStatus: TrustStatus,
		Signed: false, Notarized: false,
		ZIP: zipIdentity, Checksums: checksumsIdentity, Manifest: manifestIdentity,
		SBOM: sbomIdentity, Notices: noticesIdentity,
		ExtractedZIPSmoke: extractedSmoke,
		TimestampUTC:      manifest.Build.TimestampUTC,
	}
	encoded, err := json.MarshalIndent(provenance, "", "  ")
	if err != nil {
		return DetachedProvenance{}, err
	}
	encoded = append(encoded, '\n')
	if err := writeFile(provenancePath, encoded, 0o644); err != nil {
		return DetachedProvenance{}, err
	}
	return provenance, nil
}

func CompareFiles(first, second string) error {
	firstFiles, err := collectFiles(first, nil)
	if err != nil {
		return err
	}
	secondFiles, err := collectFiles(second, nil)
	if err != nil {
		return err
	}
	sort.Slice(firstFiles, func(i, j int) bool { return firstFiles[i].Path < firstFiles[j].Path })
	sort.Slice(secondFiles, func(i, j int) bool { return secondFiles[i].Path < secondFiles[j].Path })
	if len(firstFiles) != len(secondFiles) {
		return fmt.Errorf("file counts differ: %d != %d", len(firstFiles), len(secondFiles))
	}
	for i := range firstFiles {
		if firstFiles[i] != secondFiles[i] {
			return fmt.Errorf("file identity differs: %+v != %+v", firstFiles[i], secondFiles[i])
		}
	}
	return nil
}
