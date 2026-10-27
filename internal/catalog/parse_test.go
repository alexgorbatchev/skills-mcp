package catalog

import (
	"strings"
	"testing"
)

func skillMD(frontmatter, body string) []byte {
	return []byte("---\n" + frontmatter + "---\n" + body)
}

func TestSplitFrontmatter(t *testing.T) {
	tests := []struct {
		name, in, fm, body string
		wantErr            bool
	}{
		{name: "basic", in: "---\nname: a\n---\nbody\n", fm: "name: a\n", body: "body\n"},
		{name: "crlf", in: "---\r\nname: a\r\n---\r\nbody", fm: "name: a\r\n", body: "body"},
		{name: "empty body", in: "---\nname: a\n---\n", fm: "name: a\n", body: ""},
		{name: "closing at eof", in: "---\nname: a\n---", fm: "name: a\n", body: ""},
		{name: "no opening", in: "name: a\n---\n", wantErr: true},
		{name: "no closing", in: "---\nname: a\n", wantErr: true},
		{name: "single line", in: "---", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, body, err := SplitFrontmatter([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil || string(fm) != tt.fm || body != tt.body {
				t.Fatalf("SplitFrontmatter = %q, %q, %v", fm, body, err)
			}
		})
	}
}

func TestParseSkill(t *testing.T) {
	prefix := PrefixRule{Enabled: true, Except: []string{"alexgorbatchev"}}
	long := strings.Repeat("a", 1025)
	tests := []struct {
		name      string
		content   []byte
		dir       string
		rule      PrefixRule
		served    string
		wantCodes []string
	}{
		{name: "valid", content: skillMD("name: golang\ndescription: Go rules\n", "body"), dir: "golang", served: "golang"},
		{name: "owner not prefixed", content: skillMD("name: golang\ndescription: d\nauthor: alexgorbatchev\n", ""), dir: "golang", rule: prefix, served: "golang"},
		{name: "vendored prefixed", content: skillMD("name: grilling\ndescription: d\nauthor: mattpocock\n", ""), dir: "grilling", rule: prefix, served: "mattpocock-grilling"},
		{name: "mixed case author lowercased", content: skillMD("name: caveman\ndescription: d\nauthor: JuliusBrussee\n", ""), dir: "caveman", rule: prefix, served: "juliusbrussee-caveman"},
		{name: "prefix already present", content: skillMD("name: shadcn-ui\ndescription: d\nauthor: shadcn\n", ""), dir: "shadcn-ui", rule: prefix, served: "shadcn-ui"},
		{name: "missing author unprefixed", content: skillMD("name: x\ndescription: d\n", ""), dir: "x", rule: prefix, served: "x"},
		{name: "prefix disabled", content: skillMD("name: x\ndescription: d\nauthor: other\n", ""), dir: "x", served: "x"},
		{name: "invalid author", content: skillMD("name: x\ndescription: d\nauthor: Some Body\n", ""), dir: "x", rule: prefix, served: "x", wantCodes: []string{CodeInvalidAuthor}},
		{name: "non-string author", content: skillMD("name: x\ndescription: d\nauthor: [a]\n", ""), dir: "x", rule: prefix, served: "x", wantCodes: []string{CodeInvalidAuthor}},
		{name: "no frontmatter", content: []byte("# title\n"), dir: "x", served: "x", wantCodes: []string{CodeInvalidFrontmatter}},
		{name: "not a mapping", content: skillMD("- a\n", ""), dir: "x", served: "x", wantCodes: []string{CodeInvalidFrontmatter}},
		{name: "bad yaml", content: skillMD("a: [\n", ""), dir: "x", served: "x", wantCodes: []string{CodeInvalidFrontmatter}},
		{name: "missing name", content: skillMD("description: d\n", ""), dir: "x", served: "x", wantCodes: []string{CodeInvalidName}},
		{name: "numeric name", content: skillMD("name: 12\ndescription: d\n", ""), dir: "x", served: "x", wantCodes: []string{CodeInvalidName}},
		{name: "invalid name", content: skillMD("name: Bad_Name\ndescription: d\n", ""), dir: "Bad_Name", served: "Bad_Name", wantCodes: []string{CodeInvalidName, CodeInvalidName}},
		{name: "name mismatch", content: skillMD("name: a\ndescription: d\n", ""), dir: "b", served: "b", wantCodes: []string{CodeNameMismatch}},
		{name: "missing description", content: skillMD("name: x\n", ""), dir: "x", served: "x", wantCodes: []string{CodeInvalidDescription}},
		{name: "long description", content: skillMD("name: x\ndescription: "+long+"\n", ""), dir: "x", served: "x", wantCodes: []string{CodeInvalidDescription}},
		{name: "prefixed name too long", content: skillMD("name: "+strings.Repeat("a", 60)+"\ndescription: d\nauthor: someone\n", ""), dir: strings.Repeat("a", 60), rule: prefix, served: "someone-" + strings.Repeat("a", 60), wantCodes: []string{CodeInvalidName}},
		{name: "reserved name", content: skillMD("name: skill-create\ndescription: d\n", ""), dir: "skill-create", served: "skill-create", wantCodes: []string{CodeReservedName}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, issues := ParseSkill(tt.content, tt.dir, tt.rule)
			var codes []string
			for _, is := range issues {
				codes = append(codes, is.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tt.wantCodes, ",") {
				t.Fatalf("codes = %v, want %v (%+v)", codes, tt.wantCodes, issues)
			}
			if p.ServedName != tt.served {
				t.Fatalf("served = %q, want %q", p.ServedName, tt.served)
			}
		})
	}
}
