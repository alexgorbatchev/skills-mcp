package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/proc"
)

type prepareRun struct {
	bundle string
	dir    string // repo-relative directory the rule runs in
}

// prepare runs the source's prepare rules. Matching directories are computed
// once from the filtered source files before any rule runs, so directories a
// rule creates are never matched. A cancellation or caller deadline aborts
// the extraction with an error; a rule's own failure refuses the bundle.
func (e *extraction) prepare(ctx context.Context, dir string, rec *catalog.RenderRecord) error {
	if len(e.src.Prepare) == 0 {
		return nil
	}
	runs := make([][]prepareRun, len(e.src.Prepare))
	for _, bundle := range rec.Bundles {
		if len(rec.BundleDiagnostics(bundle)) > 0 {
			continue
		}
		if e.prepareOnly != nil && !e.prepareOnly[bundle] {
			e.inheritPrepareDiagnostics(bundle, rec)
			continue
		}
		for i, rule := range e.src.Prepare {
			for _, f := range rec.Files[bundle] {
				if path.Base(f) == rule.WhenFile {
					runs[i] = append(runs[i], prepareRun{bundle: bundle, dir: path.Join(bundle, path.Dir(f))})
				}
			}
		}
	}
	failed := map[string]bool{}
	for i, rule := range e.src.Prepare {
		for _, run := range runs[i] {
			if failed[run.bundle] {
				continue
			}
			if err := e.shutdown.Err(); err != nil {
				return fmt.Errorf("prepare interrupted: %w", errShutdown)
			}
			abs := filepath.Join(dir, filepath.FromSlash(run.dir))
			_, err := proc.Run(ctx, proc.Command{Name: rule.Run[0], Args: rule.Run[1:], Dir: abs, Timeout: rule.Timeout})
			if errors.Is(err, proc.ErrCanceled) {
				return fmt.Errorf("prepare interrupted: %w", err)
			}
			// Whatever the outcome, no installed file may share an inode
			// with a package cache before permissions are set; a file that
			// cannot be unshared aborts the extraction.
			linkErr, unshareErr := checkPrepareOutput(abs)
			if unshareErr != nil {
				return fmt.Errorf("unsharing prepare output: %w", unshareErr)
			}
			if err == nil {
				err = linkErr
			}
			if err != nil {
				failed[run.bundle] = true
				rec.PrepareRetry = true
				rec.Diagnostics = append(rec.Diagnostics, catalog.Diagnostic{
					Code: catalog.CodePrepareFailed, Source: e.src.Name, Bundle: run.bundle,
					File:    strings.TrimPrefix(strings.TrimPrefix(run.dir, run.bundle), "/"),
					Message: "prepare " + strings.Join(rule.Run, " ") + ": " + prepareMessage(err),
				})
			}
		}
	}
	return nil
}

func (e *extraction) inheritPrepareDiagnostics(bundle string, rec *catalog.RenderRecord) {
	if e.baseRecord == nil {
		return
	}
	for _, d := range e.baseRecord.BundleDiagnostics(bundle) {
		if d.Code == catalog.CodePrepareFailed {
			rec.Diagnostics = append(rec.Diagnostics, d)
		}
	}
}

func prepareMessage(err error) string {
	var exitErr *proc.ExitError
	if errors.As(err, &exitErr) {
		if line := proc.LastLine(exitErr.Stderr); line != "" {
			return fmt.Sprintf("exit status %d: %s", exitErr.Code, line)
		}
		return fmt.Sprintf("exit status %d", exitErr.Code)
	}
	return err.Error()
}

var (
	errLinkEscapes = errors.New("resolves outside its directory")
	errShutdown    = errors.New("interrupted by shutdown")
)

// checkPrepareOutput walks the whole directory: it replaces every
// multiply-linked file with a private copy, returning the first symbolic
// link that does not resolve inside dir and, separately, every failure to
// unshare or walk.
func checkPrepareOutput(dir string) (linkErr, unshareErr error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var unshareErrs []error
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			unshareErrs = append(unshareErrs, err)
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if linkErr != nil {
				return nil
			}
			rel, _ := filepath.Rel(dir, p) // p is below dir
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				linkErr = fmt.Errorf("link %s does not resolve: %w", filepath.ToSlash(rel), errLinkEscapes)
			} else if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
				linkErr = fmt.Errorf("link %s %w", filepath.ToSlash(rel), errLinkEscapes)
			}
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				unshareErrs = append(unshareErrs, err)
				return nil
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				if err := unshare(p, info.Mode().Perm()); err != nil {
					unshareErrs = append(unshareErrs, err)
				}
			}
		}
		return nil
	})
	return linkErr, errors.Join(append(unshareErrs, err)...)
}

// unshare replaces a hard-linked file with a private copy of the same mode,
// giving its directory owner write permission for the duration if needed.
func unshare(p string, mode fs.FileMode) error {
	parent := filepath.Dir(p)
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o200 == 0 {
		if err := os.Chmod(parent, perm|0o200); err != nil {
			return err
		}
		defer os.Chmod(parent, perm) // restores the directory's own mode; setPermissions runs later anyway
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	tmp := p + ".unshare"
	if err := os.WriteFile(tmp, data, mode|0o200); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	return os.Rename(tmp, p)
}
