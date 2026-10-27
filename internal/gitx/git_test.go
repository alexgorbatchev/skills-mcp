package gitx

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newGit(t *testing.T) *Git {
	t.Helper()
	g, err := New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return g
}

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
	g := newGit(t)
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

// isolateConfig points global configuration at an empty home.
func isolateConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return home
}

// Hooks defined in configuration (Git 2.54+) ignore core.hooksPath; every
// configured hook name is disabled, including names containing "=".
func TestRunDisablesConfiguredHooks(t *testing.T) {
	isolateConfig(t)
	ctx := context.Background()
	g := newGit(t)
	dir := newRepo(t, g)
	marker := filepath.Join(dir, "hook-ran")
	for _, kv := range [][2]string{
		{"hook.mark.command", "touch " + marker + " #"},
		{"hook.mark.event", "pre-commit"},
		{"hook.a=b.command", "touch " + marker + " #"},
		{"hook.a=b.event", "reference-transaction"},
	} {
		if _, err := g.Run(ctx, dir, Opts{}, "config", "--add", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := configuredHooks(ctx, []string{"-C", dir}, g.env)
	want := []string{"--config-env=hook.mark.enabled=" + hookSwitchEnv, "--config-env=hook.a=b.enabled=" + hookSwitchEnv}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("configuredHooks = %q, %v; want %q", got, err, want)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "commit", "-q", "--allow-empty", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("configured hook ran")
	}
	if !supportsConfigHooks(t) {
		t.Log("git predates configured hooks; skipping the control run")
		return
	}
	// Control: plain git runs the same hooks.
	cmd := exec.Command("git", "-C", dir, "commit", "-q", "--allow-empty", "-m", "third")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("control commit: %v: %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control commit did not run the configured hook")
	}
}

// supportsConfigHooks reports whether git is 2.54 or later.
func supportsConfigHooks(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(out), "git version %d.%d", &major, &minor); err != nil {
		t.Fatalf("parsing %q: %v", out, err)
	}
	return major > 2 || major == 2 && minor >= 54
}

func TestRunConfiguredHooksListingFails(t *testing.T) {
	isolateConfig(t)
	_, err := newGit(t).Run(context.Background(), filepath.Join(t.TempDir(), "missing"), Opts{}, "status")
	if err == nil || !strings.Contains(err.Error(), "listing configured git hooks") {
		t.Fatalf("error = %v", err)
	}
}

// Bare runs name the repository explicitly, which safe.bareRepository=explicit
// requires.
func TestRunBareRepository(t *testing.T) {
	home := isolateConfig(t)
	ctx := context.Background()
	g := newGit(t)
	src := newRepo(t, g)
	bare := filepath.Join(t.TempDir(), "repo.git")
	if _, err := g.Run(ctx, src, Opts{}, "clone", "-q", "--bare", src, bare); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[safe]\n\tbareRepository = explicit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, bare, Opts{}, "rev-parse", "HEAD"); err == nil {
		t.Fatal("implicit bare repository accepted; the test does not exercise safe.bareRepository")
	}
	if _, err := g.Run(ctx, bare, Opts{Bare: true}, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("bare run: %v", err)
	}
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
	g := newGit(t)
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
		{"stale lease", "!\tabc:refs/heads/main\t[rejected] (stale info)\n", true, false, true},
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

func TestNewWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := New(context.Background()); err == nil || !strings.Contains(err.Error(), "listing git's repository variables") {
		t.Fatalf("error = %v", err)
	}
}

// Credentials are never requested through an askpass program, whichever
// variable names one.
func TestRunNeverAsksForCredentials(t *testing.T) {
	isolateConfig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	marker := filepath.Join(t.TempDir(), "asked")
	askpass := filepath.Join(t.TempDir(), "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\ntouch "+marker+"\necho secret\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("GIT_ASKPASS", askpass)
	if _, err := newGit(t).Run(context.Background(), t.TempDir(), Opts{}, "ls-remote", srv.URL+"/repo.git"); err == nil {
		t.Fatal("ls-remote succeeded against a 401 server")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("askpass program ran")
	}
}

// Inherited repository-location variables never redirect a command, and
// discovery never climbs from a directory into an enclosing repository.
func TestRunRepositoryLocation(t *testing.T) {
	isolateConfig(t)
	ctx := context.Background()
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	g := newGit(t)
	outer := newRepo(t, g)
	top, err := g.Output(ctx, outer, "rev-parse", "--show-toplevel")
	if want, _ := filepath.EvalSymlinks(outer); err != nil || top != want {
		t.Fatalf("toplevel = %q, %v; want %q", top, err, want)
	}
	inner := filepath.Join(outer, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, inner, Opts{}, "rev-parse", "--git-dir"); err == nil {
		t.Fatal("discovery climbed into the enclosing repository")
	}
}

// core.fsmonitor names a hook program that core.hooksPath does not cover.
func TestRunDisablesFSMonitor(t *testing.T) {
	isolateConfig(t)
	ctx := context.Background()
	g := newGit(t)
	dir := newRepo(t, g)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	monitor := filepath.Join(t.TempDir(), "monitor")
	if err := os.WriteFile(monitor, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "config", "core.fsmonitor", monitor); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Run(ctx, dir, Opts{}, "status", "--porcelain"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("fsmonitor ran")
	}
	// Control: plain git runs it.
	if out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("control status: %v: %s", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("control status did not run the fsmonitor")
	}
}

// Configuration given through the environment survives the removal of
// repository-location variables.
func TestNewKeepsEnvironmentConfiguration(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "from-env")
	g := newGit(t)
	dir := newRepo(t, g)
	if got, err := g.Output(context.Background(), dir, "config", "--get", "user.name"); err != nil || got != "from-env" {
		t.Fatalf("user.name = %q, %v", got, err)
	}
}
