package authoring

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func TestAcquireHonorsContext(t *testing.T) {
	c := newChange(&Record{ChangeID: "chg_1"})
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	mustErr(t, c.acquire(ctx), "waiting for change chg_1")
	c.release()
}

func TestHeartbeatRepeatsStep(t *testing.T) {
	old := progressInterval
	progressInterval = 10 * time.Millisecond
	defer func() { progressInterval = old }()
	var mu sync.Mutex
	var got []string
	hb := newHeartbeat(func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	hb.report("pushing")
	ctx, cancel := context.WithCancel(context.Background())
	go hb.run(ctx)
	time.Sleep(60 * time.Millisecond)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(got) < 3 || got[len(got)-1] != "pushing" {
		t.Fatalf("heartbeats = %v", got)
	}
	newHeartbeat(nil).report("no sink")
}

func TestWrongStateAndUnknownChange(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()
	if err := e.svc.Discard(ctx, id); err != nil {
		t.Fatal(err)
	}
	mustErr(t, e.svc.Discard(ctx, id), "is discarded")
	_, err := e.submit(id, "m")
	mustErr(t, err, "is discarded")
	_, err = e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: "skills/agent/golang"}, 1000)
	mustErr(t, err, "not open")
	_, err = e.svc.Diff(ctx, DiffArgs{ChangeID: "chg_0000000000000002"}, 1000)
	mustErr(t, err, "unknown change")
	_, err = e.svc.WriteFile(ctx, WriteArgs{ChangeID: "chg_0000000000000002"})
	mustErr(t, err, "unknown change")
	_, err = e.svc.Create(ctx, CreateArgs{ChangeID: "chg_0000000000000002"})
	mustErr(t, err, "unknown change")
	mustErr(t, e.svc.Discard(ctx, "chg_0000000000000002"), "unknown change")
}

func TestWriteOverDirectoryAndReadErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()
	_, err := e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "references", Content: "x"})
	mustErr(t, err, "is not a regular file")
	_, err = e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "references/a.md/nested.md", Content: "x"})
	mustErr(t, err, "creating skills/agent/golang/references/a.md")
	_, err = e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "missing.md"}, 1000)
	mustErr(t, err, "reading missing.md")
	_, err = e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "../x"}, 1000)
	mustErr(t, err, "must not contain")
	_, err = e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: "elsewhere/x"}, 1000)
	mustErr(t, err, "is not <root>/<name>")
	_, err = e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: "skills/agent/../agent/golang", Path: "a", Content: "x"})
	mustErr(t, err, "is not <root>/<name>")
}

func TestStageFailureUndoesWrite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()
	gitDir := run(t, e.worktree(id), "rev-parse", "--absolute-git-dir")
	// A stale index lock makes `git add` fail after the file is written.
	lock := filepath.Join(gitDir, "index.lock")
	write(t, lock, "")
	_, err := e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "new.md", Content: "x"})
	if err == nil {
		t.Fatal("write succeeded with a locked index")
	}
	if _, statErr := os.Stat(filepath.Join(e.worktree(id), "skills/agent/golang/new.md")); statErr == nil {
		t.Fatal("failed write left the file behind")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "new.md", Content: "x"}); err != nil {
		t.Fatal(err)
	}
}

func TestUnexpectedWorktreeChangesBlockSubmit(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	write(t, filepath.Join(e.worktree(id), "stray-at-root.txt"), "x")
	_, err := e.submit(id, "m")
	mustErr(t, err, "unexpected changes: ?? stray-at-root.txt")
	if strings.Contains(e.remote.show(e.remote.tip(), "skills/agent/golang/SKILL.md"), "agent") {
		t.Fatal("pushed despite a dirty worktree")
	}
}

func TestRecoverReportsUnreachableRemote(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	c, _ := e.svc.store.get(id)
	c.rec.PushStarted = true
	if err := e.svc.store.persist(c.rec); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.remote.bare, e.remote.bare+".gone"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(e.remote.bare+".gone", e.remote.bare)
	e.clock.advance(25 * time.Hour) // expired, but publication cannot be ruled out
	e.start()
	e.svc.Recover(context.Background())
	e.svc.Sweep(context.Background())
	if rec := e.record(id); rec.State != StateOpen || !rec.PushStarted {
		t.Fatalf("record = %+v", rec)
	}
}

func TestAcquireWaitsForRelease(t *testing.T) {
	c := newChange(&Record{ChangeID: "chg_2"})
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.release()
	}()
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.release()
}

func commitOutsidePipeline(t *testing.T, e *env, id string) *change {
	t.Helper()
	c, _ := e.svc.store.get(id)
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.release()
	if err := e.svc.commit(context.Background(), c.rec, "agent change"); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAmendedCommitIsReset(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	c := commitOutsidePipeline(t, e, id)
	run(t, e.worktree(id), "commit", "-q", "--amend", "-m", "amended behind the daemon")
	res, err := e.submit(id, "fresh message")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message != "fresh message" || res.Commit == "" || c.rec.State != StateSubmitted {
		t.Fatalf("result = %+v", res)
	}
}

func TestSubmitFindsPublishedChange(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	c := commitOutsidePipeline(t, e, id)
	commit := *c.rec.Commit
	run(t, e.src.Repo("reg").Dir, "push", "-q", "origin", commit+":refs/heads/main")
	run(t, e.src.Repo("reg").Dir, "fetch", "-q", "origin", "+refs/heads/main:refs/heads/main")
	res, err := e.submit(id, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	if res.Publication.Attempts != 0 || res.Commit != commit {
		t.Fatalf("result = %+v", res)
	}
}

func TestSubmitWithMissingWorktree(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	if err := os.RemoveAll(e.worktree(id)); err != nil {
		t.Fatal(err)
	}
	_, err := e.submit(id, "m")
	mustErr(t, err, "worktree is missing")
	if e.record(id).State != StateDiscarded {
		t.Fatal("change with a missing worktree not discarded")
	}
}

func TestPushFailureWithReachableRemote(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	repo := e.src.Repo("reg").Dir
	run(t, repo, "config", "remote.origin.pushurl", "/nonexistent/push.git")
	_, err := e.submit(id, "m")
	mustErr(t, err, "push failed")
	rec := e.record(id)
	if rec.PushStarted || rec.State != StateOpen {
		t.Fatalf("record = %+v", rec)
	}
	run(t, repo, "config", "--unset", "remote.origin.pushurl")
	if _, err := e.submit(id, "m"); err != nil {
		t.Fatal(err)
	}
}

func TestUnregisteredWorktreeDiscarded(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	gitDir := run(t, e.worktree(id), "rev-parse", "--absolute-git-dir")
	if err := os.RemoveAll(gitDir); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "a.md", Content: "x"})
	mustErr(t, err, "not open")
	if rec := e.record(id); rec.State != StateDiscarded || !strings.Contains(*rec.LastError, "not registered") {
		t.Fatalf("record = %+v", rec)
	}
}

func TestFailedStageRestoresExistingFile(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	gitDir := run(t, e.worktree(id), "rev-parse", "--absolute-git-dir")
	lock := filepath.Join(gitDir, "index.lock")
	write(t, lock, "")
	_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "references/a.md", Content: "replaced"})
	if err == nil {
		t.Fatal("write succeeded with a locked index")
	}
	data, readErr := os.ReadFile(filepath.Join(e.worktree(id), "skills/agent/golang/references/a.md"))
	if readErr != nil || string(data) != "ref\n" {
		t.Fatalf("existing file not restored: %q, %v", data, readErr)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
}

func TestRemovedSourceDoesNotCrashRecovery(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	sources := e.cfg.Sources
	e.cfg.Sources = nil // the source was removed from config.yaml
	e.start()
	e.svc.Recover(context.Background()) // must not panic
	_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "a", Content: "x"})
	mustErr(t, err, "no longer configured")
	_, err = e.submit(id, "m")
	mustErr(t, err, "no longer configured")
	e.clock.advance(25 * time.Hour)
	e.svc.Sweep(context.Background())
	if rec := e.record(id); rec.State != StateDiscarded || !strings.Contains(*rec.LastError, "no longer configured") {
		t.Fatalf("record = %+v", rec)
	}
	e.cfg.Sources = sources
	e.start()
	stale := e.open()
	e.clock.advance(25 * time.Hour)
	e.cfg.Sources = []*config.Source{{Name: "reg", Kind: config.KindPath}}
	e.start()
	e.svc.Recover(context.Background())
	if rec := e.record(stale); rec.State != StateDiscarded || !strings.Contains(*rec.LastError, "not a git source") {
		t.Fatalf("stale = %+v", rec)
	}
}

func TestReadOnlySourceRefusesOpenChanges(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "secret.example")
	src := e.cfg.Sources[0]
	src.Writable = false
	before := e.remote.tip()
	_, err := e.submit(id, "m")
	mustErr(t, err, "is not writable")
	src.Writable = true
	patterns := src.DenyPatterns
	src.DenyPatterns = nil
	_, err = e.submit(id, "m")
	mustErr(t, err, "has no deny_patterns")
	if e.remote.tip() != before {
		t.Fatal("pushed from an unusable source")
	}
	src.DenyPatterns = patterns
	if err := e.svc.checkDeny(context.Background(), &Record{}, &config.Source{Name: "x"}); err == nil {
		t.Fatal("deny scan without patterns passed")
	}
}
