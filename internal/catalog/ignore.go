package catalog

import (
	"bufio"
	"bytes"
	"path"
	"slices"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

const nodeModules = "node_modules"

// IgnoreRules evaluates one bundle's ignore files with the registry's
// semantics: each ignore file at or below the bundle directory applies
// gitignore patterns to paths below its own directory, and its lines starting
// with "./" also exclude that path prefix relative to its directory. Ignore
// files above the bundle are not read.
type IgnoreRules struct {
	fileName string
	matcher  gitignore.Matcher
	prefixes []string
	keep     map[string]bool
}

// NewIgnoreRules builds the rules from the ignore files found in a bundle.
// files maps bundle-relative ignore file paths to their content; keep lists
// bundle-relative paths exempt from filtering.
func NewIgnoreRules(fileName string, files map[string][]byte, keep []string) *IgnoreRules {
	r := &IgnoreRules{fileName: fileName, keep: map[string]bool{}}
	for _, k := range keep {
		r.keep[k] = true
	}
	var patterns []gitignore.Pattern
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	// Deeper ignore files take precedence, as in gitignore.
	slices.SortFunc(keys, func(a, b string) int { return strings.Count(a, "/") - strings.Count(b, "/") })
	for _, file := range keys {
		dir := path.Dir(file)
		var domain []string
		if dir != "." {
			domain = strings.Split(dir, "/")
		}
		sc := bufio.NewScanner(bytes.NewReader(files[file]))
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if rest, ok := strings.CutPrefix(trimmed, "./"); ok {
				prefix := strings.TrimSuffix(path.Join(dir, rest), "/")
				if prefix != "." && prefix != "" {
					r.prefixes = append(r.prefixes, prefix)
				}
				continue
			}
			patterns = append(patterns, gitignore.ParsePattern(line, domain))
		}
	}
	r.matcher = gitignore.NewMatcher(patterns)
	return r
}

// Excluded reports whether a bundle-relative regular file is filtered out.
func (r *IgnoreRules) Excluded(rel string) bool {
	if r.keep[rel] {
		return false
	}
	parts := strings.Split(rel, "/")
	if slices.Contains(parts, nodeModules) {
		return true
	}
	if r.fileName != "" && parts[len(parts)-1] == r.fileName {
		return true
	}
	for _, p := range r.prefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return r.matcher.Match(parts, false)
}
