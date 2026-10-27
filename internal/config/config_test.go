package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
listen: 127.0.0.1:7341
default_write_source: registry
harnesses:
  codex: { max_result_bytes: 30000 }
sources:
  - name: registry
    kind: git
    url: git@example.com:owner/registry.git
    branch: main
    writable: true
    roots:
      - { path: skills/agent, invocation: model }
      - { path: skills/user, invocation: user }
    harness_roots:
      - { path: "harnesses/{harness}/skills/agent", invocation: model }
    ignore_file: .registry-ignore
    author_prefix_except: [alexgorbatchev]
    template:
      vars:
        repo_root: ~/checkout
        skills_dir: ~/checkout/skills
      env: [DOCS_INTERNAL_DIR]
    prepare:
      - when_file: package.json
        run: [bun, install]
        keep: [bun.lock]
    validate: [bun, validate.ts, "{bundle_dir}"]
    deny_patterns: ['internal\.example$']
  - name: local
    kind: path
    path: ~/skills
    roots:
      - { path: agent, invocation: model }
`

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestParseValid(t *testing.T) {
	home := setHome(t)
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:7341" || cfg.FetchInterval != time.Minute || cfg.SubmitTimeout != 10*time.Minute {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.SnapshotRetention != 24*time.Hour || cfg.ChangeTTL != 168*time.Hour {
		t.Fatalf("unexpected retention defaults: %+v", cfg)
	}
	if got := cfg.Harnesses[Codex].MaxResultBytes; got != 30000 {
		t.Fatalf("codex max = %d", got)
	}
	if got := cfg.Harnesses[Pi].MaxResultBytes; got != 19000 {
		t.Fatalf("pi default max = %d", got)
	}
	reg := cfg.SourceByName("registry")
	if reg == nil || !reg.AuthorPrefix || reg.AuthorPrefixExcept[0] != "alexgorbatchev" {
		t.Fatalf("registry source = %+v", reg)
	}
	if reg.Template.Vars["repo_root"] != filepath.Join(home, "checkout") {
		t.Fatalf("repo_root = %q", reg.Template.Vars["repo_root"])
	}
	if !reg.DenyPatterns[0].MatchString("host.internal.example") {
		t.Fatal("deny pattern not compiled")
	}
	if reg.Prepare[0].Timeout != 5*time.Minute {
		t.Fatalf("prepare timeout = %v", reg.Prepare[0].Timeout)
	}
	local := cfg.SourceByName("local")
	if local.Path != filepath.Join(home, "skills") || local.AuthorPrefix {
		t.Fatalf("local source = %+v", local)
	}
	if local.Template.Vars["repo_root"] != "{snapshot}" {
		t.Fatalf("default repo_root = %q", local.Template.Vars["repo_root"])
	}
	if cfg.SourceByName("missing") != nil {
		t.Fatal("SourceByName returned a missing source")
	}
	roots := reg.ExpandedRoots(Pi)
	if roots[2].Path != "harnesses/pi/skills/agent" {
		t.Fatalf("expanded roots = %+v", roots)
	}
	all := reg.AllRootPaths()
	want := []string{"harnesses/claude-code/skills/agent", "harnesses/codex/skills/agent", "harnesses/pi/skills/agent", "skills/agent", "skills/user"}
	if strings.Join(all, ",") != strings.Join(want, ",") {
		t.Fatalf("AllRootPaths = %v", all)
	}
}

func TestParseNoWriteSource(t *testing.T) {
	setHome(t)
	cfg, err := Parse([]byte("sources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultWriteSource != "" {
		t.Fatalf("DefaultWriteSource = %q", cfg.DefaultWriteSource)
	}
}

func TestParseErrors(t *testing.T) {
	gitSource := func(extra string) string {
		return "sources:\n  - name: s\n    kind: git\n    url: u\n    branch: main\n    roots: [{path: a, invocation: model}]\n" + extra
	}
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown field", "bogus: 1\n", "field bogus not found"},
		{"listen host", "listen: 0.0.0.0:1\n", "listen: host must be 127.0.0.1"},
		{"listen invalid", "listen: nope\n", "listen: invalid address"},
		{"submit timeout minimum", "submit_timeout: 30s\n", "submit_timeout: must be at least 1m0s"},
		{"fetch interval zero", "fetch_interval: 0s\n", "fetch_interval: must be positive"},
		{"retention negative", "snapshot_retention: -1h\n", "snapshot_retention: must be positive"},
		{"bad duration", "change_ttl: soon\n", `change_ttl: invalid duration "soon"`},
		{"unknown harness", "harnesses: {vim: {max_result_bytes: 1}}\n", "harnesses.vim: unknown harness"},
		{"negative max", "harnesses: {pi: {max_result_bytes: -1}}\n", "harnesses.pi.max_result_bytes: must be positive"},
		{"bad source name", "sources: [{name: Bad, kind: path, path: /x}]\n", `invalid source name "Bad"`},
		{"duplicate source", "sources: [{name: a, kind: path, path: /x}, {name: a, kind: path, path: /y}]\n", `duplicate source name "a"`},
		{"bad kind", "sources: [{name: a, kind: svn}]\n", `sources[0].kind: must be "git" or "path"`},
		{"git without url", "sources: [{name: a, kind: git, branch: main}]\n", "sources[0].url: required"},
		{"git without branch", "sources: [{name: a, kind: git, url: u}]\n", "sources[0].branch: required"},
		{"writable without deny", gitSource("    writable: true\n"), "deny_patterns: must be non-empty"},
		{"bad deny pattern", gitSource("    deny_patterns: ['(']\n"), "deny_patterns[0]: invalid pattern"},
		{"path without path", "sources: [{name: a, kind: path}]\n", "sources[0].path: required"},
		{"relative path", "sources: [{name: a, kind: path, path: rel}]\n", "must be absolute or ~-prefixed"},
		{"writable path", "sources: [{name: a, kind: path, path: /x, writable: true}]\n", "path sources are read-only"},
		{"root outside", gitSource("    harness_roots: [{path: ../x, invocation: model}]\n"), "harness_roots[0].path: must be a repo-relative path"},
		{"root not clean", gitSource("    harness_roots: [{path: skills/agent/, invocation: model}]\n"), `must be written in clean form, e.g. "skills/agent"`},
		{"root with dot", gitSource("    harness_roots: [{path: ./skills, invocation: model}]\n"), "must be written in clean form"},
		{"root invocation", gitSource("    harness_roots: [{path: x, invocation: both}]\n"), `harness_roots[0].invocation: must be "model" or "user"`},
		{"var name", gitSource("    template: {vars: {Bad: x}}\n"), "template.vars.Bad: invalid variable name"},
		{"reserved var", gitSource("    template: {vars: {file_dir: x}}\n"), "template.vars.file_dir: reserved variable name"},
		{"env name", gitSource("    template: {env: [lower]}\n"), `invalid environment variable name "lower"`},
		{"prepare when_file", gitSource("    prepare: [{when_file: a/b, run: [x]}]\n"), "prepare[0].when_file: must be a file name"},
		{"prepare run", gitSource("    prepare: [{when_file: a}]\n"), "prepare[0].run: must not be empty"},
		{"prepare timeout", gitSource("    prepare: [{when_file: a, run: [x], timeout: 0s}]\n"), "prepare[0].timeout: must be positive"},
		{"write source missing", "default_write_source: nope\n", `default_write_source: "nope" is not a writable git source`},
		{"write source read-only", gitSource("default_write_source: s\n"), `default_write_source: "s" is not a writable git source`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHome(t)
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	setHome(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("Load(missing) error = %v", err)
	}
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}

func TestParseWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := Parse([]byte("{}\n")); err == nil {
		t.Fatal("Parse without HOME succeeded")
	}
}

func TestExpandHomeTilde(t *testing.T) {
	home := setHome(t)
	cfg, err := Parse([]byte("sources: [{name: a, kind: path, path: '~'}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources[0].Path != home {
		t.Fatalf("path = %q", cfg.Sources[0].Path)
	}
}
