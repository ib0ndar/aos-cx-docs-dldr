package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"aos-cx-docs-dldr/internal/releasepkg"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: aoscx-release-package COMMAND [OPTIONS]")
	}
	var err error
	switch os.Args[1] {
	case "extract":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = extract(ctx, os.Args[2:])
		stop()
	case "auth-header":
		err = authHeader(os.Args[2:], os.Stdin)
	case "pin":
		err = pin(os.Args[2:])
	case "verify-source":
		err = verifySource(os.Args[2:])
	case "source-identity":
		err = sourceIdentity(os.Args[2:])
	case "stage-tree":
		err = stageTree(os.Args[2:])
	case "verify-sidecars":
		err = verifySidecars(os.Args[2:])
	case "checksums":
		err = checksums(os.Args[2:])
	case "generate":
		err = generate(os.Args[2:])
	case "verify-bundle":
		err = verifyBundle(os.Args[2:])
	case "archive":
		err = archive(os.Args[2:])
	case "verify-zip":
		err = verifyZIP(os.Args[2:])
	case "provenance":
		err = provenance(os.Args[2:])
	case "compare":
		err = compare(os.Args[2:])
	default:
		fatalf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func extract(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("extract", flag.ContinueOnError)
	archivePath := flags.String("archive", "", "explicit local archive path")
	destination := flags.String("destination", "", "new extraction destination")
	format := flags.String("format", "", "archive format: zip or tar.gz")
	expectedSHA256 := flags.String("sha256", "", "exact expected archive SHA-256")
	var roots stringList
	var selected stringList
	flags.Var(&roots, "root", "allowlisted top-level archive root")
	flags.Var(&selected, "select", "exact ARCHIVE_PATH=OUTPUT_RELATIVE_PATH regular-file mapping")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *archivePath == "" || *destination == "" ||
		*format == "" || *expectedSHA256 == "" || len(roots) == 0 {
		return fmt.Errorf("extract requires --archive, --destination, --format, --sha256, and --root")
	}
	selections := make([]releasepkg.ExtractSelection, 0, len(selected))
	for _, value := range selected {
		if strings.Count(value, "=") != 1 {
			return fmt.Errorf("--select requires exactly one ARCHIVE_PATH=OUTPUT_RELATIVE_PATH separator")
		}
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("--select requires exact ARCHIVE_PATH=OUTPUT_RELATIVE_PATH")
		}
		selections = append(selections, releasepkg.ExtractSelection{
			ArchivePath: parts[0],
			OutputPath:  parts[1],
		})
	}
	return releasepkg.ExtractArchive(ctx, releasepkg.ExtractOptions{
		ArchivePath: *archivePath, Destination: *destination, Format: *format,
		SHA256: *expectedSHA256, AllowedRoots: roots, Selections: selections,
	})
}

func authHeader(arguments []string, input io.Reader) error {
	flags := flag.NewFlagSet("auth-header", flag.ContinueOnError)
	output := flags.String("output", "", "new private curl header path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *output == "" {
		return fmt.Errorf("auth-header requires --output")
	}
	return releasepkg.WriteBearerHeader(input, *output)
}

func pin(arguments []string) error {
	if len(arguments) != 1 {
		return fmt.Errorf("usage: pin NAME")
	}
	value, err := releasepkg.Pin(arguments[0])
	if err != nil {
		return err
	}
	fmt.Println(value)
	return nil
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func verifySource(arguments []string) error {
	flags := flag.NewFlagSet("verify-source", flag.ContinueOnError)
	root := flags.String("root", "", "checksum root")
	checksums := flags.String("checksums", "", "complete SHA256SUMS file")
	output := flags.String("output", "", "verification JSON output")
	var required stringList
	flags.Var(&required, "required", "required checksummed subtree")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *root == "" || *checksums == "" || *output == "" || len(required) == 0 {
		return fmt.Errorf("verify-source requires --root, --checksums, --output, and --required")
	}
	result, err := releasepkg.VerifyChecksums(*root, *checksums, required)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		ChecksumsSHA256 string `json:"checksums_sha256"`
		FileCount       int    `json:"file_count"`
	}{result.ChecksumsSHA256, result.FileCount}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(*output, data, 0o600)
}

func sourceIdentity(arguments []string) error {
	flags := flag.NewFlagSet("source-identity", flag.ContinueOnError)
	repository := flags.String("repository", "", "repository root")
	list := flags.String("files-list", "", "sorted NUL-delimited source file list")
	output := flags.String("output", "", "source identity JSON output")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *repository == "" || *list == "" || *output == "" {
		return fmt.Errorf("source-identity requires --repository, --files-list, and --output")
	}
	digest, count, err := releasepkg.HashSourceList(*repository, *list)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		ContentSHA256   string `json:"content_sha256"`
		SourceFileCount int    `json:"source_file_count"`
	}{digest, count}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(*output, data, 0o600)
}

func stageTree(arguments []string) error {
	flags := flag.NewFlagSet("stage-tree", flag.ContinueOnError)
	source := flags.String("source", "", "verified source directory")
	destination := flags.String("destination", "", "new destination directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *source == "" || *destination == "" {
		return fmt.Errorf("stage-tree requires --source and --destination")
	}
	return releasepkg.StageTree(*source, *destination)
}

func verifySidecars(arguments []string) error {
	flags := flag.NewFlagSet("verify-sidecars", flag.ContinueOnError)
	chrome := flags.String("chrome", "", "Chrome sidecar directory")
	qpdf := flags.String("qpdf", "", "qpdf sidecar directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *chrome == "" || *qpdf == "" {
		return fmt.Errorf("verify-sidecars requires --chrome and --qpdf")
	}
	_, err := releasepkg.VerifySidecars(*chrome, *qpdf)
	return err
}

func checksums(arguments []string) error {
	flags := flag.NewFlagSet("checksums", flag.ContinueOnError)
	root := flags.String("root", "", "file-set root")
	output := flags.String("output", "", "SHA256SUMS output")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *root == "" || *output == "" {
		return fmt.Errorf("checksums requires --root and --output")
	}
	return releasepkg.WriteChecksums(*root, *output, nil)
}

func generate(arguments []string) error {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	bundle := flags.String("bundle", "", "bundle root")
	repository := flags.String("repository", "", "repository root")
	dependencies := flags.String("dependencies", "", "go list -deps JSON")
	commit := flags.String("commit", "", "source commit")
	dirty := flags.Bool("dirty", false, "source tree was dirty")
	goVersion := flags.String("go-version", "", "exact Go toolchain")
	goRoot := flags.String("goroot", "", "exact local GOROOT")
	epoch := flags.Int64("source-date-epoch", 0, "deterministic timestamp")
	inputVerification := flags.String("input-verification", "", "verified input JSON")
	sourceVerification := flags.String("source-verification", "", "source identity JSON")
	headTree := flags.String("head-tree", "", "Git HEAD tree identity")
	movedSmoke := flags.Bool("moved-smoke", false, "moved conversion smoke passed")
	helpSmoke := flags.Bool("help-smoke", false, "offline help smoke passed")
	versionSmoke := flags.Bool("version-smoke", false, "offline version smoke passed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	data, err := os.ReadFile(*inputVerification)
	if err != nil {
		return err
	}
	var input struct {
		ChecksumsSHA256 string `json:"checksums_sha256"`
		FileCount       int    `json:"file_count"`
	}
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	data, err = os.ReadFile(*sourceVerification)
	if err != nil {
		return err
	}
	var source struct {
		ContentSHA256   string `json:"content_sha256"`
		SourceFileCount int    `json:"source_file_count"`
	}
	if err := json.Unmarshal(data, &source); err != nil {
		return err
	}
	_, err = releasepkg.GenerateMetadata(releasepkg.MetadataOptions{
		Bundle: *bundle, Repository: *repository, DependenciesJSON: *dependencies,
		Commit: *commit, Dirty: *dirty, GoVersion: *goVersion, SourceDateEpoch: *epoch,
		GOROOT:         *goRoot,
		InputChecksums: input.ChecksumsSHA256, InputFileCount: input.FileCount,
		HeadTree: *headTree, SourceContentSHA256: source.ContentSHA256,
		SourceFileCount:    source.SourceFileCount,
		MovedSmokeComplete: *movedSmoke, HelpSmokeComplete: *helpSmoke,
		VersionSmokeComplete: *versionSmoke,
	})
	return err
}

func verifyBundle(arguments []string) error {
	flags := flag.NewFlagSet("verify-bundle", flag.ContinueOnError)
	bundle := flags.String("bundle", "", "bundle root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	return releasepkg.VerifyBundle(*bundle)
}

func archive(arguments []string) error {
	flags := flag.NewFlagSet("archive", flag.ContinueOnError)
	bundle := flags.String("bundle", "", "bundle root")
	output := flags.String("output", "", "ZIP output")
	rootName := flags.String("root-name", "", "ZIP root name")
	epoch := flags.Int64("source-date-epoch", 0, "deterministic timestamp")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	return releasepkg.CreateZIP(*bundle, *output, *rootName, *epoch)
}

func verifyZIP(arguments []string) error {
	flags := flag.NewFlagSet("verify-zip", flag.ContinueOnError)
	path := flags.String("zip", "", "ZIP path")
	extract := flags.String("extract", "", "new extraction root")
	rootName := flags.String("root-name", "", "ZIP root name")
	epoch := flags.Int64("source-date-epoch", 0, "deterministic timestamp")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	bundle, err := releasepkg.VerifyAndExtractZIP(*path, *extract, *rootName, *epoch)
	if err == nil {
		fmt.Println(bundle)
	}
	return err
}

func provenance(arguments []string) error {
	flags := flag.NewFlagSet("provenance", flag.ContinueOnError)
	bundle := flags.String("bundle", "", "bundle root")
	zipPath := flags.String("zip", "", "ZIP path")
	output := flags.String("output", "", "provenance JSON path")
	shaOutput := flags.String("sha-output", "", "detached ZIP SHA-256 path")
	extractedSmoke := flags.Bool("extracted-smoke", false, "extracted ZIP smoke passed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	_, err := releasepkg.WriteDetachedProvenance(
		*bundle, *zipPath, *output, *shaOutput, *extractedSmoke,
	)
	return err
}

func compare(arguments []string) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	first := flags.String("first", "", "first directory")
	second := flags.String("second", "", "second directory")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	return releasepkg.CompareFiles(*first, *second)
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
