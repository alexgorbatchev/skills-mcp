package authoring

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func TestRecoverMissingWorktreeOrphansAndQuarantine(t *testing.T) {
	e := newEnv(t)
	gone := e.open()
	kept := e.open()
	if err := os.RemoveAll(e.worktree(gone)); err != nil {
		t.Fatal(err)
	}
	// An orphaned change directory and branch, and an unreadable record.
	orphan := "chg_00000000000000aa"
	if err := os.MkdirAll(filepath.Join(e.dirs.Changes(), orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := e.src.Repo("reg").Dir
	run(t, repo, "branch", "skills-mcp/change/"+orphan, "refs/heads/main")
	bad := "chg_00000000000000bb"
	write(t, filepath.Join(e.dirs.Changes(), bad, "change.json"), "{not json")
	if err := os.MkdirAll(filepath.Join(e.dirs.Changes(), ".quarantine"), 0o755); err != nil {
		t.Fatal(err)
	}

	e.start()
	e.svc.Recover(context.Background())
	if rec := e.record(gone); rec.State != StateDiscarded || !strings.Contains(*rec.LastError, "worktree is missing") {
		t.Fatalf("gone = %+v", rec)
	}
	if rec := e.record(kept); rec.State != StateOpen {
		t.Fatalf("kept = %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(e.dirs.Changes(), orphan)); err == nil {
		t.Fatal("orphan directory kept")
	}
	if out := run(t, repo, "branch", "--list", "skills-mcp/change/"+orphan); out != "" {
		t.Fatal("orphan branch kept")
	}
	if _, err := os.Stat(filepath.Join(e.dirs.Changes(), ".quarantine", bad+".json")); err != nil {
		t.Fatal("bad record not quarantined")
	}
	if _, ok := e.svc.Change(bad); ok {
		t.Fatal("bad record loaded")
	}
}

func TestExpiryAndSweep(t *testing.T) {
	e := newEnv(t)
	open := e.open()
	done := e.open()
	if err := e.svc.Discard(context.Background(), done); err != nil {
		t.Fatal(err)
	}
	e.clock.advance(25 * time.Hour)
	e.svc.Sweep(context.Background())
	if _, ok := e.svc.Change(done); ok {
		t.Fatal("expired terminal record kept")
	}
	if rec := e.record(open); rec.State != StateDiscarded || *rec.LastError != "change expired" {
		t.Fatalf("expired open change = %+v", rec)
	}
	if len(e.svc.BaseRevisions()["reg"]) != 0 {
		t.Fatal("discarded change still pins its base")
	}

	// Startup deletes expired terminal records and discards expired changes.
	stale := e.open()
	e.clock.advance(25 * time.Hour)
	e.start()
	e.svc.Recover(context.Background())
	if rec := e.record(stale); rec.State != StateDiscarded {
		t.Fatalf("stale = %+v", rec)
	}
	if _, ok := e.svc.Change(open); ok {
		t.Fatal("expired terminal record survived startup")
	}
}

func TestSweepRetriesCleanup(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	if err := e.svc.Discard(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Simulate a cleanup that failed: the branch is still there.
	run(t, e.src.Repo("reg").Dir, "branch", "skills-mcp/change/"+id, "refs/heads/main")
	e.svc.Sweep(context.Background())
	if out := run(t, e.src.Repo("reg").Dir, "branch", "--list", "skills-mcp/change/"+id); out != "" {
		t.Fatal("sweep did not retry cleanup")
	}
}

func TestReconcileAfterCrashes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()
	wt := e.worktree(id)
	editGolang(t, e, id, "body", "agent")

	// Crash between `git add` and the record write: a staged file outside the
	// recorded touched bundles, plus an unstaged leftover.
	write(t, filepath.Join(wt, "skills/user/commit/extra.md"), "extra\n")
	run(t, wt, "add", "--", "skills/user/commit/extra.md")
	write(t, filepath.Join(wt, "skills/agent/golang/stray.md"), "never staged\n")
	write(t, filepath.Join(wt, "skills/agent/golang/SKILL.md"), "half written")

	// Crash between `git commit` and the record write.
	run(t, wt, "commit", "-q", "-m", "orphan commit")

	if _, err := e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: "skills/agent/golang"}, 100000); err != nil {
		t.Fatal(err)
	}
	rec := e.record(id)
	if strings.Join(rec.TouchedBundles, ",") != "skills/agent/golang,skills/user/commit" {
		t.Fatalf("touched = %v", rec.TouchedBundles)
	}
	if head := run(t, wt, "rev-parse", "HEAD"); head != rec.BaseRevision {
		t.Fatal("orphan commit not reset")
	}
	if _, err := os.Stat(filepath.Join(wt, "skills/agent/golang/stray.md")); err == nil {
		t.Fatal("unstaged file not discarded")
	}
	data, _ := os.ReadFile(filepath.Join(wt, "skills/agent/golang/SKILL.md"))
	if !strings.Contains(string(data), "agent") {
		t.Fatalf("SKILL.md not restored from the index: %q", data)
	}
	res, err := e.submit(id, "recovered")
	if err != nil {
		t.Fatal(err)
	}
	if e.remote.show(res.Commit, "skills/user/commit/extra.md") != "extra" {
		t.Fatal("recovered staged file not pushed")
	}
}

func TestReconcileInterruptedRebase(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	wt := e.worktree(id)
	editGolang(t, e, id, "body", "agent body")
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.commit(context.Background(), c.rec, "agent change"); err != nil {
		t.Fatal(err)
	}
	// Upstream moves; a rebase onto it completes but the daemon dies before
	// persisting the new base and commit.
	write(t, filepath.Join(e.remote.work, "skills/user/commit/n.md"), "n\n")
	tip := e.remote.push("upstream")
	run(t, e.src.Repo("reg").Dir, "fetch", "-q", "origin", "+refs/heads/main:refs/heads/main")
	c.rec.RebaseOnto = strPtr(tip)
	if err := e.svc.store.persist(c.rec); err != nil {
		t.Fatal(err)
	}
	run(t, wt, "rebase", "-q", "--onto", tip, c.rec.BaseRevision)
	rebased := run(t, wt, "rev-parse", "HEAD")
	c.release()

	e.start()
	e.svc.Recover(context.Background())
	rec := e.record(id)
	if rec.BaseRevision != tip || rec.Commit == nil || *rec.Commit != rebased || rec.RebaseOnto != nil {
		t.Fatalf("record = %+v", rec)
	}
	res, err := e.submit(id, "ignored")
	if err != nil || res.Commit != rebased {
		t.Fatalf("submit = %+v, %v", res, err)
	}
}

func TestReconcileAbortsRebaseInProgress(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	wt := e.worktree(id)
	editGolang(t, e, id, "body", "agent body")
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.commit(context.Background(), c.rec, "agent change"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.remote.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "upstream\n"))
	tip := e.remote.push("conflict")
	run(t, e.src.Repo("reg").Dir, "fetch", "-q", "origin", "+refs/heads/main:refs/heads/main")
	c.rec.RebaseOnto = strPtr(tip)
	if err := e.svc.store.persist(c.rec); err != nil {
		t.Fatal(err)
	}
	commit := *c.rec.Commit
	cmd := []string{"rebase", "-q", "--onto", tip, c.rec.BaseRevision}
	if out, err := gitCmd(wt, cmd...); err == nil {
		t.Fatalf("rebase unexpectedly succeeded: %s", out)
	}
	c.release()

	e.start()
	e.svc.Recover(context.Background())
	rec := e.record(id)
	if rec.State != StateOpen || rec.RebaseOnto != nil {
		t.Fatalf("record = %+v", rec)
	}
	if head := run(t, wt, "rev-parse", "HEAD"); head != commit {
		t.Fatalf("HEAD = %s, want the pre-rebase commit %s", head, commit)
	}
}

func TestLockedInitializingWorktree(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	run(t, e.src.Repo("reg").Dir, "worktree", "lock", "--reason", "initializing", e.worktree(id))
	e.start()
	e.svc.Recover(context.Background())
	if rec := e.record(id); rec.State != StateDiscarded || !strings.Contains(*rec.LastError, "interrupted") {
		t.Fatalf("record = %+v", rec)
	}
}

func TestWorktreeAddFailure(t *testing.T) {
	e := newEnv(t)
	// An unwritable worktrees directory makes `git worktree add` fail
	// after the change record exists.
	worktrees := filepath.Join(e.src.Repo("reg").Dir, "worktrees")
	if err := os.MkdirAll(worktrees, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(worktrees, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(worktrees, 0o755)
	_, err := e.svc.Open(context.Background(), config.ClaudeCode, "", "s")
	mustErr(t, err, "creating worktree")
	for _, c := range e.svc.store.all() {
		if c.rec.State != StateDiscarded {
			t.Fatalf("failed open left %+v", c.rec)
		}
	}
}

func TestStoreHelpers(t *testing.T) {
	s := newStore(filepath.Join(t.TempDir(), "missing"), time.Now)
	if err := s.load(time.Hour, func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	if o, err := s.orphans(); err != nil || o != nil {
		t.Fatalf("orphans = %v, %v", o, err)
	}
	if deref(nil) != "" || !s.expired("garbage", time.Hour) {
		t.Fatal("helpers wrong")
	}
	if got := bundleOf(sourceConfig("x"), config.Pi, "harnesses/pi/skills/a/b.md"); got != "harnesses/pi/skills/a" {
		t.Fatalf("bundleOf = %q", got)
	}
	if bundleOf(sourceConfig("x"), config.Pi, "docs/x") != "" {
		t.Fatal("bundleOf outside roots")
	}
}
