package render

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
)

var contRe = regexp.MustCompile(`<continuation (?:revision|sha)="([^"]*)" next_offset="(\d+)" total_bytes="(\d+)">`)

// reassemble reads every chunk of c and returns the concatenated content.
func reassemble(t *testing.T, c Chunk) string {
	t.Helper()
	var got strings.Builder
	for i := 0; ; i++ {
		if i > 10000 {
			t.Fatal("too many chunks")
		}
		out, err := c.Render()
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > c.Budget {
			t.Fatalf("chunk of %d bytes exceeds budget %d", len(out), c.Budget)
		}
		open := element(c.Element, c.Attrs) + "\n"
		body := strings.TrimPrefix(out, open)
		end := strings.Index(body, "\n</"+c.Element+">\n")
		got.WriteString(body[:end])
		m := contRe.FindStringSubmatch(out)
		if m == nil {
			return got.String()
		}
		next, _ := strconv.Atoi(m[2])
		if m[1] != c.PinValue || m[3] != strconv.Itoa(len(c.Content)) {
			t.Fatalf("continuation = %v", m)
		}
		c.Offset = next
	}
}

func TestChunkReassembles(t *testing.T) {
	contents := map[string]string{
		"lines":     strings.Repeat("line of text\n", 200),
		"long line": strings.Repeat("x", 3000),
		"multibyte": strings.Repeat("héllo wörld ✓ ", 300),
		"mixed":     strings.Repeat("a\n", 10) + strings.Repeat("é", 500) + "\nend",
		"short":     "tiny",
	}
	for name, content := range contents {
		for _, budget := range []int{400, 1000, 100000} {
			t.Run(fmt.Sprintf("%s/%d", name, budget), func(t *testing.T) {
				c := Chunk{Element: "skill", Attrs: []Attr{{"name", "x"}}, Content: []byte(content), First: "<extra/>\n", PinName: "revision", PinValue: "abc", Budget: budget}
				if got := reassemble(t, c); got != content {
					t.Fatalf("reassembled %d bytes, want %d", len(got), len(content))
				}
			})
		}
	}
}

func TestChunkNewlineCut(t *testing.T) {
	c := Chunk{Element: "e", Content: []byte(strings.Repeat("abcdefgh\n", 100)), PinName: "revision", PinValue: "r", Budget: 300}
	out, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	m := contRe.FindStringSubmatch(out)
	next, _ := strconv.Atoi(m[2])
	if next%9 != 0 || !strings.Contains(out, "abcdefgh\n\n</e>") {
		t.Fatalf("cut not on a newline: next=%d chunk=%q", next, out)
	}
}

func TestChunkFirstOnlyOnFirstChunk(t *testing.T) {
	c := Chunk{Element: "e", Content: []byte(strings.Repeat("z\n", 200)), First: "<bundled_files>\nf\n</bundled_files>\n", PinName: "revision", PinValue: "r", Budget: 300}
	first, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "<bundled_files>") {
		t.Fatal("first chunk lacks First")
	}
	c.Offset = 2
	second, err := c.Render()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second, "<bundled_files>") {
		t.Fatal("later chunk repeats First")
	}
}

func TestChunkErrors(t *testing.T) {
	content := []byte("é" + strings.Repeat("a", 100))
	tests := []struct {
		name   string
		offset int
		budget int
		want   string
	}{
		{"negative offset", -1, 1000, "outside"},
		{"past end", 1000, 1000, "outside"},
		{"mid rune", 1, 1000, "rune boundary"},
		{"overhead exceeds budget", 0, 50, "leaves no room"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Chunk{Element: "e", Content: content, PinName: "revision", PinValue: "r", Offset: tt.offset, Budget: tt.budget}.Render()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	_, err := Chunk{Element: "e", Content: content, Offset: 1}.Render()
	if !errors.Is(err, ErrOffset) {
		t.Fatalf("error %v is not ErrOffset", err)
	}
	if out, err := (Chunk{Element: "e", Content: content, Offset: len(content), Budget: 100}).Render(); err != nil || out != "<e>\n\n</e>\n" {
		t.Fatalf("offset at end = %q, %v", out, err)
	}
}

func TestCut(t *testing.T) {
	if cut([]byte("abc"), 0) != nil {
		t.Fatal("zero limit returned content")
	}
	if string(cut([]byte("éé"), 3)) != "é" {
		t.Fatal("rune boundary cut wrong")
	}
	if string(cut([]byte("ab"), 5)) != "ab" {
		t.Fatal("limit beyond content")
	}
}

func testSkill() *catalog.Skill {
	return &catalog.Skill{
		Name: "golang", Source: "reg", Bundle: "skills/agent/golang", Revision: "3f2a",
		BaseDir: `/cache/a "b"`, Body: "body\n", Files: []string{"references/setup.md", "references/x.md"},
	}
}

func TestSkillLoad(t *testing.T) {
	out, err := SkillLoad(testSkill(), 0, 10000, "<catalog_diagnostics/>\n")
	if err != nil {
		t.Fatal(err)
	}
	want := `<skill name="golang" source="reg" bundle="skills/agent/golang" revision="3f2a" base_dir="/cache/a &quot;b&quot;">` + "\nbody\n\n</skill>\n<bundled_files>\nreferences/setup.md\nreferences/x.md\n</bundled_files>\n<catalog_diagnostics/>\n"
	if out != want {
		t.Fatalf("SkillLoad =\n%s\nwant\n%s", out, want)
	}
	s := testSkill()
	s.Files = nil
	out, _ = SkillLoad(s, 0, 10000, "")
	if strings.Contains(out, "bundled_files") {
		t.Fatal("empty bundled_files emitted")
	}
}

func TestSkillFile(t *testing.T) {
	s := testSkill()
	out, err := SkillFile(s, "references/setup.md", "/abs/setup.md", []byte("content"), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `<skill_file skill="golang" path="references/setup.md" revision="3f2a" abs_path="/abs/setup.md">`) || !strings.Contains(out, "\ncontent\n</skill_file>") {
		t.Fatalf("SkillFile = %q", out)
	}
	bin, err := SkillFile(s, "img.png", "/abs/img.png", []byte("\x00\x01"), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if bin != `<skill_file skill="golang" path="img.png" revision="3f2a" abs_path="/abs/img.png" binary="true" bytes="2"></skill_file>`+"\n" {
		t.Fatalf("binary = %q", bin)
	}
}

func TestDiagnostics(t *testing.T) {
	if Diagnostics(nil) != "" {
		t.Fatal("empty diagnostics rendered")
	}
	line := 12
	ds := []catalog.Diagnostic{{Source: "reg", Bundle: "skills/user/disk-cleanup", File: "SKILL.md", Line: &line, Message: `unsupported template tag "<%= x %>"`}}
	out := Diagnostics(ds)
	want := `<catalog_diagnostics count="1">` + "\ndisk-cleanup reg:skills/user/disk-cleanup/SKILL.md:12 unsupported template tag \"<%= x %>\"\nThese skills are not loaded. Tell the user; `skills-mcp catalog list` shows details.\n</catalog_diagnostics>\n"
	if out != want {
		t.Fatalf("Diagnostics =\n%s", out)
	}
	var many []catalog.Diagnostic
	for i := range 100 {
		many = append(many, catalog.Diagnostic{Source: "reg", Bundle: fmt.Sprintf("skills/agent/bundle-%03d", i), File: "SKILL.md", Message: "a long enough message to fill the budget quickly"})
	}
	out = Diagnostics(many)
	if len(out) > maxDiagnosticsBytes {
		t.Fatalf("diagnostics element is %d bytes", len(out))
	}
	m := regexp.MustCompile(`and (\d+) more\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatal("no truncation line")
	}
	shown := strings.Count(out, "reg:skills/agent/bundle-")
	if n, _ := strconv.Atoi(m[1]); shown+n != 100 {
		t.Fatalf("shown %d + more %s != 100", shown, m[1])
	}
	huge := []catalog.Diagnostic{{Bundle: "b", Message: strings.Repeat("x", 3000)}}
	if out := Diagnostics(huge); !strings.Contains(out, "and 1 more") || len(out) > maxDiagnosticsBytes {
		t.Fatalf("oversized single diagnostic = %q", out)
	}
}
