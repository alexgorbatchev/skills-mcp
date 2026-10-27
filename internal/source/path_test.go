package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func pathSource(dir string) *config.Source {
	return &config.Source{
		Name: "local", Kind: config.KindPath, Path: dir,
		Roots:    []config.Root{{Path: "agent", Invocation: config.Model}},
		Template: config.Template{Vars: map[string]string{"repo_root": "{snapshot}"}},
	}
}

func TestPathSource(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", "at <%- repo_root %>"), 0o644)
	writeFile(t, filepath.Join(real, "agent/a/run.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(real, "outside/x"), "not copied", 0o644)
	if err := os.Symlink("a", filepath.Join(real, "agent/linked")); err != nil {
		t.Fatal(err)
	}
	// The configured path is itself a symlink; it is resolved once.
	link := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, pathSource(link))
	h.init()
	h.refresh("local")
	rev := h.m.Current("local")
	if !strings.HasPrefix(rev, "local-") || len(rev) != len("local-")+12 {
		t.Fatalf("revision = %q", rev)
	}
	snap := h.m.SnapshotDir("local", rev)
	if got := readSnapshotFile(t, h.m, "local", rev, "agent/a/SKILL.md"); !strings.Contains(got, "at "+snap) {
		t.Fatalf("not rendered: %s", got)
	}
	assertMode(t, filepath.Join(snap, "agent/a/run.sh"), 0o555)
	if _, err := os.Stat(filepath.Join(snap, "outside")); err == nil {
		t.Fatal("file outside roots copied")
	}
	if got := diagCodes(h.lastView(config.ClaudeCode).Diagnostics); got != "symlink@agent/linked" {
		t.Fatalf("diagnostics = %s", got)
	}

	// An unchanged tree keeps its revision and rebuilds nothing.
	views := len(h.views)
	h.refresh("local")
	if h.m.Current("local") != rev || len(h.views) != views {
		t.Fatal("unchanged path source produced a new revision")
	}

	// Content changes produce a new revision; so do symlink changes.
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", "changed"), 0o644)
	h.refresh("local")
	second := h.m.Current("local")
	if second == rev {
		t.Fatal("content change kept the revision")
	}
	if err := os.Remove(filepath.Join(real, "agent/linked")); err != nil {
		t.Fatal(err)
	}
	h.refresh("local")
	if h.m.Current("local") == second {
		t.Fatal("symlink removal kept the revision")
	}
	if len(h.lastView(config.ClaudeCode).Diagnostics) != 0 {
		t.Fatal("removed symlink still reported")
	}
}

func TestPathSourceMissingRoot(t *testing.T) {
	h := newHarness(t, pathSource(t.TempDir()))
	h.init()
	h.refresh("local")
	if h.m.Current("local") == "" {
		t.Fatal("empty path source has no revision")
	}
}

func TestPathSourceUnresolvable(t *testing.T) {
	h := newHarness(t, pathSource(filepath.Join(t.TempDir(), "missing")))
	if err := h.m.Init(); err == nil {
		t.Fatal("Init with a missing path source succeeded")
	}
}

func TestWatch(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", "one"), 0o644)
	h := newHarness(t, pathSource(real), gitSourceConfig("file:///unused"))
	h.init()
	h.refresh("local")
	first := h.m.Current("local")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.m.Watch(ctx) }()
	time.Sleep(200 * time.Millisecond) // let the watcher register

	// A new directory is watched too; a write inside it triggers a refresh.
	writeFile(t, filepath.Join(real, "agent/b/SKILL.md"), skillDoc("b", "two"), 0o644)
	time.Sleep(100 * time.Millisecond)
	writeFile(t, filepath.Join(real, "agent/b/extra.md"), "x", 0o644)
	deadline := time.Now().Add(5 * time.Second)
	for h.m.Current("local") == first && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	cur := h.m.Current("local")
	if cur == first {
		t.Fatal("watcher did not refresh")
	}
	rec, err := h.m.Record("local", cur)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rec.Files["agent/b"], ",") != "SKILL.md,extra.md" {
		t.Fatalf("new directory not picked up: %v", rec.Files)
	}
}

func TestOwnerOf(t *testing.T) {
	owner := map[string]string{"/a": "x"}
	if ownerOf(owner, "/a/b/c") != "x" || ownerOf(owner, "/other") != "" {
		t.Fatal("ownerOf wrong")
	}
}

func TestUnchangedPathSourceSkipsPrepare(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", ""), 0o644)
	writeFile(t, filepath.Join(real, "agent/a/package.json"), "{}", 0o644)
	counter := filepath.Join(t.TempDir(), "runs")
	src := pathSource(real)
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", "echo run >> " + counter}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	h.init()
	for range 3 {
		h.refresh("local")
	}
	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(data), "run"); runs != 1 {
		t.Fatalf("prepare ran %d times for an unchanged source", runs)
	}
}

func TestPathSourcePrepareRetry(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", ""), 0o644)
	writeFile(t, filepath.Join(real, "agent/a/package.json"), "{}", 0o644)
	marker := filepath.Join(t.TempDir(), "fail")
	writeFile(t, marker, "", 0o644)
	src := pathSource(real)
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", "test ! -e " + marker}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	h.init()
	h.refresh("local")
	if h.lastView(config.ClaudeCode).Skill("a") != nil {
		t.Fatal("failed bundle served")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(time.Minute)
	h.refresh("local")
	if h.lastView(config.ClaudeCode).Skill("a") == nil {
		t.Fatal("retried bundle not served")
	}

	// A failing render or prepare step fails the refresh.
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"true"}, Timeout: 10 * time.Second}}
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", "changed"), 0o644)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	h.m.opts.Shutdown = stopped
	if err := h.m.Refresh(context.Background(), "local", RefreshOptions{}); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Refresh during shutdown = %v", err)
	}
}

func TestPathSourceRefusesSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "a/SKILL.md"), skillDoc("a", ""), 0o644)
	if err := os.Symlink(elsewhere, filepath.Join(real, "agent")); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, pathSource(real))
	h.init()
	err := h.m.Refresh(context.Background(), "local", RefreshOptions{})
	if err == nil || !strings.Contains(err.Error(), "is a symbolic link") {
		t.Fatalf("Refresh = %v", err)
	}
}

func TestInstalledDependenciesAndIgnoredLinksDoNotRefuse(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "agent/a/SKILL.md"), skillDoc("a", ""), 0o644)
	writeFile(t, filepath.Join(real, "agent/a/node_modules/pkg/cli.js"), "x", 0o755)
	writeFile(t, filepath.Join(real, "agent/a/.registry-ignore"), "fixtures/\n", 0o644)
	if err := os.MkdirAll(filepath.Join(real, "agent/a/node_modules/.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../pkg/cli.js", filepath.Join(real, "agent/a/node_modules/.bin/cli")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(real, "agent/a/fixtures"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../SKILL.md", filepath.Join(real, "agent/a/fixtures/link.md")); err != nil {
		t.Fatal(err)
	}
	src := pathSource(real)
	src.IgnoreFile = ".registry-ignore"
	h := newHarness(t, src)
	h.init()
	h.refresh("local")
	v := h.lastView(config.ClaudeCode)
	if v.Skill("a") == nil || len(v.Diagnostics) != 0 {
		t.Fatalf("skill refused: %+v", v.Diagnostics)
	}
	problems, err := catalog.ValidateDir(filepath.Join(real, "agent/a"), src, os.LookupEnv)
	if err != nil || len(problems) != 0 {
		t.Fatalf("bundle validate disagrees: %v, %v", problems, err)
	}
	// An unignored link still refuses the bundle in both.
	if err := os.Symlink("SKILL.md", filepath.Join(real, "agent/a/real-link.md")); err != nil {
		t.Fatal(err)
	}
	h.refresh("local")
	if got := diagCodes(h.lastView(config.ClaudeCode).Diagnostics); got != "symlink@agent/a" {
		t.Fatalf("diagnostics = %s", got)
	}
	if problems, _ := catalog.ValidateDir(filepath.Join(real, "agent/a"), src, os.LookupEnv); len(problems) != 1 {
		t.Fatalf("bundle validate = %v", problems)
	}
}

func TestIgnoredSymlinkSkillFileRefusesOnlyItsBundle(t *testing.T) {
	real := t.TempDir()
	writeFile(t, filepath.Join(real, "agent/good/SKILL.md"), skillDoc("good", ""), 0o644)
	writeFile(t, filepath.Join(real, "agent/bad/real.md"), skillDoc("bad", ""), 0o644)
	writeFile(t, filepath.Join(real, "agent/bad/.registry-ignore"), "SKILL.md\n", 0o644)
	if err := os.Symlink("real.md", filepath.Join(real, "agent/bad/SKILL.md")); err != nil {
		t.Fatal(err)
	}
	src := pathSource(real)
	src.IgnoreFile = ".registry-ignore"
	h := newHarness(t, src)
	h.init()
	h.refresh("local") // must not fail the whole catalog
	v := h.lastView(config.ClaudeCode)
	if v.Skill("good") == nil || diagCodes(v.Diagnostics) != "missing_skill_md@agent/bad" {
		t.Fatalf("skills = %+v, diagnostics = %s", v.Skills, diagCodes(v.Diagnostics))
	}
	problems, err := catalog.ValidateDir(filepath.Join(real, "agent/bad"), src, os.LookupEnv)
	if err != nil || len(problems) == 0 || !strings.Contains(strings.Join(problems, "\n"), "missing_skill_md") {
		t.Fatalf("bundle validate = %v, %v", problems, err)
	}
}
