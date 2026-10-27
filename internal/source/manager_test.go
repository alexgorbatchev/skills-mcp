package source

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func TestGitRefreshExtractsRenderedSnapshot(t *testing.T) {
	u := newUpstream(t)
	u.write("package.json", `{"workspaces":["skills/agent/tool"]}`)
	u.write("README.md", "outside roots")
	u.write("skills/agent/tool/SKILL.md", skillDoc("tool", "root <%- repo_root %>\ndir <%- file_dir %>\ndocs <%- env(\"DOCS_DIR\", \"fb\") %>\nck <%- checkout %>\n"))
	u.write("skills/agent/tool/references/ref.md", "ref <%- file_path %>")
	u.write("skills/agent/tool/.registry-ignore", "scripts/__tests__/\njustfile\n")
	u.write("skills/agent/tool/justfile", "ignored")
	u.write("skills/agent/tool/scripts/__tests__/a.test.ts", "ignored")
	u.write("skills/agent/tool/bin.png", "\x00binary <%- nope %>")
	writeFile(t, filepath.Join(u.dir, "skills/agent/tool/scripts/run.sh"), "#!/bin/sh\n", 0o755)
	u.write("skills/user/commit/SKILL.md", skillDoc("commit", "body"))
	u.write("harnesses/pi/skills/pi-only/SKILL.md", skillDoc("pi-only", "pi"))
	u.write("skills/agent/broken/SKILL.md", skillDoc("broken", "<%= bad %>\n"))
	sha := u.commit("init")

	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")

	if got := h.m.Current("reg"); got != sha {
		t.Fatalf("current = %q, want %q", got, sha)
	}
	snap := h.m.SnapshotDir("reg", sha)
	body := readSnapshotFile(t, h.m, "reg", sha, "skills/agent/tool/SKILL.md")
	for _, want := range []string{"root " + snap, "dir " + filepath.Join(snap, "skills/agent/tool"), "docs docs/x", "ck /checkout"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered SKILL.md missing %q:\n%s", want, body)
		}
	}
	if got := readSnapshotFile(t, h.m, "reg", sha, "skills/agent/tool/references/ref.md"); got != "ref "+filepath.Join(snap, "skills/agent/tool/references/ref.md") {
		t.Errorf("ref.md = %q", got)
	}
	if got := readSnapshotFile(t, h.m, "reg", sha, "skills/agent/tool/bin.png"); !strings.Contains(got, "<%- nope %>") {
		t.Error("binary file was rendered")
	}
	for _, gone := range []string{"package.json", "README.md", "skills/agent/tool/justfile", "skills/agent/tool/scripts/__tests__", "skills/agent/tool/.registry-ignore"} {
		if _, err := os.Stat(filepath.Join(snap, gone)); err == nil {
			t.Errorf("%s reached the snapshot", gone)
		}
	}
	if raw := readSnapshotFile(t, h.m, "reg", sha, "skills/agent/broken/SKILL.md"); !strings.Contains(raw, "<%= bad %>") {
		t.Error("refused bundle was rendered")
	}
	assertMode(t, filepath.Join(snap, "skills/agent/tool/scripts/run.sh"), 0o555)
	assertMode(t, filepath.Join(snap, "skills/agent/tool/SKILL.md"), 0o444)
	assertMode(t, filepath.Join(snap, "skills/agent/tool"), 0o555|os.ModeDir)

	rec, err := h.m.Record("reg", sha)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.Files["skills/agent/tool"], ","); got != "SKILL.md,bin.png,references/ref.md,scripts/run.sh" {
		t.Errorf("files = %s", got)
	}
	if got := diagCodes(rec.Diagnostics); got != "unsupported_template_tag@skills/agent/broken" {
		t.Errorf("diagnostics = %s", got)
	}
	cc := h.lastView(config.ClaudeCode)
	if cc.Skill("tool") == nil || cc.Skill("commit") == nil || cc.Skill("pi-only") != nil {
		t.Errorf("claude view = %+v", cc.Skills)
	}
	if h.lastView(config.Pi).Skill("pi-only") == nil {
		t.Error("pi view lacks its harness skill")
	}
	if h.m.Views() == nil || h.m.Sources()[0] != "reg" || h.m.Source("reg") == nil || h.m.Source("nope") != nil {
		t.Error("accessors broken")
	}
}

func assertMode(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (os.ModeDir | os.ModePerm); got != want {
		t.Errorf("%s mode = %v, want %v", p, got, want)
	}
}

func TestRefreshMovesCurrentAndRetains(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "v1"))
	first := u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	views := len(h.views)

	// A refresh with no upstream change rebuilds nothing.
	h.refresh("reg")
	if len(h.views) != views {
		t.Fatal("views rebuilt without a change")
	}

	u.write("skills/agent/a/SKILL.md", skillDoc("a", "v2"))
	second := u.commit("two")
	h.refresh("reg")
	if h.m.Current("reg") != second || !h.m.Retained("reg", first) {
		t.Fatalf("current = %s, first retained = %v", h.m.Current("reg"), h.m.Retained("reg", first))
	}
	if h.lastView(config.ClaudeCode).Skill("a").Body != "v2" {
		t.Fatal("view not rebuilt")
	}

	// The base revision of an open change survives retention.
	h.bases["reg"] = map[string]bool{first: true}
	h.clock.advance(2 * time.Hour)
	h.refresh("reg")
	if !h.m.Retained("reg", first) {
		t.Fatal("retention deleted an open change's base")
	}
	h.bases["reg"] = nil
	h.refresh("reg")
	if h.m.Retained("reg", first) || !h.m.Retained("reg", second) {
		t.Fatal("retention did not delete the superseded snapshot")
	}
	if _, err := h.m.Snapshot("reg", first); err == nil {
		t.Fatal("Snapshot of a deleted revision succeeded")
	}
}

func TestRefreshFetchFailureServesLocalRef(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "v1"))
	sha := u.commit("one")
	src := gitSourceConfig(u.url())
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")

	// Offline restart after a render-context change: the snapshot is wiped
	// and must be re-extracted from repo.git.
	src.URL = "file:///nonexistent/repo"
	h.env["DOCS_DIR"] = "changed"
	h.m = h.newManager()
	h.init()
	if h.m.Current("reg") != "" {
		t.Fatal("render-context reset kept current")
	}
	err := h.m.Refresh(context.Background(), "reg", RefreshOptions{})
	if err == nil || !strings.Contains(err.Error(), "fetching") {
		t.Fatalf("Refresh error = %v", err)
	}
	if h.m.Current("reg") != sha || !h.m.Retained("reg", sha) {
		t.Fatal("offline refresh did not serve the local ref")
	}
}

func TestRefreshCloneFailure(t *testing.T) {
	h := newHarness(t, gitSourceConfig("file:///nonexistent/repo"))
	h.init()
	if err := h.m.Refresh(context.Background(), "reg", RefreshOptions{}); err == nil || !strings.Contains(err.Error(), "cloning") {
		t.Fatalf("error = %v", err)
	}
	if h.m.Current("reg") != "" {
		t.Fatal("current set without a clone")
	}
	snaps, err := h.m.CurrentSnapshots()
	if err != nil || len(snaps) != 0 {
		t.Fatalf("snapshots = %v, %v", snaps, err)
	}
	if err := h.m.Refresh(context.Background(), "nope", RefreshOptions{}); err == nil {
		t.Fatal("unknown source refreshed")
	}
	if h.m.Current("nope") != "" || h.m.Repo("nope") != nil {
		t.Fatal("unknown source accessors returned values")
	}
}

func TestOriginFollowsConfiguredURL(t *testing.T) {
	a := newUpstream(t)
	a.write("skills/agent/a/SKILL.md", skillDoc("a", "from a"))
	a.commit("a")
	b := newUpstream(t)
	b.write("skills/agent/a/SKILL.md", skillDoc("a", "from b"))
	shaB := b.commit("b")
	src := gitSourceConfig(a.url())
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")
	src.URL = b.url()
	h.m = h.newManager()
	h.init()
	h.refresh("reg")
	if h.m.Current("reg") != shaB {
		t.Fatal("origin not updated to the configured URL")
	}
}

func TestStartupCleanup(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	sha := u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	rev := filepath.Join(h.dirs.SourceCache("reg"), "rev")
	repo := h.m.Repo("reg").Dir
	debris := []string{
		filepath.Join(rev, ".tmp-abc-123"),
		filepath.Join(rev, "unindexed"),
		repo + ".tmp-999",
	}
	for _, d := range debris {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(rev, "unindexed.render.json"), "{}", 0o644)
	writeFile(t, filepath.Join(rev, "orphan.render.json"), "{}", 0o644)
	writeFile(t, filepath.Join(repo, "index.lock"), "", 0o644)
	writeFile(t, filepath.Join(repo, "refs", "heads", "main.lock"), "", 0o644)

	h.m = h.newManager()
	h.init()
	for _, p := range append(debris,
		filepath.Join(rev, "unindexed.render.json"), filepath.Join(rev, "orphan.render.json"),
		filepath.Join(repo, "index.lock"), filepath.Join(repo, "refs", "heads", "main.lock")) {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived startup", p)
		}
	}
	if !h.m.Retained("reg", sha) {
		t.Fatal("startup deleted the current snapshot")
	}
}

func TestCorruptIndexRebuilt(t *testing.T) {
	h := newHarness(t, gitSourceConfig("file:///x"))
	idx := filepath.Join(h.dirs.SourceCache("reg"), "snapshots.json")
	writeFile(t, idx, "not json", 0o644)
	h.init()
	var got Index
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil || got.RenderContext == "" {
		t.Fatalf("index = %s", data)
	}
}

func TestSpecialEntries(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/ok/SKILL.md", skillDoc("ok", ""))
	u.write("skills/agent/inner/SKILL.md", skillDoc("inner", ""))
	if err := os.Symlink("../ok/SKILL.md", filepath.Join(u.dir, "skills/agent/inner/link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ok", filepath.Join(u.dir, "skills/agent/linked")); err != nil {
		t.Fatal(err)
	}
	u.git("add", "-A")
	u.git("update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",skills/agent/vendored")
	u.git("commit", "-q", "-m", "specials")
	sha := u.git("rev-parse", "HEAD")

	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	rec, err := h.m.Record("reg", sha)
	if err != nil {
		t.Fatal(err)
	}
	want := "symlink@skills/agent/inner,symlink@skills/agent/linked,submodule@skills/agent/vendored"
	if got := diagCodes(rec.Diagnostics); got != want {
		t.Fatalf("diagnostics = %s, want %s", got, want)
	}
	if got := diagCodes(h.lastView(config.ClaudeCode).Diagnostics); got != want {
		t.Fatalf("view diagnostics = %s", got)
	}
	if _, err := os.Lstat(filepath.Join(h.m.SnapshotDir("reg", sha), "skills/agent/linked")); err == nil {
		t.Fatal("symlink materialized")
	}
}

func TestMissingSkillMD(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	u.write("skills/agent/a/.registry-ignore", "SKILL.md\n")
	sha := u.commit("hide")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	rec, _ := h.m.Record("reg", sha)
	if got := diagCodes(rec.Diagnostics); got != "missing_skill_md@skills/agent/a" {
		t.Fatalf("diagnostics = %s", got)
	}
}

func TestPrepare(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/dep/SKILL.md", skillDoc("dep", ""))
	u.write("skills/agent/dep/scripts/package.json", `{}`)
	u.write("skills/agent/dep/scripts/bun.lock", "lock")
	u.write("skills/agent/dep/.registry-ignore", "scripts/bun.lock\nnode_modules/\n")
	u.write("skills/agent/plain/SKILL.md", skillDoc("plain", ""))
	sha := u.commit("deps")

	marker := filepath.Join(t.TempDir(), "fail")
	src := gitSourceConfig(u.url())
	install := `test -e ` + marker + ` && { echo "registry unreachable" >&2; exit 7; }
mkdir -p node_modules/pkg/.bin node_modules/inner
echo dep > node_modules/pkg/index.js
printf '#!/bin/sh\n' > node_modules/pkg/cli.sh; chmod 755 node_modules/pkg/cli.sh
ln -s ../cli.sh node_modules/pkg/.bin/cli
echo shared > node_modules/shared.js; ln node_modules/shared.js node_modules/shared-link.js
echo '{}' > node_modules/inner/package.json`
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", install}, Keep: []string{"bun.lock"}, Timeout: 10 * time.Second}}
	writeFile(t, marker, "", 0o644)

	h := newHarness(t, src)
	h.init()
	h.refresh("reg")
	rec, _ := h.m.Record("reg", sha)
	if got := diagCodes(rec.Diagnostics); got != "prepare_failed@skills/agent/dep" || !rec.PrepareRetry {
		t.Fatalf("diagnostics = %s retry=%v", got, rec.PrepareRetry)
	}
	if !strings.Contains(rec.Diagnostics[0].Message, "exit status 7: registry unreachable") {
		t.Fatalf("message = %q", rec.Diagnostics[0].Message)
	}
	if h.lastView(config.ClaudeCode).Skill("dep") != nil {
		t.Fatal("failed bundle served")
	}

	// Before the backoff elapses nothing is retried.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	h.refresh("reg")
	if rec, _ := h.m.Record("reg", sha); !rec.PrepareRetry {
		t.Fatal("retried before the backoff elapsed")
	}
	h.clock.advance(time.Minute)
	h.refresh("reg")
	rec, _ = h.m.Record("reg", sha)
	if rec.PrepareRetry || len(rec.Diagnostics) != 0 {
		t.Fatalf("retry did not succeed: %+v", rec.Diagnostics)
	}
	if h.lastView(config.ClaudeCode).Skill("dep") == nil {
		t.Fatal("repaired bundle not served after retry")
	}
	scripts := filepath.Join(h.m.SnapshotDir("reg", sha), "skills/agent/dep/scripts")
	if _, err := os.Stat(filepath.Join(scripts, "bun.lock")); err != nil {
		t.Fatal("kept lockfile was filtered")
	}
	assertMode(t, filepath.Join(scripts, "node_modules/pkg/cli.sh"), 0o555)
	assertMode(t, filepath.Join(scripts, "node_modules/pkg/index.js"), 0o444)
	info, err := os.Stat(filepath.Join(scripts, "node_modules/shared.js"))
	if err != nil {
		t.Fatal(err)
	}
	if n := info.Sys().(*syscall.Stat_t).Nlink; n != 1 {
		t.Fatalf("hard link survived: nlink = %d", n)
	}
	if got := strings.Join(rec.Files["skills/agent/dep"], ","); got != "SKILL.md,scripts/bun.lock,scripts/package.json" {
		t.Fatalf("files include prepare output: %s", got)
	}
}

func TestPrepareRefusesEscapingLinks(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/dep/SKILL.md", skillDoc("dep", ""))
	u.write("skills/agent/dep/package.json", `{}`)
	sha := u.commit("deps")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", "ln -s ../../../../.. escape"}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")
	rec, _ := h.m.Record("reg", sha)
	if len(rec.Diagnostics) != 1 || !strings.Contains(rec.Diagnostics[0].Message, "link escape resolves outside its directory") {
		t.Fatalf("diagnostics = %+v", rec.Diagnostics)
	}
}

func TestPrepareCancellationAbortsExtraction(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/dep/SKILL.md", skillDoc("dep", ""))
	u.write("skills/agent/dep/package.json", `{}`)
	sha := u.commit("deps")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sleep", "30"}, Timeout: time.Minute}}
	h := newHarness(t, src)
	h.init()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := h.m.Refresh(ctx, "reg", RefreshOptions{})
	if err == nil || !strings.Contains(err.Error(), "prepare interrupted") {
		t.Fatalf("error = %v", err)
	}
	if h.m.Retained("reg", sha) {
		t.Fatal("interrupted extraction was finalized")
	}
	entries, _ := os.ReadDir(filepath.Join(h.dirs.SourceCache("reg"), "rev"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") || strings.HasSuffix(e.Name(), ".render.json") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestEnsureSnapshotAndMaterialize(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "<%- repo_root %>"))
	u.write("skills/agent/a/package.json", "{}")
	u.write("skills/agent/b/SKILL.md", skillDoc("b", ""))
	u.write("skills/agent/b/package.json", "{}")
	first := u.commit("one")
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "v2 <%- repo_root %>"))
	second := u.commit("two")
	src := gitSourceConfig(u.url())
	src.Prepare = []config.PrepareRule{{WhenFile: "package.json", Run: []string{"sh", "-c", "touch prepared"}, Timeout: 10 * time.Second}}
	h := newHarness(t, src)
	h.init()
	h.refresh("reg")

	if err := h.m.EnsureSnapshot(context.Background(), "reg", first); err != nil {
		t.Fatal(err)
	}
	if !h.m.Retained("reg", first) {
		t.Fatal("EnsureSnapshot did not extract")
	}
	if err := h.m.EnsureSnapshot(context.Background(), "reg", first); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(2 * time.Hour)
	h.refresh("reg")
	if h.m.Retained("reg", first) {
		t.Fatal("on-demand snapshot never retired")
	}

	base := &catalog.RenderRecord{Diagnostics: []catalog.Diagnostic{{Code: catalog.CodePrepareFailed, Bundle: "skills/agent/b", Message: "old"}}}
	tree, err := h.m.MaterializeCommit(context.Background(), "reg", second, []string{"skills/agent/a"}, base)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Remove()
	if !strings.HasPrefix(filepath.Base(tree.Dir), ".tmp-verify-") {
		t.Fatalf("tree dir = %s", tree.Dir)
	}
	body, err := os.ReadFile(filepath.Join(tree.Dir, "skills/agent/a/SKILL.md"))
	if err != nil || !strings.Contains(string(body), "v2 "+tree.Dir) {
		t.Fatalf("verification tree not rendered in place: %s", body)
	}
	if _, err := os.Stat(filepath.Join(tree.Dir, "skills/agent/a/prepared")); err != nil {
		t.Fatal("touched bundle not prepared")
	}
	if _, err := os.Stat(filepath.Join(tree.Dir, "skills/agent/b/prepared")); err == nil {
		t.Fatal("untouched bundle prepared")
	}
	if got := diagCodes(tree.Record.Diagnostics); got != "prepare_failed@skills/agent/b" {
		t.Fatalf("inherited diagnostics = %s", got)
	}
	if err := tree.Remove(); err != nil {
		t.Fatal(err)
	}
}

func TestMaintain(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()), &config.Source{Name: "local", Kind: config.KindPath, Path: t.TempDir()})
	h.init()
	h.refresh("reg")
	if err := h.m.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.m.SourceLock("reg") == nil {
		t.Fatal("no source lock")
	}
}

func TestSnapshotErrors(t *testing.T) {
	h := newHarness(t, gitSourceConfig("file:///x"))
	h.init()
	if _, err := h.m.Snapshot("nope", "x"); err == nil {
		t.Fatal("unknown source snapshot succeeded")
	}
	if _, err := h.m.Snapshot("reg", ""); err == nil {
		t.Fatal("empty revision snapshot succeeded")
	}
	if h.m.Retained("nope", "x") {
		t.Fatal("unknown source retained")
	}
	if _, err := h.m.Record("reg", "missing"); err == nil {
		t.Fatal("missing record loaded")
	}
	rev := filepath.Join(h.dirs.SourceCache("reg"), "rev")
	writeFile(t, filepath.Join(rev, "bad.render.json"), "{", 0o644)
	if _, err := h.m.Record("reg", "bad"); err == nil || !errors.Is(err, err) {
		t.Fatal("corrupt record loaded")
	}
}

func TestShadowedAcrossSources(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "from git"))
	u.write("skills/agent/broken/SKILL.md", "no frontmatter")
	u.commit("one")
	local := t.TempDir()
	writeFile(t, filepath.Join(local, "skills/agent/a/SKILL.md"), skillDoc("a", "from path"), 0o644)
	ps := pathSource(local)
	ps.Roots = []config.Root{{Path: "skills/agent", Invocation: config.Model}}
	var logs strings.Builder
	h := newHarness(t, gitSourceConfig(u.url()), ps)
	h.m.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.init()
	h.refresh("reg")
	h.refresh("local")
	if h.lastView(config.ClaudeCode).Skill("a").Body != "from git" {
		t.Fatal("later source won")
	}
	h.m.invalidate(h.m.byName["reg"], h.m.Current("reg"))
	if err := h.m.RebuildViews(); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if strings.Count(out, "skill shadowed") != 1 || strings.Count(out, "skill refused") != 1 {
		t.Fatalf("logs:\n%s", out)
	}
}

func TestConcurrentExtractionKeepsFirst(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	sha := u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	before, err := os.Stat(h.m.SnapshotDir("reg", sha))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.extractGit(context.Background(), h.m.byName["reg"], sha, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(h.m.SnapshotDir("reg", sha))
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("existing snapshot replaced without replace")
	}
	if _, err := h.m.extractGit(context.Background(), h.m.byName["reg"], strings.Repeat("0", 40), false); err == nil {
		t.Fatal("extracting a missing commit succeeded")
	}
}

func TestTipWithoutBranch(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	repo := h.m.Repo("reg")
	repo.Branch = "missing"
	if _, err := repo.Tip(context.Background()); err == nil {
		t.Fatal("Tip of a missing branch succeeded")
	}
	if err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("fetching a missing branch succeeded")
	}
	if err := os.RemoveAll(filepath.Join(repo.Dir, "config")); err != nil {
		t.Fatal(err)
	}
	if err := repo.Fetch(context.Background()); err == nil {
		t.Fatal("fetch without a remote succeeded")
	}
}

func TestLoadIndexesReadOnly(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", ""))
	sha := u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	tmp := filepath.Join(h.dirs.SourceCache("reg"), "rev", ".tmp-in-progress")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	m := h.newManager()
	if err := m.LoadIndexes(); err != nil {
		t.Fatal(err)
	}
	if m.Current("reg") != sha {
		t.Fatal("index not loaded")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatal("LoadIndexes cleaned up a running daemon's extraction")
	}
	if err := os.Chmod(filepath.Join(h.dirs.SourceCache("reg"), "snapshots.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	if err := m.LoadIndexes(); err == nil {
		t.Fatal("unreadable index loaded")
	}
}

func TestStaleRebuildNeverRegressesViews(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "v1"))
	u.commit("one")
	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	staleSnaps, staleGens, err := h.m.currentSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "v2"))
	u.commit("two")
	h.refresh("reg")
	if err := h.m.rebuildFrom(staleSnaps, staleGens); err != nil {
		t.Fatal(err)
	}
	if got := h.m.Views()[config.ClaudeCode].Skill("a").Body; got != "v2" {
		t.Fatalf("stale rebuild regressed the view to %q", got)
	}
}

func TestRefreshWaitHonorsDeadline(t *testing.T) {
	h := newHarness(t, gitSourceConfig("file:///x"))
	h.init()
	st := h.m.byName["reg"]
	st.refreshLock <- struct{}{} // a long refresh holds the lock
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := h.m.Refresh(ctx, "reg", RefreshOptions{})
	if err == nil || !strings.Contains(err.Error(), "waiting to refresh reg") {
		t.Fatalf("Refresh = %v", err)
	}
	<-st.refreshLock
}

// A global configuration that renames the default remote and refuses
// implicit bare repositories still clones, fetches, extracts, and maintains.
func TestHostileGlobalGitConfig(t *testing.T) {
	u := newUpstream(t)
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "one"))
	u.commit("one")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	writeFile(t, filepath.Join(home, ".gitconfig"), "[clone]\n\tdefaultRemoteName = upstream\n[safe]\n\tbareRepository = explicit\n", 0o644)

	h := newHarness(t, gitSourceConfig(u.url()))
	h.init()
	h.refresh("reg")
	u.write("skills/agent/a/SKILL.md", skillDoc("a", "two"))
	two := u.commit("two")
	h.refresh("reg")
	if got := h.m.Current("reg"); got != two {
		t.Fatalf("current = %q, want %q", got, two)
	}
	if err := h.m.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
