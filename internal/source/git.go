package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
)

// Repo is a bare clone of a git source.
type Repo struct {
	Git    *gitx.Git
	Dir    string // repo.git
	URL    string
	Branch string
}

// Ref is the tracked branch ref.
func (r *Repo) Ref() string { return "refs/heads/" + r.Branch }

// Exists reports whether the bare clone exists.
func (r *Repo) Exists() bool {
	_, err := os.Stat(r.Dir)
	return err == nil
}

// Clone clones into a temporary directory and renames it into place, so an
// interrupted clone never leaves a partial repo.git behind.
func (r *Repo) Clone(ctx context.Context) error {
	parent := filepath.Dir(r.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", parent, err)
	}
	tmp, err := os.MkdirTemp(parent, filepath.Base(r.Dir)+".tmp-")
	if err != nil {
		return fmt.Errorf("creating clone directory: %w", err)
	}
	if _, err := r.Git.Run(ctx, parent, gitx.Opts{}, "clone", "--bare", "--quiet", "--branch", r.Branch, r.URL, tmp); err != nil {
		_ = os.RemoveAll(tmp) // best-effort cleanup; the clone error is returned
		return fmt.Errorf("cloning %s: %w", r.URL, err)
	}
	if err := os.Rename(tmp, r.Dir); err != nil {
		_ = os.RemoveAll(tmp) // best-effort cleanup; the rename error is returned
		return fmt.Errorf("renaming clone into place: %w", err)
	}
	return nil
}

// Fetch updates the tracked branch ref, first pointing origin at the
// configured URL when it changed.
func (r *Repo) Fetch(ctx context.Context) error {
	current, err := r.Git.Output(ctx, r.Dir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("reading origin URL: %w", err)
	}
	if current != r.URL {
		if _, err := r.Git.Run(ctx, r.Dir, gitx.Opts{}, "remote", "set-url", "origin", r.URL); err != nil {
			return fmt.Errorf("updating origin URL: %w", err)
		}
	}
	if _, err := r.Git.Run(ctx, r.Dir, gitx.Opts{}, "fetch", "--quiet", "origin", "+"+r.Ref()+":"+r.Ref()); err != nil {
		return fmt.Errorf("fetching %s: %w", r.URL, err)
	}
	return nil
}

// Tip returns the commit at the tracked branch ref.
func (r *Repo) Tip(ctx context.Context) (string, error) {
	sha, err := r.Git.Output(ctx, r.Dir, "rev-parse", "--verify", r.Ref()+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", r.Ref(), err)
	}
	return sha, nil
}

type treeEntry struct {
	mode, kind, oid, path string
}

// lsTree lists the entries of sha under the given repo-relative roots.
func (r *Repo) lsTree(ctx context.Context, sha string, roots []string) ([]treeEntry, error) {
	args := append([]string{"ls-tree", "-r", "-z", sha, "--"}, roots...)
	out, err := r.Git.Run(ctx, r.Dir, gitx.Opts{}, args...)
	if err != nil {
		return nil, fmt.Errorf("listing tree %s: %w", sha, err)
	}
	var entries []treeEntry
	for _, rec := range gitx.SplitNUL(out) {
		meta, p, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ls-tree record %q", rec)
		}
		entries = append(entries, treeEntry{mode: fields[0], kind: fields[1], oid: fields[2], path: p})
	}
	return entries, nil
}

// populate writes the blobs of sha under roots into dir. Symlinks and
// submodules are recorded instead of written.
func (r *Repo) populate(sha string, roots []string) func(ctx context.Context, dir string) ([]special, error) {
	return func(ctx context.Context, dir string) ([]special, error) {
		entries, err := r.lsTree(ctx, sha, roots)
		if err != nil {
			return nil, err
		}
		var specials []special
		var blobs []treeEntry
		for _, e := range entries {
			switch {
			case e.mode == "120000":
				specials = append(specials, special{path: e.path, kind: specialSymlink, target: e.oid})
			case e.kind == "commit":
				specials = append(specials, special{path: e.path, kind: specialSubmodule, target: e.oid})
			case e.kind == "blob":
				blobs = append(blobs, e)
			}
		}
		return specials, r.writeBlobs(ctx, dir, blobs)
	}
}

// writeBlobs streams every blob through one `git cat-file --batch`.
func (r *Repo) writeBlobs(ctx context.Context, dir string, blobs []treeEntry) error {
	if len(blobs) == 0 {
		return nil
	}
	var stdin strings.Builder
	for _, b := range blobs {
		stdin.WriteString(b.oid + "\n")
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := readBatch(bufio.NewReader(pr), dir, blobs)
		_ = pr.CloseWithError(err) // unblocks git if parsing stopped early
		done <- err
	}()
	_, runErr := r.Git.Run(ctx, r.Dir, gitx.Opts{Stdin: strings.NewReader(stdin.String()), Stdout: pw}, "cat-file", "--batch")
	_ = pw.CloseWithError(runErr) // a nil error closes the pipe normally
	if err := errors.Join(runErr, <-done); err != nil {
		return fmt.Errorf("reading blobs: %w", err)
	}
	return nil
}

func readBatch(br *bufio.Reader, dir string, blobs []treeEntry) error {
	for _, b := range blobs {
		header, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading batch header for %s: %w", b.path, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != b.oid {
			return fmt.Errorf("unexpected batch header %q for %s", strings.TrimSpace(header), b.path)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return fmt.Errorf("parsing blob size %q: %w", fields[2], err)
		}
		mode := fs.FileMode(0o644)
		if b.mode == "100755" {
			mode = 0o755
		}
		if err := writeBlob(filepath.Join(dir, filepath.FromSlash(b.path)), io.LimitReader(br, size), mode); err != nil {
			return err
		}
		if _, err := br.ReadByte(); err != nil { // the newline after each blob
			return fmt.Errorf("reading batch separator: %w", err)
		}
	}
	return nil
}

func writeBlob(p string, r io.Reader, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, r)
	return errors.Join(copyErr, f.Close())
}
