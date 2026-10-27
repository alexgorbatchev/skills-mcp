// Package daemon runs skills-mcp: startup, the HTTP server, background
// refresh and maintenance, and the shutdown sequence.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/alexgorbatchev/skills-mcp/internal/authoring"
	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/mcpserver"
	"github.com/alexgorbatchev/skills-mcp/internal/paths"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
	"github.com/alexgorbatchev/skills-mcp/internal/token"
)

// Shutdown timings; the whole sequence fits systemd's 60-second stop timeout.
const (
	httpShutdownTimeout = 5 * time.Second
	pipelineWait        = 30 * time.Second
	killWait            = 10 * time.Second
)

// sweepInterval is how often changes are swept and repositories maintained;
// a variable so tests can shorten it.
var sweepInterval = time.Hour

// Options configures a daemon run.
type Options struct {
	ConfigPath string
	Dirs       paths.Dirs
	Logger     *slog.Logger
	Version    string
	// Ready, when set, receives the listening address once serving starts.
	Ready func(addr string)
}

// ErrLocked reports that another daemon holds the lock.
var ErrLocked = errors.New("another skills-mcp daemon is running")

// Run starts the daemon and blocks until ctx ends, then shuts down.
func Run(ctx context.Context, opts Options) error {
	log := opts.Logger
	unlock, err := lock(opts.Dirs.LockFile())
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	tok, err := token.Ensure(opts.Dirs.TokenFile())
	if err != nil {
		return err
	}

	shutdown, beginShutdown := context.WithCancel(context.WithoutCancel(ctx))
	defer beginShutdown()
	kill, killNow := context.WithCancel(context.WithoutCancel(ctx))
	defer killNow()

	g := gitx.New(kill)
	var svc *authoring.Service
	var servers *mcpserver.Servers
	src := source.NewManager(source.Options{
		Config: cfg, Dirs: opts.Dirs, Git: g, Logger: log, Shutdown: shutdown,
		BaseRevisions: func() map[string]map[string]bool {
			if svc == nil {
				return nil
			}
			return svc.BaseRevisions()
		},
		OnViews: func(v map[config.HarnessID]*catalog.View) { servers.ApplyViews(v) },
	})
	if cfg.DefaultWriteSource != "" {
		svc = authoring.NewService(authoring.Options{
			Config: cfg, Dirs: opts.Dirs, Git: g, Sources: src, Logger: log, Shutdown: shutdown, Kill: kill,
		})
	}
	servers = mcpserver.New(mcpserver.Options{Config: cfg, Sources: src, Authoring: svc, Logger: log, Version: opts.Version})

	if err := startup(kill, cfg, src, svc, log); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}
	httpServer := &http.Server{Handler: servers.Handler(tok), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(ln) }()
	log.Info("serving", "address", ln.Addr().String())
	if opts.Ready != nil {
		opts.Ready(ln.Addr().String())
	}

	var background sync.WaitGroup
	startBackground(shutdown, kill, &background, cfg, src, svc, log)

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		log.Error("http server stopped", "error", err)
	}
	log.Info("shutting down")
	beginShutdown()
	servers.CloseSessions()
	stopCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	if err := httpServer.Shutdown(stopCtx); err != nil {
		_ = httpServer.Close() // remaining connections are cut; Shutdown's error is the timeout
	}
	cancel()
	if svc != nil && !svc.Wait(pipelineWait) {
		killNow()
		svc.Wait(killWait)
	}
	killNow()
	background.Wait()
	return nil
}

// startup runs the startup sequence after the lock, config, and token.
func startup(ctx context.Context, cfg *config.Config, src *source.Manager, svc *authoring.Service, log *slog.Logger) error {
	if err := src.Init(); err != nil {
		return err
	}
	if svc != nil {
		if err := svc.Load(); err != nil {
			return err
		}
	}
	for _, s := range cfg.Sources {
		if s.Kind != config.KindGit {
			continue
		}
		if err := src.Refresh(ctx, s.Name, source.RefreshOptions{SuppressRebuild: true}); err != nil {
			log.Warn("refreshing source failed", "source", s.Name, "error", err)
		}
		if repo := src.Repo(s.Name); s.Writable && repo.Exists() {
			if _, err := repo.Git.Run(ctx, repo.Dir, gitx.Opts{}, "ls-remote", "origin", repo.Ref()); err != nil {
				log.Warn("the remote of a writable source is unreachable; pushes will fail", "source", s.Name, "error", err)
			}
		}
	}
	for _, s := range cfg.Sources {
		if s.Kind != config.KindPath {
			continue
		}
		if err := src.Refresh(ctx, s.Name, source.RefreshOptions{SuppressRebuild: true}); err != nil {
			log.Warn("refreshing source failed", "source", s.Name, "error", err)
		}
	}
	if svc != nil {
		svc.Recover(ctx)
	}
	return src.RebuildViews()
}

// startBackground starts the path watchers, the refresh loop, and the
// hourly sweep and maintenance. They stop scheduling work at shutdown; a
// running refresh finishes or is killed with the kill context.
func startBackground(shutdown, kill context.Context, wg *sync.WaitGroup, cfg *config.Config, src *source.Manager, svc *authoring.Service, log *slog.Logger) {
	wg.Go(func() {
		if err := src.Watch(shutdown); err != nil {
			log.Warn("watching path sources failed", "error", err)
		}
	})
	wg.Go(func() {
		every(shutdown, cfg.FetchInterval, func() {
			for _, name := range src.Sources() {
				if err := src.Refresh(kill, name, source.RefreshOptions{}); err != nil {
					log.Warn("refreshing source failed", "source", name, "error", err)
				}
			}
		})
	})
	wg.Go(func() {
		every(shutdown, sweepInterval, func() {
			if svc != nil {
				svc.Sweep(kill)
			}
			if err := src.Maintain(kill); err != nil {
				log.Warn("git maintenance failed", "error", err)
			}
		})
	})
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// lock takes an exclusive, non-blocking flock on the lock file.
func lock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w (lock file %s)", ErrLocked, path)
	}
	return func() { f.Close() }, nil
}
