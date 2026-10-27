package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexgorbatchev/skills-mcp/internal/paths"
	"github.com/alexgorbatchev/skills-mcp/internal/token"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func setup(t *testing.T, writable bool) (Options, string) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	write(t, empty, "")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	bare := filepath.Join(root, "up.git")
	work := filepath.Join(root, "work")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, root, "clone", "-q", bare, work)
	git(t, work, "checkout", "-q", "-b", "main")
	write(t, filepath.Join(work, "skills/agent/golang/SKILL.md"), "---\nname: golang\ndescription: Go rules\n---\nbody\n")
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "init")
	git(t, work, "push", "-q", "origin", "main")
	local := filepath.Join(root, "local")
	write(t, filepath.Join(local, "agent/notes/SKILL.md"), "---\nname: notes\ndescription: Notes\n---\nbody\n")

	dirs := paths.Dirs{Config: filepath.Join(root, "config"), Cache: filepath.Join(root, "cache"), State: filepath.Join(root, "state")}
	cfg := `listen: 127.0.0.1:0
fetch_interval: 1h
sources:
  - name: reg
    kind: git
    url: file://` + bare + `
    branch: main
    roots: [{path: skills/agent, invocation: model}]
  - name: local
    kind: path
    path: ` + local + `
    roots: [{path: agent, invocation: model}]
`
	if writable {
		cfg = strings.Replace(cfg, "    branch: main\n", "    branch: main\n    writable: true\n    deny_patterns: ['secret']\n", 1) + "default_write_source: reg\n"
	}
	path := filepath.Join(dirs.Config, "config.yaml")
	write(t, path, cfg)
	t.Cleanup(func() {
		_ = filepath.WalkDir(dirs.Cache, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	return Options{ConfigPath: path, Dirs: dirs, Logger: slog.New(slog.DiscardHandler), Version: "test"}, root
}

func TestRunServesAndShutsDown(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only", true: "writable"}[writable], func(t *testing.T) {
			opts, _ := setup(t, writable)
			ready := make(chan string, 1)
			opts.Ready = func(addr string) { ready <- addr }
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- Run(ctx, opts) }()
			var addr string
			select {
			case addr = <-ready:
			case err := <-done:
				t.Fatalf("Run exited early: %v", err)
			case <-time.After(30 * time.Second):
				t.Fatal("daemon did not become ready")
			}

			resp, err := http.Get("http://" + addr + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "ok" {
				t.Fatalf("healthz = %q", body)
			}
			tok, err := token.Read(opts.Dirs.TokenFile())
			if err != nil {
				t.Fatal(err)
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
			cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
				Endpoint: "http://" + addr + "/mcp/claude-code", HTTPClient: &http.Client{Transport: bearer{tok}},
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tl := range tools.Tools {
				names = append(names, tl.Name)
			}
			want := 2
			if writable {
				want = 10
			}
			if len(names) != want {
				t.Fatalf("tools = %v", names)
			}

			if err := Run(context.Background(), opts); !errors.Is(err, ErrLocked) {
				t.Fatalf("second daemon = %v", err)
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("shutdown hung")
			}
			_ = cs.Close()
		})
	}
}

func TestRunConfigErrors(t *testing.T) {
	opts, root := setup(t, false)
	opts.ConfigPath = filepath.Join(root, "missing.yaml")
	if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "reading config") {
		t.Fatalf("missing config = %v", err)
	}
	opts, _ = setup(t, false)
	write(t, opts.Dirs.TokenFile(), "bad")
	if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "token file") {
		t.Fatalf("bad token = %v", err)
	}
	opts, _ = setup(t, false)
	occupied, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg, _ := os.ReadFile(opts.ConfigPath)
	write(t, opts.ConfigPath, strings.Replace(string(cfg), "127.0.0.1:0", occupied.Addr().String(), 1))
	if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "listening on") {
		t.Fatalf("occupied port = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Dirs.State = filepath.Join(root, "file", "state")
	if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "creating state directory") {
		t.Fatalf("bad state dir = %v", err)
	}
}

// Git sources need git; path sources alone do not.
func TestRunWithoutGit(t *testing.T) {
	opts, root := setup(t, false)
	t.Setenv("PATH", t.TempDir())
	if err := Run(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "listing git's repository variables") {
		t.Fatalf("Run without git = %v", err)
	}
	write(t, opts.ConfigPath, "listen: 127.0.0.1:0\nsources:\n  - name: local\n    kind: path\n    path: "+filepath.Join(root, "local")+"\n    roots: [{path: agent, invocation: model}]\n")
	ready := make(chan string, 1)
	opts.Ready = func(addr string) { ready <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, opts) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("path-only daemon exited: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("path-only daemon did not become ready")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("shutdown = %v", err)
	}
}

func TestBackgroundLoopsAndOfflineRestart(t *testing.T) {
	old := sweepInterval
	sweepInterval = 50 * time.Millisecond
	defer func() { sweepInterval = old }()
	opts, root := setup(t, true)
	cfg, _ := os.ReadFile(opts.ConfigPath)
	write(t, opts.ConfigPath, strings.Replace(string(cfg), "fetch_interval: 1h", "fetch_interval: 50ms", 1))

	runFor := func(d time.Duration) {
		t.Helper()
		ready := make(chan string, 1)
		opts.Ready = func(addr string) { ready <- addr }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, opts) }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("Run exited early: %v", err)
		}
		time.Sleep(d) // let the refresh and sweep loops tick
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	runFor(300 * time.Millisecond)

	// Restart with the remote gone: startup logs warnings and still serves.
	if err := os.Rename(filepath.Join(root, "up.git"), filepath.Join(root, "gone.git")); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	opts.Logger = slog.New(slog.NewTextHandler(&syncWriter{w: &logs}, nil))
	runFor(150 * time.Millisecond)
	out := logs.String()
	for _, want := range []string{"refreshing source failed", "remote of a writable source is unreachable"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
