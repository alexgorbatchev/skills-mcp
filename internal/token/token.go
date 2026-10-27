// Package token creates and reads the daemon's bearer token file.
package token

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const fileMode fs.FileMode = 0o600

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Ensure creates the token file when it is absent and returns the token.
// Creation writes a temporary file and links it into place, so a concurrent
// reader never observes a partially written token.
func Ensure(path string) (string, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := create(path); err != nil {
			return "", err
		}
	}
	return Read(path)
}

func create(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating token directory: %w", err)
	}
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)                     // crypto/rand.Read never returns an error; it crashes the program instead
	tmp, err := os.CreateTemp(dir, ".token-") // created with mode 0600
	if err != nil {
		return fmt.Errorf("creating temporary token file: %w", err)
	}
	defer os.Remove(tmp.Name()) // the linked name survives; the temporary name never does
	_, writeErr := tmp.WriteString(hex.EncodeToString(raw) + "\n")
	if err := errors.Join(writeErr, tmp.Sync(), tmp.Close()); err != nil {
		return fmt.Errorf("writing token file: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("linking token file: %w", err)
	}
	return nil
}

// Read returns the token, rejecting a file that is not exactly 64 lowercase
// hex characters (one trailing newline allowed) or whose mode is not 0600.
func Read(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading token file: %w", err)
	}
	if mode := info.Mode().Perm(); mode != fileMode {
		return "", fmt.Errorf("token file %s has mode %04o, want 0600", path, mode)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading token file: %w", err)
	}
	tok := strings.TrimSuffix(string(data), "\n")
	if !tokenPattern.MatchString(tok) {
		return "", fmt.Errorf("token file %s does not contain a 64-character hex token", path)
	}
	return tok, nil
}
