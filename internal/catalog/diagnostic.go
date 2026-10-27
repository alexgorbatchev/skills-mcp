// Package catalog parses skill bundles, renders the fixed template context,
// validates bundles, and builds one view of served skills per harness.
package catalog

import (
	"cmp"
	"fmt"
	"slices"
)

// Diagnostic codes.
const (
	CodeInvalidFrontmatter      = "invalid_frontmatter"
	CodeInvalidName             = "invalid_name"
	CodeNameMismatch            = "name_mismatch"
	CodeInvalidDescription      = "invalid_description"
	CodeInvalidAuthor           = "invalid_author"
	CodeReservedName            = "reserved_name"
	CodeDuplicateName           = "duplicate_name"
	CodeSymlink                 = "symlink"
	CodeSubmodule               = "submodule"
	CodeMissingSkillMD          = "missing_skill_md"
	CodePrepareFailed           = "prepare_failed"
	CodeUnsupportedTemplateTag  = "unsupported_template_tag"
	CodeUnknownTemplateVariable = "unknown_template_variable"
	CodeEnvNotAllowed           = "env_not_allowed"
	CodeEnvUnset                = "env_unset"
)

// SkillFile is the name of a bundle's skill definition.
const SkillFile = "SKILL.md"

// Diagnostic explains why a bundle is not served.
type Diagnostic struct {
	Code     string `json:"code"`
	Source   string `json:"source"`
	Revision string `json:"revision"`
	Bundle   string `json:"bundle"`
	File     string `json:"file"`
	Line     *int   `json:"line"`
	Message  string `json:"message"`
}

func (d Diagnostic) location() string {
	if d.Line == nil {
		return fmt.Sprintf("%s:%s/%s", d.Source, d.Bundle, d.File)
	}
	return fmt.Sprintf("%s:%s/%s:%d", d.Source, d.Bundle, d.File, *d.Line)
}

// String renders the diagnostic as "<location> <message>".
func (d Diagnostic) String() string {
	return d.location() + " " + d.Message
}

// SortDiagnostics orders diagnostics by source, bundle, file, and line.
func SortDiagnostics(ds []Diagnostic) {
	slices.SortStableFunc(ds, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Source, b.Source),
			cmp.Compare(a.Bundle, b.Bundle),
			cmp.Compare(a.File, b.File),
			cmp.Compare(lineOf(a), lineOf(b)),
			cmp.Compare(a.Code, b.Code),
		)
	})
}

func lineOf(d Diagnostic) int {
	if d.Line == nil {
		return 0
	}
	return *d.Line
}

// RenderRecord is the persisted result of extracting one snapshot.
type RenderRecord struct {
	Revision      string              `json:"revision"`
	RenderContext string              `json:"render_context"`
	Bundles       []string            `json:"bundles"`
	Files         map[string][]string `json:"files"`
	PrepareRetry  bool                `json:"prepare_retry"`
	Diagnostics   []Diagnostic        `json:"diagnostics"`
}

// BundleDiagnostics returns the record's diagnostics for one bundle.
func (r *RenderRecord) BundleDiagnostics(bundle string) []Diagnostic {
	var out []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Bundle == bundle {
			out = append(out, d)
		}
	}
	return out
}
