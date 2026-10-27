package catalog

import (
	"cmp"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sync"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

// Snapshot is one source's extracted revision.
type Snapshot struct {
	Source   *config.Source
	Revision string
	Root     string // absolute snapshot directory
	Record   *RenderRecord
}

// Skill is a served skill.
type Skill struct {
	Name        string // served name
	SkillName   string // frontmatter name and bundle directory name
	Description string
	Invocation  config.Invocation
	Source      string
	Revision    string
	Bundle      string // repo-relative bundle directory
	BaseDir     string // absolute snapshot directory of the bundle
	Body        string // rendered SKILL.md without frontmatter
	Files       []string
}

// Shadow records a skill hidden by an earlier source.
type Shadow struct {
	Name, Winner, Loser string
}

// View is the set of skills one harness is served.
type View struct {
	Harness     config.HarnessID
	Skills      []*Skill // sorted by name
	Diagnostics []Diagnostic
	Revisions   map[string]string // source name → revision the view was built from
	Shadowed    []Shadow
}

// Skill returns the named skill or nil.
func (v *View) Skill(name string) *Skill {
	i, ok := slices.BinarySearchFunc(v.Skills, name, func(s *Skill, n string) int { return cmp.Compare(s.Name, n) })
	if !ok {
		return nil
	}
	return v.Skills[i]
}

// parsedBundle is the harness-independent parse of one bundle.
type parsedBundle struct {
	key   string
	skill *Skill
	diags []Diagnostic
}

type cacheKey struct{ source, revision string }

// Cache holds parsed bundles per (source, revision). It is safe for
// concurrent use.
type Cache struct {
	mu      sync.Mutex
	entries map[cacheKey]map[string]*parsedBundle
}

// Drop forgets one revision.
func (c *Cache) Drop(source, revision string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, cacheKey{source, revision})
}

func (c *Cache) parsed(snap Snapshot) (map[string]*parsedBundle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cacheKey{snap.Source.Name, snap.Revision}
	if got, ok := c.entries[key]; ok {
		return got, nil
	}
	got, err := ParseSnapshot(snap)
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = map[cacheKey]map[string]*parsedBundle{}
	}
	c.entries[key] = got
	return got, nil
}

// ParseSnapshot parses every bundle of a snapshot without caching.
func ParseSnapshot(snap Snapshot) (map[string]*parsedBundle, error) {
	rule := PrefixRule{Enabled: snap.Source.AuthorPrefix, Except: snap.Source.AuthorPrefixExcept}
	out := map[string]*parsedBundle{}
	for _, bundle := range snap.Record.Bundles {
		pb, err := parseBundle(snap, bundle, rule)
		if err != nil {
			return nil, err
		}
		out[bundle] = pb
	}
	return out, nil
}

func parseBundle(snap Snapshot, bundle string, rule PrefixRule) (*parsedBundle, error) {
	dirName := path.Base(bundle)
	pb := &parsedBundle{key: dirName, diags: snap.Record.BundleDiagnostics(bundle)}
	baseDir := filepath.Join(snap.Root, filepath.FromSlash(bundle))
	content, err := os.ReadFile(filepath.Join(baseDir, SkillFile))
	if err != nil {
		if len(pb.diags) > 0 {
			return pb, nil // a symlink or submodule entry has no SKILL.md to read
		}
		return nil, fmt.Errorf("reading %s/%s: %w", bundle, SkillFile, err)
	}
	parsed, issues := ParseSkill(content, dirName, rule)
	pb.key = parsed.ServedName
	for _, is := range issues {
		pb.diags = append(pb.diags, Diagnostic{
			Code: is.Code, Source: snap.Source.Name, Revision: snap.Revision,
			Bundle: bundle, File: SkillFile, Message: is.Message,
		})
	}
	if len(pb.diags) > 0 {
		return pb, nil
	}
	files := slices.DeleteFunc(slices.Clone(snap.Record.Files[bundle]), func(f string) bool { return f == SkillFile })
	pb.skill = &Skill{
		Name:        parsed.ServedName,
		SkillName:   dirName,
		Description: parsed.Frontmatter.Description,
		Source:      snap.Source.Name,
		Revision:    snap.Revision,
		Bundle:      bundle,
		BaseDir:     baseDir,
		Body:        parsed.Body,
		Files:       files,
	}
	return pb, nil
}

type candidate struct {
	pb         *parsedBundle
	invocation config.Invocation
	bundle     string
}

// Views builds every harness view from the snapshots, in source order.
func (c *Cache) Views(snaps []Snapshot) (map[config.HarnessID]*View, error) {
	parsed := make([]map[string]*parsedBundle, len(snaps))
	for i, snap := range snaps {
		p, err := c.parsed(snap)
		if err != nil {
			return nil, fmt.Errorf("source %s: %w", snap.Source.Name, err)
		}
		parsed[i] = p
	}
	views := map[config.HarnessID]*View{}
	for _, h := range config.Harnesses {
		views[h] = buildView(h, snaps, parsed)
	}
	return views, nil
}

func buildView(h config.HarnessID, snaps []Snapshot, parsed []map[string]*parsedBundle) *View {
	v := &View{Harness: h, Revisions: map[string]string{}}
	winners := map[string]string{} // key → source name
	for i, snap := range snaps {
		v.Revisions[snap.Source.Name] = snap.Revision
		for key, cand := range sourceCandidates(h, snap, parsed[i]) {
			if winner, taken := winners[key]; taken {
				v.Shadowed = append(v.Shadowed, Shadow{Name: key, Winner: winner, Loser: snap.Source.Name})
				continue
			}
			winners[key] = snap.Source.Name
			if len(cand.diags) > 0 {
				v.Diagnostics = append(v.Diagnostics, cand.diags...)
				continue
			}
			s := *cand.skill
			s.Invocation = cand.invocation
			v.Skills = append(v.Skills, &s)
		}
	}
	slices.SortFunc(v.Skills, func(a, b *Skill) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(v.Shadowed, func(a, b Shadow) int { return cmp.Compare(a.Name, b.Name) })
	SortDiagnostics(v.Diagnostics)
	return v
}

type resolved struct {
	skill      *Skill
	diags      []Diagnostic
	invocation config.Invocation
}

// sourceCandidates resolves one source's bundles for a harness: duplicates
// within roots or within harness roots refuse every copy, and a harness-root
// bundle replaces the roots bundle with the same key.
func sourceCandidates(h config.HarnessID, snap Snapshot, parsed map[string]*parsedBundle) map[string]resolved {
	collect := func(roots []config.Root) map[string][]candidate {
		byKey := map[string][]candidate{}
		for _, r := range roots {
			for _, bundle := range snap.Record.Bundles {
				if path.Dir(bundle) != r.Path {
					continue
				}
				pb := parsed[bundle]
				byKey[pb.key] = append(byKey[pb.key], candidate{pb: pb, invocation: r.Invocation, bundle: bundle})
			}
		}
		return byKey
	}
	var harnessRoots []config.Root
	for _, r := range snap.Source.HarnessRoots {
		harnessRoots = append(harnessRoots, config.Root{Path: config.ExpandHarness(r.Path, h), Invocation: r.Invocation})
	}
	out := map[string]resolved{}
	for _, group := range []map[string][]candidate{collect(snap.Source.Roots), collect(harnessRoots)} {
		for key, cands := range group {
			out[key] = resolve(snap, key, cands)
		}
	}
	return out
}

func resolve(snap Snapshot, key string, cands []candidate) resolved {
	if len(cands) == 1 {
		c := cands[0]
		return resolved{skill: c.pb.skill, diags: c.pb.diags, invocation: c.invocation}
	}
	var diags []Diagnostic
	for _, c := range cands {
		diags = append(diags, c.pb.diags...)
		diags = append(diags, Diagnostic{
			Code: CodeDuplicateName, Source: snap.Source.Name, Revision: snap.Revision,
			Bundle: c.bundle, File: SkillFile, Message: fmt.Sprintf("served name %q is used by %d bundles", key, len(cands)),
		})
	}
	return resolved{diags: diags}
}
