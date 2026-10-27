// Package source materializes skill sources into immutable, rendered
// snapshots and keeps each source's current revision fresh.
package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

// Kinds of non-regular tree entries that are never materialized.
const (
	specialSymlink   = "symlink"
	specialSubmodule = "submodule"
)

// special is a recorded tree entry that was not written.
type special struct {
	path   string // repo-relative
	kind   string
	target string // link target or submodule commit
}

// tree is a materialized, filtered, rendered, and prepared temporary
// directory that has not been renamed into place yet.
type tree struct {
	dir    string
	record *catalog.RenderRecord
}

// extraction describes one snapshot extraction.
type extraction struct {
	src      *config.Source
	revDir   string // the source's rev directory
	revision string // empty for path sources: computed after filtering
	tmpName  string // os.MkdirTemp pattern
	// populate writes regular files into dir and returns the entries it
	// recorded instead of writing.
	populate func(ctx context.Context, dir string) ([]special, error)
	vars     map[string]string // template vars, "{snapshot}" unexpanded
	env      []string
	getenv   func(string) (string, bool)
	hash     string // RenderContextHash
	// shutdown stops prepare from starting new commands once cancelled.
	shutdown context.Context
	// prepareOnly limits prepare to these bundles; nil runs every bundle.
	prepareOnly map[string]bool
	// baseRecord supplies prepare diagnostics for bundles that skip prepare.
	baseRecord *catalog.RenderRecord
	// renderInPlace renders against the temporary directory itself
	// (verification trees are never renamed into place).
	renderInPlace bool
}

// build runs extraction steps 1–6 in a new temporary directory. On error
// the temporary directory is removed.
func (e *extraction) build(ctx context.Context) (*tree, error) {
	t, err := e.stage(ctx)
	if err != nil {
		return nil, err
	}
	return e.finish(ctx, t)
}

// stage runs extraction steps 1–3 (populate, find bundles, filter) and
// computes the path source revision. On error the directory is removed.
func (e *extraction) stage(ctx context.Context) (t *tree, err error) {
	dir, err := os.MkdirTemp(e.revDir, e.tmpName)
	if err != nil {
		return nil, fmt.Errorf("creating extraction directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = removeTree(dir) // best-effort cleanup of a failed extraction
		}
	}()
	specials, err := e.populate(ctx, dir)
	if err != nil {
		return nil, err
	}
	rec := &catalog.RenderRecord{Revision: e.revision, RenderContext: e.hash, Files: map[string][]string{}}
	bundles, err := findBundles(dir, e.src, specials)
	if err != nil {
		return nil, err
	}
	for _, b := range bundles {
		rec.Bundles = append(rec.Bundles, b.path)
		rec.Diagnostics = append(rec.Diagnostics, b.diags...)
	}
	for _, b := range bundles {
		if b.special {
			continue
		}
		files, diags, err := filterBundle(dir, b.path, e.src)
		if err != nil {
			return nil, err
		}
		rec.Files[b.path] = files
		rec.Diagnostics = append(rec.Diagnostics, diags...)
	}
	if rec.Revision == "" {
		if rec.Revision, err = pathRevision(dir, specials); err != nil {
			return nil, err
		}
	}
	return &tree{dir: dir, record: rec}, nil
}

// finish runs extraction steps 4–6 (render, prepare, permissions) on a
// staged tree. On error the directory is removed.
func (e *extraction) finish(ctx context.Context, t *tree) (_ *tree, err error) {
	defer func() {
		if err != nil {
			_ = removeTree(t.dir) // best-effort cleanup of a failed extraction
		}
	}()
	rec := t.record
	final := filepath.Join(e.revDir, rec.Revision)
	if e.renderInPlace {
		final = t.dir
	}
	if err := e.render(t.dir, final, rec); err != nil {
		return nil, err
	}
	if err := e.prepare(ctx, t.dir, rec); err != nil {
		return nil, err
	}
	for i := range rec.Diagnostics {
		rec.Diagnostics[i].Revision = rec.Revision
	}
	catalog.SortDiagnostics(rec.Diagnostics)
	if err := setPermissions(t.dir); err != nil {
		return nil, err
	}
	return t, nil
}

type foundBundle struct {
	path    string
	special bool
	diags   []catalog.Diagnostic
}

// findBundles lists every directory directly under an expanded root that
// contains SKILL.md, and every special entry directly under a root.
func findBundles(dir string, src *config.Source, specials []special) ([]foundBundle, error) {
	byPath := map[string]*foundBundle{}
	for _, root := range src.AllRootPaths() {
		entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(root)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("listing root %s: %w", root, err)
		}
		for _, ent := range entries {
			bundle := path.Join(root, ent.Name())
			if !ent.IsDir() {
				continue
			}
			info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(bundle), catalog.SkillFile))
			if err == nil && info.Mode().IsRegular() {
				byPath[bundle] = &foundBundle{path: bundle}
			}
		}
		for _, sp := range specials {
			if path.Dir(sp.path) == root {
				byPath[sp.path] = &foundBundle{path: sp.path, special: true}
			}
			if path.Dir(path.Dir(sp.path)) == root && path.Base(sp.path) == catalog.SkillFile {
				if _, ok := byPath[path.Dir(sp.path)]; !ok {
					byPath[path.Dir(sp.path)] = &foundBundle{path: path.Dir(sp.path)}
				}
			}
		}
	}
	for _, sp := range specials {
		for bp, b := range byPath {
			if sp.path != bp && !strings.HasPrefix(sp.path, bp+"/") {
				continue
			}
			code := catalog.CodeSymlink
			if sp.kind == specialSubmodule {
				code = catalog.CodeSubmodule
			}
			rel := strings.TrimPrefix(strings.TrimPrefix(sp.path, bp), "/")
			if rel == "" {
				rel = path.Base(bp)
			}
			b.diags = append(b.diags, catalog.Diagnostic{
				Code: code, Source: src.Name, Bundle: bp, File: rel,
				Message: fmt.Sprintf("%s entries are not served", sp.kind),
			})
		}
	}
	out := make([]foundBundle, 0, len(byPath))
	for _, b := range byPath {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// filterBundle applies the source's ignore file to one bundle, deleting
// excluded files, and returns the remaining source files.
func filterBundle(dir, bundle string, src *config.Source) ([]string, []catalog.Diagnostic, error) {
	base := filepath.Join(dir, filepath.FromSlash(bundle))
	files, err := listFiles(base)
	if err != nil {
		return nil, nil, err
	}
	if src.IgnoreFile == "" {
		return files, nil, nil
	}
	ignoreFiles := map[string][]byte{}
	var keep []string
	for _, f := range files {
		if path.Base(f) == src.IgnoreFile {
			data, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(f)))
			if err != nil {
				return nil, nil, fmt.Errorf("reading %s: %w", f, err)
			}
			ignoreFiles[f] = data
		}
		for _, rule := range src.Prepare {
			if path.Base(f) == rule.WhenFile {
				for _, k := range rule.Keep {
					keep = append(keep, path.Join(path.Dir(f), k))
				}
			}
		}
	}
	rules := catalog.NewIgnoreRules(src.IgnoreFile, ignoreFiles, keep)
	var kept []string
	var diags []catalog.Diagnostic
	for _, f := range files {
		if !rules.Excluded(f) {
			kept = append(kept, f)
			continue
		}
		if f == catalog.SkillFile {
			kept = append(kept, f)
			diags = append(diags, catalog.Diagnostic{
				Code: catalog.CodeMissingSkillMD, Source: src.Name, Bundle: bundle, File: f,
				Message: "ignore rules match SKILL.md",
			})
			continue
		}
		if err := os.Remove(filepath.Join(base, filepath.FromSlash(f))); err != nil {
			return nil, nil, fmt.Errorf("removing ignored file %s: %w", f, err)
		}
	}
	if err := pruneEmptyDirs(base); err != nil {
		return nil, nil, err
	}
	return kept, diags, nil
}

// pruneEmptyDirs removes directories left empty by ignore filtering.
func pruneEmptyDirs(base string) error {
	var dirs []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != base {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("listing %s: %w", base, err)
	}
	for _, d := range slices.Backward(dirs) {
		if entries, err := os.ReadDir(d); err == nil && len(entries) == 0 {
			if err := os.Remove(d); err != nil {
				return fmt.Errorf("removing empty directory: %w", err)
			}
		}
	}
	return nil
}

// listFiles returns every regular file below base as sorted slash paths.
func listFiles(base string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", base, err)
	}
	slices.Sort(files)
	return files, nil
}

// pathRevision hashes every regular file's path, mode, and content plus
// every recorded special entry.
func pathRevision(dir string, specials []special) (string, error) {
	h := sha256.New()
	files, err := listFiles(dir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		info, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(h, "%s\x00%o\x00%x\n", f, info.Mode().Perm(), sum)
	}
	sorted := slices.Clone(specials)
	slices.SortFunc(sorted, func(a, b special) int { return strings.Compare(a.path, b.path) })
	for _, sp := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00%s\n", sp.path, sp.kind, sp.target)
	}
	return "local-" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// render renders every source file of bundles without diagnostics in place.
// Template issues become diagnostics and leave the bundle's files raw.
func (e *extraction) render(dir, final string, rec *catalog.RenderRecord) error {
	vars := map[string]string{}
	for k, v := range e.vars {
		vars[k] = strings.ReplaceAll(v, "{snapshot}", final)
	}
	ctx := catalog.TemplateContext{Vars: vars, Env: e.env, Getenv: e.getenv}
	for _, bundle := range rec.Bundles {
		files, ok := rec.Files[bundle]
		if !ok {
			continue
		}
		rendered := map[string][]byte{}
		var issues []catalog.Diagnostic
		for _, f := range files {
			rel := path.Join(bundle, f)
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return fmt.Errorf("reading %s: %w", rel, err)
			}
			if catalog.IsBinary(data) {
				continue
			}
			out, tplIssues := catalog.Render(data, filepath.Join(final, filepath.FromSlash(rel)), ctx)
			for _, is := range tplIssues {
				line := is.Line
				issues = append(issues, catalog.Diagnostic{
					Code: is.Code, Source: e.src.Name, Bundle: bundle, File: f, Line: &line, Message: is.Message,
				})
			}
			rendered[rel] = out
		}
		rec.Diagnostics = append(rec.Diagnostics, issues...)
		if len(rec.BundleDiagnostics(bundle)) > 0 {
			continue
		}
		for rel, out := range rendered {
			if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), out, 0o600); err != nil {
				return fmt.Errorf("writing %s: %w", rel, err)
			}
		}
	}
	return nil
}

// setPermissions makes the tree read-only: directories 0555, files 0555
// when owner-executable and 0444 otherwise. Links are left unchanged.
func setPermissions(dir string) error {
	var dirs []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			dirs = append(dirs, p)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := fs.FileMode(0o444)
			if info.Mode().Perm()&0o100 != 0 {
				mode = 0o555
			}
			return os.Chmod(p, mode)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	for _, d := range slices.Backward(dirs) {
		if err := os.Chmod(d, 0o555); err != nil {
			return fmt.Errorf("setting permissions: %w", err)
		}
	}
	return nil
}

// finalize writes the RenderRecord and renames the tree into place. With
// replace, an existing snapshot of the same revision is swapped out;
// otherwise an existing snapshot wins and the tree is discarded.
func (t *tree) finalize(revDir string, replace bool) error {
	final := filepath.Join(revDir, t.record.Revision)
	if _, err := os.Lstat(final); err == nil {
		if !replace {
			// Another extraction won; its record already describes it.
			return removeTree(t.dir)
		}
		// Move the old tree aside before writing the new record, so a crash
		// leaves either no tree (re-extracted later) or the new pair.
		old := t.dir + ".old"
		if err := os.Rename(final, old); err != nil {
			_ = removeTree(t.dir) // best-effort cleanup; the rename error is returned
			return fmt.Errorf("moving old snapshot aside: %w", err)
		}
		defer removeTree(old) // the old tree is garbage either way; startup cleanup removes leftovers
	}
	if err := writeJSONAtomic(final+".render.json", t.record); err != nil {
		_ = removeTree(t.dir) // best-effort cleanup; the write error is returned
		return err
	}
	if err := os.Rename(t.dir, final); err != nil {
		return fmt.Errorf("renaming snapshot into place: %w", err)
	}
	return nil
}

// removeTree deletes a read-only tree by restoring owner write permission
// on its directories first.
func removeTree(dir string) error {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // RemoveAll below reports what remains
		}
		if d.IsDir() {
			return os.Chmod(p, 0o755)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("unlocking %s: %w", dir, err)
	}
	return os.RemoveAll(dir)
}

// writeJSONAtomic writes v to path through a synced temporary file.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", filepath.Base(path), err)
	}
	return writeFileAtomic(path, append(data, '\n'))
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

// renderContextHash identifies the context snapshots were rendered with.
func renderContextHash(src *config.Source, getenv func(string) (string, bool)) string {
	env := map[string]*string{}
	for _, name := range src.Template.Env {
		if v, ok := getenv(name); ok {
			env[name] = &v
		} else {
			env[name] = nil
		}
	}
	type prepareRule struct {
		WhenFile string   `json:"when_file"`
		Run      []string `json:"run"`
		Keep     []string `json:"keep"`
		Timeout  string   `json:"timeout"`
	}
	var prepare []prepareRule
	for _, r := range src.Prepare {
		prepare = append(prepare, prepareRule{r.WhenFile, r.Run, r.Keep, r.Timeout.String()})
	}
	data, _ := json.Marshal(struct { // marshaling plain maps and slices cannot fail
		Renderer     int                `json:"renderer"`
		Roots        []config.Root      `json:"roots"`
		HarnessRoots []config.Root      `json:"harness_roots"`
		Vars         map[string]string  `json:"vars"`
		Env          map[string]*string `json:"env"`
		IgnoreFile   string             `json:"ignore_file"`
		Prepare      []prepareRule      `json:"prepare"`
	}{catalog.RendererVersion, src.Roots, src.HarnessRoots, src.Template.Vars, env, src.IgnoreFile, prepare})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}
