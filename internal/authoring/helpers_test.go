package authoring

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/paths"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

// isolateGit keeps the user's global git configuration (signing, hooks)
// out of tests and gives commits an identity.
func isolateGit(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "tester")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "tester@example.com")
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillDoc(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
}

// remote is a bare upstream plus a working clone that tests push from.
type remote struct {
	t    *testing.T
	bare string
	work string
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	root := t.TempDir()
	r := &remote{t: t, bare: filepath.Join(root, "up.git"), work: filepath.Join(root, "work")}
	run(t, root, "init", "-q", "--bare", "-b", "main", r.bare)
	run(t, root, "clone", "-q", r.bare, r.work)
	run(t, r.work, "checkout", "-q", "-b", "main")
	write(t, filepath.Join(r.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "body\n"))
	write(t, filepath.Join(r.work, "skills/agent/golang/references/a.md"), "ref\n")
	write(t, filepath.Join(r.work, "skills/user/commit/SKILL.md"), skillDoc("commit", "Commit", "body\n"))
	write(t, filepath.Join(r.work, "skills/agent/validator/SKILL.md"), skillDoc("validator", "Validator", "v\n"))
	write(t, filepath.Join(r.work, "skills/agent/validator/scripts/check.sh"),
		"#!/bin/sh\ntest -e \"$1/FAIL\" && { echo \"validator rejected $1\"; exit 1; }\nexit 0\n")
	write(t, filepath.Join(r.work, ".gitignore"), "*.log\n")
	r.push("init")
	return r
}

func (r *remote) url() string { return "file://" + r.bare }

func (r *remote) push(msg string) string {
	r.t.Helper()
	run(r.t, r.work, "add", "-A")
	run(r.t, r.work, "commit", "-q", "--allow-empty", "-m", msg)
	run(r.t, r.work, "push", "-q", "origin", "main")
	return run(r.t, r.work, "rev-parse", "HEAD")
}

func (r *remote) tip() string {
	r.t.Helper()
	return run(r.t, r.bare, "rev-parse", "refs/heads/main")
}

func (r *remote) show(rev, file string) string {
	r.t.Helper()
	return run(r.t, r.bare, "show", rev+":"+file)
}

type env struct {
	t      *testing.T
	remote *remote
	cfg    *config.Config
	dirs   paths.Dirs
	git    *gitx.Git
	src    *source.Manager
	svc    *Service
	clock  *clock
	views  []map[config.HarnessID]*catalog.View
	mu     sync.Mutex
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func sourceConfig(url string) *config.Source {
	return &config.Source{
		Name: "reg", Kind: config.KindGit, URL: url, Branch: "main", Writable: true,
		Roots: []config.Root{
			{Path: "skills/agent", Invocation: config.Model},
			{Path: "skills/user", Invocation: config.User},
		},
		HarnessRoots: []config.Root{{Path: "harnesses/{harness}/skills", Invocation: config.Model}},
		Template:     config.Template{Vars: map[string]string{"repo_root": "{snapshot}"}},
		IgnoreFile:   ".registry-ignore",
		AuthorPrefix: true, AuthorPrefixExcept: []string{"owner"},
		Validate:     []string{"sh", "skills/agent/validator/scripts/check.sh", "{bundle_dir}"},
		DenyPatterns: []*regexp.Regexp{regexp.MustCompile(`secret\.example`)},
	}
}

func newEnv(t *testing.T, mutate ...func(*config.Source)) *env {
	t.Helper()
	isolateGit(t)
	r := newRemote(t)
	src := sourceConfig(r.url())
	for _, m := range mutate {
		m(src)
	}
	root := t.TempDir()
	e := &env{
		t: t, remote: r,
		cfg: &config.Config{
			FetchInterval: time.Minute, SubmitTimeout: 5 * time.Minute, SnapshotRetention: time.Hour,
			ChangeTTL: 24 * time.Hour, DefaultWriteSource: "reg", Sources: []*config.Source{src},
		},
		dirs:  paths.Dirs{Config: filepath.Join(root, "config"), Cache: filepath.Join(root, "cache"), State: filepath.Join(root, "state")},
		git:   gitx.New(),
		clock: &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	e.start()
	t.Cleanup(func() { unlock(e.dirs.Cache) })
	if err := e.src.Refresh(context.Background(), "reg", source.RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	return e
}

// start builds the manager and service, as a daemon restart would.
func (e *env) start() {
	e.t.Helper()
	e.svc = nil
	e.src = source.NewManager(source.Options{
		Config: e.cfg, Dirs: e.dirs, Git: e.git, Now: e.clock.Now,
		BaseRevisions: func() map[string]map[string]bool { return e.svc.BaseRevisions() },
		OnViews: func(v map[config.HarnessID]*catalog.View) {
			e.mu.Lock()
			e.views = append(e.views, v)
			e.mu.Unlock()
		},
	})
	e.svc = NewService(Options{Config: e.cfg, Dirs: e.dirs, Git: e.git, Sources: e.src, Now: e.clock.Now})
	if err := e.src.Init(); err != nil {
		e.t.Fatal(err)
	}
	if err := e.svc.Load(); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) view(h config.HarnessID) *catalog.View {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.views[len(e.views)-1][h]
}

func (e *env) open() string {
	e.t.Helper()
	res, err := e.svc.Open(context.Background(), config.ClaudeCode, "", "test change")
	if err != nil {
		e.t.Fatal(err)
	}
	return res.ChangeID
}

func (e *env) submit(id, msg string) (SubmitResult, error) {
	return e.svc.Submit(context.Background(), id, msg, nil)
}

func (e *env) record(id string) Record {
	e.t.Helper()
	rec, ok := e.svc.Change(id)
	if !ok {
		e.t.Fatalf("change %s not found", id)
	}
	return rec
}

func (e *env) worktree(id string) string { return e.svc.store.worktreePath(id) }

func unlock(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func mustErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}

var contPattern = regexp.MustCompile(`<continuation sha="([0-9a-f]+)" next_offset="(\d+)"`)

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func errOf(_ string, err error) error { return err }

func sourceRefresh() source.RefreshOptions { return source.RefreshOptions{} }

func gitCmd(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

func discardErr(_ DiscardResult, err error) error { return err }
