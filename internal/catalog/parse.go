package catalog

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

const (
	maxNameLength        = 64
	maxDescriptionLength = 1024
)

// NamePattern is the pattern skill names, authors, and served names match.
var NamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// AuthoringTools lists the authoring tool names; skills may not collide with
// them after pi maps "-" to "_".
var AuthoringTools = []string{
	"skill_change_open", "skill_change_read", "skill_create", "skill_edit",
	"skill_write_file", "skill_change_diff", "skill_change_submit", "skill_change_discard",
}

var errNoFrontmatter = errors.New("SKILL.md must start with a --- line and contain a closing --- line")

// SplitFrontmatter separates the YAML frontmatter block from the body.
func SplitFrontmatter(content []byte) (frontmatter []byte, body string, err error) {
	text := string(content)
	first, rest, ok := strings.Cut(text, "\n")
	if !ok || strings.TrimRight(first, "\r") != "---" {
		return nil, "", errNoFrontmatter
	}
	offset := 0
	for line := range strings.SplitAfterSeq(rest, "\n") {
		if strings.TrimRight(line, "\r\n") == "---" {
			return []byte(rest[:offset]), rest[offset+len(line):], nil
		}
		offset += len(line)
	}
	return nil, "", errNoFrontmatter
}

// Frontmatter holds the fields the server reads.
type Frontmatter struct {
	Name        string
	Description string
	Author      string
	HasAuthor   bool
	// keys that are present but not strings
	badName, badDescription, badAuthor bool
}

// ParseFrontmatter decodes the frontmatter block, which must be a mapping.
func ParseFrontmatter(block []byte) (Frontmatter, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(block, &doc); err != nil {
		return Frontmatter{}, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return Frontmatter{}, errors.New("frontmatter is not a YAML mapping")
	}
	var fm Frontmatter
	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i].Value, m.Content[i+1]
		str, isStr := scalarString(val)
		switch key {
		case "name":
			fm.Name, fm.badName = str, !isStr
		case "description":
			fm.Description, fm.badDescription = str, !isStr
		case "author":
			fm.Author, fm.badAuthor, fm.HasAuthor = str, !isStr, true
		}
	}
	return fm, nil
}

func scalarString(n *yaml.Node) (string, bool) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return "", false
	}
	return n.Value, true
}

// PrefixRule is a source's author-prefix configuration.
type PrefixRule struct {
	Enabled bool
	Except  []string
}

// ServedName applies the author prefix to a skill name.
func (r PrefixRule) ServedName(name string, fm Frontmatter) string {
	if !r.Enabled || !fm.HasAuthor || slices.Contains(r.Except, fm.Author) {
		return name
	}
	prefix := strings.ToLower(fm.Author) + "-"
	if strings.HasPrefix(name, prefix) {
		return name
	}
	return prefix + name
}

// Parsed is a validated SKILL.md.
type Parsed struct {
	Frontmatter Frontmatter
	ServedName  string
	Body        string
}

// ParseSkill validates SKILL.md for a bundle directory named dirName and
// returns the parsed skill and its structural issues. Each issue is a
// (code, message) pair; Parsed is meaningful only without issues, except
// that ServedName is always the precedence key.
func ParseSkill(content []byte, dirName string, rule PrefixRule) (Parsed, []TemplateIssue) {
	p := Parsed{ServedName: dirName}
	block, body, err := SplitFrontmatter(content)
	if err != nil {
		return p, []TemplateIssue{{Code: CodeInvalidFrontmatter, Message: err.Error()}}
	}
	fm, err := ParseFrontmatter(block)
	if err != nil {
		return p, []TemplateIssue{{Code: CodeInvalidFrontmatter, Message: "frontmatter: " + firstLine(err.Error())}}
	}
	p.Frontmatter, p.Body = fm, body
	var issues []TemplateIssue
	add := func(code, msg string) { issues = append(issues, TemplateIssue{Code: code, Message: msg}) }

	authorOK := !rule.Enabled || !fm.HasAuthor || (!fm.badAuthor && NamePattern.MatchString(strings.ToLower(fm.Author)))
	if !authorOK {
		add(CodeInvalidAuthor, "author must be a string whose lowercase form matches "+NamePattern.String())
	}
	switch {
	case fm.badName || fm.Name == "":
		add(CodeInvalidName, "name must be a non-empty string")
	case !validName(fm.Name):
		add(CodeInvalidName, "name must be 1-64 characters matching "+NamePattern.String())
	case fm.Name != dirName:
		add(CodeNameMismatch, "name "+quote(fm.Name)+" does not match directory "+quote(dirName))
	}
	if authorOK {
		p.ServedName = rule.ServedName(dirName, fm)
	}
	if !validName(p.ServedName) {
		add(CodeInvalidName, "served name "+quote(p.ServedName)+" must be 1-64 characters matching "+NamePattern.String())
	}
	if slices.Contains(AuthoringTools, strings.ReplaceAll(p.ServedName, "-", "_")) {
		add(CodeReservedName, "served name "+quote(p.ServedName)+" collides with an authoring tool")
	}
	switch {
	case fm.badDescription || fm.Description == "":
		add(CodeInvalidDescription, "description must be a non-empty string")
	case utf8.RuneCountInString(fm.Description) > maxDescriptionLength:
		add(CodeInvalidDescription, "description exceeds 1,024 characters")
	}
	return p, issues
}

func validName(name string) bool {
	return len(name) <= maxNameLength && NamePattern.MatchString(name)
}

func quote(s string) string { return `"` + s + `"` }

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
