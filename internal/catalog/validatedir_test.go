package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateDir(t *testing.T) {
	src := &config.Source{
		Template:     config.Template{Vars: map[string]string{"repo_root": "/r"}, Env: []string{"DOCS"}},
		AuthorPrefix: true, AuthorPrefixExcept: []string{"owner"},
	}
	getenv := func(string) (string, bool) { return "", false }
	root := t.TempDir()

	good := filepath.Join(root, "good")
	writeTree(t, good, map[string]string{
		"SKILL.md":              "---\nname: good\ndescription: d\nauthor: owner\n---\n<%- repo_root %> <%- env(\"DOCS\", \"x\") %>\n",
		"references/a.md":       "<%- file_dir %>",
		"node_modules/x/bad.md": "<%= ignored %>",
		"img.png":               "\x00<%= binary %>",
	})
	problems, err := ValidateDir(good, src, getenv)
	if err != nil || len(problems) != 0 {
		t.Fatalf("good = %v, %v", problems, err)
	}

	bad := filepath.Join(root, "bad")
	writeTree(t, bad, map[string]string{
		"SKILL.md":   "---\nname: other\ndescription: d\n---\n<%- missing %>\n",
		"ref/x.md":   "line\n<%= x %>",
		"ref/env.md": "<%- env(\"HOME\") %>",
	})
	if err := os.Symlink("ref/x.md", filepath.Join(bad, "link.md")); err != nil {
		t.Fatal(err)
	}
	problems, err = ValidateDir(bad, src, getenv)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		"name_mismatch: SKILL.md",
		"unknown_template_variable: SKILL.md:5",
		"unsupported_template_tag: ref/x.md:2",
		"env_not_allowed: ref/env.md:1",
		"symlink: link.md",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q:\n%s", want, joined)
		}
	}

	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	problems, _ = ValidateDir(empty, src, getenv)
	if len(problems) != 1 || !strings.Contains(problems[0], "SKILL.md: missing") {
		t.Fatalf("empty = %v", problems)
	}
	if _, err := ValidateDir(filepath.Join(root, "absent"), src, getenv); err == nil {
		t.Fatal("missing directory validated")
	}
	if _, err := ValidateDir(filepath.Join(good, "SKILL.md"), src, getenv); err == nil {
		t.Fatal("file validated as a directory")
	}
	unreadable := filepath.Join(root, "unreadable")
	writeTree(t, unreadable, map[string]string{"SKILL.md": "x"})
	if err := os.Chmod(filepath.Join(unreadable, "SKILL.md"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateDir(unreadable, src, getenv); err == nil {
		t.Fatal("unreadable file validated")
	}
}
