package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"aos-cx-docs-dldr/internal/storage"
)

// migrationOperation is the history operation recorded by builds that upgraded
// a library from an earlier application version. This application no longer
// performs upgrades, but libraries carrying that record remain valid and their
// provenance is still validated.
const migrationOperation = "application-upgrade"

func (r *Run) readHistory(directory string) ([]History, error) {
	data, err := readBoundedData(r.root, path.Join(directory, "history.json"))
	if err != nil {
		return nil, fmt.Errorf("invalid native library history at %s: %w", directory, err)
	}
	var history []History
	if err := decodeStrictJSON(data, &history); err != nil {
		return nil, fmt.Errorf("invalid native library history at %s: %w", directory, err)
	}
	if len(history) == 0 {
		return nil, fmt.Errorf("native library history is empty at %s", directory)
	}
	expectedSnapshotPrefix := path.Join(".snapshots", path.Base(r.target)) + "/"
	// History is a log. Records written by earlier application versions keep
	// their original version string, so each one must be well formed rather
	// than current. Preflight checks the manifest version, and
	// validateLibraryProvenance ties the latest record to it.
	for index, record := range history {
		if _, err := time.Parse(time.RFC3339Nano, record.At); err != nil ||
			record.Changes == nil {
			return nil, fmt.Errorf("invalid native library history record %d at %s", index, directory)
		}
		if _, err := parseApplicationVersion(record.ApplicationVersion); err != nil {
			return nil, fmt.Errorf("invalid application version in history record %d at %s", index, directory)
		}
		if record.PreviousSnapshot != "" {
			if err := storage.SafePath(record.PreviousSnapshot); err != nil ||
				!strings.HasPrefix(record.PreviousSnapshot, expectedSnapshotPrefix) {
				return nil, fmt.Errorf("invalid native library history snapshot in record %d at %s", index, directory)
			}
		}
		if record.UpgradedFromVersion != "" {
			upgradedFrom, err := parseApplicationVersion(record.UpgradedFromVersion)
			current, currentErr := parseApplicationVersion(record.ApplicationVersion)
			if err != nil || currentErr != nil || !applicationVersionBefore(upgradedFrom, current) {
				return nil, fmt.Errorf("invalid upgraded-from version in history record %d at %s", index, directory)
			}
		}
		switch record.Operation {
		case "":
			if record.PreviousApplicationVersion != "" || record.SnapshotDigest != "" {
				return nil, fmt.Errorf("unexpected migration identity in history record %d at %s", index, directory)
			}
		case migrationOperation:
			previous, previousErr := parseApplicationVersion(record.PreviousApplicationVersion)
			current, currentErr := parseApplicationVersion(record.ApplicationVersion)
			if record.PreviousSnapshot == "" ||
				record.PreviousApplicationVersion == "" ||
				record.PreviousApplicationVersion == record.ApplicationVersion ||
				record.UpgradedFromVersion != record.PreviousApplicationVersion ||
				!sha256Pattern.MatchString(record.SnapshotDigest) ||
				previousErr != nil || currentErr != nil ||
				!applicationVersionBefore(previous, current) {
				return nil, fmt.Errorf("invalid migration history record %d at %s", index, directory)
			}
		default:
			return nil, fmt.Errorf("unknown history operation %q at %s", record.Operation, directory)
		}
	}
	return history, nil
}

func validateLibraryProvenance(manifest Manifest, history []History) error {
	if len(history) == 0 {
		return errors.New("native library history is empty")
	}
	last := history[len(history)-1]
	if last.ApplicationVersion != manifest.ApplicationVersion {
		return errors.New("native library manifest and latest history application versions differ")
	}
	if last.Operation == "" {
		if last.UpgradedFromVersion != manifest.UpgradedFromVersion {
			return errors.New("native library manifest and latest history upgrade provenance differ")
		}
		return nil
	}
	if manifest.UpgradedFromVersion == "" {
		return errors.New("migrated native library manifest lacks upgrade provenance")
	}
	sticky, stickyErr := parseApplicationVersion(manifest.UpgradedFromVersion)
	immediate, immediateErr := parseApplicationVersion(last.PreviousApplicationVersion)
	if stickyErr != nil || immediateErr != nil ||
		applicationVersionBefore(immediate, sticky) {
		return errors.New("native library sticky upgrade provenance is inconsistent")
	}
	return nil
}

func (r *Run) libraryTreeDigest(ctx context.Context, directory string) (string, error) {
	manifest, err := r.readLibrary(directory)
	if err != nil {
		return "", err
	}
	allowedFinder := map[string]bool{".DS_Store": true}
	for _, guide := range manifest.Guides {
		allowedFinder[path.Join(guide.Path, ".DS_Store")] = true
		if guide.Format == "html" {
			allowedFinder[path.Join(guide.Path, "pages", ".DS_Store")] = true
			allowedFinder[path.Join(guide.Path, "assets", ".DS_Store")] = true
		}
	}
	if err := r.validateFinderLocations(directory, "", allowedFinder); err != nil {
		return "", err
	}
	return storage.TreeDigest(ctx, r.root, directory)
}

func (r *Run) validateFinderLocations(directory, relative string, allowed map[string]bool) error {
	name := directory
	if relative != "" {
		name = path.Join(directory, relative)
	}
	dir, err := r.root.Open(name)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	for _, entry := range entries {
		child := entry.Name()
		if relative != "" {
			child = path.Join(relative, entry.Name())
		}
		if entry.Name() == ".DS_Store" && !allowed[child] {
			return fmt.Errorf("Finder metadata is outside an allowed library location: %s", child)
		}
		if entry.IsDir() {
			if err := r.validateFinderLocations(directory, child, allowed); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Run) managedPathExists(name string) (bool, error) {
	if err := storage.Check(r.root, name); err != nil {
		return false, err
	}
	_, err := r.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (r *Run) cleanupOwnedTree(name string) error {
	if err := r.checkOwnership(); err != nil {
		return err
	}
	if err := storage.RemoveTree(r.root, name); err != nil {
		return err
	}
	return storage.SyncDir(r.root, path.Dir(name))
}
