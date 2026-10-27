package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/paths"
)

const maxRetryDelay = time.Hour

// Index is a source's persisted snapshot index (snapshots.json).
type Index struct {
	RenderContext string            `json:"render_context"`
	Current       string            `json:"current"`
	Superseded    map[string]string `json:"superseded"` // revision → RFC 3339 time
}

// Options configures a Manager.
type Options struct {
	Config *config.Config
	Dirs   paths.Dirs
	Git    *gitx.Git
	Logger *slog.Logger
	Getenv func(string) (string, bool) // nil uses os.LookupEnv
	// BaseRevisions returns, per source, the base revisions of open changes,
	// which retention must keep.
	BaseRevisions func() map[string]map[string]bool
	// OnViews receives rebuilt views under the view mutex.
	OnViews func(map[config.HarnessID]*catalog.View)
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Shutdown, when cancelled, stops extractions from starting new prepare
	// commands; nil never stops them.
	Shutdown context.Context
}

// Manager owns every source's snapshots and current revision.
type Manager struct {
	opts    Options
	sources []*sourceState
	byName  map[string]*sourceState
	cache   catalog.Cache
	flight  singleflight.Group

	viewMu   sync.Mutex
	views    map[config.HarnessID]*catalog.View
	stale    bool              // a retained revision was re-extracted; rebuild even if revisions match
	builtGen map[string]uint64 // source generations the views were built from
	logged   map[string]bool
}

type sourceState struct {
	src      *config.Source
	repo     *Repo // git sources
	pathRoot string
	revDir   string
	hash     string

	refreshLock chan struct{} // one-slot: whole Refresh runs; waiting honors the caller's context
	srcMu       sync.Mutex    // Git commands on repo.git and worktrees
	indexMu     sync.Mutex
	index       Index
	records     sync.Map // revision → *catalog.RenderRecord

	retryDelay time.Duration
	nextRetry  time.Time
	generation uint64 // incremented whenever current changes; guarded by indexMu
}

// NewManager creates a manager; call Init before use.
func NewManager(opts Options) *Manager {
	if opts.Getenv == nil {
		opts.Getenv = os.LookupEnv
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Shutdown == nil {
		opts.Shutdown = context.Background()
	}
	if opts.BaseRevisions == nil {
		opts.BaseRevisions = func() map[string]map[string]bool { return nil }
	}
	m := &Manager{opts: opts, byName: map[string]*sourceState{}, logged: map[string]bool{}}
	for _, src := range opts.Config.Sources {
		st := &sourceState{
			src:         src,
			revDir:      filepath.Join(opts.Dirs.SourceCache(src.Name), "rev"),
			refreshLock: make(chan struct{}, 1),
			hash:        renderContextHash(src, opts.Getenv),
		}
		if src.Kind == config.KindGit {
			st.repo = &Repo{Git: opts.Git, Dir: filepath.Join(opts.Dirs.SourceState(src.Name), "repo.git"), URL: src.URL, Branch: src.Branch}
		}
		m.sources = append(m.sources, st)
		m.byName[src.Name] = st
	}
	return m
}

func (st *sourceState) indexPath() string {
	return filepath.Join(filepath.Dir(st.revDir), "snapshots.json")
}

// Init performs startup cleanup: stale Git locks, interrupted clones and
// extractions, unindexed snapshots, and the render-context reset.
func (m *Manager) Init() error {
	var errs []error
	for _, st := range m.sources {
		if err := m.initSource(st); err != nil {
			errs = append(errs, fmt.Errorf("source %s: %w", st.src.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) initSource(st *sourceState) error {
	if err := os.MkdirAll(st.revDir, 0o755); err != nil {
		return err
	}
	if st.src.Kind == config.KindPath {
		root, err := filepath.EvalSymlinks(st.src.Path)
		if err != nil {
			return fmt.Errorf("resolving %s: %w", st.src.Path, err)
		}
		st.pathRoot = root
	}
	if st.repo != nil {
		if err := cleanGitState(st.repo.Dir); err != nil {
			return err
		}
	}
	idx, err := readIndex(st.indexPath())
	if err != nil {
		return err
	}
	if idx.RenderContext != st.hash {
		m.opts.Logger.Info("render context changed; discarding snapshots", "source", st.src.Name)
		idx = Index{RenderContext: st.hash, Superseded: map[string]string{}}
		if err := clearDir(st.revDir); err != nil {
			return err
		}
	}
	st.index = idx
	if err := cleanRevDir(st.revDir, idx); err != nil {
		return err
	}
	return writeJSONAtomic(st.indexPath(), st.index)
}

func readIndex(p string) (Index, error) {
	idx := Index{Superseded: map[string]string{}}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return Index{Superseded: map[string]string{}}, nil // a corrupt index is rebuilt; snapshots are disposable
	}
	if idx.Superseded == nil {
		idx.Superseded = map[string]string{}
	}
	return idx, nil
}

// cleanGitState removes interrupted clones and stale lock files. The daemon
// lock guarantees no daemon-owned Git process is running.
func cleanGitState(repoDir string) error {
	matches, err := filepath.Glob(repoDir + ".tmp-*")
	if err != nil {
		return err
	}
	for _, mt := range matches {
		if err := os.RemoveAll(mt); err != nil {
			return err
		}
	}
	err = filepath.WalkDir(repoDir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			return os.Remove(p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("removing stale git locks: %w", err)
	}
	return nil
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeTree(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// cleanRevDir deletes temporary trees, orphaned records, snapshots without a
// record, and snapshots the index does not know.
func cleanRevDir(dir string, idx Index) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	known := func(rev string) bool {
		_, sup := idx.Superseded[rev]
		return rev == idx.Current || sup
	}
	for _, e := range entries {
		name := e.Name()
		full := filepath.Join(dir, name)
		var remove bool
		switch {
		case strings.HasPrefix(name, ".tmp-"):
			remove = true
		case strings.HasSuffix(name, ".render.json"):
			rev := strings.TrimSuffix(name, ".render.json")
			_, statErr := os.Stat(filepath.Join(dir, rev))
			remove = statErr != nil || !known(rev)
		default:
			_, statErr := os.Stat(full + ".render.json")
			remove = statErr != nil || !known(name)
		}
		if remove {
			if err := removeTree(full); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadIndexes reads every source's snapshot index without the startup
// cleanup, for read-only use alongside a running daemon.
func (m *Manager) LoadIndexes() error {
	for _, st := range m.sources {
		idx, err := readIndex(st.indexPath())
		if err != nil {
			return fmt.Errorf("source %s: %w", st.src.Name, err)
		}
		st.indexMu.Lock()
		st.index = idx
		st.indexMu.Unlock()
	}
	return nil
}

// SourceLock returns the source mutex that serializes Git commands on the
// source's repository and change worktrees.
func (m *Manager) SourceLock(name string) *sync.Mutex { return &m.byName[name].srcMu }

// Repo returns a git source's repository, or nil.
func (m *Manager) Repo(name string) *Repo {
	if st, ok := m.byName[name]; ok {
		return st.repo
	}
	return nil
}

// Current returns a source's current revision ("" when none).
func (m *Manager) Current(name string) string {
	st, ok := m.byName[name]
	if !ok {
		return ""
	}
	st.indexMu.Lock()
	defer st.indexMu.Unlock()
	return st.index.Current
}

// RefreshOptions adjusts one Refresh run.
type RefreshOptions struct {
	SuppressRebuild bool
}

// Refresh fetches (or clones) a source, extracts its tip when needed, makes
// it current, rebuilds the views, and applies retention. Errors from every
// step are joined; a fetch failure still serves the local branch ref.
func (m *Manager) Refresh(ctx context.Context, name string, opts RefreshOptions) error {
	st, ok := m.byName[name]
	if !ok {
		return fmt.Errorf("unknown source %q", name)
	}
	select {
	case st.refreshLock <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("waiting to refresh %s: %w", name, ctx.Err())
	}
	defer func() { <-st.refreshLock }()
	var errs []error
	var err error
	if st.src.Kind == config.KindGit {
		err = m.refreshGit(ctx, st)
	} else {
		err = m.refreshPath(ctx, st)
	}
	if err != nil {
		errs = append(errs, err)
	}
	if !opts.SuppressRebuild {
		if err := m.RebuildViews(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := m.retain(st); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) refreshGit(ctx context.Context, st *sourceState) error {
	var fetchErr error
	st.srcMu.Lock()
	if !st.repo.Exists() {
		fetchErr = st.repo.Clone(ctx)
	} else {
		fetchErr = st.repo.Fetch(ctx)
	}
	var tip string
	var tipErr error
	if st.repo.Exists() {
		tip, tipErr = st.repo.Tip(ctx)
	}
	st.srcMu.Unlock()
	if !st.repo.Exists() || tipErr != nil {
		return errors.Join(fetchErr, tipErr)
	}
	switch m.extractionNeed(st, tip) {
	case needNone:
		return errors.Join(fetchErr, m.setCurrent(st, tip))
	case needReplace:
		rec, err := m.extractGit(ctx, st, tip, true)
		if err != nil {
			return errors.Join(fetchErr, err)
		}
		m.noteRetry(st, rec)
	case needExtract:
		rec, err := m.extractGit(ctx, st, tip, false)
		if err != nil {
			return errors.Join(fetchErr, err)
		}
		m.noteRetry(st, rec)
	}
	return errors.Join(fetchErr, m.setCurrent(st, tip))
}

func (m *Manager) refreshPath(ctx context.Context, st *sourceState) error {
	e := m.pathExtraction(st)
	staged, err := e.stage(ctx)
	if err != nil {
		return err
	}
	rev := staged.record.Revision
	need := m.extractionNeed(st, rev)
	if need == needNone {
		// Unchanged: skip rendering and prepare entirely.
		if err := removeTree(staged.dir); err != nil {
			return err
		}
		return m.setCurrent(st, rev)
	}
	t, err := e.finish(ctx, staged)
	if err != nil {
		return err
	}
	if err := t.finalize(st.revDir, need == needReplace); err != nil {
		return err
	}
	st.records.Store(rev, t.record)
	if need == needReplace {
		m.invalidate(st, rev)
	}
	m.noteRetry(st, t.record)
	return m.setCurrent(st, rev)
}

// invalidate forgets the parsed catalog of a re-extracted revision and forces
// the next view rebuild.
func (m *Manager) invalidate(st *sourceState, rev string) {
	m.cache.Drop(st.src.Name, rev)
	m.viewMu.Lock()
	m.stale = true
	m.viewMu.Unlock()
}

type need int

const (
	needNone    need = iota // the snapshot exists and is usable
	needExtract             // the snapshot is missing
	needReplace             // the snapshot's prepare retry is due
)

// extractionNeed decides whether rev must be extracted or re-extracted.
func (m *Manager) extractionNeed(st *sourceState, rev string) need {
	if !m.snapshotExists(st, rev) {
		return needExtract
	}
	rec, err := m.record(st, rev)
	if err != nil {
		return needReplace
	}
	if rec.PrepareRetry && !m.opts.Now().Before(st.nextRetry) {
		return needReplace
	}
	return needNone
}

func (m *Manager) noteRetry(st *sourceState, rec *catalog.RenderRecord) {
	if !rec.PrepareRetry {
		st.retryDelay = 0
		return
	}
	if st.retryDelay == 0 {
		st.retryDelay = m.opts.Config.FetchInterval
	} else {
		st.retryDelay = min(st.retryDelay*2, maxRetryDelay)
	}
	st.nextRetry = m.opts.Now().Add(st.retryDelay)
}

func (m *Manager) setCurrent(st *sourceState, rev string) error {
	st.indexMu.Lock()
	defer st.indexMu.Unlock()
	prev := st.index.Current
	if prev != "" && prev != rev && m.snapshotExists(st, prev) {
		st.index.Superseded[prev] = m.opts.Now().UTC().Format(time.RFC3339)
	}
	delete(st.index.Superseded, rev)
	if st.index.Current != rev {
		st.generation++
	}
	st.index.Current = rev
	return writeJSONAtomic(st.indexPath(), st.index)
}

func (m *Manager) snapshotExists(st *sourceState, rev string) bool {
	_, err := os.Stat(filepath.Join(st.revDir, rev))
	return err == nil
}

func (m *Manager) gitExtraction(st *sourceState, sha string) *extraction {
	return &extraction{
		src: st.src, revDir: st.revDir, revision: sha, tmpName: ".tmp-" + sha + "-",
		populate: m.lockedPopulate(st, st.repo.populate(sha, st.src.AllRootPaths())),
		vars:     st.src.Template.Vars, env: st.src.Template.Env, getenv: m.opts.Getenv, hash: st.hash,
		shutdown: m.opts.Shutdown,
	}
}

// lockedPopulate holds the source mutex only while Git reads the tree.
func (m *Manager) lockedPopulate(st *sourceState, populate func(context.Context, string) ([]special, error)) func(context.Context, string) ([]special, error) {
	return func(ctx context.Context, dir string) ([]special, error) {
		st.srcMu.Lock()
		defer st.srcMu.Unlock()
		return populate(ctx, dir)
	}
}

func (m *Manager) pathExtraction(st *sourceState) *extraction {
	return &extraction{
		src: st.src, revDir: st.revDir, tmpName: ".tmp-path-",
		populate: populatePath(st.pathRoot, st.src.AllRootPaths()),
		vars:     st.src.Template.Vars, env: st.src.Template.Env, getenv: m.opts.Getenv, hash: st.hash,
		shutdown: m.opts.Shutdown,
	}
}

// extractGit extracts a commit once, deduplicating concurrent requests.
// With replace, an existing snapshot is rebuilt (prepare retry).
func (m *Manager) extractGit(ctx context.Context, st *sourceState, sha string, replace bool) (*catalog.RenderRecord, error) {
	v, err, _ := m.flight.Do(st.src.Name+"@"+sha, func() (any, error) {
		t, err := m.gitExtraction(st, sha).build(ctx)
		if err != nil {
			return nil, err
		}
		if err := t.finalize(st.revDir, replace); err != nil {
			return nil, err
		}
		rec, err := loadRecord(st.revDir, sha)
		if err != nil {
			return nil, err
		}
		st.records.Store(sha, rec)
		if replace {
			m.invalidate(st, sha)
		}
		return rec, nil
	})
	if err != nil {
		return nil, fmt.Errorf("extracting %s@%s: %w", st.src.Name, shortRev(sha), err)
	}
	return v.(*catalog.RenderRecord), nil
}

// EnsureSnapshot extracts a commit that is not current (a submit base or a
// rebase tip) when its snapshot is missing, and records it for retention.
func (m *Manager) EnsureSnapshot(ctx context.Context, name, sha string) error {
	st := m.byName[name]
	if m.snapshotExists(st, sha) {
		return nil
	}
	if _, err := m.extractGit(ctx, st, sha, false); err != nil {
		return err
	}
	st.indexMu.Lock()
	defer st.indexMu.Unlock()
	if _, ok := st.index.Superseded[sha]; ok || st.index.Current == sha {
		return nil
	}
	st.index.Superseded[sha] = m.opts.Now().UTC().Format(time.RFC3339)
	return writeJSONAtomic(st.indexPath(), st.index)
}

// Tree is a materialized commit used for submit verification.
type Tree struct {
	Dir    string
	Record *catalog.RenderRecord
}

// Remove deletes the tree.
func (t *Tree) Remove() error { return removeTree(t.Dir) }

// MaterializeCommit extracts a commit into a verification tree rendered in
// place. prepare runs only in the touched bundles; the others take their
// prepare diagnostics from base.
func (m *Manager) MaterializeCommit(ctx context.Context, name, sha string, touched []string, base *catalog.RenderRecord) (*Tree, error) {
	st := m.byName[name]
	e := m.gitExtraction(st, sha)
	e.tmpName = ".tmp-verify-"
	e.renderInPlace = true
	e.baseRecord = base
	e.prepareOnly = map[string]bool{}
	for _, b := range touched {
		e.prepareOnly[b] = true
	}
	t, err := e.build(ctx)
	if err != nil {
		return nil, err
	}
	return &Tree{Dir: t.dir, Record: t.record}, nil
}

// SnapshotDir returns the directory of a retained snapshot.
func (m *Manager) SnapshotDir(name, rev string) string {
	return filepath.Join(m.byName[name].revDir, rev)
}

// Record returns a retained snapshot's RenderRecord.
func (m *Manager) Record(name, rev string) (*catalog.RenderRecord, error) {
	return m.record(m.byName[name], rev)
}

func (m *Manager) record(st *sourceState, rev string) (*catalog.RenderRecord, error) {
	if v, ok := st.records.Load(rev); ok {
		return v.(*catalog.RenderRecord), nil
	}
	rec, err := loadRecord(st.revDir, rev)
	if err != nil {
		return nil, err
	}
	st.records.Store(rev, rec)
	return rec, nil
}

func loadRecord(revDir, rev string) (*catalog.RenderRecord, error) {
	data, err := os.ReadFile(filepath.Join(revDir, rev+".render.json"))
	if err != nil {
		return nil, err
	}
	var rec catalog.RenderRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("decoding render record %s: %w", rev, err)
	}
	return &rec, nil
}

// Snapshot returns a retained revision as a catalog snapshot.
func (m *Manager) Snapshot(name, rev string) (catalog.Snapshot, error) {
	st, ok := m.byName[name]
	if !ok {
		return catalog.Snapshot{}, fmt.Errorf("unknown source %q", name)
	}
	if rev == "" || !m.snapshotExists(st, rev) {
		return catalog.Snapshot{}, fmt.Errorf("revision %s of %s is not retained", shortRev(rev), name)
	}
	rec, err := m.record(st, rev)
	if err != nil {
		return catalog.Snapshot{}, err
	}
	return catalog.Snapshot{Source: st.src, Revision: rev, Root: filepath.Join(st.revDir, rev), Record: rec}, nil
}

// Retained reports whether a revision's snapshot is retained.
func (m *Manager) Retained(name, rev string) bool {
	st, ok := m.byName[name]
	return ok && rev != "" && m.snapshotExists(st, rev)
}

// CurrentSnapshots returns the current snapshot of every source that has one.
func (m *Manager) CurrentSnapshots() ([]catalog.Snapshot, error) {
	snaps, _, err := m.currentSnapshots()
	return snaps, err
}

// currentSnapshots also returns each source's generation, read with its
// current revision.
func (m *Manager) currentSnapshots() ([]catalog.Snapshot, map[string]uint64, error) {
	var snaps []catalog.Snapshot
	gens := map[string]uint64{}
	for _, st := range m.sources {
		st.indexMu.Lock()
		cur, gen := st.index.Current, st.generation
		st.indexMu.Unlock()
		gens[st.src.Name] = gen
		if cur == "" {
			continue
		}
		snap, err := m.Snapshot(st.src.Name, cur)
		if err != nil {
			return nil, nil, err
		}
		snaps = append(snaps, snap)
	}
	return snaps, gens, nil
}

// RebuildViews rebuilds every harness view from the current snapshots when
// any source's current revision differs from what the views were built
// from, and hands them to OnViews.
func (m *Manager) RebuildViews() error {
	snaps, gens, err := m.currentSnapshots()
	if err != nil {
		return err
	}
	return m.rebuildFrom(snaps, gens)
}

// rebuildFrom builds views from snapshots read at the given generations.
// A caller holding snapshots older than the built views skips the rebuild,
// so concurrent refreshes never move views backwards.
func (m *Manager) rebuildFrom(snaps []catalog.Snapshot, gens map[string]uint64) error {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	for name, gen := range gens {
		if gen < m.builtGen[name] {
			return nil
		}
	}
	if m.views != nil && !m.stale && upToDate(m.views[config.ClaudeCode], snaps) {
		return nil
	}
	m.stale = false
	m.builtGen = gens
	views := m.cache.Views(snaps)
	m.views = views
	m.logDiagnostics(views)
	if m.opts.OnViews != nil {
		m.opts.OnViews(views)
	}
	return nil
}

func upToDate(v *catalog.View, snaps []catalog.Snapshot) bool {
	if len(v.Revisions) != len(snaps) {
		return false
	}
	for _, s := range snaps {
		if v.Revisions[s.Source.Name] != s.Revision {
			return false
		}
	}
	return true
}

// Views returns the current views; nil before the first build.
func (m *Manager) Views() map[config.HarnessID]*catalog.View {
	m.viewMu.Lock()
	defer m.viewMu.Unlock()
	return m.views
}

// Cache returns the catalog cache for building views of retained revisions.
func (m *Manager) Cache() *catalog.Cache { return &m.cache }

func (m *Manager) logDiagnostics(views map[config.HarnessID]*catalog.View) {
	for _, h := range config.Harnesses {
		for _, d := range views[h].Diagnostics {
			key := d.Code + "|" + d.String() + "|" + d.Revision
			if m.logged[key] {
				continue
			}
			m.logged[key] = true
			m.opts.Logger.Warn("skill refused", "code", d.Code, "skill", d.Bundle, "location", d.String())
		}
		for _, s := range views[h].Shadowed {
			key := "shadow|" + s.Name + "|" + s.Winner + "|" + s.Loser
			if m.logged[key] {
				continue
			}
			m.logged[key] = true
			m.opts.Logger.Warn("skill shadowed", "skill", s.Name, "served_from", s.Winner, "hidden_in", s.Loser)
		}
	}
}

// retain deletes superseded snapshots older than the retention period,
// except current and the base revisions of open changes.
func (m *Manager) retain(st *sourceState) error {
	keep := m.opts.BaseRevisions()[st.src.Name]
	st.indexMu.Lock()
	defer st.indexMu.Unlock()
	cutoff := m.opts.Now().Add(-m.opts.Config.SnapshotRetention)
	changed := false
	var errs []error
	for rev, stamp := range st.index.Superseded {
		t, err := time.Parse(time.RFC3339, stamp)
		if err == nil && t.After(cutoff) || rev == st.index.Current || keep[rev] {
			continue
		}
		if err := removeTree(filepath.Join(st.revDir, rev)); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Remove(filepath.Join(st.revDir, rev+".render.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		delete(st.index.Superseded, rev)
		st.records.Delete(rev)
		m.cache.Drop(st.src.Name, rev)
		changed = true
	}
	if changed {
		errs = append(errs, writeJSONAtomic(st.indexPath(), st.index))
	}
	return errors.Join(errs...)
}

// Maintain runs `git maintenance run --auto` on every git source.
func (m *Manager) Maintain(ctx context.Context) error {
	var errs []error
	for _, st := range m.sources {
		if st.repo == nil || !st.repo.Exists() {
			continue
		}
		st.srcMu.Lock()
		_, err := st.repo.Git.Run(ctx, st.repo.Dir, gitx.Opts{}, "maintenance", "run", "--auto")
		st.srcMu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("maintenance of %s: %w", st.src.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Sources lists the configured source names in precedence order.
func (m *Manager) Sources() []string {
	names := make([]string, 0, len(m.sources))
	for _, st := range m.sources {
		names = append(names, st.src.Name)
	}
	return names
}

// Source returns a source's configuration.
func (m *Manager) Source(name string) *config.Source {
	if st, ok := m.byName[name]; ok {
		return st.src
	}
	return nil
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
