package gitx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newRepo(t *testing.T, g *Git) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@example.com"},
		{"commit", "-q", "--allow-empty", "-m", "base"},
	} {
		if _, err := g.Run(ctx, dir, Opts{}, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestRunEnvironment(t *testing.T) {
	ctx := context.Background()
	g := New(ctx)
	dir := newRepo(t, g)

	// Hooks never run, even with a configured hooks path.
	hooks := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "hook-ran")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "config", "core.hooksPath", hooks); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "commit", "-q", "--allow-empty", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook ran")
	}

	// Literal pathspecs: a glob matches nothing.
	if err := os.WriteFile(filepath.Join(dir, "abc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "add", "--", "a*"); err == nil {
		t.Fatal("glob pathspec matched under literal pathspecs")
	}

	// check-ignore runs without literal pathspec magic.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("node_*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, Opts{Stdin: strings.NewReader("node_x\x00")}, "check-ignore", "--stdin", "-z", "-q"); err != nil {
		t.Fatalf("check-ignore: %v", err)
	}
	_, err := g.Run(ctx, dir, Opts{Stdin: strings.NewReader("other\x00")}, "check-ignore", "--stdin", "-z", "-q")
	if ExitCode(err) != 1 {
		t.Fatalf("check-ignore of unignored path: %v", err)
	}

	head, err := g.Output(ctx, dir, "rev-parse", "HEAD")
	if err != nil || len(head) != 40 {
		t.Fatalf("Output = %q, %v", head, err)
	}
	var sb strings.Builder
	if _, err := g.Run(ctx, dir, Opts{Stdout: &sb}, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(sb.String()) != head {
		t.Fatalf("Stdout writer = %q, %v", sb.String(), err)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "maintenance", "run", "--auto"); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
}

func TestNewRespectsSSHCommand(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GIT_SSH_COMMAND", "custom-ssh")
	g := New(ctx)
	if !contains(g.env, "GIT_SSH_COMMAND=custom-ssh") || contains(g.env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Fatal("GIT_SSH_COMMAND overridden")
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\tsshCommand = other-ssh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if contains(New(ctx).env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Fatal("batch ssh set despite core.sshCommand")
	}
	if err := os.Remove(filepath.Join(home, ".gitconfig")); err != nil {
		t.Fatal(err)
	}
	if !contains(New(ctx).env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Fatal("batch ssh not set")
	}
}

func contains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestSubcommand(t *testing.T) {
	if got := subcommand([]string{"--no-pager", "push", "x"}); got != "push" {
		t.Fatalf("subcommand = %q", got)
	}
	if got := subcommand([]string{"-c", "transfer.fsckObjects=true", "fetch"}); got != "fetch" {
		t.Fatalf("subcommand with -c = %q", got)
	}
	if got := subcommand(nil); got != "" {
		t.Fatalf("subcommand(nil) = %q", got)
	}
}

func TestExitCode(t *testing.T) {
	g := New(context.Background())
	_, err := g.Run(context.Background(), t.TempDir(), Opts{}, "rev-parse", "HEAD")
	if ExitCode(err) <= 0 {
		t.Fatalf("ExitCode = %d for %v", ExitCode(err), err)
	}
	if ExitCode(os.ErrNotExist) != -1 {
		t.Fatal("ExitCode of non-exit error")
	}
}

func TestSplitNUL(t *testing.T) {
	got := SplitNUL([]byte("a\x00b c\x00"))
	if len(got) != 2 || got[1] != "b c" {
		t.Fatalf("SplitNUL = %q", got)
	}
}

func TestParsePush(t *testing.T) {
	const ref = "refs/heads/main"
	tests := []struct {
		name              string
		out               string
		parsed, ok, nonFF bool
	}{
		{"new", "To x\n*\tabc:refs/heads/main\t[new branch]\nDone\n", true, true, false},
		{"fast-forward", "To x\n \tabc:refs/heads/main\t1..2\nDone\n", true, true, false},
		{"up to date", "To x\n=\tabc:refs/heads/main\t[up to date]\nDone\n", true, true, false},
		{"fetch first", "To x\n!\tabc:refs/heads/main\t[rejected] (fetch first)\nDone\n", true, false, true},
		{"non-fast-forward", "!\tabc:refs/heads/main\t[rejected] (non-fast-forward)\n", true, false, true},
		{"remote rejected", "!\tabc:refs/heads/main\t[remote rejected] (hook declined)\n", true, false, false},
		{"other ref", "*\tabc:refs/heads/other\t[new branch]\n", false, false, false},
		{"empty", "", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ParsePush([]byte(tt.out), ref)
			if r.Parsed != tt.parsed || r.Accepted() != tt.ok || r.NonFastForward() != tt.nonFF {
				t.Fatalf("ParsePush = %+v accepted=%v nonFF=%v", r, r.Accepted(), r.NonFastForward())
			}
		})
	}
}
