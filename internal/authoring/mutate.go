package authoring

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/render"
)

const maxFileBytes = 1 << 20

// FrontmatterEntry is one extra SKILL.md frontmatter key.
type FrontmatterEntry struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

// CreateArgs are skill_create's arguments.
type CreateArgs struct {
	ChangeID    string             `json:"change_id"`
	Root        string             `json:"root"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Frontmatter []FrontmatterEntry `json:"frontmatter,omitempty"`
	Body        string             `json:"body"`
}

// CreateResult is skill_create's response.
type CreateResult struct {
	Bundle     string `json:"bundle"`
	ServedName string `json:"served_name"`
}

// EditArgs are skill_edit's arguments.
type EditArgs struct {
	ChangeID   string `json:"change_id"`
	Bundle     string `json:"bundle"`
	Path       string `json:"path,omitempty"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditResult is skill_edit's response.
type EditResult struct {
	Bundle       string `json:"bundle"`
	Path         string `json:"path"`
	Replacements int    `json:"replacements"`
}

// WriteArgs are skill_write_file's arguments.
type WriteArgs struct {
	ChangeID string `json:"change_id"`
	Bundle   string `json:"bundle"`
	Path     string `json:"path"`
	Content  string `json:"content"`
}

// WriteResult is skill_write_file's response.
type WriteResult struct {
	Bundle string `json:"bundle"`
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
}

// ReadArgs are skill_change_read's arguments.
type ReadArgs struct {
	ChangeID string `json:"change_id"`
	Bundle   string `json:"bundle"`
	Path     string `json:"path,omitempty"`
	Offset   *int   `json:"offset,omitempty"`
	SHA      string `json:"sha,omitempty"`
}

// DiffArgs are skill_change_diff's arguments.
type DiffArgs struct {
	ChangeID string `json:"change_id"`
	Offset   *int   `json:"offset,omitempty"`
	SHA      string `json:"sha,omitempty"`
}

// detached returns a bounded context that survives the request but dies
// with the daemon.
func (s *Service) detached() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.opts.Kill, authoringTimeout)
}

// session acquires an open change and reconciles it for a mutation or read.
func (s *Service) session(reqCtx context.Context, id string) (*change, context.Context, func(), error) {
	c, err := s.lockOpen(reqCtx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := s.detached()
	done := func() { cancel(); c.release() }
	if err := s.reconcile(ctx, c, reconcileNormal); err != nil {
		done()
		return nil, nil, nil, err
	}
	if c.rec.State != StateOpen {
		done()
		return nil, nil, nil, fmt.Errorf("%w: %s is %s", ErrNotOpen, id, c.rec.State)
	}
	return c, ctx, done, nil
}

// Create writes a new skill bundle.
func (s *Service) Create(reqCtx context.Context, a CreateArgs) (CreateResult, error) {
	c, ctx, done, err := s.session(reqCtx, a.ChangeID)
	if err != nil {
		return CreateResult{}, err
	}
	defer done()
	src := s.opts.Config.SourceByName(c.rec.Source)
	roots := src.ExpandedRoots(config.HarnessID(c.rec.Harness))
	if !slices.ContainsFunc(roots, func(r config.Root) bool { return r.Path == a.Root }) {
		return CreateResult{}, fmt.Errorf("root %q is not a root of source %s", a.Root, c.rec.Source)
	}
	if !catalog.NamePattern.MatchString(a.Name) {
		return CreateResult{}, fmt.Errorf("name %q must match %s", a.Name, catalog.NamePattern)
	}
	bundle := path.Join(a.Root, a.Name)
	root, err := os.OpenRoot(s.store.worktreePath(c.rec.ChangeID))
	if err != nil {
		return CreateResult{}, err
	}
	defer root.Close()
	if _, err := root.Lstat(bundle); err == nil {
		return CreateResult{}, fmt.Errorf("bundle %s already exists", bundle)
	}
	content, err := skillDocument(a)
	if err != nil {
		return CreateResult{}, err
	}
	parsed, err := s.checkContent(src, bundle, catalog.SkillFile, content)
	if err != nil {
		return CreateResult{}, err
	}
	if err := s.write(ctx, c, root, bundle, catalog.SkillFile, content); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Bundle: bundle, ServedName: parsed}, nil
}

// skillDocument renders SKILL.md for skill_create.
func skillDocument(a CreateArgs) ([]byte, error) {
	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	m := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		str("name"), str(a.Name), str("description"), str(a.Description),
	}}
	seen := map[string]bool{"name": true, "description": true}
	for _, e := range a.Frontmatter {
		if seen[e.Key] || e.Key == "" {
			return nil, fmt.Errorf("frontmatter key %q is empty, duplicated, or reserved", e.Key)
		}
		seen[e.Key] = true
		var v yaml.Node
		if err := v.Encode(e.Value); err != nil {
			return nil, fmt.Errorf("encoding frontmatter %q: %w", e.Key, err)
		}
		m.Content = append(m.Content, str(e.Key), &v)
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding frontmatter: %w", err)
	}
	return []byte("---\n" + string(out) + "---\n" + a.Body), nil
}

// Edit replaces text in a bundle file.
func (s *Service) Edit(reqCtx context.Context, a EditArgs) (EditResult, error) {
	rel := cmp.Or(a.Path, catalog.SkillFile)
	c, ctx, done, err := s.session(reqCtx, a.ChangeID)
	if err != nil {
		return EditResult{}, err
	}
	defer done()
	root, src, err := s.openBundle(c, a.Bundle)
	if err != nil {
		return EditResult{}, err
	}
	defer root.Close()
	if err := checkRelPath(rel); err != nil {
		return EditResult{}, err
	}
	current, err := root.ReadFile(path.Join(a.Bundle, rel))
	if err != nil {
		return EditResult{}, fmt.Errorf("reading %s: %w", rel, err)
	}
	if a.OldString == "" {
		return EditResult{}, errors.New("old_string must not be empty")
	}
	n := strings.Count(string(current), a.OldString)
	switch {
	case n == 0:
		return EditResult{}, fmt.Errorf("old_string not found in %s", rel)
	case n > 1 && !a.ReplaceAll:
		return EditResult{}, fmt.Errorf("old_string occurs %d times in %s; pass replace_all or a longer old_string", n, rel)
	case !a.ReplaceAll:
		n = 1
	}
	updated := []byte(strings.Replace(string(current), a.OldString, a.NewString, n))
	if _, err := s.checkContent(src, a.Bundle, rel, updated); err != nil {
		return EditResult{}, err
	}
	if err := s.write(ctx, c, root, a.Bundle, rel, updated); err != nil {
		return EditResult{}, err
	}
	return EditResult{Bundle: a.Bundle, Path: rel, Replacements: n}, nil
}

// WriteFile adds or replaces a bundle file.
func (s *Service) WriteFile(reqCtx context.Context, a WriteArgs) (WriteResult, error) {
	c, ctx, done, err := s.session(reqCtx, a.ChangeID)
	if err != nil {
		return WriteResult{}, err
	}
	defer done()
	root, src, err := s.openBundle(c, a.Bundle)
	if err != nil {
		return WriteResult{}, err
	}
	defer root.Close()
	if err := checkRelPath(a.Path); err != nil {
		return WriteResult{}, err
	}
	content := []byte(a.Content)
	if _, err := s.checkContent(src, a.Bundle, a.Path, content); err != nil {
		return WriteResult{}, err
	}
	if err := s.write(ctx, c, root, a.Bundle, a.Path, content); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Bundle: a.Bundle, Path: a.Path, Bytes: len(content)}, nil
}

// Read returns raw source text from the worktree, chunked.
func (s *Service) Read(reqCtx context.Context, a ReadArgs, budget int) (string, error) {
	rel := cmp.Or(a.Path, catalog.SkillFile)
	c, _, done, err := s.session(reqCtx, a.ChangeID)
	if err != nil {
		return "", err
	}
	defer done()
	root, _, err := s.openBundle(c, a.Bundle)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := checkRelPath(rel); err != nil {
		return "", err
	}
	content, err := root.ReadFile(path.Join(a.Bundle, rel))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", rel, err)
	}
	attrs := []render.Attr{{Name: "change_id", Value: a.ChangeID}, {Name: "bundle", Value: a.Bundle}, {Name: "path", Value: rel}}
	return chunkPinned("skill_source", attrs, content, a.Offset, a.SHA, budget)
}

// Diff returns the staged diff against the base revision, chunked.
func (s *Service) Diff(reqCtx context.Context, a DiffArgs, budget int) (string, error) {
	c, ctx, done, err := s.session(reqCtx, a.ChangeID)
	if err != nil {
		return "", err
	}
	defer done()
	out, err := s.git(ctx, c.rec, gitx.Opts{}, "diff", "--cached", "-M", "--no-ext-diff", "--no-textconv", "--no-color", c.rec.BaseRevision)
	if err != nil {
		return "", fmt.Errorf("diffing change: %w", err)
	}
	return chunkPinned("skill_change_diff", []render.Attr{{Name: "change_id", Value: a.ChangeID}}, out, a.Offset, a.SHA, budget)
}

// chunkPinned chunks content identified by its hash; a continuation whose
// hash no longer matches must restart at offset 0.
func chunkPinned(element string, attrs []render.Attr, content []byte, offset *int, pin string, budget int) (string, error) {
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])[:12]
	start := 0
	switch {
	case (offset == nil) != (pin == ""):
		return "", errors.New("offset and sha must be given together")
	case offset != nil && pin != sha:
		return "", errors.New("the content changed since it was read; read again without offset and sha")
	case offset != nil:
		start = *offset
	}
	return render.Chunk{
		Element: element, Attrs: append(attrs, render.Attr{Name: "sha", Value: sha}),
		Content: content, PinName: "sha", PinValue: sha, Offset: start, Budget: budget,
	}.Render()
}

// openBundle opens the worktree root and enforces the bundle-directory rule.
func (s *Service) openBundle(c *change, bundle string) (*os.Root, *config.Source, error) {
	src := s.opts.Config.SourceByName(c.rec.Source)
	if !bundleInRoots(src, config.HarnessID(c.rec.Harness), bundle) {
		return nil, nil, fmt.Errorf("bundle %q is not <root>/<name> for a root of source %s", bundle, c.rec.Source)
	}
	root, err := os.OpenRoot(s.store.worktreePath(c.rec.ChangeID))
	if err != nil {
		return nil, nil, err
	}
	if info, err := root.Lstat(path.Join(bundle, catalog.SkillFile)); err != nil || !info.Mode().IsRegular() {
		root.Close()
		return nil, nil, fmt.Errorf("bundle %s has no SKILL.md", bundle)
	}
	return root, src, nil
}

func bundleInRoots(src *config.Source, h config.HarnessID, bundle string) bool {
	if bundle != path.Clean(bundle) || strings.HasPrefix(bundle, "/") {
		return false
	}
	return slices.ContainsFunc(src.ExpandedRoots(h), func(r config.Root) bool {
		name := strings.TrimPrefix(bundle, r.Path+"/")
		return name != bundle && catalog.NamePattern.MatchString(name)
	})
}

// checkRelPath enforces bundle-relative path rules.
func checkRelPath(rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") || rel != path.Clean(rel) {
		return fmt.Errorf("path %q must be a clean relative path", rel)
	}
	for part := range strings.SplitSeq(rel, "/") {
		switch part {
		case "..", ".git":
			return fmt.Errorf("path %q must not contain %q", rel, part)
		case ".gitattributes":
			return fmt.Errorf("path %q: .gitattributes files cannot be written", rel)
		}
	}
	return nil
}

// checkContent enforces content rules and, for SKILL.md, structural
// validity. It returns the served name for SKILL.md.
func (s *Service) checkContent(src *config.Source, bundle, rel string, content []byte) (string, error) {
	switch {
	case len(content) > maxFileBytes:
		return "", fmt.Errorf("%s exceeds 1 MiB", rel)
	case !utf8.Valid(content):
		return "", fmt.Errorf("%s is not valid UTF-8", rel)
	case bytes.IndexByte(content, 0) >= 0:
		return "", fmt.Errorf("%s contains a NUL byte", rel)
	}
	if rel != catalog.SkillFile {
		return "", nil
	}
	parsed, issues := catalog.ParseSkill(content, path.Base(bundle), catalog.PrefixRule{Enabled: src.AuthorPrefix, Except: src.AuthorPrefixExcept})
	if len(issues) > 0 {
		var msgs []string
		for _, is := range issues {
			msgs = append(msgs, is.Code+": "+is.Message)
		}
		return "", fmt.Errorf("SKILL.md would be invalid: %s", strings.Join(msgs, "; "))
	}
	return parsed.ServedName, nil
}

// write records the bundle as touched, writes the file atomically through
// the worktree root, and stages it. A failed stage undoes the write.
func (s *Service) write(ctx context.Context, c *change, root *os.Root, bundle, rel string, content []byte) error {
	rec := c.rec
	repoPath := path.Join(bundle, rel)
	if err := s.checkIgnored(ctx, rec, repoPath); err != nil {
		return err
	}
	if touchBundle(rec, bundle) {
		if err := s.store.persist(rec); err != nil {
			return err
		}
	}
	if rec.Commit != nil {
		if err := s.resetToBase(ctx, rec); err != nil {
			return err
		}
	}
	previous, existed, err := writeAtomic(root, repoPath, content)
	if err != nil {
		return err
	}
	if _, err := s.git(ctx, rec, gitx.Opts{}, "add", "--", repoPath); err != nil {
		// Undo without git: the index may be the reason staging failed.
		var undoErr error
		if existed {
			_, _, undoErr = writeAtomic(root, repoPath, previous)
		} else {
			undoErr = root.Remove(repoPath)
		}
		return errors.Join(fmt.Errorf("staging %s: %w", repoPath, err), undoErr)
	}
	rec.UpdatedAt = s.store.timestamp()
	return s.store.persist(rec)
}

func (s *Service) checkIgnored(ctx context.Context, rec *Record, repoPath string) error {
	_, err := s.git(ctx, rec, gitx.Opts{Stdin: strings.NewReader(repoPath + "\x00")}, "check-ignore", "--stdin", "-z", "-q")
	switch {
	case err == nil:
		return fmt.Errorf("%s is ignored by git and cannot be written", repoPath)
	case gitx.ExitCode(err) == 1:
		return nil
	}
	return fmt.Errorf("checking ignore rules: %w", err)
}

// writeAtomic writes through a temporary file in the target directory,
// keeping an existing file's permission bits (0644 for new files). It
// returns the previous content when the file existed.
func writeAtomic(root *os.Root, rel string, content []byte) ([]byte, bool, error) {
	mode := fs.FileMode(0o644)
	var previous []byte
	info, err := root.Lstat(rel)
	existed := err == nil
	if existed {
		if !info.Mode().IsRegular() {
			return nil, false, fmt.Errorf("%s is not a regular file", rel)
		}
		mode = info.Mode().Perm()
		if previous, err = root.ReadFile(rel); err != nil {
			return nil, false, fmt.Errorf("reading %s: %w", rel, err)
		}
	}
	dir := path.Dir(rel)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return nil, false, fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp := path.Join(dir, "."+path.Base(rel)+".skills-mcp-tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return nil, false, fmt.Errorf("writing %s: %w", rel, err)
	}
	_, writeErr := f.Write(content)
	if err := errors.Join(writeErr, f.Chmod(mode), f.Close()); err != nil {
		_ = root.Remove(tmp) // best-effort cleanup; the write error is returned
		return nil, false, fmt.Errorf("writing %s: %w", rel, err)
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp) // best-effort cleanup; the rename error is returned
		return nil, false, fmt.Errorf("writing %s: %w", rel, err)
	}
	return previous, existed, nil
}
