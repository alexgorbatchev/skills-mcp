package token

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestEnsureCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "token")
	first, err := Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d", len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
	second, err := Ensure(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("Ensure replaced an existing token")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover temporary files: %v", entries)
	}
}

func TestEnsureConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	const n = 16
	got := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { got[i], errs[i] = Ensure(path) })
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if got[i] != got[0] {
			t.Fatalf("concurrent Ensure returned different tokens")
		}
	}
}

func TestReadRejects(t *testing.T) {
	valid := strings.Repeat("a", 64)
	tests := []struct {
		name    string
		content string
		mode    os.FileMode
		want    string
	}{
		{"valid with newline", valid + "\n", 0o600, ""},
		{"valid without newline", valid, 0o600, ""},
		{"empty", "", 0o600, "does not contain"},
		{"short", "abc", 0o600, "does not contain"},
		{"uppercase", strings.Repeat("A", 64), 0o600, "does not contain"},
		{"wide mode", valid, 0o644, "mode 0644"},
		{"narrow mode", valid, 0o400, "mode 0400"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			got, err := Read(path)
			if tt.want == "" {
				if err != nil || got != valid {
					t.Fatalf("Read() = %q, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Read() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestReadMissing(t *testing.T) {
	if _, err := Read(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("Read(missing) succeeded")
	}
}

func TestEnsureUnwritableDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := Ensure(filepath.Join(dir, "token")); err == nil {
		t.Fatal("Ensure in read-only directory succeeded")
	}
	if _, err := Ensure(filepath.Join(dir, "sub", "token")); err == nil {
		t.Fatal("Ensure under read-only directory succeeded")
	}
}
