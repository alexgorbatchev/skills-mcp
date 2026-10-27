package authoring

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func TestCreateEditSubmit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()

	created, err := e.svc.Create(ctx, CreateArgs{
		ChangeID: id, Root: "skills/agent", Name: "fresh", Description: "A new skill: does things",
		Frontmatter: []FrontmatterEntry{{Key: "author", Value: "owner"}, {Key: "metadata", Value: map[string]any{"status": "current"}}},
		Body:        "# Fresh\nuse it\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Bundle != "skills/agent/fresh" || created.ServedName != "fresh" {
		t.Fatalf("created = %+v", created)
	}
	if _, err := e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: created.Bundle, Path: "references/r.md", Content: "reference\n"}); err != nil {
		t.Fatal(err)
	}
	edited, err := e.svc.Edit(ctx, EditArgs{ChangeID: id, Bundle: created.Bundle, OldString: "use it", NewString: "use it well"})
	if err != nil || edited.Replacements != 1 {
		t.Fatalf("edit = %+v, %v", edited, err)
	}
	diff, err := e.svc.Diff(ctx, DiffArgs{ChangeID: id}, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+use it well") || !strings.Contains(diff, "references/r.md") {
		t.Fatalf("diff lacks new files:\n%s", diff)
	}
	read, err := e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: created.Bundle}, 100000)
	if err != nil || !strings.Contains(read, "author: owner") || !strings.Contains(read, "description: 'A new skill: does things'") {
		t.Fatalf("read = %s, %v", read, err)
	}

	res, err := e.submit(id, "feat(skills): add fresh")
	if err != nil {
		t.Fatal(err)
	}
	if res.Publication == nil || res.Publication.Attempts != 1 || res.Publication.Commit != e.remote.tip() {
		t.Fatalf("result = %+v", res)
	}
	if got := e.remote.show(res.Commit, "skills/agent/fresh/references/r.md"); got != "reference" {
		t.Fatalf("pushed file = %q", got)
	}
	msg := run(t, e.remote.bare, "log", "-1", "--format=%B", res.Commit)
	if !strings.Contains(msg, "Skills-MCP-Change: "+id) {
		t.Fatalf("message lacks trailer:\n%s", msg)
	}
	if e.view(config.ClaudeCode).Skill("fresh") == nil {
		t.Fatal("pushed skill not live when submit returned")
	}
	rec := e.record(id)
	if rec.State != StateSubmitted {
		t.Fatalf("state = %s", rec.State)
	}
	if _, err := os.Stat(e.worktree(id)); err == nil {
		t.Fatal("worktree not removed")
	}
	if out := run(t, e.src.Repo("reg").Dir, "branch", "--list", "skills-mcp/change/*"); out != "" {
		t.Fatalf("change branch left: %s", out)
	}
	// Submitting again returns the recorded result.
	again, err := e.submit(id, "ignored")
	if err != nil || again.Commit != res.Commit {
		t.Fatalf("resubmit = %+v, %v", again, err)
	}
	if _, err := e.svc.Edit(ctx, EditArgs{ChangeID: id, Bundle: created.Bundle, OldString: "x", NewString: "y"}); err == nil {
		t.Fatal("edit on a submitted change succeeded")
	}
}

func TestEditExistingAndReadChunks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()
	b := "skills/agent/golang"
	long := strings.Repeat("line of reference text\n", 200)
	if _, err := e.svc.WriteFile(ctx, WriteArgs{ChangeID: id, Bundle: b, Path: "references/long.md", Content: long}); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	args := ReadArgs{ChangeID: id, Bundle: b, Path: "references/long.md"}
	for range 100 {
		out, err := e.svc.Read(ctx, args, 1500)
		if err != nil {
			t.Fatal(err)
		}
		start := strings.Index(out, "\n") + 1
		end := strings.Index(out, "\n</skill_source>")
		got.WriteString(out[start:end])
		m := contPattern.FindStringSubmatch(out)
		if m == nil {
			break
		}
		args.SHA = m[1]
		off := atoi(t, m[2])
		args.Offset = &off
	}
	if got.String() != long {
		t.Fatalf("reassembled %d bytes, want %d", got.Len(), len(long))
	}

	// A stale continuation is rejected after the content changes.
	if _, err := e.svc.Edit(ctx, EditArgs{ChangeID: id, Bundle: b, Path: "references/long.md", OldString: "line of", NewString: "LINE OF", ReplaceAll: true}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Read(ctx, args, 1500)
	mustErr(t, err, "content changed")
	mustErr(t, errOf(e.svc.Read(ctx, ReadArgs{ChangeID: id, Bundle: b, SHA: "abc"}, 1500)), "offset and sha must be given together")

	tests := []struct {
		name string
		args EditArgs
		want string
	}{
		{"not found", EditArgs{ChangeID: id, Bundle: b, OldString: "absent", NewString: "x"}, "not found"},
		{"ambiguous", EditArgs{ChangeID: id, Bundle: b, Path: "references/long.md", OldString: "LINE OF", NewString: "x"}, "occurs 200 times"},
		{"empty old", EditArgs{ChangeID: id, Bundle: b, OldString: "", NewString: "x"}, "must not be empty"},
		{"missing file", EditArgs{ChangeID: id, Bundle: b, Path: "nope.md", OldString: "a", NewString: "b"}, "reading nope.md"},
		{"breaks SKILL.md", EditArgs{ChangeID: id, Bundle: b, OldString: "name: golang", NewString: "name: other"}, "name_mismatch"},
		{"bundle outside roots", EditArgs{ChangeID: id, Bundle: "docs/golang", OldString: "a", NewString: "b"}, "is not <root>/<name>"},
		{"bundle without SKILL.md", EditArgs{ChangeID: id, Bundle: "skills/agent/nothing", OldString: "a", NewString: "b"}, "has no SKILL.md"},
		{"traversal", EditArgs{ChangeID: id, Bundle: b, Path: "../commit/SKILL.md", OldString: "a", NewString: "b"}, `must not contain ".."`},
		{"git dir", EditArgs{ChangeID: id, Bundle: b, Path: ".git/config", OldString: "a", NewString: "b"}, `must not contain ".git"`},
		{"unknown change", EditArgs{ChangeID: "chg_0000000000000000", Bundle: b, OldString: "a", NewString: "b"}, "unknown change"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.Edit(ctx, tt.args)
			mustErr(t, err, tt.want)
		})
	}

	writes := []struct {
		name string
		args WriteArgs
		want string
	}{
		{"gitattributes", WriteArgs{ChangeID: id, Bundle: b, Path: ".gitattributes", Content: "* diff=x"}, ".gitattributes files cannot be written"},
		{"nul", WriteArgs{ChangeID: id, Bundle: b, Path: "a.bin", Content: "a\x00b"}, "NUL byte"},
		{"invalid utf8", WriteArgs{ChangeID: id, Bundle: b, Path: "a.txt", Content: "\xff"}, "not valid UTF-8"},
		{"too large", WriteArgs{ChangeID: id, Bundle: b, Path: "big.txt", Content: strings.Repeat("a", maxFileBytes+1)}, "exceeds 1 MiB"},
		{"ignored", WriteArgs{ChangeID: id, Bundle: b, Path: "debug.log", Content: "x"}, "ignored by git"},
		{"absolute", WriteArgs{ChangeID: id, Bundle: b, Path: "/etc/passwd", Content: "x"}, "clean relative path"},
	}
	for _, tt := range writes {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.WriteFile(ctx, tt.args)
			mustErr(t, err, tt.want)
		})
	}
}

func TestCreateErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.open()
	tests := []struct {
		name string
		args CreateArgs
		want string
	}{
		{"unknown root", CreateArgs{ChangeID: id, Root: "elsewhere", Name: "x", Description: "d"}, "is not a root"},
		{"bad name", CreateArgs{ChangeID: id, Root: "skills/agent", Name: "Bad", Description: "d"}, "must match"},
		{"exists", CreateArgs{ChangeID: id, Root: "skills/agent", Name: "golang", Description: "d"}, "already exists"},
		{"empty description", CreateArgs{ChangeID: id, Root: "skills/agent", Name: "x", Description: ""}, "invalid_description"},
		{"reserved key", CreateArgs{ChangeID: id, Root: "skills/agent", Name: "x", Description: "d", Frontmatter: []FrontmatterEntry{{Key: "name", Value: "y"}}}, "reserved"},
		{"duplicate key", CreateArgs{ChangeID: id, Root: "skills/agent", Name: "x", Description: "d", Frontmatter: []FrontmatterEntry{{Key: "a", Value: 1}, {Key: "a", Value: 2}}}, "duplicated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.Create(ctx, tt.args)
			mustErr(t, err, tt.want)
		})
	}
	// Harness roots expand with the change's harness.
	if _, err := e.svc.Create(ctx, CreateArgs{ChangeID: id, Root: "harnesses/claude-code/skills", Name: "cc-only", Description: "d"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Create(ctx, CreateArgs{ChangeID: id, Root: "harnesses/pi/skills", Name: "pi-only", Description: "d"})
	mustErr(t, err, "is not a root")
}

func TestOpenErrors(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.cfg.Sources = append(e.cfg.Sources,
		&config.Source{Name: "local", Kind: config.KindPath},
		&config.Source{Name: "ro", Kind: config.KindGit},
	)
	tests := []struct{ source, want string }{
		{"missing", "unknown source"},
		{"local", "is a path source"},
		{"ro", "is not writable"},
	}
	for _, tt := range tests {
		_, err := e.svc.Open(ctx, config.ClaudeCode, tt.source, "s")
		mustErr(t, err, tt.want)
	}
	e.cfg.Sources[2].Writable = true
	_, err := e.svc.Open(ctx, config.ClaudeCode, "ro", "s")
	mustErr(t, err, "has no snapshot")
}

func TestRecordPersisted(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	data, err := os.ReadFile(filepath.Join(e.dirs.Changes(), id, "change.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil || rec.State != StateOpen || rec.Branch != "skills-mcp/change/"+id {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	if !e.svc.BaseRevisions()["reg"][rec.BaseRevision] {
		t.Fatal("open change base not indexed")
	}
	if _, ok := e.svc.Change("chg_ffffffffffffffff"); ok {
		t.Fatal("unknown change found")
	}
}
