package source

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func readOnly(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func TestInitErrors(t *testing.T) {
	h := newHarness(t, gitSourceConfig("file:///x"))
	readOnly(t, h.dirs.Cache)
	if err := h.m.Init(); err == nil || !strings.Contains(err.Error(), "source reg") {
		t.Fatalf("Init with read-only cache = %v", err)
	}

	h = newHarness(t, gitSourceConfig("file:///x"))
	idx := filepath.Join(h.dirs.SourceCache("reg"), "snapshots.json")
	if err := os.MkdirAll(idx, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Init(); err == nil {
		t.Fatal("Init with an unreadable index succeeded")
	}
}

func TestRefreshErrors(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	u.commit("one")

	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	readOnly(t, filepath.Join(h.dirs.SourceCache("reg"), "rev"))
	if err := h.m.Refresh(context.Background(), "reg", RefreshOptions{}); err == nil || !strings.Contains(err.Error(), "creating extraction directory") {
		t.Fatalf("Refresh with read-only rev dir = %v", err)
	}

	h = newHarness(t, gitSourceConfig(u.url()))
	h.init()
	readOnly(t, filepath.Dir(h.m.Repo("reg").Dir))
	if err := h.m.Refresh(context.Background(), "reg", RefreshOptions{}); err == nil || !strings.Contains(err.Error(), "creating clone directory") {
		t.Fatalf("Refresh with read-only state dir = %v", err)
	}

	local := t.TempDir()
	writeFile(t, filepath.Join(local, "agent/a/SKILL.md"), skillDoc("a", ""), 0o644)
	h = newHarness(t, pathSource(local))
	h.init()
	if err := os.Chmod(filepath.Join(local, "agent/a/SKILL.md"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Refresh(context.Background(), "local", RefreshOptions{}); err == nil {
		t.Fatal("Refresh of an unreadable path source succeeded")
	}
}

func TestEnsureSnapshotMissingCommit(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	if err := h.m.EnsureSnapshot(context.Background(), "reg", strings.Repeat("1", 40)); err == nil {
		t.Fatal("EnsureSnapshot of a missing commit succeeded")
	}
}

func TestPrepareMessages(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/quiet/SKILL.md", skillDoc("quiet", ""))
	u.write("skills/agent/quiet/package.json", "{}")
	sha := u.commit("one")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{
		{WhenFile: "package.json", Run: []string{"sh", "-c", "exit 3"}, Timeout: 10 * time.Second},
		{WhenFile: "package.json", Run: []string{"sh", "-c", "touch never"}, Timeout: 10 * time.Second},
	}
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")
	rec, _ := h.m.Record("reg", sha)
	if len(rec.Diagnostics) != 1 || !strings.HasSuffix(rec.Diagnostics[0].Message, ": exit status 3") {
		t.Fatalf("diagnostics = %+v", rec.Diagnostics)
	}
	if _, err := os.Stat(filepath.Join(h.m.SnapshotDir("reg", sha), "skills/agent/quiet/never")); err == nil {
		t.Fatal("later rule ran after an earlier rule failed")
	}

	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond}}
	h = newHarness(t, src)
	h.init()
	h.refresh("reg")
	rec, _ = h.m.Record("reg", sha)
	if len(rec.Diagnostics) != 1 || !strings.Contains(rec.Diagnostics[0].Message, "timed out") {
		t.Fatalf("diagnostics = %+v", rec.Diagnostics)
	}
}

func TestInheritWithoutBase(t *testing.T) {
	e := &extraction{}
	rec := &catalog.RenderRecord{}
	e.inheritPrepareDiagnostics("b", rec)
	if len(rec.Diagnostics) != 0 {
		t.Fatal("inherited from a nil base")
	}
}

func TestWriteFileAtomicErrors(t *testing.T) {
	if err := writeFileAtomic(filepath.Join(t.TempDir(), "missing", "f"), nil); err == nil {
		t.Fatal("write into a missing directory succeeded")
	}
	if err := writeJSONAtomic(filepath.Join(t.TempDir(), "f"), func() {}); err == nil {
		t.Fatal("encoding a function succeeded")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(target, []byte("x")); err == nil {
		t.Fatal("renaming over a non-empty directory succeeded")
	}
}

func TestFinalizeReplace(t *testing.T) {
	rev := t.TempDir()
	mk := func(content string) *tree {
		dir, err := os.MkdirTemp(rev, ".tmp-")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "f"), content, 0o644)
		return &tree{dir: dir, record: &catalog.RenderRecord{Revision: "r"}}
	}
	if err := mk("one").finalize(rev, false); err != nil {
		t.Fatal(err)
	}
	if err := mk("two").finalize(rev, true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(rev, "r", "f"))
	if err != nil || string(data) != "two" {
		t.Fatalf("replace kept %q", data)
	}
	entries, _ := os.ReadDir(rev)
	if len(entries) != 2 {
		t.Fatalf("leftovers: %v", entries)
	}
	readOnly(t, rev)
	absent := &tree{dir: filepath.Join(rev, "absent"), record: &catalog.RenderRecord{Revision: "r2"}}
	if err := absent.finalize(rev, true); err == nil {
		t.Fatal("finalize into a read-only directory succeeded")
	}
}

func TestClearAndCleanErrors(t *testing.T) {
	if err := clearDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("clearDir of a missing directory succeeded")
	}
	if err := cleanRevDir(filepath.Join(t.TempDir(), "missing"), Index{}); err == nil {
		t.Fatal("cleanRevDir of a missing directory succeeded")
	}
	if err := cleanGitState(filepath.Join(t.TempDir(), "absent.git")); err != nil {
		t.Fatalf("cleanGitState of a missing repo = %v", err)
	}
	if _, err := listFiles(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("listFiles of a missing directory succeeded")
	}
}

func TestReadBatchErrors(t *testing.T) {
	blobs := []treeEntry{{oid: "abc", path: "f", mode: "100644"}}
	tests := []struct{ name, in, want string }{
		{"eof", "", "reading batch header"},
		{"wrong oid", "xyz blob 1\nx\n", "unexpected batch header"},
		{"bad size", "abc blob nope\n", "parsing blob size"},
		{"no separator", "abc blob 1\nx", "reading batch separator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := readBatch(bufio.NewReader(strings.NewReader(tt.in)), t.TempDir(), blobs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("readBatch = %v, want %q", err, tt.want)
			}
		})
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "f"), "exists", 0o644)
	if err := readBatch(bufio.NewReader(strings.NewReader("abc blob 1\nx\n")), dir, blobs); err == nil {
		t.Fatal("overwrote an existing file")
	}
}

func TestCacheAccessor(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	snaps, err := h.m.CurrentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	views, err := h.m.Cache().Views(snaps)
	if err != nil || views[config.ClaudeCode].Skill("a") == nil {
		t.Fatalf("Cache().Views = %v, %v", views, err)
	}
}

func TestManagerDefaults(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "<%- env(\"SKILLS_MCP_TEST_UNSET\", \"fb\") %>"))
	sha := u.commit("one")
	src := gitSourceConfig(u.url())
	src.Template.Env = []string{"SKILLS_MCP_TEST_UNSET"}
	h := newHarness(t, src)
	m := NewManager(Options{Config: h.cfg, Dirs: h.dirs, Git: h.m.opts.Git})
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(context.Background(), "reg", RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := readSnapshotFile(t, m, "reg", sha, "skills/agent/a/SKILL.md"); !strings.HasSuffix(got, "fb") {
		t.Fatalf("default getenv not used: %q", got)
	}
}

func TestPruneEmptyDirs(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "a/b/c"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "keep/file"), "x", 0o644)
	if err := pruneEmptyDirs(base); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "a")); err == nil {
		t.Fatal("empty tree kept")
	}
	if _, err := os.Stat(filepath.Join(base, "keep/file")); err != nil {
		t.Fatal("non-empty directory removed")
	}
	if err := pruneEmptyDirs(filepath.Join(base, "missing")); err == nil {
		t.Fatal("pruning a missing directory succeeded")
	}
	locked := filepath.Join(base, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	readOnly(t, locked)
	if err := pruneEmptyDirs(base); err == nil {
		t.Fatal("removing from a read-only directory succeeded")
	}
}

func TestFailedPrepareNeverSharesCacheInodes(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "pkg-cache")
	writeFile(t, filepath.Join(cache, "f"), "cached", 0o644)
	u := newUpstream(t)
	u.write("skills/agent/dep/SKILL.md", skillDoc("dep", ""))
	u.write("skills/agent/dep/package.json", "{}")
	sha := u.commit("one")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", "ln " + filepath.Join(cache, "f") + " hl; exit 1"}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")
	info, err := os.Stat(filepath.Join(cache, "f"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("package cache file mode = %v, %v", info.Mode(), err)
	}
	rec, _ := h.m.Record("reg", sha)
	if len(rec.Diagnostics) != 1 || rec.Diagnostics[0].Code != catalog.CodePrepareFailed {
		t.Fatalf("diagnostics = %+v", rec.Diagnostics)
	}
}

func TestPrepareRefusesChainedEscapingLinks(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/dep/SKILL.md", skillDoc("dep", ""))
	u.write("skills/agent/dep/package.json", "{}")
	sha := u.commit("one")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", "mkdir -p p/q && ln -s .. p/q/b && ln -s p/q/b/../../.. a"}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")
	rec, _ := h.m.Record("reg", sha)
	if len(rec.Diagnostics) != 1 || !strings.Contains(rec.Diagnostics[0].Message, "resolves outside its directory") {
		t.Fatalf("diagnostics = %+v", rec.Diagnostics)
	}
	src.Prepare[0].Run = []string{"sh", "-c", "ln -s missing dangling"}
	h = newHarness(t, src)
	h.init()
	h.refresh("reg")
	rec, _ = h.m.Record("reg", sha)
	if len(rec.Diagnostics) != 1 || !strings.Contains(rec.Diagnostics[0].Message, "does not resolve") {
		t.Fatalf("dangling diagnostics = %+v", rec.Diagnostics)
	}
}

func TestShutdownStopsPrepare(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/dep/SKILL.md", skillDoc("dep", ""))
	u.write("skills/agent/dep/package.json", "{}")
	sha := u.commit("one")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"touch", "ran"}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	h.m.opts.Shutdown = stopped
	h.init()
	err := h.m.Refresh(context.Background(), "reg", RefreshOptions{})
	if err == nil || !strings.Contains(err.Error(), "interrupted by shutdown") || h.m.Retained("reg", sha) {
		t.Fatalf("Refresh = %v, retained = %v", err, h.m.Retained("reg", sha))
	}
}
