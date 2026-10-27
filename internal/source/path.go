package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const watchDebounce = 500 * time.Millisecond

// populatePath copies the regular files under the roots of a path source.
// Symbolic links are recorded with their targets, never followed or copied.
func populatePath(root string, roots []string) func(ctx context.Context, dir string) ([]special, error) {
	return func(_ context.Context, dir string) ([]special, error) {
		var specials []special
		for _, r := range roots {
			base := filepath.Join(root, filepath.FromSlash(r))
			if err := checkRootComponents(root, r); err != nil {
				return nil, err
			}
			err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
				if errors.Is(err, fs.ErrNotExist) && p == base {
					return fs.SkipAll
				}
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				switch {
				case d.Type()&fs.ModeSymlink != 0:
					target, err := os.Readlink(p)
					if err != nil {
						return err
					}
					specials = append(specials, special{path: filepath.ToSlash(rel), kind: specialSymlink, target: target})
				case d.Type().IsRegular():
					return copyFile(p, filepath.Join(dir, rel))
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("copying %s: %w", base, err)
			}
		}
		return specials, nil
	}
}

// checkRootComponents refuses a root whose path below the source contains a
// symbolic link, which would serve content from outside the source.
func checkRootComponents(root, rel string) error {
	cur := root
	for part := range strings.SplitSeq(rel, "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("root %s: %s is a symbolic link", rel, cur)
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeBlob(dst, in, info.Mode().Perm()|0o200)
}

// Watch refreshes path sources on filesystem changes, debounced, until ctx
// ends. Directories created later are added to the watch.
func (m *Manager) Watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer w.Close()
	owner := map[string]string{} // watched directory → source name
	for _, st := range m.sources {
		if st.pathRoot == "" {
			continue
		}
		for _, r := range st.src.AllRootPaths() {
			addTree(w, filepath.Join(st.pathRoot, filepath.FromSlash(r)), st.src.Name, owner, m.opts.Logger)
		}
	}
	pending := map[string]bool{}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			name := ownerOf(owner, ev.Name)
			if name == "" {
				continue
			}
			if ev.Has(fsnotify.Create) {
				if info, err := os.Lstat(ev.Name); err == nil && info.IsDir() {
					addTree(w, ev.Name, name, owner, m.opts.Logger)
				}
			}
			pending[name] = true
			timer.Reset(watchDebounce)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			m.opts.Logger.Warn("watch error", "error", err)
		case <-timer.C:
			for name := range pending {
				if err := m.Refresh(ctx, name, RefreshOptions{}); err != nil {
					m.opts.Logger.Warn("refresh failed", "source", name, "error", err)
				}
			}
			clear(pending)
		}
	}
}

func addTree(w *fsnotify.Watcher, dir, source string, owner map[string]string, log *slog.Logger) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a vanished directory needs no watch
		}
		if d.IsDir() {
			if err := w.Add(p); err != nil {
				return err
			}
			owner[p] = source
		}
		return nil
	})
	if err != nil {
		log.Warn("watch failed", "dir", dir, "error", err)
	}
}

func ownerOf(owner map[string]string, p string) string {
	for dir := p; ; dir = filepath.Dir(dir) {
		if name, ok := owner[dir]; ok {
			return name
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}
