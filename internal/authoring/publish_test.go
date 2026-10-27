package authoring

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func editGolang(t *testing.T, e *env, id, old, new string) {
	t.Helper()
	if _, err := e.svc.Edit(context.Background(), EditArgs{ChangeID: id, Bundle: "skills/agent/golang", OldString: old, NewString: new}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedPushRebasesAndRetries(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "body from agent")
	// Someone else pushes an unrelated change first.
	write(t, filepath.Join(e.remote.work, "skills/user/commit/notes.md"), "upstream\n")
	upstream := e.remote.push("upstream")

	res, err := e.submit(id, "agent change")
	if err != nil {
		t.Fatal(err)
	}
	if res.Publication.Attempts != 2 {
		t.Fatalf("attempts = %d", res.Publication.Attempts)
	}
	parent := run(t, e.remote.bare, "rev-parse", res.Commit+"^")
	if parent != upstream {
		t.Fatalf("not rebased onto upstream: parent %s, want %s", parent, upstream)
	}
	if e.remote.show(res.Commit, "skills/user/commit/notes.md") != "upstream" {
		t.Fatal("upstream change lost")
	}
}

func TestRebaseConflict(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent body")
	write(t, filepath.Join(e.remote.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "upstream body\n"))
	upstream := e.remote.push("conflicting")

	_, err := e.submit(id, "agent change")
	mustErr(t, err, "conflicting paths: skills/agent/golang/SKILL.md")
	if e.remote.tip() != upstream {
		t.Fatal("conflicting change pushed")
	}
	rec := e.record(id)
	if rec.State != StateOpen || rec.RebaseOnto != nil {
		t.Fatalf("record = %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(e.worktree(id), ".git")); err != nil {
		t.Fatal("worktree damaged")
	}
	if err := e.svc.Discard(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if e.record(id).State != StateDiscarded {
		t.Fatal("discard failed")
	}
}

func TestAlreadyAppliedUpstream(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "same body")
	write(t, filepath.Join(e.remote.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "same body\n"))
	e.remote.push("same change upstream")
	_, err := e.submit(id, "agent change")
	mustErr(t, err, "already applied upstream")
	rec := e.record(id)
	if rec.Commit != nil || rec.BaseRevision != e.remote.tip() {
		t.Fatalf("record = %+v", rec)
	}
}

func TestRemoteRejectsPush(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	hook := filepath.Join(e.remote.bare, "hooks", "pre-receive")
	script := "#!/bin/sh\necho 'rejected by hook' >&2\nexit 1\n"
	write(t, hook, script)
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := e.submit(id, "agent change")
	mustErr(t, err, "push rejected")
	rec := e.record(id)
	if rec.PushStarted || rec.State != StateOpen {
		t.Fatalf("record = %+v", rec)
	}
}

func TestUnreachableRemoteKeepsPushStarted(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	if err := os.Rename(e.remote.bare, e.remote.bare+".gone"); err != nil {
		t.Fatal(err)
	}
	_, err := e.submit(id, "agent change")
	mustErr(t, err, "publication state unknown")
	rec := e.record(id)
	if !rec.PushStarted || rec.Commit == nil {
		t.Fatalf("record = %+v", rec)
	}
	// While the remote is unreachable, mutations cannot proceed safely.
	_, err = e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "x.md", Content: "x"})
	mustErr(t, err, "publication state unknown")
	if err := os.Rename(e.remote.bare+".gone", e.remote.bare); err != nil {
		t.Fatal(err)
	}
	res, err := e.submit(id, "agent change")
	if err != nil {
		t.Fatal(err)
	}
	if res.Publication == nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestCrashAfterPushDetectedAsPublished(t *testing.T) {
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
	// Push behind the daemon's back, then mark the push as started, as a
	// crash between push and record write would leave it.
	run(t, e.src.Repo("reg").Dir, "push", "-q", "origin", commit+":refs/heads/main")
	c.rec.PushStarted = true
	if err := e.svc.store.persist(c.rec); err != nil {
		t.Fatal(err)
	}
	c.release()

	e.start()
	e.svc.Recover(context.Background())
	rec := e.record(id)
	if rec.State != StateSubmitted || rec.Publication.Attempts != 0 || rec.Publication.Commit != commit {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSubmitContinuesAfterRequestCancel(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, err := e.svc.Submit(ctx, id, "agent change", nil)
	mustErr(t, err, "continues in the background")
	if !e.svc.Wait(time.Minute) {
		t.Fatal("pipeline did not finish")
	}
	res, err := e.submit(id, "ignored")
	if err != nil || res.Publication == nil {
		t.Fatalf("outcome = %+v, %v", res, err)
	}
}

func TestShutdownInterruptsPipeline(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.svc.opts.Shutdown = ctx
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	_, err := e.submit(id, "agent change")
	mustErr(t, err, "interrupted by shutdown")
	if !strings.Contains(*e.record(id).LastError, "interrupted by shutdown") {
		t.Fatal("interruption not recorded")
	}
}

func TestPostPushRefreshFailureRecorded(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	editGolang(t, e, id, "body", "agent")
	// The remote disappears right after accepting the push, so the
	// post-push fetch fails.
	hook := filepath.Join(e.remote.bare, "hooks", "post-receive")
	write(t, hook, "#!/bin/sh\nmv \"$PWD\" \"$PWD.moved\"\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := e.submit(id, "agent change")
	if err != nil {
		t.Fatal(err)
	}
	if res.Publication == nil || res.Publication.RefreshError == nil {
		t.Fatalf("result = %+v", res)
	}
	if e.record(id).State != StateSubmitted {
		t.Fatal("published change not submitted")
	}
}
