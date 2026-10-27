package authoring

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

type reconcileMode int

const (
	reconcileNormal  reconcileMode = iota
	reconcileStartup               // Refresh runs with view rebuilds suppressed
)

// errPublicationUnknown is returned when a push may have landed but the
// remote cannot be checked.
var errPublicationUnknown = errors.New("publication state unknown; retry when the remote is reachable")

// reconcile repairs an open change's worktree and record after crashes or
// interrupted steps. It may leave the change submitted or discarded.
func (s *Service) reconcile(ctx context.Context, c *change, mode reconcileMode) error {
	rec := c.rec
	if ok, err := s.checkWorktree(ctx, c); err != nil || !ok {
		return err
	}
	if err := s.abortRebase(ctx, rec); err != nil {
		return err
	}
	head, _ := s.gitOutput(ctx, rec, "rev-parse", "HEAD")
	parent, _ := s.gitOutput(ctx, rec, "rev-parse", "--verify", "--quiet", "HEAD^")
	if rec.RebaseOnto != nil {
		onto := *rec.RebaseOnto
		switch onto {
		case parent:
			rec.BaseRevision, rec.Commit = onto, strPtr(head)
		case head:
			rec.BaseRevision, rec.Commit, rec.Message = onto, nil, nil
		}
		rec.RebaseOnto = nil
		if err := s.store.persist(rec); err != nil {
			return err
		}
	}
	if published, err := s.detectPublished(ctx, c, mode); err != nil || published {
		return err
	}
	switch {
	case rec.Commit != nil && (head != *rec.Commit || parent != rec.BaseRevision):
		if err := s.resetToBase(ctx, rec); err != nil {
			return err
		}
	case rec.Commit == nil && head != rec.BaseRevision:
		if _, err := s.git(ctx, rec, gitx.Opts{}, "reset", "--quiet", "--soft", rec.BaseRevision); err != nil {
			return fmt.Errorf("resetting to base: %w", err)
		}
	}
	if err := s.recoverTouched(ctx, rec); err != nil {
		return err
	}
	if err := s.discardUnstaged(ctx, rec); err != nil {
		return err
	}
	return s.store.persist(rec)
}

// reconcileForDiscard runs only the missing-worktree and published checks.
func (s *Service) reconcileForDiscard(ctx context.Context, c *change, mode reconcileMode) error {
	if ok, err := s.checkWorktree(ctx, c); err != nil || !ok {
		return err
	}
	_, err := s.detectPublished(ctx, c, mode)
	return err
}

// checkWorktree discards the change when its worktree is missing, not
// registered, or locked mid-creation. It reports whether the change is
// still usable.
func (s *Service) checkWorktree(ctx context.Context, c *change) (bool, error) {
	rec := c.rec
	wt := s.store.worktreePath(rec.ChangeID)
	reason := ""
	if _, err := os.Stat(wt); err != nil {
		reason = "worktree is missing"
	} else if state, err := s.worktreeState(ctx, rec.Source, wt); err != nil {
		return false, err
	} else if state != "" {
		reason = state
	}
	if reason == "" {
		return true, nil
	}
	rec.State = StateDiscarded
	rec.LastError = strPtr("worktree " + wt + ": " + reason)
	rec.UpdatedAt = s.store.timestamp()
	if err := s.store.persist(rec); err != nil {
		return false, err
	}
	s.cleanup(ctx, rec)
	return false, nil
}

// worktreeState returns "" for a registered, usable worktree, or why not.
func (s *Service) worktreeState(ctx context.Context, sourceName, wt string) (string, error) {
	repo := s.opts.Sources.Repo(sourceName)
	var out []byte
	var err error
	s.withSource(sourceName, func() {
		out, err = repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "worktree", "list", "--porcelain", "-z")
	})
	if err != nil {
		return "", fmt.Errorf("listing worktrees: %w", err)
	}
	want, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return "worktree is missing", nil
	}
	var current, found bool
	for _, line := range gitx.SplitNUL(out) {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			resolved, _ := filepath.EvalSymlinks(p)
			current = resolved == want
			found = found || current
			continue
		}
		if current && strings.HasPrefix(line, "locked") && strings.Contains(line, "initializing") {
			return "worktree creation was interrupted", nil
		}
	}
	if found {
		return "", nil
	}
	return "worktree is not registered", nil
}

func (s *Service) abortRebase(ctx context.Context, rec *Record) error {
	gitDir, err := s.gitOutput(ctx, rec, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("locating git dir: %w", err)
	}
	for _, d := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, d)); err == nil {
			if _, err := s.git(ctx, rec, gitx.Opts{}, "rebase", "--abort"); err != nil {
				return fmt.Errorf("aborting rebase: %w", err)
			}
			return nil
		}
	}
	return nil
}

// detectPublished marks the change submitted when its commit is already on
// the tracked branch. After an unconfirmed push it fetches first.
func (s *Service) detectPublished(ctx context.Context, c *change, mode reconcileMode) (bool, error) {
	rec := c.rec
	if rec.PushStarted {
		if err := s.opts.Sources.Refresh(ctx, rec.Source, source.RefreshOptions{SuppressRebuild: mode == reconcileStartup}); err != nil {
			return false, fmt.Errorf("%w: %v", errPublicationUnknown, err)
		}
		rec.PushStarted = false
		if err := s.store.persist(rec); err != nil {
			return false, err
		}
	}
	if rec.Commit == nil || !s.onBranch(ctx, rec, *rec.Commit) {
		return false, nil
	}
	s.markPublished(ctx, c, *rec.Commit, 0, nil)
	return true, nil
}

// onBranch reports whether commit is reachable from the tracked branch.
func (s *Service) onBranch(ctx context.Context, rec *Record, commit string) bool {
	repo := s.opts.Sources.Repo(rec.Source)
	var err error
	s.withSource(rec.Source, func() {
		_, err = repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "merge-base", "--is-ancestor", commit, repo.Ref())
	})
	return err == nil
}

func (s *Service) markPublished(ctx context.Context, c *change, commit string, attempts int, refreshErr *string) {
	rec := c.rec
	repo := s.opts.Sources.Repo(rec.Source)
	rec.Commit = strPtr(commit)
	rec.Publication = &Publication{
		Remote: "origin", Ref: repo.Ref(), Commit: commit, Attempts: attempts,
		PushedAt: s.store.timestamp(), RefreshError: refreshErr,
	}
	rec.State = StateSubmitted
	rec.LastError = nil
	rec.UpdatedAt = s.store.timestamp()
	if err := s.store.persist(rec); err != nil {
		s.log.Warn("persisting published change failed", "change_id", rec.ChangeID, "error", err)
	}
	s.cleanup(ctx, rec)
}

func (s *Service) resetToBase(ctx context.Context, rec *Record) error {
	if _, err := s.git(ctx, rec, gitx.Opts{}, "reset", "--quiet", "--soft", rec.BaseRevision); err != nil {
		return fmt.Errorf("resetting to base: %w", err)
	}
	rec.Commit, rec.Message = nil, nil
	return nil
}

// recoverTouched adds the bundles of every staged path to touched_bundles.
func (s *Service) recoverTouched(ctx context.Context, rec *Record) error {
	out, err := s.git(ctx, rec, gitx.Opts{}, "diff", "--cached", "--name-only", "-z", rec.BaseRevision)
	if err != nil {
		return fmt.Errorf("listing staged paths: %w", err)
	}
	src := s.opts.Config.SourceByName(rec.Source)
	for _, p := range gitx.SplitNUL(out) {
		if b := bundleOf(src, config.HarnessID(rec.Harness), p); b != "" {
			touchBundle(rec, b)
		}
	}
	return nil
}

// discardUnstaged restores touched bundles to the index, which records
// every applied mutation.
func (s *Service) discardUnstaged(ctx context.Context, rec *Record) error {
	var tracked []string
	for _, b := range rec.TouchedBundles {
		out, err := s.git(ctx, rec, gitx.Opts{}, "ls-files", "-z", "--", b)
		if err != nil {
			return fmt.Errorf("listing %s: %w", b, err)
		}
		if len(out) > 0 {
			tracked = append(tracked, b)
		}
	}
	if len(tracked) > 0 {
		if _, err := s.git(ctx, rec, gitx.Opts{}, append([]string{"restore", "--worktree", "--"}, tracked...)...); err != nil {
			return fmt.Errorf("restoring touched bundles: %w", err)
		}
	}
	if len(rec.TouchedBundles) > 0 {
		if _, err := s.git(ctx, rec, gitx.Opts{}, append([]string{"clean", "-fdxq", "--"}, rec.TouchedBundles...)...); err != nil {
			return fmt.Errorf("cleaning touched bundles: %w", err)
		}
	}
	return nil
}

// bundleOf returns the bundle directory containing a repo-relative path, or
// "" when the path is outside every root.
func bundleOf(src *config.Source, h config.HarnessID, p string) string {
	for _, r := range src.ExpandedRoots(h) {
		rest, ok := strings.CutPrefix(p, r.Path+"/")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, "/")
		return path.Join(r.Path, name)
	}
	return ""
}
