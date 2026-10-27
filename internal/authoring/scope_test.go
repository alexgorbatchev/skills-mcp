package authoring

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

func TestMutationAfterCommitResetsIt(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "first")
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.commit(context.Background(), c.rec, "first"); err != nil {
		t.Fatal(err)
	}
	c.release()
	editGolang(t, e, id, "first", "second")
	rec := e.record(id)
	if rec.Commit != nil || rec.Message != nil {
		t.Fatalf("commit not cleared: %+v", rec)
	}
	res, err := e.submit(id, "combined")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.remote.show(res.Commit, "skills/agent/golang/SKILL.md"); got != skillDoc("golang", "Go rules", "second")[:len(got)] {
		t.Fatalf("pushed = %q", got)
	}
}

func TestScopeViolations(t *testing.T) {
	tests := []struct {
		name  string
		stage func(t *testing.T, wt string)
		want  string
	}{
		{"file outside bundles", func(t *testing.T, wt string) {
			write(t, filepath.Join(wt, "README.md"), "x")
			run(t, wt, "add", "README.md")
		}, "changes README.md outside the touched bundles"},
		{"deletion", func(t *testing.T, wt string) {
			run(t, wt, "rm", "-q", "skills/agent/golang/references/a.md")
		}, "deletes skills/agent/golang/references/a.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.open()
			editGolang(t, e, id, "body", "agent")
			tt.stage(t, e.worktree(id))
			_, err := e.submit(id, "m")
			mustErr(t, err, tt.want)
		})
	}
}

func TestShadowingAnotherSourceFails(t *testing.T) {
	e := newEnv(t)
	local := t.TempDir()
	write(t, filepath.Join(local, "skills/agent/local-only/SKILL.md"), skillDoc("local-only", "from path", "x"))
	e.cfg.Sources = append(e.cfg.Sources, &config.Source{
		Name: "local", Kind: config.KindPath, Path: local,
		Roots:    []config.Root{{Path: "skills/agent", Invocation: config.Model}},
		Template: config.Template{Vars: map[string]string{}},
	})
	e.start()
	for _, name := range []string{"reg", "local"} {
		if err := e.src.Refresh(context.Background(), name, source.RefreshOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	id := e.open()
	if _, err := e.svc.Create(context.Background(), CreateArgs{ChangeID: id, Root: "skills/agent", Name: "local-only", Description: "takes over"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.submit(id, "m")
	mustErr(t, err, `skill "local-only" from source local would no longer be served`)
}

func TestSourceWithoutValidator(t *testing.T) {
	e := newEnv(t, func(s *config.Source) { s.Validate = nil })
	id := e.open()
	if _, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "FAIL", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.submit(id, "m"); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTimeout(t *testing.T) {
	e := newEnv(t)
	e.svc.pipelines.Add(1)
	if e.svc.Wait(10 * time.Millisecond) {
		t.Fatal("Wait reported completion with a running pipeline")
	}
	e.svc.pipelines.Done()
	if !e.svc.Wait(time.Second) {
		t.Fatal("Wait timed out with nothing running")
	}
}

func TestCleanupWithoutRepository(t *testing.T) {
	e := newEnv(t)
	rec := &Record{ChangeID: "chg_0000000000000003", Source: "missing"}
	e.svc.cleanup(context.Background(), rec) // must not panic
	if err := os.RemoveAll(e.src.Repo("reg").Dir); err != nil {
		t.Fatal(err)
	}
	e.svc.cleanup(context.Background(), &Record{ChangeID: "chg_0000000000000004", Source: "reg"})
	e.svc.removeOrphans(context.Background())
}

func TestLockedIndexFailsSubmit(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	gitDir := run(t, e.worktree(id), "rev-parse", "--absolute-git-dir")
	write(t, filepath.Join(gitDir, "index.lock"), "")
	before := e.remote.tip()
	_, err := e.submit(id, "m")
	mustErr(t, err, "index.lock")
	if rec := e.record(id); rec.Commit != nil || rec.LastError == nil || e.remote.tip() != before {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSweepStopsWhenContextEnds(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	e.svc.Sweep(ctx) // returns instead of blocking on the held change
}

func TestRecoverSkipsTerminalChanges(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	if err := e.svc.Discard(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.svc.Recover(context.Background())
	if rec := e.record(id); rec.State != StateDiscarded {
		t.Fatalf("record = %+v", rec)
	}
}

func TestPushFindsCommitAlreadyUpstream(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.commit(context.Background(), c.rec, "agent change"); err != nil {
		t.Fatal(err)
	}
	commit := *c.rec.Commit
	c.release()
	// Someone publishes the commit and builds on it before the daemon pushes.
	run(t, e.src.Repo("reg").Dir, "push", "-q", "origin", commit+":refs/heads/main")
	run(t, e.remote.work, "pull", "-q", "origin", "main")
	write(t, filepath.Join(e.remote.work, "skills/user/commit/later.md"), "later\n")
	e.remote.push("on top")

	res, err := e.submit(id, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != commit || res.Publication.Attempts != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestReconcileAfterUpstreamAppliedRebase(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "same body")
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.commit(context.Background(), c.rec, "agent change"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.remote.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "same body\n"))
	tip := e.remote.push("identical")
	run(t, e.src.Repo("reg").Dir, "fetch", "-q", "origin", "+refs/heads/main:refs/heads/main")
	c.rec.RebaseOnto = strPtr(tip)
	if err := e.svc.store.persist(c.rec); err != nil {
		t.Fatal(err)
	}
	run(t, e.worktree(id), "rebase", "-q", "--onto", tip, c.rec.BaseRevision)
	c.release()
	e.start()
	e.svc.Recover(context.Background())
	rec := e.record(id)
	if rec.BaseRevision != tip || rec.Commit != nil || rec.RebaseOnto != nil {
		t.Fatalf("record = %+v", rec)
	}
}

func TestKilledDaemonFailsOperations(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	kill, cancel := context.WithCancel(context.Background())
	cancel()
	e.svc.opts.Kill = kill
	ctx := context.Background()
	_, err := e.svc.Open(ctx, config.ClaudeCode, "", "s")
	mustErr(t, err, "canceled")
	_, err = e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "a.md", Content: "x"})
	mustErr(t, err, "canceled")
	_, err = e.submit(id, "m")
	mustErr(t, err, "canceled")
	if rec := e.record(id); rec.LastError == nil {
		t.Fatal("pipeline failure not recorded")
	}
	mustErr(t, e.svc.Discard(ctx, id), "canceled")
}
