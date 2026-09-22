package releasepkg

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type ExtractionLimits struct {
	MaxArchiveBytes     int64
	MaxMembers          int
	MaxPathBytes        int
	MaxFileBytes        uint64
	MaxTotalBytes       uint64
	MaxTarTrailingBytes int64
}

var DefaultExtractionLimits = ExtractionLimits{
	MaxArchiveBytes:     2 << 30,
	MaxMembers:          100_000,
	MaxPathBytes:        4 << 10,
	MaxFileBytes:        4 << 30,
	MaxTotalBytes:       8 << 30,
	MaxTarTrailingBytes: 1 << 20,
}

type ExtractOptions struct {
	ArchivePath  string
	Destination  string
	Format       string
	SHA256       string
	AllowedRoots []string
	Selections   []ExtractSelection
	Limits       ExtractionLimits
}

type ExtractSelection struct {
	ArchivePath string
	OutputPath  string
}

type archiveMember struct {
	name     string
	output   string
	mode     fs.FileMode
	size     uint64
	isDir    bool
	isLink   bool
	linkname string
	explicit bool
}

func ExtractArchive(ctx context.Context, options ExtractOptions) error {
	if ctx == nil {
		return errors.New("archive extraction requires a context")
	}
	limits, err := extractionLimits(options.Limits)
	if err != nil {
		return err
	}
	expected, err := parseExpectedSHA256(options.SHA256)
	if err != nil {
		return err
	}
	allowed, err := validateAllowedRoots(options.AllowedRoots, limits.MaxPathBytes)
	if err != nil {
		return err
	}
	selections, err := validateSelections(options.Selections, allowed, limits.MaxPathBytes)
	if err != nil {
		return err
	}
	selective := len(selections) != 0
	format := strings.ToLower(options.Format)
	if format != "zip" && format != "tar.gz" {
		return fmt.Errorf("unsupported acquisition archive format %q", options.Format)
	}
	if options.ArchivePath == "" || options.Destination == "" {
		return errors.New("archive and destination paths are required")
	}
	if _, err := os.Lstat(options.Destination); err == nil {
		return fmt.Errorf("refusing to replace existing extraction destination: %s", options.Destination)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect extraction destination: %w", err)
	}

	info, err := os.Lstat(options.ArchivePath)
	if err != nil {
		return fmt.Errorf("inspect acquisition archive: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("acquisition archive must be an explicit nonsymlink regular file")
	}
	if info.Size() < 0 || info.Size() > limits.MaxArchiveBytes {
		return fmt.Errorf("acquisition archive exceeds the %d-byte limit", limits.MaxArchiveBytes)
	}
	input, err := os.Open(options.ArchivePath)
	if err != nil {
		return fmt.Errorf("open acquisition archive: %w", err)
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("acquisition archive identity changed while opening")
	}
	if err := verifyOpenFileHash(ctx, input, info.Size(), expected); err != nil {
		return err
	}

	var members []archiveMember
	switch format {
	case "zip":
		members, err = preflightZIP(ctx, input, info.Size(), allowed, limits, selective)
	case "tar.gz":
		members, err = preflightTarGZ(ctx, input, allowed, limits, selective)
	}
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return errors.New("acquisition archive contains no members")
	}
	members, err = prepareExtractionMembers(members, selections)
	if err != nil {
		return err
	}
	if err := verifyOpenFileHash(ctx, input, info.Size(), expected); err != nil {
		return err
	}
	if err := verifyArchiveIdentity(options.ArchivePath, input, info); err != nil {
		return err
	}

	stage, ownership, err := createPrivateStage(options.Destination)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			removeOwnedStage(stage, ownership.stage)
		}
	}()

	switch format {
	case "zip":
		err = extractZIP(ctx, input, info.Size(), stage, members, limits.MaxPathBytes, selective)
	case "tar.gz":
		err = extractTarGZ(
			ctx, input, stage, members, limits.MaxPathBytes, limits.MaxTarTrailingBytes, selective,
		)
	}
	if err != nil {
		return err
	}
	if err := syncExtractedTree(stage, members); err != nil {
		return err
	}
	if err := verifyOpenFileHash(ctx, input, info.Size(), expected); err != nil {
		return err
	}
	if err := verifyArchiveIdentity(options.ArchivePath, input, info); err != nil {
		return err
	}
	if _, err := os.Lstat(options.Destination); err == nil {
		return fmt.Errorf("refusing to replace existing extraction destination: %s", options.Destination)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect extraction destination: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyStageOwnership(stage, filepath.Dir(options.Destination), ownership); err != nil {
		return err
	}
	if err := os.Rename(stage, options.Destination); err != nil {
		return fmt.Errorf("publish extracted archive: %w", err)
	}
	published = true
	return syncDirectory(filepath.Dir(options.Destination))
}

func extractionLimits(limits ExtractionLimits) (ExtractionLimits, error) {
	if limits == (ExtractionLimits{}) {
		return DefaultExtractionLimits, nil
	}
	if limits.MaxArchiveBytes <= 0 || limits.MaxMembers <= 0 || limits.MaxPathBytes <= 0 ||
		limits.MaxFileBytes == 0 || limits.MaxTotalBytes == 0 || limits.MaxTarTrailingBytes <= 0 {
		return ExtractionLimits{}, errors.New("all extraction limits must be positive")
	}
	const maxInt64 = uint64(^uint64(0) >> 1)
	if limits.MaxArchiveBytes == int64(maxInt64) ||
		limits.MaxTarTrailingBytes == int64(maxInt64) ||
		limits.MaxFileBytes >= maxInt64 ||
		limits.MaxTotalBytes >= maxInt64 {
		return ExtractionLimits{}, errors.New("extraction byte limits are too large")
	}
	return limits, nil
}

func parseExpectedSHA256(value string) ([]byte, error) {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return nil, errors.New("expected SHA-256 must be exactly 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return nil, errors.New("expected SHA-256 must be exactly 64 lowercase hexadecimal characters")
	}
	return decoded, nil
}

func verifyOpenFileHash(ctx context.Context, file *os.File, size int64, expected []byte) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek acquisition archive: %w", err)
	}
	hash := sha256.New()
	count, err := io.Copy(hash, &contextReader{ctx: ctx, reader: io.LimitReader(file, size+1)})
	if err != nil {
		return fmt.Errorf("hash acquisition archive: %w", err)
	}
	if count != size {
		return errors.New("acquisition archive size changed while hashing")
	}
	actual := hash.Sum(nil)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return errors.New("acquisition archive SHA-256 mismatch")
	}
	return nil
}

func validateAllowedRoots(values []string, maxPathBytes int) (map[string]bool, error) {
	if len(values) == 0 {
		return nil, errors.New("at least one archive root must be allowlisted")
	}
	result := make(map[string]bool, len(values))
	folded := make(map[string]string, len(values))
	fold := cases.Fold()
	for _, value := range values {
		clean, err := canonicalMemberName(value, false, maxPathBytes)
		if err != nil || strings.Contains(clean, "/") {
			return nil, fmt.Errorf("invalid allowlisted archive root %q", value)
		}
		if result[clean] {
			return nil, fmt.Errorf("duplicate allowlisted archive root %q", value)
		}
		key := fold.String(norm.NFC.String(clean))
		if previous, exists := folded[key]; exists {
			return nil, fmt.Errorf("case-fold or normalization collision between archive roots %q and %q", previous, value)
		}
		result[clean] = true
		folded[key] = clean
	}
	return result, nil
}

func validateSelections(
	values []ExtractSelection,
	allowed map[string]bool,
	maxPathBytes int,
) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	outputNodes := make(map[string]bool)
	outputFolded := make(map[string]string)
	fold := cases.Fold()
	for _, value := range values {
		archivePath, err := canonicalMemberName(value.ArchivePath, false, maxPathBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid selected archive path %q: %w", safeMemberLabel(value.ArchivePath), err)
		}
		root := strings.SplitN(archivePath, "/", 2)[0]
		if !allowed[root] || archivePath == root {
			return nil, fmt.Errorf("selected archive path is outside an allowlisted directory root: %q", safeMemberLabel(value.ArchivePath))
		}
		if _, exists := result[archivePath]; exists {
			return nil, fmt.Errorf("duplicate selected archive path %q", archivePath)
		}
		outputPath, err := canonicalMemberName(value.OutputPath, false, maxPathBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid selected output path %q: %w", safeMemberLabel(value.OutputPath), err)
		}
		parts := strings.Split(outputPath, "/")
		for index := range parts {
			current := strings.Join(parts[:index+1], "/")
			isFile := index == len(parts)-1
			key := fold.String(norm.NFC.String(current))
			if previous, exists := outputFolded[key]; exists && previous != current {
				return nil, fmt.Errorf("case-fold or normalization collision between selected output paths %q and %q", previous, current)
			}
			outputFolded[key] = current
			if existingFile, exists := outputNodes[current]; exists {
				if existingFile || isFile {
					return nil, fmt.Errorf("duplicate or file/directory prefix collision at selected output path %q", current)
				}
			} else {
				outputNodes[current] = isFile
			}
		}
		result[archivePath] = outputPath
	}
	return result, nil
}

func prepareExtractionMembers(
	members []archiveMember,
	selections map[string]string,
) ([]archiveMember, error) {
	if len(selections) == 0 {
		for index := range members {
			members[index].output = members[index].name
		}
		return members, nil
	}
	found := make(map[string]bool, len(selections))
	for index := range members {
		member := &members[index]
		output, selected := selections[member.name]
		if !selected || !member.explicit {
			continue
		}
		if member.isDir || member.isLink {
			return nil, fmt.Errorf("selected archive path is not a regular file: %q", member.name)
		}
		member.output = output
		found[member.name] = true
	}
	for selected := range selections {
		if !found[selected] {
			return nil, fmt.Errorf("selected archive path is missing: %q", selected)
		}
	}
	return members, nil
}

func verifyArchiveIdentity(pathname string, input *os.File, original fs.FileInfo) error {
	opened, openErr := input.Stat()
	current, pathErr := os.Lstat(pathname)
	if openErr != nil || pathErr != nil || !os.SameFile(original, opened) ||
		!os.SameFile(original, current) || current.Mode()&os.ModeSymlink != 0 ||
		!current.Mode().IsRegular() {
		return errors.New("acquisition archive identity changed during extraction")
	}
	return nil
}

func canonicalMemberName(name string, directory bool, maxPathBytes int) (string, error) {
	if !utf8.ValidString(name) || name == "" || len(name) > maxPathBytes ||
		strings.ContainsRune(name, '\x00') || strings.ContainsRune(name, '\\') ||
		strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe archive member path %q", safeMemberLabel(name))
	}
	for _, value := range name {
		if unicode.IsControl(value) || unicode.In(value, unicode.Cf) {
			return "", fmt.Errorf("unsafe archive member path %q", safeMemberLabel(name))
		}
	}
	raw := name
	if directory {
		if strings.HasSuffix(raw, "/") {
			raw = strings.TrimSuffix(raw, "/")
			if strings.HasSuffix(raw, "/") {
				return "", fmt.Errorf("noncanonical archive member path %q", safeMemberLabel(name))
			}
		}
	} else if strings.HasSuffix(raw, "/") {
		return "", fmt.Errorf("noncanonical archive member path %q", safeMemberLabel(name))
	}
	clean := path.Clean(raw)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != raw {
		return "", fmt.Errorf("noncanonical archive member path %q", safeMemberLabel(name))
	}
	return clean, nil
}

func safeMemberLabel(name string) string {
	if !utf8.ValidString(name) {
		return "<invalid UTF-8>"
	}
	var builder strings.Builder
	for _, value := range name {
		if unicode.IsControl(value) || unicode.In(value, unicode.Cf) {
			builder.WriteRune('\uFFFD')
		} else {
			builder.WriteRune(value)
		}
		if builder.Len() >= 160 {
			builder.WriteString("...")
			break
		}
	}
	return builder.String()
}

type memberSet struct {
	allowed map[string]bool
	nodes   map[string]archiveMember
	folded  map[string]string
	fold    cases.Caser
	limits  ExtractionLimits
	count   int
	total   uint64
}

func newMemberSet(allowed map[string]bool, limits ExtractionLimits) *memberSet {
	return &memberSet{
		allowed: allowed,
		nodes:   make(map[string]archiveMember),
		folded:  make(map[string]string),
		fold:    cases.Fold(),
		limits:  limits,
	}
}

func (set *memberSet) add(
	name string,
	directory bool,
	link bool,
	linkname string,
	mode fs.FileMode,
	size uint64,
) error {
	clean, err := canonicalMemberName(name, directory, set.limits.MaxPathBytes)
	if err != nil {
		return err
	}
	root := strings.SplitN(clean, "/", 2)[0]
	if !set.allowed[root] {
		return fmt.Errorf("archive member is outside allowlisted roots: %q", safeMemberLabel(name))
	}
	if clean == root && !directory {
		return fmt.Errorf("allowlisted top-level archive root must be a directory: %q", safeMemberLabel(name))
	}
	if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Errorf("archive member has forbidden permission bits: %q", safeMemberLabel(name))
	}
	if !directory && !link && size > set.limits.MaxFileBytes {
		return fmt.Errorf("archive member exceeds the %d-byte per-file limit: %q", set.limits.MaxFileBytes, safeMemberLabel(name))
	}
	if !directory && !link &&
		(size > set.limits.MaxTotalBytes || set.total > set.limits.MaxTotalBytes-size) {
		return fmt.Errorf("archive exceeds the %d-byte aggregate limit", set.limits.MaxTotalBytes)
	}
	set.count++
	if set.count > set.limits.MaxMembers {
		return fmt.Errorf("archive exceeds the %d-member limit", set.limits.MaxMembers)
	}
	if !directory && !link {
		set.total += size
	}

	parts := strings.Split(clean, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")
		isLast := index == len(parts)-1
		wantDir := !isLast || directory
		key := set.fold.String(norm.NFC.String(current))
		if previous, exists := set.folded[key]; exists && previous != current {
			return fmt.Errorf("case-fold or normalization collision between archive members %q and %q", previous, current)
		}
		set.folded[key] = current
		if existing, exists := set.nodes[current]; exists {
			if existing.isDir != wantDir {
				return fmt.Errorf("file/directory prefix collision at archive member %q", current)
			}
			if isLast {
				if existing.explicit {
					return fmt.Errorf("duplicate archive member %q", current)
				}
				existing.explicit = true
				existing.mode = mode.Perm()
				existing.size = size
				existing.isLink = link
				existing.linkname = linkname
				set.nodes[current] = existing
			}
			continue
		}
		memberMode := fs.FileMode(0o755)
		if isLast {
			memberMode = mode.Perm()
		}
		set.nodes[current] = archiveMember{
			name: current, mode: memberMode, size: size, isDir: wantDir,
			isLink: isLast && link, linkname: linkname, explicit: isLast,
		}
	}
	return nil
}

func (set *memberSet) list() []archiveMember {
	result := make([]archiveMember, 0, len(set.nodes))
	for _, member := range set.nodes {
		result = append(result, member)
	}
	return result
}

func preflightZIP(
	ctx context.Context,
	input *os.File,
	size int64,
	allowed map[string]bool,
	limits ExtractionLimits,
	selective bool,
) ([]archiveMember, error) {
	reader, err := zip.NewReader(&contextReaderAt{ctx: ctx, reader: input}, size)
	if err != nil {
		return nil, fmt.Errorf("open ZIP archive: %w", err)
	}
	set := newMemberSet(allowed, limits)
	for _, file := range reader.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if set.count >= limits.MaxMembers {
			return nil, fmt.Errorf("archive exceeds the %d-member limit", limits.MaxMembers)
		}
		if file.Flags&(0x1|0x40|0x2000) != 0 {
			return nil, fmt.Errorf("encrypted or masked ZIP member is unsupported: %q", safeMemberLabel(file.Name))
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return nil, fmt.Errorf("unsupported ZIP compression method for %q", safeMemberLabel(file.Name))
		}
		mode := file.Mode()
		directory := strings.HasSuffix(file.Name, "/")
		if directory != mode.IsDir() {
			return nil, fmt.Errorf("ZIP member directory metadata disagrees with its name: %q", safeMemberLabel(file.Name))
		}
		link := mode&os.ModeSymlink != 0
		if link && !selective {
			return nil, fmt.Errorf("ZIP symbolic link is unsupported in whole-root mode: %q", safeMemberLabel(file.Name))
		}
		if (!directory && !link && !mode.IsRegular()) ||
			(directory && mode.Type() != os.ModeDir) {
			return nil, fmt.Errorf("ZIP member is not a directory or regular file: %q", safeMemberLabel(file.Name))
		}
		if directory && file.UncompressedSize64 != 0 {
			return nil, fmt.Errorf("ZIP directory has data: %q", safeMemberLabel(file.Name))
		}
		if !link {
			if err := set.add(file.Name, directory, false, "", mode, file.UncompressedSize64); err != nil {
				return nil, err
			}
			if directory {
				continue
			}
		}
		var linkname string
		source, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open ZIP member %q during preflight: %w", safeMemberLabel(file.Name), err)
		}
		if link {
			if file.UncompressedSize64 > uint64(limits.MaxPathBytes) {
				_ = source.Close()
				return nil, fmt.Errorf("ZIP symbolic link metadata is too large: %q", safeMemberLabel(file.Name))
			}
			data, readErr := io.ReadAll(&contextReader{
				ctx: ctx, reader: io.LimitReader(source, int64(file.UncompressedSize64)+1),
			})
			if readErr == nil && uint64(len(data)) != file.UncompressedSize64 {
				readErr = errors.New("ZIP symbolic link byte count differs from declared size")
			}
			if readErr == nil {
				linkname, readErr = validateLinkMetadata(string(data), limits.MaxPathBytes)
			}
			if closeErr := source.Close(); readErr == nil {
				readErr = closeErr
			}
			if readErr != nil {
				return nil, fmt.Errorf("unsafe ZIP symbolic link metadata for %q: %w", safeMemberLabel(file.Name), readErr)
			}
		} else {
			readErr := discardExpected(ctx, source, file.UncompressedSize64)
			closeErr := source.Close()
			if err := firstError(readErr, closeErr); err != nil {
				return nil, fmt.Errorf("preflight ZIP member %q: %w", safeMemberLabel(file.Name), err)
			}
		}
		if link {
			if err := set.add(file.Name, false, true, linkname, mode, file.UncompressedSize64); err != nil {
				return nil, err
			}
		}
	}
	return set.list(), nil
}

func preflightTarGZ(
	ctx context.Context,
	input *os.File,
	allowed map[string]bool,
	limits ExtractionLimits,
	selective bool,
) ([]archiveMember, error) {
	set := newMemberSet(allowed, limits)
	err := walkTarGZ(ctx, input, limits.MaxTarTrailingBytes, func(header *tar.Header, reader io.Reader) error {
		if err := validateTarMetadata(header, limits.MaxPathBytes); err != nil {
			return err
		}
		var directory, link bool
		switch header.Typeflag {
		case tar.TypeDir:
			directory = true
			if header.Size != 0 {
				return fmt.Errorf("tar directory has data: %q", safeMemberLabel(header.Name))
			}
		case tar.TypeReg, tar.TypeRegA:
		case tar.TypeSymlink, tar.TypeLink:
			link = true
			if !selective {
				return fmt.Errorf("tar link is unsupported in whole-root mode: %q", safeMemberLabel(header.Name))
			}
			if header.Size != 0 {
				return fmt.Errorf("tar link has data: %q", safeMemberLabel(header.Name))
			}
		default:
			return fmt.Errorf("tar member type is unsupported: %q", safeMemberLabel(header.Name))
		}
		if header.Size < 0 {
			return fmt.Errorf("tar member has a negative size: %q", safeMemberLabel(header.Name))
		}
		mode, err := tarMode(header.Mode)
		if err != nil {
			return fmt.Errorf("tar member has invalid mode: %q", safeMemberLabel(header.Name))
		}
		var linkname string
		if link {
			linkname, err = validateLinkMetadata(header.Linkname, limits.MaxPathBytes)
			if err != nil {
				return fmt.Errorf("unsafe tar link metadata for %q: %w", safeMemberLabel(header.Name), err)
			}
		}
		if err := set.add(header.Name, directory, link, linkname, mode, uint64(header.Size)); err != nil {
			return err
		}
		if !directory && !link {
			if err := discardExpected(ctx, reader, uint64(header.Size)); err != nil {
				return fmt.Errorf("preflight tar member %q: %w", safeMemberLabel(header.Name), err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return set.list(), nil
}

var resolvedPAXKeys = map[string]bool{
	"path": true, "linkpath": true, "size": true,
	"uid": true, "gid": true, "uname": true, "gname": true,
	"mtime": true, "atime": true, "ctime": true,
}

func validateTarMetadata(header *tar.Header, maxValueBytes int) error {
	switch header.Format {
	case tar.FormatUSTAR:
		if len(header.PAXRecords) != 0 {
			return fmt.Errorf("USTAR member has unexpected extended metadata: %q", safeMemberLabel(header.Name))
		}
	case tar.FormatPAX:
		for key, value := range header.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return fmt.Errorf("sparse tar metadata is unsupported: %q", safeMemberLabel(header.Name))
			}
			if !resolvedPAXKeys[key] {
				return fmt.Errorf("unsupported PAX metadata key for %q", safeMemberLabel(header.Name))
			}
			if err := validatePAXValue(value, maxValueBytes); err != nil {
				return fmt.Errorf("unsafe PAX metadata for %q", safeMemberLabel(header.Name))
			}
			switch key {
			case "path":
				if value != header.Name {
					return fmt.Errorf("ambiguous PAX path override for %q", safeMemberLabel(header.Name))
				}
			case "linkpath":
				if (header.Typeflag != tar.TypeSymlink && header.Typeflag != tar.TypeLink) ||
					value != header.Linkname {
					return fmt.Errorf("ambiguous PAX link override for %q", safeMemberLabel(header.Name))
				}
			case "size":
				if value != strconv.FormatInt(header.Size, 10) {
					return fmt.Errorf("ambiguous PAX size override for %q", safeMemberLabel(header.Name))
				}
			case "uid":
				if value != strconv.Itoa(header.Uid) {
					return fmt.Errorf("ambiguous PAX uid override for %q", safeMemberLabel(header.Name))
				}
			case "gid":
				if value != strconv.Itoa(header.Gid) {
					return fmt.Errorf("ambiguous PAX gid override for %q", safeMemberLabel(header.Name))
				}
			case "uname":
				if value != header.Uname {
					return fmt.Errorf("ambiguous PAX uname override for %q", safeMemberLabel(header.Name))
				}
			case "gname":
				if value != header.Gname {
					return fmt.Errorf("ambiguous PAX gname override for %q", safeMemberLabel(header.Name))
				}
			}
		}
	case tar.FormatGNU:
		if len(header.PAXRecords) != 0 {
			return fmt.Errorf("GNU tar member has unexpected PAX metadata: %q", safeMemberLabel(header.Name))
		}
	default:
		return fmt.Errorf("unsupported tar header format for %q", safeMemberLabel(header.Name))
	}
	if header.Typeflag == tar.TypeGNUSparse {
		return fmt.Errorf("sparse tar member is unsupported: %q", safeMemberLabel(header.Name))
	}
	if header.Linkname != "" &&
		header.Typeflag != tar.TypeSymlink && header.Typeflag != tar.TypeLink {
		return fmt.Errorf("tar link/path override is unsupported: %q", safeMemberLabel(header.Name))
	}
	if (header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink) &&
		header.Linkname == "" {
		return fmt.Errorf("tar link has empty metadata: %q", safeMemberLabel(header.Name))
	}
	return nil
}

func validatePAXValue(value string, maxValueBytes int) error {
	if value == "" || !utf8.ValidString(value) || len(value) > maxValueBytes {
		return errors.New("invalid PAX value")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return errors.New("invalid PAX value")
		}
	}
	return nil
}

func validateLinkMetadata(value string, maxValueBytes int) (string, error) {
	if !utf8.ValidString(value) || value == "" || len(value) > maxValueBytes ||
		strings.ContainsRune(value, '\x00') || strings.ContainsRune(value, '\\') ||
		strings.HasPrefix(value, "/") {
		return "", errors.New("link target is not bounded canonical relative UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return "", errors.New("link target contains a control or format character")
		}
	}
	clean := path.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return "", errors.New("link target is absolute, traversing, or noncanonical")
	}
	return clean, nil
}

func tarMode(value int64) (fs.FileMode, error) {
	if value < 0 || value&^0o777 != 0 {
		return 0, errors.New("mode is outside portable permission bits")
	}
	return fs.FileMode(value), nil
}

func walkTarGZ(
	ctx context.Context,
	input *os.File,
	maxTrailingBytes int64,
	visit func(*tar.Header, io.Reader) error,
) error {
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek tar.gz archive: %w", err)
	}
	buffered := bufio.NewReader(&contextReader{ctx: ctx, reader: input})
	gzipReader, err := gzip.NewReader(buffered)
	if err != nil {
		return fmt.Errorf("open tar.gz archive: %w", err)
	}
	gzipReader.Multistream(false)
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = gzipReader.Close()
			return fmt.Errorf("read tar archive: %w", err)
		}
		if err := visit(header, tarReader); err != nil {
			_ = gzipReader.Close()
			return err
		}
	}
	if err := validateTarTrailing(ctx, gzipReader, maxTrailingBytes); err != nil {
		_ = gzipReader.Close()
		return err
	}
	if err := gzipReader.Close(); err != nil {
		return fmt.Errorf("close tar.gz stream: %w", err)
	}
	if _, err := buffered.Peek(1); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("concatenated or trailing tar.gz data is unsupported")
		}
		return fmt.Errorf("inspect tar.gz trailer: %w", err)
	}
	return nil
}

func validateTarTrailing(ctx context.Context, reader io.Reader, maximum int64) error {
	if maximum <= 0 || maximum == int64(^uint64(0)>>1) {
		return errors.New("tar trailing-data limit must be positive and bounded")
	}
	limited := &contextReader{ctx: ctx, reader: io.LimitReader(reader, maximum+1)}
	buffer := make([]byte, 32<<10)
	var total int64
	for {
		count, err := limited.Read(buffer)
		for _, value := range buffer[:count] {
			if value != 0 {
				return errors.New("tar archive has nonzero data after its end marker")
			}
		}
		total += int64(count)
		if total > maximum {
			return fmt.Errorf("tar archive exceeds the %d-byte trailing-zero limit", maximum)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("validate tar.gz trailing data: %w", err)
		}
	}
}

type stageOwnership struct {
	parent fs.FileInfo
	stage  fs.FileInfo
}

func createPrivateStage(destination string) (string, stageOwnership, error) {
	parent := filepath.Dir(destination)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", stageOwnership{}, fmt.Errorf("inspect extraction destination parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return "", stageOwnership{}, errors.New("extraction destination parent must be a nonsymlink directory")
	}
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", stageOwnership{}, fmt.Errorf("create extraction staging identity: %w", err)
		}
		stage := filepath.Join(parent, "."+filepath.Base(destination)+".extract-"+hex.EncodeToString(random))
		if err := os.Mkdir(stage, 0o700); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", stageOwnership{}, fmt.Errorf("create private extraction staging directory: %w", err)
		}
		if err := os.Chmod(stage, 0o700); err != nil {
			_ = os.Remove(stage)
			return "", stageOwnership{}, fmt.Errorf("set private extraction staging mode: %w", err)
		}
		info, err := os.Lstat(stage)
		if err != nil {
			_ = os.Remove(stage)
			return "", stageOwnership{}, err
		}
		return stage, stageOwnership{parent: parentInfo, stage: info}, nil
	}
	return "", stageOwnership{}, errors.New("could not allocate a unique extraction staging directory")
}

func verifyStageOwnership(stage, parent string, ownership stageOwnership) error {
	parentInfo, err := os.Lstat(parent)
	if err != nil || !os.SameFile(ownership.parent, parentInfo) ||
		parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return errors.New("extraction destination parent identity changed")
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil || !os.SameFile(ownership.stage, stageInfo) ||
		stageInfo.Mode()&os.ModeSymlink != 0 || !stageInfo.IsDir() ||
		stageInfo.Mode().Perm() != 0o700 {
		return errors.New("private extraction staging identity changed")
	}
	return nil
}

func removeOwnedStage(stage string, owned fs.FileInfo) {
	current, err := os.Lstat(stage)
	if err == nil && os.SameFile(owned, current) && current.IsDir() && current.Mode()&os.ModeSymlink == 0 {
		_ = filepath.WalkDir(stage, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
		_ = os.RemoveAll(stage)
	}
}

func extractZIP(
	ctx context.Context,
	input *os.File,
	size int64,
	stage string,
	members []archiveMember,
	maxPathBytes int,
	selective bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	reader, err := zip.NewReader(&contextReaderAt{ctx: ctx, reader: input}, size)
	if err != nil {
		return err
	}
	byName := explicitMembers(members)
	for _, file := range reader.File {
		directory := strings.HasSuffix(file.Name, "/")
		name, err := canonicalMemberName(file.Name, directory, maxPathBytes)
		if err != nil {
			return err
		}
		member, exists := byName[name]
		if !exists {
			return fmt.Errorf("ZIP member changed after preflight: %q", safeMemberLabel(file.Name))
		}
		link := file.Mode()&os.ModeSymlink != 0
		if member.isDir != directory || member.isLink != link ||
			member.size != file.UncompressedSize64 || member.mode.Perm() != file.Mode().Perm() {
			return fmt.Errorf("ZIP member metadata changed after preflight: %q", safeMemberLabel(file.Name))
		}
		if selective && member.output == "" {
			continue
		}
		if directory {
			if err := makeExtractedDirectory(stage, member.output); err != nil {
				return err
			}
			continue
		}
		if link {
			return fmt.Errorf("ZIP symbolic link reached extraction: %q", safeMemberLabel(file.Name))
		}
		source, err := file.Open()
		if err != nil {
			return fmt.Errorf("open ZIP member %q: %w", safeMemberLabel(file.Name), err)
		}
		err = writeExtractedFile(ctx, stage, member, source)
		closeErr := source.Close()
		if err := firstError(err, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func extractTarGZ(
	ctx context.Context,
	input *os.File,
	stage string,
	members []archiveMember,
	maxPathBytes int,
	maxTrailingBytes int64,
	selective bool,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	byName := explicitMembers(members)
	return walkTarGZ(ctx, input, maxTrailingBytes, func(header *tar.Header, reader io.Reader) error {
		if err := validateTarMetadata(header, maxPathBytes); err != nil {
			return err
		}
		directory := header.Typeflag == tar.TypeDir
		link := header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink
		name, err := canonicalMemberName(header.Name, directory, maxPathBytes)
		if err != nil {
			return err
		}
		member, exists := byName[name]
		if !exists {
			return fmt.Errorf("tar member changed after preflight: %q", safeMemberLabel(header.Name))
		}
		mode, err := tarMode(header.Mode)
		if err != nil || header.Size < 0 || member.isDir != directory ||
			member.isLink != link || member.size != uint64(header.Size) ||
			member.mode.Perm() != mode.Perm() || member.linkname != header.Linkname {
			return fmt.Errorf("tar member metadata changed after preflight: %q", safeMemberLabel(header.Name))
		}
		if link {
			if _, err := validateLinkMetadata(header.Linkname, maxPathBytes); err != nil {
				return fmt.Errorf("unsafe tar link metadata for %q: %w", safeMemberLabel(header.Name), err)
			}
			return nil
		}
		if selective && member.output == "" {
			if directory {
				return nil
			}
			return discardExpected(ctx, reader, uint64(header.Size))
		}
		if directory {
			return makeExtractedDirectory(stage, member.output)
		}
		return writeExtractedFile(ctx, stage, member, reader)
	})
}

func explicitMembers(members []archiveMember) map[string]archiveMember {
	result := make(map[string]archiveMember)
	for _, member := range members {
		if member.explicit {
			result[member.name] = member
		}
	}
	return result
}

func makeExtractedDirectory(stage, name string) error {
	destination := filepath.Join(stage, filepath.FromSlash(name))
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return fmt.Errorf("create archive directory %q: %w", name, err)
	}
	return nil
}

func writeExtractedFile(ctx context.Context, stage string, member archiveMember, source io.Reader) error {
	destination := filepath.Join(stage, filepath.FromSlash(member.output))
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create archive parent for %q: %w", member.output, err)
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create archive member %q: %w", member.output, err)
	}
	count, copyErr := io.Copy(output, &contextReader{
		ctx: ctx, reader: io.LimitReader(source, int64(member.size)+1),
	})
	if copyErr == nil && uint64(count) != member.size {
		copyErr = fmt.Errorf("archive member byte count differs from declared size for %q", member.name)
	}
	chmodErr := output.Chmod(member.mode.Perm())
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := firstError(copyErr, chmodErr, syncErr, closeErr); err != nil {
		return err
	}
	return nil
}

func discardExpected(ctx context.Context, reader io.Reader, expected uint64) error {
	count, err := io.Copy(io.Discard, &contextReader{
		ctx: ctx, reader: io.LimitReader(reader, int64(expected)+1),
	})
	if err != nil {
		return err
	}
	if uint64(count) != expected {
		return errors.New("archive member byte count differs from declared size")
	}
	return nil
}

func syncExtractedTree(stage string, members []archiveMember) error {
	modes := make(map[string]fs.FileMode)
	for _, member := range members {
		if member.isDir && member.output != "" {
			modes[member.output] = member.mode.Perm()
		}
		if member.isDir || member.output == "" {
			continue
		}
		parts := strings.Split(member.output, "/")
		for index := 1; index < len(parts); index++ {
			parent := strings.Join(parts[:index], "/")
			if _, exists := modes[parent]; !exists {
				modes[parent] = 0o700
			}
		}
	}
	directories := make([]string, 0, len(modes))
	for directory := range modes {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(first, second int) bool {
		firstDepth := strings.Count(directories[first], "/")
		secondDepth := strings.Count(directories[second], "/")
		if firstDepth != secondDepth {
			return firstDepth > secondDepth
		}
		return directories[first] > directories[second]
	})
	for _, directory := range directories {
		name := filepath.Join(stage, filepath.FromSlash(directory))
		handle, err := os.Open(name)
		if err != nil {
			return fmt.Errorf("open archive directory %q: %w", directory, err)
		}
		chmodErr := handle.Chmod(modes[directory])
		syncErr := handle.Sync()
		closeErr := handle.Close()
		if err := firstError(chmodErr, syncErr, closeErr); err != nil {
			return fmt.Errorf("set archive directory mode %q: %w", directory, err)
		}
	}
	return syncDirectory(stage)
}

func syncDirectory(name string) error {
	directory, err := os.Open(name)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return firstError(syncErr, closeErr)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

type contextReaderAt struct {
	ctx    context.Context
	reader io.ReaderAt
}

func (reader *contextReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.ReadAt(buffer, offset)
	if err == nil {
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return count, contextErr
		}
	}
	return count, err
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.reader.Read(buffer)
	if err == nil {
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return count, contextErr
		}
	}
	return count, err
}
