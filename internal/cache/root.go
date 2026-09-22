package cache

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"aos-cx-docs-dldr/internal/storage"
)

const Application = "aos-cx-docs-dldr"
const RootSchema = 2
const RootMarkerName = ".aos-cx-docs-dldr-cache-root"

type rootMarker struct {
	Application        string `json:"application"`
	CacheSchemaVersion int    `json:"cache_schema_version"`
}

var expectedRootMarker = rootMarker{
	Application:        Application,
	CacheSchemaVersion: RootSchema,
}

func Prepare(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("cache root path is required")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	created := false
	info, err := os.Lstat(absolute)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(absolute, 0o700); err != nil {
			return err
		}
		created = true
	case err != nil:
		return err
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("cache root must not be a symbolic link: %s", absolute)
	case !info.IsDir():
		return fmt.Errorf("cache root must be a directory, not a special or regular file: %s", absolute)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return err
	}
	admitErr := admitRoot(ctx, root, absolute)
	closeErr := root.Close()
	if admitErr != nil && created {
		_ = os.Remove(absolute)
	}
	return errors.Join(admitErr, closeErr)
}

func admitRoot(ctx context.Context, root *os.Root, absolute string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(RootMarkerName)
		if err == nil {
			if !info.Mode().IsRegular() {
				return incompatibleRootError(absolute, "native marker is a symlink or special file")
			}
			if info.Mode().Perm() != 0o600 {
				return incompatibleRootError(absolute, fmt.Sprintf("native marker permissions are %04o, expected private 0600", info.Mode().Perm()))
			}
			rootInfo, statErr := root.Stat(".")
			if statErr != nil {
				return statErr
			}
			if rootInfo.Mode().Perm() != 0o700 {
				return incompatibleRootError(absolute, fmt.Sprintf("cache root permissions are %04o, expected private 0700", rootInfo.Mode().Perm()))
			}
			return validateRootMarker(root, absolute)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(-1)
		closeErr := dir.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(entries) == 0 {
			rootInfo, err := root.Stat(".")
			if err != nil {
				return err
			}
			previousMode := rootInfo.Mode().Perm()
			if err := root.Chmod(".", 0o700); err != nil {
				return err
			}
			err = initializeRootMarker(ctx, root)
			if err != nil && previousMode != 0o700 {
				err = errors.Join(err, root.Chmod(".", previousMode))
			}
			return err
		}
		initializing := true
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "."+RootMarkerName+"-") || !strings.HasSuffix(entry.Name(), ".part") {
				initializing = false
				break
			}
		}
		if !initializing {
			if _, err := root.Lstat(RootMarkerName); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return incompatibleRootError(absolute, "nonempty cache has no native root marker")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func initializeRootMarker(ctx context.Context, root *os.Root) error {
	return initializeRootMarkerWithWriter(ctx, root, func(ctx context.Context, file *os.File, data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := file.Write(data)
		return err
	})
}

func initializeRootMarkerWithWriter(
	ctx context.Context,
	root *os.Root,
	write func(context.Context, *os.File, []byte) error,
) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(expectedRootMarker, "", "  ")
	if err != nil {
		return err
	}
	temp := "." + RootMarkerName + "-" + rand.Text() + ".part"
	f, err := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := root.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	if err := ctx.Err(); err != nil {
		return errors.Join(err, f.Close())
	}
	writeErr := write(ctx, f, append(data, '\n'))
	syncErr, closeErr := f.Sync(), f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := root.Rename(temp, RootMarkerName); err != nil {
		return err
	}
	return storage.SyncDir(root, ".")
}

func validateRootMarker(root *os.Root, absolute string) error {
	if err := storage.Check(root, RootMarkerName); err != nil {
		return incompatibleRootError(absolute, err.Error())
	}
	f, err := root.Open(RootMarkerName)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 4097))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if len(data) > 4096 {
		return incompatibleRootError(absolute, "native marker exceeds its size limit")
	}
	var marker rootMarker
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return incompatibleRootError(absolute, "native marker is malformed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return incompatibleRootError(absolute, "native marker has trailing data")
	}
	if marker.Application != expectedRootMarker.Application {
		return incompatibleRootError(absolute, fmt.Sprintf("marker application %q is not %q", marker.Application, expectedRootMarker.Application))
	}
	if marker.CacheSchemaVersion != expectedRootMarker.CacheSchemaVersion {
		return incompatibleRootError(absolute, fmt.Sprintf(
			"cache schema %d is incompatible with supported schema %d",
			marker.CacheSchemaVersion, expectedRootMarker.CacheSchemaVersion,
		))
	}
	return nil
}

func incompatibleRootError(absolute, reason string) error {
	return fmt.Errorf(
		"incompatible raw cache at %s: %s; existing state is preserved; choose a new empty cache path with --raw-cache and redownload (cache directories this application did not create are never reused)",
		absolute, reason,
	)
}

func openRoot(ctx context.Context, name string) (*os.Root, error) {
	if err := Prepare(ctx, name); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	if err := admitRoot(ctx, root, absolute); err != nil {
		root.Close()
		return nil, err
	}
	if err := storage.Check(root, path.Clean(RootMarkerName)); err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}
