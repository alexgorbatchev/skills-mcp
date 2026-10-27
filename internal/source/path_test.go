package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
