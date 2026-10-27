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
			abs := filepath.Join(dir, filepath.FromSlash(run.dir))
			_, err := proc.Run(ctx, proc.Command{Name: rule.Run[0], Args: rule.Run[1:], Dir: abs, Timeout: rule.Timeout})
			if errors.Is(err, proc.ErrCanceled) {
				return fmt.Errorf("prepare interrupted: %w", err)
			}
			if err == nil {
				err = checkPrepareOutput(abs)
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

// checkPrepareOutput refuses links that resolve outside dir and replaces
// multiply-linked files with private copies, so the snapshot shares no inode
// with a package cache.
func checkPrepareOutput(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			resolved := target
			if !filepath.IsAbs(target) {
				resolved = filepath.Join(filepath.Dir(p), target)
			}
			if resolved != dir && !strings.HasPrefix(resolved, dir+string(filepath.Separator)) {
				rel, _ := filepath.Rel(dir, p) // p is below dir
				return fmt.Errorf("link %s resolves outside its directory", filepath.ToSlash(rel))
			}
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return unshare(p, info.Mode().Perm())
			}
		}
		return nil
	})
}

// unshare replaces a hard-linked file with a private copy of the same mode.
func unshare(p string, mode fs.FileMode) error {
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	tmp := p + ".unshare"
	if err := os.WriteFile(tmp, data, mode|0o200); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
