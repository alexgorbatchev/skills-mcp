package authoring

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/proc"
)

const validateTimeout = 120 * time.Second

// verify runs every gate against the exact commit to be pushed: catalog
// diagnostics, served-name stability, scope and protected paths, the deny
// scan, and the source's validate command. A failure resets the commit.
func (s *Service) verify(ctx context.Context, c *change, progress func(string)) error {
	err := s.verifyCommit(ctx, c, progress)
	if err == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	return errors.Join(err, s.resetToBase(cleanupCtx, c.rec), s.store.persist(c.rec))
}

func (s *Service) verifyCommit(ctx context.Context, c *change, progress func(string)) error {
	rec := c.rec
	commit := *rec.Commit
	src := s.opts.Config.SourceByName(rec.Source)
	progress("verifying: preparing the base snapshot")
	if err := s.opts.Sources.EnsureSnapshot(ctx, rec.Source, rec.BaseRevision); err != nil {
		return err
	}
	baseRecord, err := s.opts.Sources.Record(rec.Source, rec.BaseRevision)
	if err != nil {
		return err
	}
	progress("verifying: materializing the commit")
	tree, err := s.opts.Sources.MaterializeCommit(ctx, rec.Source, commit, rec.TouchedBundles, baseRecord)
	if err != nil {
		return err
	}
	defer func() {
		if err := tree.Remove(); err != nil {
			s.log.Warn("removing verification tree failed", "dir", tree.Dir, "error", err)
		}
	}()
	progress("verifying: catalog")
	if err := s.checkCatalog(rec, tree.Dir, tree.Record); err != nil {
		return err
	}
	progress("verifying: changed paths")
	if err := s.checkScope(ctx, rec, src); err != nil {
		return err
	}
	progress("verifying: deny patterns")
	if err := s.checkDeny(ctx, rec, src); err != nil {
		return err
	}
	return s.runValidate(ctx, rec, src, tree.Dir, progress)
}

// checkCatalog compares the views built from the commit with the views of
// the base revision.
func (s *Service) checkCatalog(rec *Record, treeDir string, treeRecord *catalog.RenderRecord) error {
	currents, err := s.opts.Sources.CurrentSnapshots()
	if err != nil {
		return err
	}
	baseSnap, err := s.opts.Sources.Snapshot(rec.Source, rec.BaseRevision)
	if err != nil {
		return err
	}
	commitSnap := catalog.Snapshot{Source: baseSnap.Source, Revision: *rec.Commit, Root: treeDir, Record: treeRecord}
	withSource := func(snap catalog.Snapshot) []catalog.Snapshot {
		out := slices.Clone(currents)
		for i := range out {
			if out[i].Source.Name == rec.Source {
				out[i] = snap
				return out
			}
		}
		return append(out, snap)
	}
	baseViews := s.opts.Sources.Cache().Views(withSource(baseSnap))
	var fresh catalog.Cache // verification catalogs never enter the shared cache
	newViews := fresh.Views(withSource(commitSnap))
	var problems []string
	for _, h := range config.Harnesses {
		problems = append(problems, compareViews(rec, baseViews[h], newViews[h])...)
	}
	slices.Sort(problems)
	problems = slices.Compact(problems)
	if len(problems) > 0 {
		return fmt.Errorf("verification failed:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

func compareViews(rec *Record, base, next *catalog.View) []string {
	var problems []string
	known := map[string]bool{}
	for _, d := range base.Diagnostics {
		known[d.Code+"|"+d.Bundle+"|"+d.File] = true
	}
	for _, d := range next.Diagnostics {
		if !known[d.Code+"|"+d.Bundle+"|"+d.File] {
			problems = append(problems, fmt.Sprintf("%s: %s %s", next.Harness, d.Code, d.String()))
		}
	}
	for _, s := range base.Skills {
		if s.Source != rec.Source {
			if after := next.Skill(s.Name); after == nil || after.Source != s.Source {
				problems = append(problems, fmt.Sprintf("%s: skill %q from source %s would no longer be served", next.Harness, s.Name, s.Source))
			}
			continue
		}
		if !slices.Contains(rec.TouchedBundles, s.Bundle) {
			continue
		}
		after := next.Skill(s.Name)
		if after == nil || after.Bundle != s.Bundle || after.Invocation != s.Invocation {
			problems = append(problems, fmt.Sprintf("%s: bundle %s would change its served name or invocation (%s, %s); renaming skills is not supported", next.Harness, s.Bundle, s.Name, s.Invocation))
		}
	}
	return problems
}

type nameStatus struct {
	status string
	paths  []string // one path, or old and new for renames and copies
}

func parseNameStatus(out []byte) ([]nameStatus, error) {
	fields := gitx.SplitNUL(out)
	var entries []nameStatus
	for i := 0; i < len(fields); {
		status := fields[i]
		n := 1
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			n = 2
		}
		if i+n >= len(fields) {
			return nil, fmt.Errorf("truncated name-status output")
		}
		entries = append(entries, nameStatus{status: status[:1], paths: fields[i+1 : i+1+n]})
		i += 1 + n
	}
	return entries, nil
}

// checkScope rejects deletions, changes outside touched bundles, and
// changes to protected validator paths.
func (s *Service) checkScope(ctx context.Context, rec *Record, src *config.Source) error {
	out, err := s.git(ctx, rec, gitx.Opts{}, "diff-tree", "-r", "-z", "-M", "--name-status", "--no-ext-diff", "--no-textconv", rec.BaseRevision, *rec.Commit)
	if err != nil {
		return fmt.Errorf("listing changed paths: %w", err)
	}
	entries, err := parseNameStatus(out)
	if err != nil {
		return err
	}
	protected, err := s.protectedPaths(ctx, rec, src)
	if err != nil {
		return err
	}
	var problems []string
	inScope := func(p string) bool {
		return slices.ContainsFunc(rec.TouchedBundles, func(b string) bool { return strings.HasPrefix(p, b+"/") })
	}
	for _, e := range entries {
		if e.status == "D" {
			problems = append(problems, "deletes "+e.paths[0]+"; deleting files is not supported")
		}
		for _, p := range e.paths {
			if !inScope(p) {
				problems = append(problems, "changes "+p+" outside the touched bundles")
			}
			if protected(p) {
				problems = append(problems, "changes protected validator path "+p)
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("verification failed:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// protectedPaths returns a predicate for paths a change may not modify:
// every validate argument naming a file in the base revision, its
// directory, and ignore files from that directory up to the repository root.
func (s *Service) protectedPaths(ctx context.Context, rec *Record, src *config.Source) (func(string) bool, error) {
	var files, dirs []string
	for _, arg := range src.Validate {
		if arg == "" || strings.Contains(arg, "{bundle_dir}") || strings.HasPrefix(arg, "/") {
			continue
		}
		clean := path.Clean(arg)
		_, err := s.git(ctx, rec, gitx.Opts{}, "cat-file", "-e", rec.BaseRevision+":"+clean)
		if err != nil {
			continue
		}
		files = append(files, clean)
		dir := path.Dir(clean)
		dirs = append(dirs, dir)
		for d := dir; src.IgnoreFile != ""; d = path.Dir(d) {
			files = append(files, path.Join(d, src.IgnoreFile))
			if d == "." {
				break
			}
		}
	}
	return func(p string) bool {
		if slices.Contains(files, p) {
			return true
		}
		return slices.ContainsFunc(dirs, func(d string) bool { return d != "." && strings.HasPrefix(p, d+"/") })
	}, nil
}

// checkDeny scans added lines, added and renamed paths, and the commit
// message against the source's deny patterns.
func (s *Service) checkDeny(ctx context.Context, rec *Record, src *config.Source) error {
	if len(src.DenyPatterns) == 0 {
		return fmt.Errorf("source %s has no deny_patterns; refusing to push unscanned content", src.Name)
	}
	commit := *rec.Commit
	patch, err := s.git(ctx, rec, gitx.Opts{}, "diff-tree", "-r", "-p", "-a", "-U0", "-M", "--no-ext-diff", "--no-textconv", "--no-color", rec.BaseRevision, commit)
	if err != nil {
		return fmt.Errorf("reading the commit diff: %w", err)
	}
	names, err := s.git(ctx, rec, gitx.Opts{}, "diff-tree", "-r", "-z", "-M", "--name-status", "--no-ext-diff", "--no-textconv", rec.BaseRevision, commit)
	if err != nil {
		return fmt.Errorf("listing changed paths: %w", err)
	}
	msg, err := s.git(ctx, rec, gitx.Opts{}, "log", "-1", "--format=%B", commit)
	if err != nil {
		return fmt.Errorf("reading the commit message: %w", err)
	}
	entries, err := parseNameStatus(names)
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range entries {
		if e.status == "A" || e.status == "R" || e.status == "C" {
			paths = append(paths, e.paths[len(e.paths)-1])
		}
	}
	added, err := addedLines(patch)
	if err != nil {
		return err
	}
	if hits := denyScan(src.DenyPatterns, added, paths, string(msg)); len(hits) > 0 {
		return fmt.Errorf("verification failed: content matches deny patterns:\n%s", strings.Join(hits, "\n"))
	}
	return nil
}

// addedLines returns the added lines of a patch. Only the "---"/"+++" pair
// before a file's first hunk is a header; inside a hunk every "+" line is
// content, even one that starts with "++ ".
func addedLines(patch []byte) ([]string, error) {
	var lines []string
	inHunk := false
	sc := bufio.NewScanner(bytes.NewReader(patch))
	sc.Buffer(make([]byte, 64*1024), maxFileBytes+1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && strings.HasPrefix(line, "+"):
			lines = append(lines, line[1:])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the commit diff: %w", err)
	}
	return lines, nil
}

// runValidate runs the source's validate command once per touched bundle
// from the trusted base snapshot.
func (s *Service) runValidate(ctx context.Context, rec *Record, src *config.Source, treeDir string, progress func(string)) error {
	if len(src.Validate) == 0 {
		return nil
	}
	cwd := s.opts.Sources.SnapshotDir(rec.Source, rec.BaseRevision)
	for _, bundle := range rec.TouchedBundles {
		dir := filepath.Join(treeDir, filepath.FromSlash(bundle))
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if err := s.opts.Shutdown.Err(); err != nil {
			return errInterrupted
		}
		progress("verifying: validate " + bundle)
		argv := make([]string, len(src.Validate))
		for i, a := range src.Validate {
			argv[i] = strings.ReplaceAll(a, "{bundle_dir}", dir)
		}
		timeout := validateTimeout
		if dl, ok := ctx.Deadline(); ok {
			timeout = min(timeout, time.Until(dl))
		}
		res, err := proc.Run(ctx, proc.Command{Name: argv[0], Args: argv[1:], Dir: cwd, Timeout: timeout})
		if err != nil {
			out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
			return fmt.Errorf("validate failed for %s: %w\n%s", bundle, err, out)
		}
	}
	return nil
}
