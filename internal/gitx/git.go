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

var networkCommands = map[string]bool{"clone": true, "fetch": true, "push": true, "ls-remote": true}

// hookSwitchEnv holds "false"; --config-env reads it to disable each hook
// defined in configuration.
const hookSwitchEnv = "SKILLS_MCP_HOOK_ENABLED"

// Git runs git commands. The zero value is not usable; call New.
type Git struct {
	env []string
}

// New prepares the git environment. Children run without a controlling
// terminal (see proc), so ssh and credential helpers cannot prompt
// whatever the user's ssh configuration says.
func New() *Git {
	return &Git{env: append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"SSH_ASKPASS_REQUIRE=never",
		hookSwitchEnv+"=false",
	)}
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
// porcelain output of a rejected push.
func (g *Git) Run(ctx context.Context, dir string, opts Opts, args ...string) ([]byte, error) {
	repo := []string{"-C", dir}
	if opts.Bare {
		repo = append(repo, "--git-dir=.")
	}
	hooks, err := g.configuredHooks(ctx, repo)
	if err != nil {
		return nil, err
	}
	sub := subcommand(args)
	full := append(append(slices.Clone(repo), "-c", "core.hooksPath=/dev/null"), hooks...)
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
	env := g.env
	// check-ignore rejects literal pathspec magic and already matches its
	// input as literal paths.
	if sub != "check-ignore" {
		env = append(slices.Clone(env), "GIT_LITERAL_PATHSPECS=1")
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

// configuredHooks returns options that disable every hook defined in
// configuration (hook.<name>.command, Git 2.54 and later), which
// core.hooksPath does not cover. The listing runs in the same repository
// context as the command, so local and conditionally included
// configuration count. --config-env splits at the last "=", so hook names
// containing "=" are disabled too.
func (g *Git) configuredHooks(ctx context.Context, repo []string) ([]string, error) {
	args := append(slices.Clone(repo), "config", "-z", "--name-only", "--get-regexp", `^hook\..+\.(command|event)$`)
	res, err := proc.Run(ctx, proc.Command{Name: "git", Args: args, Env: g.env, Timeout: LocalTimeout})
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
