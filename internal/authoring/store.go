// Package authoring implements change handles: agents open a change, edit
// skill bundles in a dedicated worktree, and submit it as one verified
// commit pushed to the source's tracked branch.
package authoring

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Change states.
const (
	StateOpen      = "open"
	StateSubmitted = "submitted"
	StateDiscarded = "discarded"
)

var changeIDPattern = regexp.MustCompile(`^chg_[0-9a-f]{16}$`)

// Publication records a successful push.
type Publication struct {
	Remote       string  `json:"remote"`
	Ref          string  `json:"ref"`
	Commit       string  `json:"commit"`
	Attempts     int     `json:"attempts"`
	PushedAt     string  `json:"pushed_at"`
	RefreshError *string `json:"refresh_error"`
}

// Record is a persisted change (change.json).
type Record struct {
	ChangeID       string       `json:"change_id"`
	Source         string       `json:"source"`
	Harness        string       `json:"harness"`
	BaseRevision   string       `json:"base_revision"`
	Branch         string       `json:"branch"`
	Summary        string       `json:"summary"`
	State          string       `json:"state"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
	TouchedBundles []string     `json:"touched_bundles"`
	Message        *string      `json:"message"`
	Commit         *string      `json:"commit"`
	RebaseOnto     *string      `json:"rebase_onto"`
	PushStarted    bool         `json:"push_started"`
	LastError      *string      `json:"last_error"`
	Publication    *Publication `json:"publication"`
}

// change is one change's in-memory state. lock is a one-slot channel so
// waiting for the change mutex can honor a context.
type change struct {
	lock chan struct{}
	rec  *Record
}

func newChange(rec *Record) *change {
	return &change{lock: make(chan struct{}, 1), rec: rec}
}

func (c *change) acquire(ctx context.Context) error {
	select {
	case c.lock <- struct{}{}:
		return nil
	default:
	}
	select {
	case c.lock <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for change %s: %w", c.rec.ChangeID, ctx.Err())
	}
}

func (c *change) release() { <-c.lock }

// store persists change records and indexes open changes' base revisions.
type store struct {
	dir string
	now func() time.Time

	mu      sync.Mutex
	changes map[string]*change
	bases   map[string][2]string // change id → {source, base revision} of open changes
}

func newStore(dir string, now func() time.Time) *store {
	return &store{dir: dir, now: now, changes: map[string]*change{}, bases: map[string][2]string{}}
}

func (s *store) changeDir(id string) string    { return filepath.Join(s.dir, id) }
func (s *store) recordPath(id string) string   { return filepath.Join(s.dir, id, "change.json") }
func (s *store) worktreePath(id string) string { return filepath.Join(s.dir, id, "worktree") }

func newChangeID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error; it crashes instead
	return "chg_" + hex.EncodeToString(b)
}

func (s *store) timestamp() string { return s.now().UTC().Format(time.RFC3339) }

// persist writes the record atomically and refreshes the base index.
func (s *store) persist(rec *Record) error {
	if err := os.MkdirAll(s.changeDir(rec.ChangeID), 0o700); err != nil {
		return fmt.Errorf("creating change directory: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding change record: %w", err)
	}
	if err := writeFileAtomic(s.recordPath(rec.ChangeID), append(data, '\n')); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.State == StateOpen {
		s.bases[rec.ChangeID] = [2]string{rec.Source, rec.BaseRevision}
	} else {
		delete(s.bases, rec.ChangeID)
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	_, writeErr := tmp.Write(data)
	if err := errors.Join(writeErr, tmp.Sync(), tmp.Close()); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func (s *store) add(c *change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.changes[c.rec.ChangeID] = c
}

func (s *store) get(id string) (*change, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.changes[id]
	return c, ok
}

func (s *store) all() []*change {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*change, 0, len(s.changes))
	for _, c := range s.changes {
		out = append(out, c)
	}
	return out
}

func (s *store) remove(id string) error {
	s.mu.Lock()
	delete(s.changes, id)
	delete(s.bases, id)
	s.mu.Unlock()
	return os.RemoveAll(s.changeDir(id))
}

// baseRevisions returns, per source, the base revisions of open changes.
func (s *store) baseRevisions() map[string]map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]map[string]bool{}
	for _, sb := range s.bases {
		if out[sb[0]] == nil {
			out[sb[0]] = map[string]bool{}
		}
		out[sb[0]][sb[1]] = true
	}
	return out
}

// load reads every change record: unparsable records are quarantined and
// terminal records older than ttl are deleted.
func (s *store) load(ttl time.Duration, warn func(msg string, args ...any)) error {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing changes: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || !changeIDPattern.MatchString(e.Name()) {
			continue
		}
		data, err := os.ReadFile(s.recordPath(e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // a directory without a record is an orphan, removed later
		}
		var rec Record
		if err == nil {
			err = json.Unmarshal(data, &rec)
		}
		if err != nil || rec.ChangeID != e.Name() {
			warn("quarantining unreadable change record", "change_id", e.Name(), "error", err)
			if qerr := s.quarantine(e.Name()); qerr != nil {
				return qerr
			}
			continue
		}
		if rec.State != StateOpen && s.expired(rec.UpdatedAt, ttl) {
			if err := os.RemoveAll(s.changeDir(rec.ChangeID)); err != nil {
				return err
			}
			continue
		}
		s.add(newChange(&rec))
		if rec.State == StateOpen {
			s.mu.Lock()
			s.bases[rec.ChangeID] = [2]string{rec.Source, rec.BaseRevision}
			s.mu.Unlock()
		}
	}
	return nil
}

func (s *store) quarantine(id string) error {
	q := filepath.Join(s.dir, ".quarantine")
	if err := os.MkdirAll(q, 0o700); err != nil {
		return err
	}
	if err := os.Rename(s.recordPath(id), filepath.Join(q, id+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *store) expired(stamp string, ttl time.Duration) bool {
	t, err := time.Parse(time.RFC3339, stamp)
	return err != nil || s.now().Sub(t) > ttl
}

// orphans lists change directories without a record.
func (s *store) orphans() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !changeIDPattern.MatchString(e.Name()) {
			continue
		}
		if _, err := os.Stat(s.recordPath(e.Name())); errors.Is(err, fs.ErrNotExist) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func strPtr(s string) *string { return &s }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
