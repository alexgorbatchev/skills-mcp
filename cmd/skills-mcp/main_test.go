package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const skillAlert = "ALERT: Agents must read `AGENT=1 skills-mcp skill` before using this tool.\n"

func execute(args ...string) (stdout, stderr string, code int) {
	out, errOut := new(bytes.Buffer), new(bytes.Buffer)
	cmd := newRootCommand()
	cmd.SetArgs(args)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	code = run(cmd)
	return out.String(), errOut.String(), code
}

// env isolates XDG directories and git configuration under a temp root.
func env(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	empty := filepath.Join(root, "gitconfig")
	write(t, empty, "")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(root, "cache"), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	return root
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillDoc(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
}

// pathConfig writes a config with one path source and returns its root.
func pathConfig(t *testing.T, root string) string {
	t.Helper()
	local := filepath.Join(root, "skills")
	write(t, filepath.Join(local, "agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "body"))
	write(t, filepath.Join(local, "user/commit/SKILL.md"), skillDoc("commit", "Commit", "body"))
	write(t, filepath.Join(local, "agent/broken/SKILL.md"), "no frontmatter")
	write(t, filepath.Join(root, "config/skills-mcp/config.yaml"), `listen: 127.0.0.1:0
sources:
  - name: local
    kind: path
    path: `+local+`
    roots:
      - {path: agent, invocation: model}
      - {path: user, invocation: user}
    template:
      env: [DOCS]
`)
	return local
}

func TestVersionAndErrors(t *testing.T) {
	for _, tt := range []struct {
		mode, want string
	}{{"0", "[ERROR] unknown command"}, {"1", "ERR: unknown command"}} {
		t.Setenv("AGENT", tt.mode)
		out, errOut, code := execute("--version")
		if code != 0 || out != version+"\n" || errOut != "" {
			t.Fatalf("version = %q %q %d", out, errOut, code)
		}
		_, errOut, code = execute("nope")
		if code != 1 || !strings.Contains(errOut, tt.want) {
			t.Fatalf("unknown command = %q %d", errOut, code)
		}
	}
}

func TestSkillCommand(t *testing.T) {
	for _, mode := range []string{"0", "1"} {
		t.Setenv("AGENT", mode)
		out, _, code := execute("skill")
		if code != 0 || out != skillText {
			t.Fatalf("skill output = %q", out)
		}
	}
	_, errOut, code := execute("skill", "extra")
	if code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Fatalf("extra argument = %q", errOut)
	}
	// The printed URL points at the skill in this repository, and its
	// frontmatter name matches the printed --skill value.
	data, err := os.ReadFile("../../skills/skills-mcp/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\nname: \"skills-mcp\"\n") ||
		!strings.Contains(skillText, "--skill skills-mcp") ||
		!strings.Contains(skillText, "/tree/main/skills/skills-mcp\n") {
		t.Fatal("skill command and SKILL.md disagree")
	}
}

func TestSkillOutputError(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "closed"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := newRootCommand()
	cmd.SetOut(f)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"skill"})
	if err := cmd.Execute(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("error = %v", err)
	}
}

func walk(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd.Hidden {
		return
	}
	visit(cmd)
	for _, c := range cmd.Commands() {
		walk(c, visit)
	}
}

func commandTree() *cobra.Command {
	cmd := newRootCommand()
	cmd.InitDefaultHelpCmd()
	return cmd
}

func TestHelpAlert(t *testing.T) {
	for _, mode := range []string{"0", "1", "true", "yes"} {
		t.Setenv("AGENT", mode)
		walk(commandTree(), func(c *cobra.Command) {
			args := append(strings.Fields(c.CommandPath())[1:], "--help")
			out, _, code := execute(args...)
			if code != 0 {
				t.Fatalf("%s --help failed", c.CommandPath())
			}
			has := strings.HasPrefix(out, skillAlert) && strings.Count(out, skillAlert) == 1
			if (mode == "0") == has {
				t.Errorf("mode %s: %s help alert = %v", mode, c.CommandPath(), has)
			}
		})
		out, _, _ := execute()
		if mode != "0" && !strings.HasPrefix(out, skillAlert) {
			t.Fatalf("bare invocation lacks alert: %q", out)
		}
	}
}

// TestSkillDocumentsCommandInterface fails when SKILL.md drifts from the
// live command tree and flag metadata.
func TestSkillDocumentsCommandInterface(t *testing.T) {
	data, err := os.ReadFile("../../skills/skills-mcp/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	walk(commandTree(), func(c *cobra.Command) {
		if !c.Runnable() && c.Parent() != nil {
			return
		}
		usage := c.Use
		if c.Parent() != nil {
			usage = c.Parent().CommandPath() + " " + usage
		}
		_, section, ok := strings.Cut(doc, "## `"+usage+"`\n")
		if !ok {
			t.Errorf("SKILL.md lacks a section for %s", usage)
			return
		}
		section, _, _ = strings.Cut(section, "\n## ")
		c.InitDefaultHelpFlag()
		c.InitDefaultVersionFlag()
		check := func(f *pflag.Flag) {
			if f.Hidden || (f.Name == "help" && c.Parent() != nil) {
				return
			}
			short := "—"
			if f.Shorthand != "" {
				short = "`-" + f.Shorthand + "`"
			}
			def := f.DefValue
			if def == "" {
				def = `""`
			}
			row := fmt.Sprintf("| `--%s` | %s | `%s` | `%s` |", f.Name, short, f.Value.Type(), def)
			if !strings.Contains(section, row) {
				t.Errorf("%s section lacks %s", usage, row)
			}
		}
		c.LocalNonPersistentFlags().VisitAll(check)
		if c.Parent() == nil {
			c.PersistentFlags().VisitAll(check)
		}
	})
}

func TestTokenShow(t *testing.T) {
	env(t)
	out, _, code := execute("token", "show")
	if code != 0 || len(strings.TrimSpace(out)) != 64 {
		t.Fatalf("token = %q", out)
	}
	again, _, _ := execute("token", "show")
	if again != out {
		t.Fatal("token changed between calls")
	}
	if err := os.Chmod(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "skills-mcp/token"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := execute("token", "show")
	if code != 1 || !strings.Contains(errOut, "~/config/skills-mcp/token") {
		t.Fatalf("bad mode = %q", errOut)
	}
}

func TestBundleValidate(t *testing.T) {
	root := env(t)
	pathConfig(t, root)
	cfg := filepath.Join(root, "config/skills-mcp/config.yaml")
	good := filepath.Join(root, "bundles/good")
	write(t, filepath.Join(good, "SKILL.md"), skillDoc("good", "Good", "<%- env(\"DOCS\", \"d\") %>"))
	bad := filepath.Join(root, "bundles/bad")
	write(t, filepath.Join(bad, "SKILL.md"), skillDoc("other", "Bad", "<%= x %>"))

	t.Setenv("AGENT", "1")
	out, errOut, code := execute("bundle", "validate", "--source", "local", good, bad)
	if code != 1 || out != "OK: ~/bundles/good\n" {
		t.Fatalf("stdout = %q code %d", out, code)
	}
	for _, want := range []string{"ERR: ~/bundles/bad: name_mismatch", "ERR: ~/bundles/bad: unsupported_template_tag: SKILL.md:5"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	t.Setenv("AGENT", "0")
	out, _, code = execute("--config", cfg, "bundle", "validate", "--source", "local", good)
	if code != 0 || out != "[OK] ~/bundles/good\n" {
		t.Fatalf("human = %q %d", out, code)
	}
	_, errOut, code = execute("bundle", "validate", good)
	if code != 1 || !strings.Contains(errOut, `no source ""`) {
		t.Fatalf("no default source = %q", errOut)
	}
	_, errOut, code = execute("bundle", "validate", "--source", "local", filepath.Join(root, "missing"))
	if code != 1 || !strings.Contains(errOut, "~/missing") {
		t.Fatalf("missing dir = %q", errOut)
	}
	_, errOut, code = execute("--config", filepath.Join(root, "nope.yaml"), "bundle", "validate", good)
	if code != 1 || !strings.Contains(errOut, "reading config") {
		t.Fatalf("missing config = %q", errOut)
	}
}

func TestCatalogListAndServerRun(t *testing.T) {
	root := env(t)
	pathConfig(t, root)
	_, errOut, code := execute("catalog", "list")
	if code != 1 || !strings.Contains(errOut, "no snapshots yet") {
		t.Fatalf("before daemon = %q", errOut)
	}

	// Run the daemon briefly so snapshots exist.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := newRootCommand()
	logs := new(bytes.Buffer)
	cmd.SetArgs([]string{"server", "run"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(logs)
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("server run: %v\n%s", err, logs)
	}
	if !strings.Contains(logs.String(), "msg=serving") || strings.Contains(logs.String(), root) {
		t.Fatalf("logs:\n%s", logs)
	}

	t.Setenv("AGENT", "1")
	out, errOut, code := execute("catalog", "list", "--harness", "pi")
	if code != 1 {
		t.Fatalf("refused skill did not fail the listing: %d", code)
	}
	if !strings.Contains(out, "harness=pi name=commit invocation=user source=local") ||
		!strings.Contains(out, "harness=pi name=golang invocation=model") {
		t.Fatalf("agent list = %q", out)
	}
	if !strings.Contains(errOut, "ERR: refused code=invalid_frontmatter local:agent/broken/SKILL.md") {
		t.Fatalf("agent diagnostics = %q", errOut)
	}
	t.Setenv("AGENT", "0")
	out, errOut, _ = execute("catalog", "list")
	if !strings.Contains(out, "claude-code\n├─ commit (prompt)  Commit\n╰─ golang  Go rules\n") || strings.Count(out, "[WARN]") != 0 {
		t.Fatalf("human list = %q", out)
	}
	if strings.Count(errOut, "[WARN] refused:") != 1 {
		t.Fatalf("human diagnostics = %q", errOut)
	}
	_, errOut, code = execute("catalog", "list", "--harness", "vim")
	if code != 1 || !strings.Contains(errOut, "unknown harness") {
		t.Fatalf("bad harness = %q", errOut)
	}
}

func TestCatalogListClean(t *testing.T) {
	root := env(t)
	local := pathConfig(t, root)
	if err := os.RemoveAll(filepath.Join(local, "agent/broken")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := newRootCommand()
	cmd.SetArgs([]string{"server", "run"})
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := execute("catalog", "list")
	if code != 0 || errOut != "" {
		t.Fatalf("clean list = %q %d", errOut, code)
	}
}

func TestServerRunErrors(t *testing.T) {
	root := env(t)
	_, errOut, code := execute("server", "run")
	if code != 1 || !strings.Contains(errOut, "reading config") || !strings.Contains(errOut, "~/config/skills-mcp/config.yaml") {
		t.Fatalf("missing config = %q", errOut)
	}
	_, errOut, code = execute("catalog", "list")
	if code != 1 || !strings.Contains(errOut, "reading config") {
		t.Fatalf("catalog without config = %q", errOut)
	}
	pathConfig(t, root)
	write(t, filepath.Join(root, "cache/skills-mcp/sources/local/snapshots.json"), "{}")
	if err := os.Chmod(filepath.Join(root, "cache/skills-mcp/sources/local/snapshots.json"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = execute("catalog", "list")
	if code != 1 || !strings.Contains(errOut, "source local") {
		t.Fatalf("unreadable index = %q", errOut)
	}
}

func TestResolveWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	for _, args := range [][]string{{"token", "show"}, {"catalog", "list"}, {"bundle", "validate", "x"}, {"server", "run"}} {
		if _, _, code := execute(args...); code != 1 {
			t.Errorf("%v without HOME succeeded", args)
		}
	}
}

func TestClip(t *testing.T) {
	if got := clip("abcdefghij", 8); got != "abcde..." {
		t.Fatalf("clip = %q", got)
	}
	if got := clip("abc", 8); got != "abc" {
		t.Fatalf("clip short = %q", got)
	}
	if got := clip("abcdef", 2); got != "abcdef" {
		t.Fatalf("clip tiny width = %q", got)
	}
}

func TestMainBinary(t *testing.T) {
	if os.Getenv("SKILLS_MCP_RUN_MAIN") == "1" {
		os.Args = []string{"skills-mcp", "--version"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainBinary$")
	cmd.Env = append(os.Environ(), "SKILLS_MCP_RUN_MAIN=1")
	out, err := cmd.Output()
	if err != nil || string(out) != version+"\n" {
		t.Fatalf("main output = %q, %v", out, err)
	}
}
