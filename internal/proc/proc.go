// Package proc runs child processes in their own session, without a
// controlling terminal, with bounded timeouts, terminating the whole process
// group on timeout or cancellation.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// DefaultKillGrace is how long a process group gets between SIGTERM and SIGKILL.
const DefaultKillGrace = 5 * time.Second

// ErrCanceled reports that the caller's context ended the command.
var ErrCanceled = errors.New("command canceled")

// Command describes one child process.
type Command struct {
	Name      string
	Args      []string
	Dir       string
	Env       []string // full environment; nil inherits the daemon's
	Stdin     io.Reader
	Stdout    io.Writer // nil captures into Result.Stdout
	Timeout   time.Duration
	KillGrace time.Duration // zero uses DefaultKillGrace
}

// Result holds captured output.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// TimeoutError reports that the command's own timeout expired.
type TimeoutError struct {
	Command string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("%s timed out after %s", e.Command, e.Timeout)
}

// ExitError reports a non-zero exit.
type ExitError struct {
	Command string
	Code    int
	Stderr  string
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("%s exited with status %d", e.Command, e.Code)
	}
	return fmt.Sprintf("%s exited with status %d: %s", e.Command, e.Code, msg)
}

// Run executes the command. A timeout or a cancelled ctx sends SIGTERM to the
// process group, then SIGKILL after the grace period. Errors are
// *TimeoutError, *ExitError, or wrap ErrCanceled.
func Run(ctx context.Context, c Command) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("%w: %s: %w", ErrCanceled, c.Name, err)
	}
	grace := c.KillGrace
	if grace == 0 {
		grace = DefaultKillGrace
	}
	runCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdin = c.Stdin
	cmd.SysProcAttr = sysProcAttr()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	cmd.Stderr = &stderr
	var cancelled time.Time
	cmd.Cancel = func() error {
		cancelled = time.Now()
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = grace + time.Second

	err := cmd.Run()
	if !cancelled.IsZero() {
		// The leader may exit on SIGTERM while group members ignore it:
		// finish the grace period, then kill whatever remains.
		time.Sleep(time.Until(cancelled.Add(grace)))
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // ESRCH when the group is already gone
	}
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		return res, fmt.Errorf("%w: %s: %w", ErrCanceled, c.Name, ctx.Err())
	}
	if runCtx.Err() != nil {
		return res, &TimeoutError{Command: c.Name, Timeout: c.Timeout}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return res, &ExitError{Command: c.Name, Code: exitErr.ExitCode(), Stderr: stderr.String()}
	}
	return res, fmt.Errorf("running %s: %w", c.Name, err)
}

// LastLine returns the last non-empty line of s.
func LastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
