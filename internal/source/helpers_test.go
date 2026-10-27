package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/paths"
)

// upstream is a working repository that tests commit to and the daemon
// clones from over file://.
type upstream struct {
	t   *testing.T
	dir string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{t: t, dir: t.TempDir()}
	u.git("init", "-q", "-b", "main")
	u.git("config", "user.name", "t")
	u.git("config", "user.email", "t@example.com")
	return u
}

func (u *upstream) url() string { return "file://" + u.dir }

func (u *upstream) git(args ...string) string {
	u.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", u.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		u.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (u *upstream) write(rel, content string) {
	u.t.Helper()
	writeFile(u.t, filepath.Join(u.dir, rel), content, 0o644)
}

func (u *upstream) commit(msg string) string {
	u.t.Helper()
	u.git("add", "-A")
	u.git("commit", "-q", "--allow-empty", "-m", msg)
	return u.git("rev-parse", "HEAD")
}

func writeFile(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func skillDoc(name, body string) string {
	return "---\nname: " + name + "\ndescription: " + name + " skill\n---\n" + body
}

// clock is a controllable time source.
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
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	dirs  paths.Dirs
	cfg   *config.Config
	clock *clock
	env   map[string]string
	bases map[string]map[string]bool
	views []map[config.HarnessID]*catalog.View
	m     *Manager
}

func gitSourceConfig(url string) *config.Source {
	return &config.Source{
		Name: "reg", Kind: config.KindGit, URL: url, Branch: "main",
		Roots: []config.Root{
			{Path: "skills/agent", Invocation: config.Model},
			{Path: "skills/user", Invocation: config.User},
		},
		HarnessRoots: []config.Root{{Path: "harnesses/{harness}/skills", Invocation: config.Model}},
		Template: config.Template{
			Vars: map[string]string{"repo_root": "{snapshot}", "checkout": "/checkout"},
			Env:  []string{"DOCS_DIR"},
		},
		IgnoreFile: ".registry-ignore",
	}
}

func newHarness(t *testing.T, sources ...*config.Source) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:     t,
		dirs:  paths.Dirs{Config: filepath.Join(root, "config"), Cache: filepath.Join(root, "cache"), State: filepath.Join(root, "state")},
		cfg:   &config.Config{FetchInterval: time.Minute, SnapshotRetention: time.Hour, Sources: sources},
		clock: &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		env:   map[string]string{"DOCS_DIR": "docs/x"},
		bases: map[string]map[string]bool{},
	}
	h.m = h.newManager()
	// Snapshots are read-only; unlock them before t.TempDir's cleanup runs.
	t.Cleanup(func() { _ = removeTree(h.dirs.Cache) })
	return h
}

func (h *harness) newManager() *Manager {
	return NewManager(Options{
		Config: h.cfg,
		Dirs:   h.dirs,
		Git:    gitx.New(context.Background()),
		Getenv: func(k string) (string, bool) {
			v, ok := h.env[k]
			return v, ok
		},
		BaseRevisions: func() map[string]map[string]bool { return h.bases },
		OnViews:       func(v map[config.HarnessID]*catalog.View) { h.views = append(h.views, v) },
		Now:           h.clock.Now,
	})
}

func (h *harness) init() {
	h.t.Helper()
	if err := h.m.Init(); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) refresh(name string) {
	h.t.Helper()
	if err := h.m.Refresh(context.Background(), name, RefreshOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) lastView(id config.HarnessID) *catalog.View {
	h.t.Helper()
	if len(h.views) == 0 {
		h.t.Fatal("no views built")
	}
	return h.views[len(h.views)-1][id]
}

func readSnapshotFile(t *testing.T, m *Manager, source, rev, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.SnapshotDir(source, rev), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func diagCodes(ds []catalog.Diagnostic) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Code+"@"+d.Bundle)
	}
	return strings.Join(out, ",")
}
