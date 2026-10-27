// Package gitx runs the git binary with the daemon's fixed environment:
// no prompts, no hooks, no automatic garbage collection, literal pathspecs,
// and a timeout on every command.
package gitx

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/proc"
)

// Command timeouts.
const (
	NetworkTimeout     = 120 * time.Second
	LocalTimeout       = 60 * time.Second
	MaintenanceTimeout = 600 * time.Second
)

var networkCommands = map[string]bool{"fetch": true, "push": true, "ls-remote": true}

// hookSwitchEnv holds "false"; --config-env reads it to disable each hook
// defined in configuration.
const hookSwitchEnv = "SKILLS_MCP_HOOK_ENABLED"

// hookEvents are the githooks(5) events. hook.<event>.enabled=false
// (Git 2.55 and later) disables every configured hook for the event,
// including hooks that only a child git process's configuration defines.
var hookEvents = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "receive-report", "push-to-checkout", "pre-auto-gc", "post-rewrite",
	"sendemail-validate", "fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist",
	"p4-post-changelist", "p4-pre-submit", "post-index-change",
}

// fixedConfig applies to every invocation: no hooks, and no fsmonitor,
// which core.hooksPath does not cover.
var fixedConfig = func() []string {
	args := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
	for _, e := range hookEvents {
		args = append(args, "-c", "hook."+e+".enabled=false")
	}
	return args
}()

// Git runs git commands. The zero value is not usable; call New.
type Git struct {
	env []string
}

// New prepares the git environment. Git's repository-location variables
// (from `git rev-parse --local-env-vars`) are removed, so every command acts
// on the repository it names. Children run without a controlling terminal
// (see proc), and an empty GIT_ASKPASS stops git from falling back to
// core.askPass or SSH_ASKPASS, so nothing can prompt.
func New(ctx context.Context) (*Git, error) {
	res, err := proc.Run(ctx, proc.Command{Name: "git", Args: []string{"rev-parse", "--local-env-vars"}, Timeout: LocalTimeout})
	if err != nil {
		return nil, fmt.Errorf("listing git's repository variables: %w", err)
	}
	local := strings.Fields(string(res.Stdout))
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(local, name)
	})
	return &Git{env: append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS_REQUIRE=never",
		"GCM_INTERACTIVE=never",
		hookSwitchEnv+"=false",
	)}, nil
}

// Opts adjusts one invocation.
type Opts struct {
	Stdin  io.Reader
	Stdout io.Writer // nil captures stdout into the returned bytes
	// Bare names dir as the repository with --git-dir, which
	// safe.bareRepository=explicit requires for bare repositories.
	Bare bool
}

// Run executes `git -C dir <args>` and returns stdout. On a non-zero exit
// the captured stdout is still returned with the error, so callers can parse
// porcelain output of a rejected push. Without Bare, dir is the top level of
// a working tree (or not a repository); repository discovery never leaves
// it for an enclosing repository.
func (g *Git) Run(ctx context.Context, dir string, opts Opts, args ...string) ([]byte, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	env := slices.Clone(g.env)
	repo := []string{"-C", dir}
	if opts.Bare {
		repo = append(repo, "--git-dir=.")
	} else {
		env = append(env, "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
	}
	hooks, err := configuredHooks(ctx, repo, env)
	if err != nil {
		return nil, err
	}
	sub := subcommand(args)
	full := slices.Concat(repo, fixedConfig, hooks)
	timeout := LocalTimeout
	switch {
	case sub == "maintenance":
		full = append(full, "-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false")
		timeout = MaintenanceTimeout
	default:
		full = append(full, "-c", "gc.auto=0", "-c", "maintenance.auto=false")
		if networkCommands[sub] {
			timeout = NetworkTimeout
		}
	}
	// check-ignore rejects literal pathspec magic and already matches its
	// input as literal paths.
	if sub != "check-ignore" {
		env = append(env, "GIT_LITERAL_PATHSPECS=1")
	}
	res, err := proc.Run(ctx, proc.Command{
		Name:    "git",
		Args:    append(full, args...),
		Env:     env,
		Stdin:   opts.Stdin,
		Stdout:  opts.Stdout,
		Timeout: timeout,
	})
	return res.Stdout, err
}

// configuredHooks returns options that disable, by name, every hook the
// command's own configuration defines (hook.<name>.command). Git 2.54 has
// no event-wide switch, and on later versions a hook named after an event
// turns that event's switch into a per-hook one. The listing runs in the
// command's repository context, so local and conditionally included
// configuration count. --config-env splits at the last "=", so hook names
// containing "=" are disabled too.
func configuredHooks(ctx context.Context, repo, env []string) ([]string, error) {
	args := append(slices.Clone(repo), "config", "-z", "--name-only", "--get-regexp", `^hook\..+\.(command|event)$`)
	res, err := proc.Run(ctx, proc.Command{Name: "git", Args: args, Env: env, Timeout: LocalTimeout})
	if ExitCode(err) == 1 { // no hook keys
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing configured git hooks: %w", err)
	}
	var opts []string
	seen := map[string]bool{}
	for _, key := range SplitNUL(res.Stdout) {
		name := key[len("hook."):strings.LastIndexByte(key, '.')]
		if !seen[name] {
			seen[name] = true
			opts = append(opts, "--config-env=hook."+name+".enabled="+hookSwitchEnv)
		}
	}
	return opts, nil
}

func subcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++ // skip the option's value
		case !strings.HasPrefix(args[i], "-"):
			return args[i]
		}
	}
	return ""
}

// Output runs git and returns trimmed stdout.
func (g *Git) Output(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := g.Run(ctx, dir, Opts{}, args...)
	return strings.TrimSpace(string(out)), err
}

// ExitCode returns the exit status of a git error, or -1 when err is not an
// exit error.
func ExitCode(err error) int {
	var exitErr *proc.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return -1
}

// SplitNUL splits NUL-terminated output, dropping the empty tail.
func SplitNUL(out []byte) []string {
	var fields []string
	for f := range bytes.SplitSeq(out, []byte{0}) {
		if len(f) > 0 {
			fields = append(fields, string(f))
		}
	}
	return fields
}

// PushResult is the parsed outcome of `git push --porcelain` for one ref.
type PushResult struct {
	Parsed  bool   // a status line for the ref was found
	Flag    byte   // '*', ' ', '+', '=', '-', '!'
	Summary string // e.g. "[rejected] (fetch first)"
}

// Accepted reports whether the remote ref now points at the pushed commit.
func (r PushResult) Accepted() bool {
	return r.Parsed && strings.IndexByte("* +=", r.Flag) >= 0
}

// NonFastForward reports a rejection that a rebase can resolve: the remote
// moved (forward or backward) away from the expected base.
func (r PushResult) NonFastForward() bool {
	return r.Parsed && r.Flag == '!' &&
		(strings.Contains(r.Summary, "(non-fast-forward)") || strings.Contains(r.Summary, "(fetch first)") ||
			strings.Contains(r.Summary, "(stale info)"))
}

// ParsePush finds the status line for ref in porcelain push output.
func ParsePush(out []byte, ref string) PushResult {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), "\t")
		if len(parts) < 3 || len(parts[0]) != 1 {
			continue
		}
		_, to, ok := strings.Cut(parts[1], ":")
		if !ok || to != ref {
			continue
		}
		return PushResult{Parsed: true, Flag: parts[0][0], Summary: parts[2]}
	}
	return PushResult{}
}
