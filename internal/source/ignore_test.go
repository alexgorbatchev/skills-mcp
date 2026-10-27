package source

import "testing"

func TestIgnoreRules(t *testing.T) {
	// Patterns copied from ai-registry's bundle-level .registry-ignore files.
	files := map[string][]byte{
		".registry-ignore":        []byte("# comment\n\nscripts/node_modules/\nscripts/__tests__/\nscripts/bun.lock\njustfile\n./local-only\n"),
		"nested/.registry-ignore": []byte("*.tmp\n!keep.tmp\n./sub/\n"),
		"other/.registry-ignore":  []byte("./\n"),
	}
	r := newIgnoreRules(".registry-ignore", files, []string{"scripts/bun.lock"})
	tests := []struct {
		path string
		want bool
	}{
		{"SKILL.md", false},
		{"scripts/run.ts", false},
		{"scripts/__tests__/run.test.ts", true},
		{"scripts/node_modules/x/index.js", true},
		{"node_modules/x/index.js", true},
		{"scripts/bun.lock", false}, // kept
		{"justfile", true},
		{"nested/justfile", true},
		{"local-only", true},
		{"local-only/file", true},
		{"local-only-not", false},
		{"nested/a.tmp", true},
		{"nested/keep.tmp", false},
		{"a.tmp", false},
		{"nested/sub/x", true},
		{"nested/subway", false},
		{".registry-ignore", true},
		{"nested/.registry-ignore", true},
		{"other/file", true}, // "./" in a nested ignore file excludes its directory
	}
	for _, tt := range tests {
		if got := r.excluded(tt.path); got != tt.want {
			t.Errorf("excluded(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIgnoreRulesWithoutFileName(t *testing.T) {
	r := newIgnoreRules("", nil, nil)
	if r.excluded("a/b") {
		t.Fatal("excluded without rules")
	}
	if !r.excluded("a/node_modules/b") {
		t.Fatal("node_modules not excluded")
	}
}
