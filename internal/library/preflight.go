package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"aos-cx-docs-dldr/internal/model"
	"aos-cx-docs-dldr/internal/storage"
)

type libraryIdentity struct {
	Application         string `json:"application"`
	ApplicationVersion  string `json:"application_version"`
	NativeSchemaVersion int    `json:"native_schema_version"`
}

func Preflight(destination, platform, version string) error {
	policy, err := defaultNativeVersionPolicy()
	if err != nil {
		return err
	}
	return preflightWithPolicy(context.Background(), destination, platform, version, policy)
}

func preflightWithPolicy(
	ctx context.Context,
	destination, platform, version string,
	policy nativeVersionPolicy,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	base, err := storage.ResolveBase(destination)
	if err != nil {
		return err
	}
	p, err := storage.SafeComponent(platform)
	if err != nil {
		return err
	}
	v, err := storage.SafeComponent(version)
	if err != nil {
		return err
	}
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Go-only library base must be a real directory: %s", base)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	return preflightRootWithPolicy(ctx, root, base, p, v, platform, version, policy)
}

func preflightRoot(root *os.Root, base, platform, version string) error {
	policy, err := defaultNativeVersionPolicy()
	if err != nil {
		return err
	}
	return preflightRootWithPolicy(context.Background(), root, base, platform, version, platform, version, policy)
}

func preflightRootWithPolicy(
	ctx context.Context,
	root *os.Root,
	base, parent, targetVersion, platform, version string,
	policy nativeVersionPolicy,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := root.Lstat(parent); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := storage.Check(root, parent); err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(parent)), err.Error())
	}
	target := path.Join(parent, targetVersion)
	verifier := &Run{
		Destination:   base,
		root:          root,
		parent:        parent,
		target:        target,
		platform:      platform,
		version:       version,
		versionPolicy: policy,
	}
	journalName := path.Join(parent, "."+targetVersion+".transaction.json")
	if info, err := root.Lstat(journalName); err == nil {
		if err := storage.Check(root, journalName); err != nil || !info.Mode().IsRegular() {
			return incompatibleJournalError(filepath.Join(base, filepath.FromSlash(journalName)), "journal is a symlink or special file")
		}
		data, err := readBoundedData(root, journalName)
		if err != nil {
			return incompatibleJournalError(filepath.Join(base, filepath.FromSlash(journalName)), "journal is malformed or unreadable")
		}
		tx, err := decodeJournal(data)
		if err != nil || (tx.Application != "" && tx.Application != Application) {
			return incompatibleJournalError(filepath.Join(base, filepath.FromSlash(journalName)), "journal does not identify supported native state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	info, err := root.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := storage.Check(root, target); err != nil || !info.IsDir() {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), "target is a symlink, special file, or non-directory")
	}
	var identity libraryIdentity
	if err := readBoundedJSON(root, path.Join(target, "manifest.json"), &identity); err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), "manifest is missing, malformed, or unreadable")
	}
	if identity.Application != Application || identity.NativeSchemaVersion != LibraryNativeSchema {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), fmt.Sprintf(
			"manifest identity application=%q native_schema_version=%d is incompatible",
			identity.Application, identity.NativeSchemaVersion,
		))
	}
	if err := policy.supports(identity.ApplicationVersion); err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), err.Error())
	}
	manifest, err := verifier.readLibraryTree(target)
	if err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), err.Error())
	}
	history, err := verifier.readHistory(target)
	if err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), err.Error())
	}
	if err := validateLibraryProvenance(manifest, history); err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), err.Error())
	}
	if _, err := verifier.libraryTreeDigest(ctx, target); err != nil {
		return incompatibleLibraryError(filepath.Join(base, filepath.FromSlash(target)), err.Error())
	}

	return nil
}

func readBoundedJSON(root *os.Root, name string, value any) error {
	data, err := readBoundedData(root, name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func readBoundedData(root *os.Root, name string) ([]byte, error) {
	if err := storage.Check(root, name); err != nil {
		return nil, err
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, (8<<20)+1))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, errors.New("managed JSON exceeds limit")
	}
	return data, nil
}

func decodeStrictJSON(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON has trailing values")
		}
		return fmt.Errorf("JSON has trailing data: %w", err)
	}
	return nil
}

func decodeJournal(data []byte) (journal, error) {
	var discriminator journalDiscriminator
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return journal{}, err
	}
	switch discriminator.NativeSchemaVersion {
	case LegacyJournalNativeSchema:
		var legacy legacyPublicationJournal
		if err := decodeStrictJSON(data, &legacy); err != nil {
			return journal{}, err
		}
		return journal{
			Application:         legacy.Application,
			NativeSchemaVersion: legacy.NativeSchemaVersion,
			RunID:               legacy.RunID,
			PreviousRun:         legacy.PreviousRun,
			Stage:               legacy.Stage,
			PreviousSnapshot:    legacy.PreviousSnapshot,
		}, nil
	case UpgradeJournalNativeSchema:
		return journal{}, errors.New("upgrade journal schema is not supported by this application version")
	default:
		return journal{}, fmt.Errorf("unsupported journal native schema %d", discriminator.NativeSchemaVersion)
	}
}

func incompatibleLibraryError(target, reason string) error {
	return fmt.Errorf(
		"incompatible library target at %s: %s; the existing state is preserved; this application reads and writes only version %s libraries; choose a new base with --destination and redownload",
		target, reason, model.Version,
	)
}

func incompatibleJournalError(target, reason string) error {
	return fmt.Errorf(
		"incompatible publication journal at %s: %s; the existing state is preserved for manual inspection with its original tool; choose a new base with --destination and redownload",
		target, reason,
	)
}
