// Package render formats skill tool results and splits them into chunks that
// fit a harness's result budget.
package render

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
)

const maxDiagnosticsBytes = 2048

var attrEscaper = strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;")

// Attr is one element attribute.
type Attr struct{ Name, Value string }

func element(name string, attrs []Attr) string {
	var b strings.Builder
	b.WriteString("<" + name)
	for _, a := range attrs {
		b.WriteString(" " + a.Name + `="` + attrEscaper.Replace(a.Value) + `"`)
	}
	b.WriteString(">")
	return b.String()
}

// Chunk describes content to deliver in budget-sized pieces.
type Chunk struct {
	Element string // wrapper element name
	Attrs   []Attr
	Content []byte
	// First is appended after the closing element on the chunk that starts
	// at offset 0 (bundled files, diagnostics).
	First string
	// PinName and PinValue identify the content in continuations
	// ("revision" for snapshots, "sha" for change files).
	PinName, PinValue string
	Offset            int
	Budget            int
}

// ErrOffset reports an offset outside the content or inside a rune.
var ErrOffset = errors.New("invalid offset")

// Render returns the chunk that starts at Offset.
func (c Chunk) Render() (string, error) {
	total := len(c.Content)
	if c.Offset < 0 || c.Offset > total {
		return "", fmt.Errorf("%w: offset %d is outside 0..%d", ErrOffset, c.Offset, total)
	}
	if c.Offset < total && !utf8.RuneStart(c.Content[c.Offset]) {
		return "", fmt.Errorf("%w: offset %d is not on a UTF-8 rune boundary", ErrOffset, c.Offset)
	}
	open := element(c.Element, c.Attrs) + "\n"
	closing := "\n</" + c.Element + ">\n"
	first := ""
	if c.Offset == 0 {
		first = c.First
	}
	rest := c.Content[c.Offset:]
	fixed := len(open) + len(closing) + len(first)
	if fixed+len(rest) <= c.Budget {
		return open + string(rest) + closing + first, nil
	}
	// next_offset never has more digits than total_bytes.
	contLen := len(c.continuation(total))
	available := c.Budget - fixed - contLen
	slice := cut(rest, available)
	if len(slice) == 0 {
		return "", fmt.Errorf("result overhead of %d bytes leaves no room for content within the %d-byte budget", fixed+contLen, c.Budget)
	}
	next := c.Offset + len(slice)
	return open + string(slice) + closing + first + c.continuation(next), nil
}

func (c Chunk) continuation(next int) string {
	n := strconv.Itoa(next)
	attrs := []Attr{{c.PinName, c.PinValue}, {"next_offset", n}, {"total_bytes", strconv.Itoa(len(c.Content))}}
	return element("continuation", attrs) +
		fmt.Sprintf("Call this tool again with the same arguments plus %s %q and offset %s to read the rest.", c.PinName, c.PinValue, n) +
		"</continuation>\n"
}

// cut returns the longest prefix of b within limit bytes that ends at a
// newline, or failing that at a rune boundary.
func cut(b []byte, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if limit >= len(b) {
		return b
	}
	window := b[:limit]
	if i := strings.LastIndexByte(string(window), '\n'); i >= 0 {
		return window[:i+1]
	}
	end := limit
	for end > 0 && !utf8.RuneStart(b[end]) {
		end--
	}
	return b[:end]
}

// SkillLoad formats a skill load (SKILL.md body plus bundled files).
func SkillLoad(s *catalog.Skill, offset, budget int, diagnostics string) (string, error) {
	return Chunk{
		Element: "skill",
		Attrs: []Attr{
			{"name", s.Name}, {"source", s.Source}, {"bundle", s.Bundle},
			{"revision", s.Revision}, {"base_dir", s.BaseDir},
		},
		Content:  []byte(s.Body),
		First:    bundledFiles(s.Files) + diagnostics,
		PinName:  "revision",
		PinValue: s.Revision,
		Offset:   offset,
		Budget:   budget,
	}.Render()
}

func bundledFiles(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return "<bundled_files>\n" + strings.Join(files, "\n") + "\n</bundled_files>\n"
}

// SkillFile formats one bundled file.
func SkillFile(s *catalog.Skill, rel, absPath string, content []byte, offset, budget int) (string, error) {
	attrs := []Attr{{"skill", s.Name}, {"path", rel}, {"revision", s.Revision}, {"abs_path", absPath}}
	if catalog.IsBinary(content) {
		attrs = append(attrs, Attr{"binary", "true"}, Attr{"bytes", strconv.Itoa(len(content))})
		return element("skill_file", attrs) + "</skill_file>\n", nil
	}
	return Chunk{
		Element: "skill_file", Attrs: attrs, Content: content,
		PinName: "revision", PinValue: s.Revision, Offset: offset, Budget: budget,
	}.Render()
}

// Diagnostics formats the <catalog_diagnostics> element, capped at 2,048
// bytes; it is empty when there are no diagnostics.
func Diagnostics(ds []catalog.Diagnostic) string {
	if len(ds) == 0 {
		return ""
	}
	head := element("catalog_diagnostics", []Attr{{"count", strconv.Itoa(len(ds))}}) + "\n"
	tail := "These skills are not loaded. Tell the user; `skills-mcp list` shows details.\n</catalog_diagnostics>\n"
	var lines []string
	size := len(head) + len(tail)
	for i, d := range ds {
		line := path.Base(d.Bundle) + " " + d.String() + "\n"
		more := fmt.Sprintf("and %d more\n", len(ds)-i-1)
		if i == len(ds)-1 {
			more = ""
		}
		if size+len(line)+len(more) > maxDiagnosticsBytes {
			lines = append(lines, fmt.Sprintf("and %d more\n", len(ds)-i))
			break
		}
		lines = append(lines, line)
		size += len(line)
	}
	return head + strings.Join(lines, "") + tail
}
