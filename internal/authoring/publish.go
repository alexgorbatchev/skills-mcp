package authoring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/proc"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

const (
	maxPushes             = 3
	postPushRefreshBudget = 180 * time.Second
)

// publish pushes the verified commit to the tracked branch. A
// non-fast-forward rejection rebases the change onto the new tip and
// re-verifies it before the next push; nothing is ever force-pushed.
func (s *Service) publish(ctx context.Context, c *change, progress func(string)) error {
	rec := c.rec
	repo := s.opts.Sources.Repo(rec.Source)
	for pushes := 1; ; pushes++ {
		if err := s.step(progress, fmt.Sprintf("pushing (attempt %d)", pushes)); err != nil {
			return err
		}
		commit := *rec.Commit
		rec.PushStarted = true
		if err := s.store.persist(rec); err != nil {
			return err
		}
		var out []byte
		var pushErr error
		// The lease makes the push a compare-and-swap: it lands only when the
		// remote branch is exactly the change's base, so nothing between the
		// remote tip and the base (for example commits someone removed with
		// a force push) is ever republished unscanned.
		lease := "--force-with-lease=" + repo.Ref() + ":" + rec.BaseRevision
		s.withSource(rec.Source, func() {
			out, pushErr = repo.Run(ctx, gitx.Opts{}, "push", "--porcelain", "--no-verify", "--no-follow-tags", lease, "origin", commit+":"+repo.Ref())
		})
		res := gitx.ParsePush(out, repo.Ref())
		switch {
		case res.Accepted():
			return s.finishPublish(ctx, c, commit, pushes)
		case !res.Parsed:
			return s.unconfirmedPush(ctx, c, commit, pushes, pushErr)
		case !res.NonFastForward():
			rec.PushStarted = false
			return fmt.Errorf("push rejected: %s: %w", res.Summary, pushErr)
		}
		rec.PushStarted = false
		if err := s.store.persist(rec); err != nil {
			return err
		}
		if pushes == maxPushes {
			return fmt.Errorf("push rejected %d times: %s", maxPushes, res.Summary)
		}
		if err := s.step(progress, "fetching the new tip"); err != nil {
			return err
		}
		if err := s.opts.Sources.Refresh(ctx, rec.Source, source.RefreshOptions{}); err != nil {
			return fmt.Errorf("refreshing after a rejected push: %w", err)
		}
		if s.onBranch(ctx, rec, commit) {
			return s.finishPublish(ctx, c, commit, pushes)
		}
		if err := s.step(progress, "rebasing onto the new tip"); err != nil {
			return err
		}
		if err := s.rebase(ctx, rec); err != nil {
			return err
		}
		if err := s.checkHead(ctx, rec); err != nil {
			return err
		}
		if err := s.verify(ctx, c, progress); err != nil {
			return err
		}
	}
}

// unconfirmedPush handles a push without a ref status line (timeout or
// transport failure): the remote may or may not have the commit.
func (s *Service) unconfirmedPush(ctx context.Context, c *change, commit string, pushes int, pushErr error) error {
	rec := c.rec
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postPushRefreshBudget)
	defer cancel()
	if err := s.opts.Sources.Refresh(checkCtx, rec.Source, source.RefreshOptions{}); err != nil {
		return fmt.Errorf("push outcome unknown (%v): %w", pushErr, errPublicationUnknown)
	}
	if s.onBranch(checkCtx, rec, commit) {
		return s.finishPublish(checkCtx, c, commit, pushes)
	}
	// The push may still land after this fetch; push_started stays set so
	// the next reconciliation fetches again before deciding.
	return fmt.Errorf("push failed: %w", pushErr)
}

// rebase replays only the change commit onto the new tip.
func (s *Service) rebase(ctx context.Context, rec *Record) error {
	repo := s.opts.Sources.Repo(rec.Source)
	tip, err := repo.Tip(ctx)
	if err != nil {
		return err
	}
	rec.RebaseOnto = strPtr(tip)
	if err := s.store.persist(rec); err != nil {
		return err
	}
	_, rebaseErr := s.git(ctx, rec, gitx.Opts{}, "rebase", "--quiet", "--onto", tip, rec.BaseRevision)
	if rebaseErr != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		conflicts, _ := s.git(cleanupCtx, rec, gitx.Opts{}, "diff", "--name-only", "-z", "--diff-filter=U")
		abortErr := s.abortRebase(cleanupCtx, rec)
		rec.RebaseOnto = nil
		var stderr string
		var exitErr *proc.ExitError
		if errors.As(rebaseErr, &exitErr) {
			stderr = proc.LastLine(exitErr.Stderr)
		}
		return errors.Join(fmt.Errorf("rebasing onto %s failed (conflicting paths: %s; %s); discard this change and open a new one",
			tip, strings.Join(gitx.SplitNUL(conflicts), ", "), stderr), abortErr)
	}
	head, err := s.gitOutput(ctx, rec, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	rec.BaseRevision, rec.RebaseOnto = tip, nil
	if head == tip {
		rec.Commit, rec.Message = nil, nil
		if err := s.store.persist(rec); err != nil {
			return err
		}
		return fmt.Errorf("change already applied upstream at %s; discard it", tip)
	}
	rec.Commit = strPtr(head)
	return s.store.persist(rec)
}

// finishPublish records the publication, cleans up, and refreshes the
// source so the change is live when submit returns.
func (s *Service) finishPublish(ctx context.Context, c *change, commit string, pushes int) error {
	c.rec.PushStarted = false
	s.markPublished(ctx, c, commit, pushes, nil)
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postPushRefreshBudget)
	defer cancel()
	if err := s.opts.Sources.Refresh(refreshCtx, c.rec.Source, source.RefreshOptions{}); err != nil {
		c.rec.Publication.RefreshError = strPtr(err.Error())
		return s.store.persist(c.rec)
	}
	return nil
}
