package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

// ValidateDir checks a bundle directory on disk with the structural,
// symlink, and template rules of a source, without the bundle-directory
// rule. It returns one line per problem.
func ValidateDir(dir string, src *config.Source, getenv func(string) (string, bool)) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	var problems []string
	ctx := TemplateContext{Vars: map[string]string{}, Env: src.Template.Env, Getenv: getenv}
	for k, v := range src.Template.Vars {
		ctx.Vars[k] = v
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p) // p is below dir
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir() && d.Name() == "node_modules":
			return fs.SkipDir
		case d.Type()&fs.ModeSymlink != 0:
			problems = append(problems, fmt.Sprintf("%s: %s: symbolic links are not served", CodeSymlink, rel))
		case d.Type().IsRegular():
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if rel == SkillFile {
				_, issues := ParseSkill(content, filepath.Base(dir), PrefixRule{Enabled: src.AuthorPrefix, Except: src.AuthorPrefixExcept})
				for _, is := range issues {
					problems = append(problems, fmt.Sprintf("%s: %s: %s", is.Code, rel, is.Message))
				}
			}
			if IsBinary(content) {
				return nil
			}
			_, issues := Render(content, p, ctx)
			for _, is := range issues {
				problems = append(problems, fmt.Sprintf("%s: %s:%d: %s", is.Code, rel, is.Line, is.Message))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(dir, SkillFile)); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %s: missing", CodeInvalidFrontmatter, SkillFile))
	}
	slices.Sort(problems)
	return problems, nil
}
