package catalog

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
)

// RendererVersion changes whenever rendering or ignore semantics change, so
// snapshots rendered by an older daemon are discarded.
const RendererVersion = 1

var (
	tagOpen = []byte("<%")
	varTag  = regexp.MustCompile(`^<%-[ \t]+([a-z][a-z0-9_]*)[ \t]+%>`)
	envTag  = regexp.MustCompile(`^<%-[ \t]+env\("([A-Z_][A-Z0-9_]*)"(,[ \t]+"([^"\\]*)")?\)[ \t]+%>`)
)

// TemplateContext is the fixed rendering context of one source.
type TemplateContext struct {
	Vars   map[string]string // resolved values; "{snapshot}" already replaced
	Env    []string          // names env() may read
	Getenv func(string) (string, bool)
}

// TemplateIssue is one rejected tag.
type TemplateIssue struct {
	Code    string
	Line    int
	Message string
}

// Render replaces every accepted tag in content. filePath is the absolute
// final path of the file, which file_dir and file_path render to. Rendering
// is a single pass; any issue means the output must not be served.
func Render(content []byte, filePath string, ctx TemplateContext) ([]byte, []TemplateIssue) {
	var out bytes.Buffer
	var issues []TemplateIssue
	rest := content
	offset := 0
	for {
		i := bytes.Index(rest, tagOpen)
		if i < 0 {
			out.Write(rest)
			break
		}
		out.Write(rest[:i])
		tag := rest[i:]
		line := bytes.Count(content[:offset+i], []byte("\n")) + 1
		value, n, issue := expandTag(tag, filePath, ctx)
		if issue != nil {
			issue.Line = line
			issues = append(issues, *issue)
			n = len(tagOpen)
			value = string(tagOpen)
		}
		out.WriteString(value)
		rest = tag[n:]
		offset += i + n
	}
	return out.Bytes(), issues
}

func expandTag(tag []byte, filePath string, ctx TemplateContext) (string, int, *TemplateIssue) {
	if m := varTag.FindSubmatch(tag); m != nil {
		name := string(m[1])
		switch name {
		case "file_dir":
			return filepath.Dir(filePath), len(m[0]), nil
		case "file_path":
			return filePath, len(m[0]), nil
		}
		v, ok := ctx.Vars[name]
		if !ok {
			return "", 0, &TemplateIssue{Code: CodeUnknownTemplateVariable, Message: fmt.Sprintf("unknown template variable %q", name)}
		}
		return v, len(m[0]), nil
	}
	if m := envTag.FindSubmatch(tag); m != nil {
		name := string(m[1])
		if !slices.Contains(ctx.Env, name) {
			return "", 0, &TemplateIssue{Code: CodeEnvNotAllowed, Message: fmt.Sprintf("environment variable %q is not in template.env", name)}
		}
		if v, ok := ctx.Getenv(name); ok {
			return v, len(m[0]), nil
		}
		if m[2] != nil {
			return string(m[3]), len(m[0]), nil
		}
		return "", 0, &TemplateIssue{Code: CodeEnvUnset, Message: fmt.Sprintf("environment variable %q is unset and has no fallback", name)}
	}
	return "", 0, &TemplateIssue{Code: CodeUnsupportedTemplateTag, Message: fmt.Sprintf("unsupported template tag %q", tagPreview(tag))}
}

// tagPreview returns the tag up to its closing "%>" or the end of the line.
func tagPreview(tag []byte) string {
	end := len(tag)
	if i := bytes.Index(tag, []byte("%>")); i >= 0 {
		end = i + 2
	}
	if i := bytes.IndexByte(tag[:end], '\n'); i >= 0 {
		end = i
	}
	const maxPreview = 80
	if end > maxPreview {
		end = maxPreview
	}
	return string(tag[:end])
}

// IsBinary reports whether content has a NUL byte in its first 8,000 bytes.
func IsBinary(content []byte) bool {
	const probe = 8000
	return bytes.IndexByte(content[:min(len(content), probe)], 0) >= 0
}
