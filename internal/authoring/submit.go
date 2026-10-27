package authoring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
)

// progressInterval is how often a running step is re-reported; a variable
// so tests can shorten it.
var progressInterval = 10 * time.Second

var errInterrupted = errors.New("interrupted by shutdown")

// SubmitResult is skill_change_submit's response.
type SubmitResult struct {
	ChangeID    string       `json:"change_id"`
	Commit      string       `json:"commit"`
	Message     string       `json:"message"`
	Publication *Publication `json:"publication"`
}

func resultOf(rec *Record) SubmitResult {
	return SubmitResult{ChangeID: rec.ChangeID, Commit: deref(rec.Commit), Message: deref(rec.Message), Publication: rec.Publication}
}

// Submit commits, verifies, and pushes a change. The pipeline runs on a
// detached context that owns the change mutex until it finishes, so a
// cancelled request or client timeout never interrupts a push; a later call
// returns the recorded outcome. progress receives step descriptions.
func (s *Service) Submit(reqCtx context.Context, id, message string, progress func(string)) (SubmitResult, error) {
	c, ok := s.store.get(id)
	if !ok {
		return SubmitResult{}, fmt.Errorf("%w %q", ErrUnknownChange, id)
	}
	if err := c.acquire(reqCtx); err != nil {
		return SubmitResult{}, err
	}
	switch c.rec.State {
	case StateSubmitted:
		defer c.release()
		return resultOf(c.rec), nil
	case StateDiscarded:
		defer c.release()
		return SubmitResult{}, fmt.Errorf("%w: %s is discarded", ErrNotOpen, id)
	}
	if err := s.checkSource(c.rec.Source); err != nil {
		c.release()
		return SubmitResult{}, err
	}
	type outcome struct {
		res SubmitResult
		err error
	}
	done := make(chan outcome, 1)
	s.pipelines.Add(1)
	go func() {
		defer s.pipelines.Done()
		defer c.release()
		ctx, cancel := context.WithTimeout(s.opts.Kill, s.opts.Config.SubmitTimeout)
		defer cancel()
		res, err := s.runPipeline(ctx, c, message, newHeartbeat(progress))
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-reqCtx.Done():
		return SubmitResult{}, fmt.Errorf("the submit continues in the background; call skill_change_submit again for its outcome: %w", reqCtx.Err())
	}
}

// heartbeat forwards progress and repeats the current step every
// progressInterval so clients reset their idle timers.
type heartbeat struct {
	mu   sync.Mutex
	send func(string)
	step string
}

func newHeartbeat(send func(string)) *heartbeat {
	if send == nil {
		send = func(string) {}
	}
	return &heartbeat{send: send}
}

func (h *heartbeat) report(step string) {
	h.mu.Lock()
	h.step = step
	h.mu.Unlock()
	h.send(step)
}

func (h *heartbeat) run(ctx context.Context) {
	t := time.NewTicker(progressInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.Lock()
			step := h.step
			h.mu.Unlock()
			h.send(step)
		}
	}
}

func (s *Service) runPipeline(ctx context.Context, c *change, message string, hb *heartbeat) (SubmitResult, error) {
	hbCtx, stop := context.WithCancel(ctx)
	defer stop()
	go hb.run(hbCtx)
	err := s.pipeline(ctx, c, message, hb.report)
	rec := c.rec
	if err == nil {
		return resultOf(rec), nil
	}
	if rec.State == StateOpen {
		rec.LastError = strPtr(err.Error())
		rec.UpdatedAt = s.store.timestamp()
		if perr := s.store.persist(rec); perr != nil {
			err = errors.Join(err, perr)
		}
	}
	return SubmitResult{}, err
}

func (s *Service) pipeline(ctx context.Context, c *change, message string, progress func(string)) error {
	rec := c.rec
	if err := s.step(progress, "reconciling"); err != nil {
		return err
	}
	if err := s.reconcile(ctx, c, reconcileNormal); err != nil {
		return err
	}
	switch rec.State {
	case StateSubmitted:
		return nil
	case StateDiscarded:
		return fmt.Errorf("%w: %s", ErrNotOpen, deref(rec.LastError))
	}
	if rec.Commit == nil {
		if err := s.step(progress, "committing"); err != nil {
			return err
		}
		if err := s.commit(ctx, rec, message); err != nil {
			return err
		}
	}
	if err := s.checkHead(ctx, rec); err != nil {
		return err
	}
	if err := s.step(progress, "verifying"); err != nil {
		return err
	}
	if err := s.verify(ctx, c, progress); err != nil {
		return err
	}
	return s.publish(ctx, c, progress)
}

// step reports progress and stops the pipeline once shutdown has begun.
func (s *Service) step(progress func(string), name string) error {
	if s.opts.Shutdown.Err() != nil {
		return errInterrupted
	}
	progress(name)
	return nil
}

func (s *Service) commit(ctx context.Context, rec *Record, message string) error {
	if strings.TrimSpace(message) == "" {
		return errors.New("message must not be empty")
	}
	if len(rec.TouchedBundles) == 0 {
		return errors.New("the change has no edits")
	}
	if _, err := s.git(ctx, rec, gitx.Opts{}, "diff", "--cached", "--quiet", rec.BaseRevision); err == nil {
		return errors.New("the change has no staged edits")
	} else if gitx.ExitCode(err) != 1 {
		return fmt.Errorf("checking staged edits: %w", err)
	}
	if _, err := s.git(ctx, rec, gitx.Opts{}, "commit", "--quiet", "--no-verify", "-m", message, "--trailer", "Skills-MCP-Change: "+rec.ChangeID); err != nil {
		return fmt.Errorf("committing: %w", err)
	}
	head, err := s.gitOutput(ctx, rec, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading the new commit: %w", err)
	}
	rec.Commit, rec.Message = strPtr(head), strPtr(message)
	rec.UpdatedAt = s.store.timestamp()
	return s.store.persist(rec)
}

// checkHead requires HEAD to be the change commit on top of the base, with
// a clean worktree.
func (s *Service) checkHead(ctx context.Context, rec *Record) error {
	head, err := s.gitOutput(ctx, rec, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	parent, err := s.gitOutput(ctx, rec, "rev-parse", "HEAD^")
	if err != nil {
		return fmt.Errorf("reading HEAD^: %w", err)
	}
	if head != *rec.Commit || parent != rec.BaseRevision {
		return fmt.Errorf("worktree HEAD %s (parent %s) does not match commit %s on base %s", head, parent, *rec.Commit, rec.BaseRevision)
	}
	status, err := s.git(ctx, rec, gitx.Opts{}, "status", "--porcelain", "-z", "--ignored", "-uall")
	if err != nil {
		return fmt.Errorf("reading worktree status: %w", err)
	}
	if paths := gitx.SplitNUL(status); len(paths) > 0 {
		return fmt.Errorf("the worktree has unexpected changes: %s", strings.Join(paths, ", "))
	}
	return nil
}
