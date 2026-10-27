package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name      string
		cmd       Command
		wantOut   string
		wantErr   string
		wantErrAs any
	}{
		{name: "success", cmd: Command{Name: "sh", Args: []string{"-c", "echo hi"}, Timeout: time.Second}, wantOut: "hi\n"},
		{name: "stdin", cmd: Command{Name: "cat", Stdin: strings.NewReader("in"), Timeout: time.Second}, wantOut: "in"},
		{name: "exit code with stderr", cmd: Command{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}, Timeout: time.Second}, wantErr: "sh exited with status 3: boom", wantErrAs: new(*ExitError)},
		{name: "exit code without stderr", cmd: Command{Name: "sh", Args: []string{"-c", "exit 4"}, Timeout: time.Second}, wantErr: "sh exited with status 4", wantErrAs: new(*ExitError)},
		{name: "missing binary", cmd: Command{Name: "/nonexistent/bin", Timeout: time.Second}, wantErr: "running /nonexistent/bin"},
		{name: "timeout", cmd: Command{Name: "sleep", Args: []string{"10"}, Timeout: 50 * time.Millisecond, KillGrace: 50 * time.Millisecond}, wantErr: "sleep timed out after 50ms", wantErrAs: new(*TimeoutError)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Run(context.Background(), tt.cmd)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if string(res.Stdout) != tt.wantOut {
					t.Fatalf("stdout = %q, want %q", res.Stdout, tt.wantOut)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Run() error = %v, want %q", err, tt.wantErr)
			}
			if tt.wantErrAs != nil && !errors.As(err, tt.wantErrAs) {
				t.Fatalf("error %T does not match %T", err, tt.wantErrAs)
			}
		})
	}
}

func TestRunStdoutWriter(t *testing.T) {
	var sb strings.Builder
	res, err := Run(context.Background(), Command{Name: "echo", Args: []string{"x"}, Stdout: &sb, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if sb.String() != "x\n" || len(res.Stdout) != 0 {
		t.Fatalf("writer = %q, captured = %q", sb.String(), res.Stdout)
	}
}

func TestRunCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Command{Name: "true", Timeout: time.Second}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("pre-cancelled error = %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Run(ctx, Command{Name: "sleep", Args: []string{"10"}, Timeout: time.Minute, KillGrace: 50 * time.Millisecond})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("caller deadline error = %v", err)
	}
}

// A grandchild that ignores SIGTERM must still die with the group SIGKILL.
func TestRunKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := `sh -c 'trap "" TERM; echo $$ > ` + pidFile + `; sleep 30' & wait`
	start := time.Now()
	_, err := Run(context.Background(), Command{Name: "sh", Args: []string{"-c", script}, Timeout: 200 * time.Millisecond, KillGrace: 100 * time.Millisecond})
	if !errors.As(err, new(*TimeoutError)) {
		t.Fatalf("error = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Run waited for the grandchild instead of killing the group")
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + pid); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild %s survived the group kill", pid)
}

func TestLastLine(t *testing.T) {
	if got := LastLine("a\nb\n\n"); got != "b" {
		t.Fatalf("LastLine = %q", got)
	}
	if got := LastLine(""); got != "" {
		t.Fatalf("LastLine(empty) = %q", got)
	}
}

// A grandchild that ignores SIGTERM and holds no output pipe must still die
// after the grace period.
func TestRunKillsDetachedGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := `sh -c 'trap "" TERM; echo $$ > ` + pidFile + `; exec sleep 30' >/dev/null 2>&1 </dev/null & sleep 30`
	_, err := Run(context.Background(), Command{Name: "sh", Args: []string{"-c", script}, Timeout: 200 * time.Millisecond, KillGrace: 100 * time.Millisecond})
	if !errors.As(err, new(*TimeoutError)) {
		t.Fatalf("error = %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(data))
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + pid); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("detached grandchild %s survived", pid)
}

// A child leads its own session (so its own process group) and has no
// controlling terminal to prompt on.
func TestRunNewSession(t *testing.T) {
	res, err := Run(context.Background(), Command{Name: "sh", Args: []string{"-c", "cat /proc/$$/stat"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stat := string(res.Stdout)
	pid, _, _ := strings.Cut(stat, " ")
	fields := strings.Fields(stat[strings.LastIndexByte(stat, ')')+2:])
	// After the command name: state, ppid, pgrp, session, tty_nr.
	if fields[2] != pid || fields[3] != pid || fields[4] != "0" {
		t.Fatalf("pid %s: pgrp=%s session=%s tty_nr=%s", pid, fields[2], fields[3], fields[4])
	}
}
