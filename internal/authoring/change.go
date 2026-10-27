package authoring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/paths"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

// authoringTimeout bounds every non-submit authoring tool call.
const authoringTimeout = 120 * time.Second

// cleanupTimeout bounds cleanup after a failed or expired step.
const cleanupTimeout = 60 * time.Second

// Errors returned to callers.
var (
	ErrUnknownChange = errors.New("unknown change")
	ErrNotOpen       = errors.New("change is not open")
)

// Options configures a Service.
type Options struct {
	Config  *config.Config
	Dirs    paths.Dirs
	Git     *gitx.Git
	Sources *source.Manager
	Logger  *slog.Logger
	Now     func() time.Time
	// Shutdown is cancelled when the daemon begins to stop: pipelines start
	// no new commands. Kill is cancelled when running commands must die.
	Shutdown context.Context
	Kill     context.Context
}

// Service implements the authoring tools.
type Service struct {
	opts      Options
	store     *store
	log       *slog.Logger
	pipelines sync.WaitGroup
}

// NewService creates the service; call Load before use.
func NewService(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Shutdown == nil {
		opts.Shutdown = context.Background()
	}
	if opts.Kill == nil {
		opts.Kill = context.Background()
	}
	return &Service{opts: opts, store: newStore(opts.Dirs.Changes(), opts.Now), log: opts.Logger}
}

// Wait blocks until running submit pipelines finish or the timeout ends,
// and reports whether they all finished.
func (s *Service) Wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.pipelines.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// BaseRevisions returns the base revisions of open changes per source.
func (s *Service) BaseRevisions() map[string]map[string]bool { return s.store.baseRevisions() }

// Load reads persisted changes (startup step 5).
func (s *Service) Load() error {
	return s.store.load(s.opts.Config.ChangeTTL, s.log.Warn)
}

// Recover reconciles every open change and discards expired ones
// (startup step 9), then removes orphaned directories and branches (step 10).
func (s *Service) Recover(ctx context.Context) {
	for _, c := range s.store.all() {
		if c.rec.State != StateOpen {
			continue
		}
		if err := s.checkSource(c.rec.Source); err != nil {
			s.log.Warn("change refers to an unusable source", "change_id", c.rec.ChangeID, "error", err)
			if s.store.expired(c.rec.UpdatedAt, s.opts.Config.ChangeTTL) {
				s.retire(c, "change expired; "+err.Error())
			}
			continue
		}
		if err := s.reconcile(ctx, c, reconcileStartup); err != nil {
			s.log.Warn("reconciling change failed", "change_id", c.rec.ChangeID, "error", err)
		}
		if c.rec.State == StateOpen && s.store.expired(c.rec.UpdatedAt, s.opts.Config.ChangeTTL) {
			if err := s.discard(ctx, c, "change expired", reconcileStartup); err != nil {
				s.log.Warn("discarding expired change failed", "change_id", c.rec.ChangeID, "error", err)
			}
		}
	}
	s.removeOrphans(ctx)
}

func (s *Service) removeOrphans(ctx context.Context) {
	orphans, err := s.store.orphans()
	if err != nil {
		s.log.Warn("listing orphaned changes failed", "error", err)
	}
	for _, id := range orphans {
		if err := os.RemoveAll(s.store.changeDir(id)); err != nil {
			s.log.Warn("removing orphaned change failed", "change_id", id, "error", err)
		}
	}
	for _, name := range s.opts.Sources.Sources() {
		repo := s.opts.Sources.Repo(name)
		if repo == nil || !repo.Exists() {
			continue
		}
		s.withSource(name, func() {
			if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "worktree", "prune"); err != nil {
				s.log.Warn("pruning worktrees failed", "source", name, "error", err)
			}
			out, err := repo.Git.Output(ctx, repo.Dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/skills-mcp/change/")
			if err != nil {
				s.log.Warn("listing change branches failed", "source", name, "error", err)
				return
			}
			for _, branch := range strings.Fields(out) {
				id := strings.TrimPrefix(branch, "skills-mcp/change/")
				if _, ok := s.store.get(id); ok {
					continue
				}
				if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "branch", "-D", branch); err != nil {
					s.log.Warn("deleting orphaned branch failed", "branch", branch, "error", err)
				}
			}
		})
	}
}

// Sweep discards expired open changes, deletes expired terminal records,
// and retries pending cleanup. It runs hourly.
func (s *Service) Sweep(ctx context.Context) {
	for _, c := range s.store.all() {
		if err := c.acquire(ctx); err != nil {
			return
		}
		expired := s.store.expired(c.rec.UpdatedAt, s.opts.Config.ChangeTTL)
		srcErr := s.checkSource(c.rec.Source)
		switch {
		case c.rec.State == StateOpen && expired && srcErr != nil:
			s.retire(c, "change expired; "+srcErr.Error())
		case c.rec.State == StateOpen && expired:
			if err := s.discard(ctx, c, "change expired", reconcileNormal); err != nil {
				s.log.Warn("discarding expired change failed", "change_id", c.rec.ChangeID, "error", err)
			}
		case c.rec.State != StateOpen && expired:
			s.cleanup(ctx, c.rec)
			if err := s.store.remove(c.rec.ChangeID); err != nil {
				s.log.Warn("deleting change record failed", "change_id", c.rec.ChangeID, "error", err)
			}
		case c.rec.State != StateOpen:
			s.cleanup(ctx, c.rec)
		}
		c.release()
	}
}

// OpenResult is skill_change_open's response.
type OpenResult struct {
	ChangeID     string `json:"change_id"`
	Source       string `json:"source"`
	BaseRevision string `json:"base_revision"`
}

// Open creates a change on a writable git source's current tip.
func (s *Service) Open(_ context.Context, harness config.HarnessID, sourceName, summary string) (OpenResult, error) {
	ctx, cancel := s.detached()
	defer cancel()
	if sourceName == "" {
		sourceName = s.opts.Config.DefaultWriteSource
	}
	src := s.opts.Config.SourceByName(sourceName)
	switch {
	case src == nil:
		return OpenResult{}, fmt.Errorf("unknown source %q", sourceName)
	case src.Kind != config.KindGit:
		return OpenResult{}, fmt.Errorf("source %q is a path source; only git sources accept changes", sourceName)
	case !src.Writable:
		return OpenResult{}, fmt.Errorf("source %q is not writable", sourceName)
	case len(src.DenyPatterns) == 0:
		return OpenResult{}, fmt.Errorf("source %q has no deny_patterns", sourceName)
	}
	repo := s.opts.Sources.Repo(sourceName)
	base := s.opts.Sources.Current(sourceName)
	if base == "" || !repo.Exists() {
		return OpenResult{}, fmt.Errorf("source %s has no snapshot", sourceName)
	}
	id := newChangeID()
	now := s.store.timestamp()
	rec := &Record{
		ChangeID: id, Source: sourceName, Harness: string(harness), BaseRevision: base,
		Branch: "skills-mcp/change/" + id, Summary: summary, State: StateOpen,
		CreatedAt: now, UpdatedAt: now, TouchedBundles: []string{},
	}
	if err := s.store.persist(rec); err != nil {
		return OpenResult{}, err
	}
	c := newChange(rec)
	s.store.add(c)
	var addErr error
	s.withSource(sourceName, func() {
		_, addErr = repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "worktree", "add", "--quiet", "-b", rec.Branch, s.store.worktreePath(id), base)
	})
	if addErr != nil {
		rec.State = StateDiscarded
		rec.LastError = strPtr("creating worktree: " + addErr.Error())
		rec.UpdatedAt = s.store.timestamp()
		if err := s.store.persist(rec); err != nil {
			s.log.Warn("persisting failed change failed", "change_id", id, "error", err)
		}
		s.cleanup(ctx, rec)
		return OpenResult{}, fmt.Errorf("creating worktree: %w", addErr)
	}
	return OpenResult{ChangeID: id, Source: sourceName, BaseRevision: base}, nil
}

// Discard discards an open change.
func (s *Service) Discard(reqCtx context.Context, id string) error {
	c, err := s.lockOpen(reqCtx, id)
	if err != nil {
		return err
	}
	defer c.release()
	ctx, cancel := s.detached()
	defer cancel()
	return s.discard(ctx, c, "", reconcileNormal)
}

// discard runs the reduced reconciliation (missing worktree, published
// detection) and then discards and cleans up even if other repairs fail.
func (s *Service) discard(ctx context.Context, c *change, reason string, mode reconcileMode) error {
	if err := s.reconcileForDiscard(ctx, c, mode); err != nil {
		return err
	}
	if c.rec.State != StateOpen {
		return nil
	}
	c.rec.State = StateDiscarded
	if reason != "" {
		c.rec.LastError = strPtr(reason)
	}
	c.rec.UpdatedAt = s.store.timestamp()
	if err := s.store.persist(c.rec); err != nil {
		return err
	}
	s.cleanup(ctx, c.rec)
	return nil
}

// cleanup removes a terminal change's worktree and branch, tolerating
// targets that are already gone.
func (s *Service) cleanup(ctx context.Context, rec *Record) {
	repo := s.opts.Sources.Repo(rec.Source)
	if repo == nil || !repo.Exists() {
		return
	}
	wt := s.store.worktreePath(rec.ChangeID)
	s.withSource(rec.Source, func() {
		if _, err := os.Stat(wt); err == nil {
			if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "worktree", "remove", "-f", "-f", wt); err != nil {
				s.log.Warn("removing worktree failed", "change_id", rec.ChangeID, "error", err)
			}
		}
		if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "worktree", "prune"); err != nil {
			s.log.Warn("pruning worktrees failed", "change_id", rec.ChangeID, "error", err)
		}
		if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "show-ref", "--verify", "--quiet", "refs/heads/"+rec.Branch); err == nil {
			if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "branch", "-D", rec.Branch); err != nil {
				s.log.Warn("deleting change branch failed", "change_id", rec.ChangeID, "error", err)
			}
		}
	})
}

// checkSource reports whether a change's source still accepts changes: it
// exists, is a writable git source with deny patterns, and has a clone.
// Operations on changes of other sources are refused without touching Git.
func (s *Service) checkSource(name string) error {
	src := s.opts.Config.SourceByName(name)
	switch {
	case src == nil:
		return fmt.Errorf("source %q is no longer configured", name)
	case src.Kind != config.KindGit:
		return fmt.Errorf("source %q is not a git source", name)
	case !src.Writable:
		return fmt.Errorf("source %q is not writable", name)
	case len(src.DenyPatterns) == 0:
		return fmt.Errorf("source %q has no deny_patterns", name)
	case !s.opts.Sources.Repo(name).Exists():
		return fmt.Errorf("source %q has no clone yet", name)
	}
	return nil
}

// retire discards a change without Git, for a source that is unusable.
// Its directory is deleted with the record once it expires; startup prunes
// the worktree registration if the source returns.
func (s *Service) retire(c *change, reason string) {
	c.rec.State = StateDiscarded
	c.rec.LastError = strPtr(reason)
	c.rec.UpdatedAt = s.store.timestamp()
	if err := s.store.persist(c.rec); err != nil {
		s.log.Warn("persisting retired change failed", "change_id", c.rec.ChangeID, "error", err)
	}
	s.log.Info("change discarded", "change_id", c.rec.ChangeID, "reason", reason)
}

// lockOpen acquires an open change's mutex.
func (s *Service) lockOpen(ctx context.Context, id string) (*change, error) {
	c, ok := s.store.get(id)
	if !ok {
		return nil, fmt.Errorf("%w %q", ErrUnknownChange, id)
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	if c.rec.State != StateOpen {
		c.release()
		return nil, fmt.Errorf("%w: %s is %s", ErrNotOpen, id, c.rec.State)
	}
	if err := s.checkSource(c.rec.Source); err != nil {
		c.release()
		return nil, err
	}
	return c, nil
}

func (s *Service) withSource(name string, fn func()) {
	mu := s.opts.Sources.SourceLock(name)
	mu.Lock()
	defer mu.Unlock()
	fn()
}

// git runs a git command in the change's worktree under the source mutex.
func (s *Service) git(ctx context.Context, rec *Record, opts gitx.Opts, args ...string) ([]byte, error) {
	repo := s.opts.Sources.Repo(rec.Source)
	var out []byte
	var err error
	s.withSource(rec.Source, func() {
		out, err = repo.Git.Run(ctx, s.store.worktreePath(rec.ChangeID), opts, args...)
	})
	return out, err
}

func (s *Service) gitOutput(ctx context.Context, rec *Record, args ...string) (string, error) {
	out, err := s.git(ctx, rec, gitx.Opts{}, args...)
	return strings.TrimSpace(string(out)), err
}

// touchBundle records a bundle in touched_bundles.
func touchBundle(rec *Record, bundle string) bool {
	if slices.Contains(rec.TouchedBundles, bundle) {
		return false
	}
	rec.TouchedBundles = append(rec.TouchedBundles, bundle)
	slices.Sort(rec.TouchedBundles)
	return true
}

// Change returns a copy of a change record.
func (s *Service) Change(id string) (Record, bool) {
	c, ok := s.store.get(id)
	if !ok {
		return Record{}, false
	}
	if err := c.acquire(context.Background()); err != nil {
		return Record{}, false
	}
	defer c.release()
	return *c.rec, true
}
