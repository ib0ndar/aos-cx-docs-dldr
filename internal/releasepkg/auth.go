package releasepkg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

const (
	MaxAuthJSONBytes    = 64 << 10
	MaxBearerTokenBytes = 16 << 10
)

func WriteBearerHeader(input io.Reader, outputPath string) error {
	return writeBearerHeader(input, outputPath, bearerHeaderHooks{})
}

type bearerHeaderHooks struct {
	fileStat   func(*os.File) (fs.FileInfo, error)
	afterClose func(string, fs.FileInfo) error
}

func writeBearerHeader(input io.Reader, outputPath string, hooks bearerHeaderHooks) error {
	if input == nil {
		return errors.New("authorization JSON input is required")
	}
	if outputPath == "" {
		return errors.New("authorization header output path is required")
	}
	token, err := parseBearerToken(input)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private authorization header file: %w", err)
	}
	statFile := hooks.fileStat
	if statFile == nil {
		statFile = func(file *os.File) (fs.FileInfo, error) {
			return file.Stat()
		}
	}
	owned, statErr := statFile(file)
	if statErr != nil {
		owned, _ = file.Stat()
		_ = file.Close()
		removeOwnedHeader(outputPath, owned)
		return errors.New("inspect private authorization header file")
	}
	if !owned.Mode().IsRegular() || owned.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		removeOwnedHeader(outputPath, owned)
		return errors.New("inspect private authorization header file")
	}
	expectedSize := int64(len("Authorization: Bearer ") + len(token) + 1)
	writer := bufio.NewWriter(file)
	_, writeErr := writer.WriteString("Authorization: Bearer " + token + "\n")
	flushErr := writer.Flush()
	chmodErr := file.Chmod(0o600)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := firstError(writeErr, flushErr, chmodErr, syncErr, closeErr); err != nil {
		removeOwnedHeader(outputPath, owned)
		return errors.New("write private authorization header file")
	}
	if hooks.afterClose != nil {
		if err := hooks.afterClose(outputPath, owned); err != nil {
			removeOwnedHeader(outputPath, owned)
			return errors.New("validate private authorization header file")
		}
	}
	current, err := os.Lstat(outputPath)
	if err != nil || !os.SameFile(owned, current) ||
		current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
		current.Mode().Perm() != 0o600 || current.Size() != expectedSize {
		removeOwnedHeader(outputPath, owned)
		return errors.New("validate private authorization header file")
	}
	return nil
}

func removeOwnedHeader(path string, owned fs.FileInfo) {
	if owned == nil {
		return
	}
	current, err := os.Lstat(path)
	if err == nil && os.SameFile(owned, current) &&
		current.Mode()&os.ModeSymlink == 0 && current.Mode().IsRegular() {
		_ = os.Remove(path)
	}
}

func parseBearerToken(input io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(input, MaxAuthJSONBytes+1))
	if err != nil {
		return "", errors.New("read authorization JSON")
	}
	if len(data) > MaxAuthJSONBytes {
		return "", fmt.Errorf("authorization JSON exceeds the %d-byte limit", MaxAuthJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "", errors.New("authorization JSON must be one object")
	}
	var token string
	tokenCount := 0
	for decoder.More() {
		keyValue, err := decoder.Token()
		if err != nil {
			return "", errors.New("invalid authorization JSON")
		}
		key, ok := keyValue.(string)
		if !ok {
			return "", errors.New("invalid authorization JSON object key")
		}
		if key == "token" {
			tokenCount++
			if tokenCount > 1 {
				return "", errors.New("authorization JSON contains duplicate token fields")
			}
			var encoded json.RawMessage
			if err := decoder.Decode(&encoded); err != nil ||
				len(encoded) == 0 || encoded[0] != '"' ||
				json.Unmarshal(encoded, &token) != nil {
				return "", errors.New("authorization token must be a string")
			}
			continue
		}
		var ignored json.RawMessage
		if err := decoder.Decode(&ignored); err != nil {
			return "", errors.New("invalid authorization JSON metadata")
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return "", errors.New("invalid authorization JSON")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", errors.New("authorization JSON contains trailing data")
	}
	if tokenCount != 1 {
		return "", errors.New("authorization JSON must contain exactly one token field")
	}
	if err := validateBearerToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func validateBearerToken(token string) error {
	if token == "" {
		return errors.New("authorization token must be nonempty")
	}
	if len(token) > MaxBearerTokenBytes {
		return fmt.Errorf("authorization token exceeds the %d-byte limit", MaxBearerTokenBytes)
	}
	padding := false
	for _, value := range token {
		if value == '=' {
			padding = true
			continue
		}
		if padding || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~+/", value) {
			return errors.New("authorization token has invalid bearer-token syntax")
		}
	}
	return nil
}
