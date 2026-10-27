package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

type fixtureBundle struct {
	path    string // repo-relative bundle directory
	content string // SKILL.md; empty means no SKILL.md (symlink entry)
	files   []string
}

func skill(name, description string, extra ...string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n" + strings.Join(extra, "") + "---\nbody of " + name + "\n"
}

func newSnapshot(t *testing.T, src *config.Source, revision string, bundles []fixtureBundle, diags ...Diagnostic) Snapshot {
	t.Helper()
	root := t.TempDir()
	rec := &RenderRecord{Revision: revision, Files: map[string][]string{}, Diagnostics: diags}
	for _, b := range bundles {
		rec.Bundles = append(rec.Bundles, b.path)
		dir := filepath.Join(root, filepath.FromSlash(b.path))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if b.content != "" {
			if err := os.WriteFile(filepath.Join(dir, SkillFile), []byte(b.content), 0o644); err != nil {
				t.Fatal(err)
			}
			rec.Files[b.path] = append([]string{SkillFile}, b.files...)
		}
	}
	return Snapshot{Source: src, Revision: revision, Root: root, Record: rec}
}

func registrySource(name string) *config.Source {
	return &config.Source{
		Name: name,
		Roots: []config.Root{
			{Path: "skills/agent", Invocation: config.Model},
			{Path: "skills/user", Invocation: config.User},
		},
		HarnessRoots: []config.Root{{Path: "harnesses/{harness}/skills", Invocation: config.Model}},
		AuthorPrefix: true, AuthorPrefixExcept: []string{"owner"},
	}
}

func names(v *View) string {
	var out []string
	for _, s := range v.Skills {
		out = append(out, s.Name+":"+string(s.Invocation)+":"+s.Source)
	}
	return strings.Join(out, ",")
}

func codes(ds []Diagnostic) string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Code+"@"+d.Bundle)
	}
	return strings.Join(out, ",")
}

func TestViews(t *testing.T) {
	src := registrySource("reg")
	snap := newSnapshot(t, src, "rev1", []fixtureBundle{
		{path: "skills/agent/golang", content: skill("golang", "Go"), files: []string{"references/a.md"}},
		{path: "skills/agent/grilling", content: skill("grilling", "Grill", "author: mattpocock\n")},
		{path: "skills/user/commit", content: skill("commit", "Commit")},
		{path: "skills/agent/broken", content: "no frontmatter"},
		{path: "skills/agent/linked"},
		{path: "harnesses/pi/skills/golang", content: skill("golang", "Go for pi")},
		{path: "harnesses/codex/skills/codex-only", content: skill("codex-only", "C")},
		{path: "other/ignored", content: skill("ignored", "I")},
	}, Diagnostic{Code: CodeSymlink, Source: "reg", Revision: "rev1", Bundle: "skills/agent/linked", File: "linked", Message: "symlink"})

	var cache Cache
	views := cache.Views([]Snapshot{snap})
	tests := []struct {
		harness config.HarnessID
		skills  string
		diags   string
	}{
		{config.ClaudeCode, "commit:user:reg,golang:model:reg,mattpocock-grilling:model:reg", "invalid_frontmatter@skills/agent/broken,symlink@skills/agent/linked"},
		{config.Codex, "codex-only:model:reg,commit:user:reg,golang:model:reg,mattpocock-grilling:model:reg", "invalid_frontmatter@skills/agent/broken,symlink@skills/agent/linked"},
		{config.Pi, "commit:user:reg,golang:model:reg,mattpocock-grilling:model:reg", "invalid_frontmatter@skills/agent/broken,symlink@skills/agent/linked"},
	}
	for _, tt := range tests {
		v := views[tt.harness]
		if got := names(v); got != tt.skills {
			t.Errorf("%s skills = %s, want %s", tt.harness, got, tt.skills)
		}
		if got := codes(v.Diagnostics); got != tt.diags {
			t.Errorf("%s diags = %s, want %s", tt.harness, got, tt.diags)
		}
		if v.Revisions["reg"] != "rev1" {
			t.Errorf("revision = %q", v.Revisions["reg"])
		}
	}
	pi := views[config.Pi].Skill("golang")
	if pi.Description != "Go for pi" || pi.Bundle != "harnesses/pi/skills/golang" {
		t.Fatalf("pi golang = %+v", pi)
	}
	cc := views[config.ClaudeCode].Skill("golang")
	if cc.Body != "body of golang\n" || strings.Join(cc.Files, ",") != "references/a.md" || cc.BaseDir != filepath.Join(snap.Root, "skills/agent/golang") {
		t.Fatalf("claude golang = %+v", cc)
	}
	if views[config.ClaudeCode].Skill("missing") != nil {
		t.Fatal("Skill returned a missing skill")
	}
}

func TestViewsDuplicatesAndPrecedence(t *testing.T) {
	first := registrySource("first")
	second := registrySource("second")
	a := newSnapshot(t, first, "a1", []fixtureBundle{
		{path: "skills/agent/dup", content: skill("dup", "one")},
		{path: "skills/user/dup", content: skill("dup", "two")},
		{path: "skills/agent/refused", content: skill("refused", "")},
		{path: "skills/agent/kept", content: skill("kept", "first")},
	})
	b := newSnapshot(t, second, "b1", []fixtureBundle{
		{path: "skills/agent/refused", content: skill("refused", "would be valid")},
		{path: "skills/agent/kept", content: skill("kept", "second")},
		{path: "skills/agent/unique", content: skill("unique", "u")},
		{path: "skills/agent/dup", content: skill("dup", "three")},
	})
	var cache Cache
	views := cache.Views([]Snapshot{a, b})
	v := views[config.ClaudeCode]
	if got := names(v); got != "kept:model:first,unique:model:second" {
		t.Fatalf("skills = %s", got)
	}
	if got := codes(v.Diagnostics); got != "duplicate_name@skills/agent/dup,invalid_description@skills/agent/refused,duplicate_name@skills/user/dup" {
		t.Fatalf("diags = %s", got)
	}
	if v.Skill("kept").Description != "first" {
		t.Fatal("later source won")
	}
	if len(v.Shadowed) != 3 {
		t.Fatalf("shadowed = %+v", v.Shadowed)
	}
}

func TestViewsHarnessRootReplacesRefused(t *testing.T) {
	src := registrySource("reg")
	snap := newSnapshot(t, src, "r", []fixtureBundle{
		{path: "skills/agent/tool", content: skill("tool", "generic")},
		{path: "harnesses/pi/skills/tool", content: skill("tool", "")},
	})
	var cache Cache
	views := cache.Views([]Snapshot{snap})
	if views[config.Pi].Skill("tool") != nil || codes(views[config.Pi].Diagnostics) != "invalid_description@harnesses/pi/skills/tool" {
		t.Fatalf("pi view = %s / %s", names(views[config.Pi]), codes(views[config.Pi].Diagnostics))
	}
	if views[config.Codex].Skill("tool") == nil {
		t.Fatal("codex lost the generic skill")
	}
}

func TestCacheReuseAndDrop(t *testing.T) {
	src := registrySource("reg")
	snap := newSnapshot(t, src, "r", []fixtureBundle{{path: "skills/agent/a", content: skill("a", "first")}})
	var cache Cache
	cache.Views([]Snapshot{snap})
	if err := os.WriteFile(filepath.Join(snap.Root, "skills/agent/a", SkillFile), []byte(skill("a", "second")), 0o644); err != nil {
		t.Fatal(err)
	}
	views := cache.Views([]Snapshot{snap})
	if views[config.ClaudeCode].Skill("a").Description != "first" {
		t.Fatal("cache not reused")
	}
	cache.Drop("reg", "r")
	views = cache.Views([]Snapshot{snap})
	if views[config.ClaudeCode].Skill("a").Description != "second" {
		t.Fatal("Drop did not invalidate")
	}
}

func TestViewsMissingSkillFile(t *testing.T) {
	src := registrySource("reg")
	snap := newSnapshot(t, src, "r", []fixtureBundle{
		{path: "skills/agent/gone"},
		{path: "skills/agent/ok", content: skill("ok", "fine")},
	})
	var cache Cache
	v := cache.Views([]Snapshot{snap})[config.ClaudeCode]
	if v.Skill("ok") == nil || codes(v.Diagnostics) != "missing_skill_md@skills/agent/gone" {
		t.Fatalf("skills = %s, diagnostics = %s", names(v), codes(v.Diagnostics))
	}
}

func TestDiagnosticString(t *testing.T) {
	line := 12
	d := Diagnostic{Source: "s", Bundle: "skills/a", File: "SKILL.md", Line: &line, Message: "bad"}
	if d.String() != "s:skills/a/SKILL.md:12 bad" {
		t.Fatalf("String = %q", d.String())
	}
	d.Line = nil
	if d.String() != "s:skills/a/SKILL.md bad" {
		t.Fatalf("String = %q", d.String())
	}
}

func TestSortDiagnostics(t *testing.T) {
	two, one := 2, 1
	ds := []Diagnostic{
		{Source: "b", Bundle: "x"},
		{Source: "a", Bundle: "y", File: "f", Line: &two},
		{Source: "a", Bundle: "y", File: "f", Line: &one},
		{Source: "a", Bundle: "y", File: "f"},
	}
	SortDiagnostics(ds)
	if ds[0].Line != nil || *ds[1].Line != 1 || *ds[2].Line != 2 || ds[3].Source != "b" {
		t.Fatalf("order = %+v", ds)
	}
}

func TestDiffViews(t *testing.T) {
	mk := func(skills ...*Skill) *View { return &View{Skills: skills} }
	tool := func(name, desc string) *Skill { return &Skill{Name: name, Description: desc, Invocation: config.Model} }
	prompt := func(name, desc string) *Skill { return &Skill{Name: name, Description: desc, Invocation: config.User} }

	prev := mk(tool("a", "A"), tool("b", "B"), prompt("p", "P"), tool("moves", "M"))
	next := mk(tool("a", "A"), tool("b", "B2"), tool("c", "C"), prompt("moves", "M"))
	d := DiffViews(prev, next)
	if got := skillNames(d.AddTools); got != "b,c" {
		t.Errorf("AddTools = %s", got)
	}
	if got := strings.Join(d.RemoveTools, ","); got != "moves" {
		t.Errorf("RemoveTools = %s", got)
	}
	if got := skillNames(d.AddPrompts); got != "moves" {
		t.Errorf("AddPrompts = %s", got)
	}
	if got := strings.Join(d.RemovePrompts, ","); got != "p" {
		t.Errorf("RemovePrompts = %s", got)
	}
	if d.Empty() {
		t.Error("diff reported empty")
	}
	bodyOnly := mk(&Skill{Name: "a", Description: "A", Invocation: config.Model, Body: "changed"}, tool("b", "B"), prompt("p", "P"), tool("moves", "M"))
	if !DiffViews(prev, bodyOnly).Empty() {
		t.Error("body-only change produced a diff")
	}
	if got := skillNames(DiffViews(nil, prev).AddTools); got != "a,b,moves" {
		t.Errorf("from nil AddTools = %s", got)
	}
}

func skillNames(ss []*Skill) string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestRenderRecordBundleDiagnostics(t *testing.T) {
	r := &RenderRecord{Diagnostics: []Diagnostic{{Bundle: "a"}, {Bundle: "b"}, {Bundle: "a"}}}
	if len(r.BundleDiagnostics("a")) != 2 || len(r.BundleDiagnostics("c")) != 0 {
		t.Fatal("BundleDiagnostics filtered wrong")
	}
}
