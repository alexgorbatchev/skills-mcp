package catalog

import (
	"fmt"
	"io/fs"
	"os"
	"path"
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
	var files []string
	ignoreFiles := map[string][]byte{}
	var keep []string
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
			files = append(files, rel)
			if src.IgnoreFile != "" && d.Name() == src.IgnoreFile {
				data, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				ignoreFiles[rel] = data
			}
			for _, rule := range src.Prepare {
				if d.Name() == rule.WhenFile {
					for _, k := range rule.Keep {
						keep = append(keep, path.Join(path.Dir(rel), k))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var rules *IgnoreRules
	if src.IgnoreFile != "" {
		rules = NewIgnoreRules(src.IgnoreFile, ignoreFiles, keep)
	}
	ctx := TemplateContext{Vars: map[string]string{}, Env: src.Template.Env, Getenv: getenv}
	for k, v := range src.Template.Vars {
		ctx.Vars[k] = v
	}
	for _, rel := range files {
		if rules != nil && rules.Excluded(rel) {
			if rel == SkillFile {
				problems = append(problems, fmt.Sprintf("%s: %s: ignore rules match SKILL.md", CodeMissingSkillMD, rel))
			}
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if rel == SkillFile {
			_, issues := ParseSkill(content, filepath.Base(dir), PrefixRule{Enabled: src.AuthorPrefix, Except: src.AuthorPrefixExcept})
			for _, is := range issues {
				problems = append(problems, fmt.Sprintf("%s: %s: %s", is.Code, rel, is.Message))
			}
		}
		if IsBinary(content) {
			continue
		}
		_, issues := Render(content, filepath.Join(dir, filepath.FromSlash(rel)), ctx)
		for _, is := range issues {
			problems = append(problems, fmt.Sprintf("%s: %s:%d: %s", is.Code, rel, is.Line, is.Message))
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, SkillFile)); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %s: missing", CodeInvalidFrontmatter, SkillFile))
	}
	slices.Sort(problems)
	return problems, nil
}
