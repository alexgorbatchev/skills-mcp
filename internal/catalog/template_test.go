package catalog

import (
	"strings"
	"testing"
)

func testContext(env map[string]string) TemplateContext {
	return TemplateContext{
		Vars: map[string]string{"repo_root": "/repo", "skills_dir": "/repo/skills"},
		Env:  []string{"DOCS", "UNSET", "EMPTY"},
		Getenv: func(name string) (string, bool) {
			v, ok := env[name]
			return v, ok
		},
	}
}

func TestRender(t *testing.T) {
	ctx := testContext(map[string]string{"DOCS": "docs/x", "EMPTY": ""})
	const file = "/snap/skills/a/references/x.md"
	tests := []struct {
		name     string
		in       string
		want     string
		wantCode string
		wantLine int
	}{
		{name: "no tags", in: "plain\ntext", want: "plain\ntext"},
		{name: "variable", in: "root <%- repo_root %>/x", want: "root /repo/x"},
		{name: "tabs and runs", in: "<%-\t\tskills_dir  %>", want: "/repo/skills"},
		{name: "file_dir", in: "<%- file_dir %>", want: "/snap/skills/a/references"},
		{name: "file_path", in: "<%- file_path %>", want: file},
		{name: "env set", in: `<%- env("DOCS", "fallback") %>`, want: "docs/x"},
		{name: "env empty value wins over fallback", in: `<%- env("EMPTY", "fb") %>`, want: ""},
		{name: "env fallback", in: `<%- env("UNSET", "fb") %>`, want: "fb"},
		{name: "env empty fallback", in: `<%- env("UNSET", "") %>`, want: ""},
		{name: "env without fallback set", in: `<%- env("DOCS") %>`, want: "docs/x"},
		{name: "single pass", in: "<%- repo_root %>", want: "/repo"},
		{name: "unknown variable", in: "a\n<%- output_dir %>", wantCode: CodeUnknownTemplateVariable, wantLine: 2},
		{name: "env not allowed", in: `<%- env("HOME", "x") %>`, wantCode: CodeEnvNotAllowed, wantLine: 1},
		{name: "env unset no fallback", in: "\n\n" + `<%- env("UNSET") %>`, wantCode: CodeEnvUnset, wantLine: 3},
		{name: "escaped output", in: "<%= repo_root %>", wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
		{name: "whitespace slurp", in: "<%_ repo_root %>", wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
		{name: "comment", in: "<%# note %>", wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
		{name: "include", in: `<%- include("x") %>`, wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
		{name: "literal in prose", in: "use <% to open", wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
		{name: "no space", in: "<%-repo_root%>", wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
		{name: "lowercase env name", in: `<%- env("lower") %>`, wantCode: CodeUnsupportedTemplateTag, wantLine: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, issues := Render([]byte(tt.in), file, ctx)
			if tt.wantCode == "" {
				if len(issues) != 0 {
					t.Fatalf("issues = %+v", issues)
				}
				if string(got) != tt.want {
					t.Fatalf("Render = %q, want %q", got, tt.want)
				}
				return
			}
			if len(issues) != 1 || issues[0].Code != tt.wantCode || issues[0].Line != tt.wantLine {
				t.Fatalf("issues = %+v, want %s at line %d", issues, tt.wantCode, tt.wantLine)
			}
		})
	}
}

func TestRenderSinglePass(t *testing.T) {
	ctx := testContext(nil)
	ctx.Vars["tricky"] = "<%- repo_root %>"
	got, issues := Render([]byte("<%- tricky %>"), "/f", ctx)
	if len(issues) != 0 || string(got) != "<%- repo_root %>" {
		t.Fatalf("Render = %q, %+v", got, issues)
	}
}

func TestRenderMultipleIssues(t *testing.T) {
	_, issues := Render([]byte("<% a\n<%- nope %>\n<%- repo_root %>"), "/f", testContext(nil))
	if len(issues) != 2 || issues[0].Line != 1 || issues[1].Line != 2 {
		t.Fatalf("issues = %+v", issues)
	}
}

func TestTagPreview(t *testing.T) {
	long := "<%" + strings.Repeat("x", 200)
	if got := tagPreview([]byte(long)); len(got) != 80 {
		t.Fatalf("preview length = %d", len(got))
	}
	if got := tagPreview([]byte("<% a\nb %>")); got != "<% a" {
		t.Fatalf("preview = %q", got)
	}
}

func TestIsBinary(t *testing.T) {
	if IsBinary([]byte("text")) {
		t.Fatal("text reported binary")
	}
	if !IsBinary([]byte("a\x00b")) {
		t.Fatal("NUL not detected")
	}
	late := append([]byte(strings.Repeat("a", 9000)), 0)
	if IsBinary(late) {
		t.Fatal("NUL past the probe window detected")
	}
}
