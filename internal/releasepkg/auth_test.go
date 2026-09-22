package releasepkg

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteBearerHeaderCleansOwnedFileAfterInitialStatFailure(t *testing.T) {
	const secret = "never-disclose-this-token"
	output := filepath.Join(t.TempDir(), "header")
	err := writeBearerHeader(
		strings.NewReader(`{"token":"`+secret+`"}`),
		output,
		bearerHeaderHooks{
			fileStat: func(*os.File) (fs.FileInfo, error) {
				return nil, errors.New("injected stat failure")
			},
		},
	)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("stat failure was not redacted: %v", err)
	}
	if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
		t.Fatalf("owned header survived stat failure: %v", statErr)
	}
}

func TestWriteBearerHeaderDoesNotDeleteForeignReplacement(t *testing.T) {
	const secret = "never-disclose-this-token"
	output := filepath.Join(t.TempDir(), "header")
	foreign := strings.Repeat("f", len("Authorization: Bearer ")+len(secret)+1)
	err := writeBearerHeader(
		strings.NewReader(`{"token":"`+secret+`"}`),
		output,
		bearerHeaderHooks{
			afterClose: func(path string, _ fs.FileInfo) error {
				if err := os.Remove(path); err != nil {
					return err
				}
				return os.WriteFile(path, []byte(foreign), 0o600)
			},
		},
	)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("replacement failure was not redacted: %v", err)
	}
	data, readErr := os.ReadFile(output)
	if readErr != nil || string(data) != foreign {
		t.Fatalf("foreign replacement was removed or changed: %q err=%v", data, readErr)
	}
}

func TestWriteBearerHeaderRejectsOwnedPostCloseMutation(t *testing.T) {
	const secret = "never-disclose-this-token"
	for _, test := range []struct {
		name   string
		mutate func(string) error
	}{
		{name: "mode", mutate: func(path string) error { return os.Chmod(path, 0o644) }},
		{name: "size", mutate: func(path string) error {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, writeErr := file.WriteString("x")
			closeErr := file.Close()
			return firstError(writeErr, closeErr)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "header")
			err := writeBearerHeader(
				strings.NewReader(`{"token":"`+secret+`"}`),
				output,
				bearerHeaderHooks{
					afterClose: func(path string, _ fs.FileInfo) error {
						return test.mutate(path)
					},
				},
			)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("post-close mutation failure was not redacted: %v", err)
			}
			if _, statErr := os.Lstat(output); !os.IsNotExist(statErr) {
				t.Fatalf("mutated owned header survived validation: %v", statErr)
			}
		})
	}
}
