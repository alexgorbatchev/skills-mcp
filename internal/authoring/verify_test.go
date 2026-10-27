package authoring

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

func TestSubmitVerificationFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(e *env, id string) error
		message string
		want    string
	}{
		{
			name: "deny pattern in a new skill body",
			mutate: func(e *env, id string) error {
				_, err := e.svc.Create(context.Background(), CreateArgs{ChangeID: id, Root: "skills/agent", Name: "leak", Description: "d", Body: "see host.secret.example\n"})
				return err
			},
			want: "an added line matches",
		},
		{
			name: "deny pattern on a line starting with ++",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "notes.md", Content: "++ see host.secret.example\n"})
				return err
			},
			want: "an added line matches",
		},
		{
			name: "deny pattern in a path",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "secret.example.md", Content: "x"})
				return err
			},
			want: "path skills/agent/golang/secret.example.md matches",
		},
		{
			name: "deny pattern in the message",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "ok.md", Content: "x"})
				return err
			},
			message: "mention secret.example",
			want:    "the commit message matches",
		},
		{
			name: "duplicate served name",
			mutate: func(e *env, id string) error {
				_, err := e.svc.Create(context.Background(), CreateArgs{ChangeID: id, Root: "skills/user", Name: "golang", Description: "dup"})
				return err
			},
			want: "duplicate_name",
		},
		{
			name: "unsupported template tag",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "references/t.md", Content: "<%= nope %>"})
				return err
			},
			want: "unsupported_template_tag",
		},
		{
			name: "rename through author",
			mutate: func(e *env, id string) error {
				_, err := e.svc.Edit(context.Background(), EditArgs{ChangeID: id, Bundle: "skills/agent/golang", OldString: "description: Go rules", NewString: "description: Go rules\nauthor: someone"})
				return err
			},
			want: "would change its served name",
		},
		{
			name: "protected validator",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/validator", Path: "scripts/check.sh", Content: "exit 0\n"})
				return err
			},
			want: "protected validator path skills/agent/validator/scripts/check.sh",
		},
		{
			name: "protected ignore file above the validator",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/validator", Path: ".registry-ignore", Content: "scripts/\n"})
				return err
			},
			want: "protected validator path skills/agent/validator/.registry-ignore",
		},
		{
			name: "validate command fails",
			mutate: func(e *env, id string) error {
				_, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "FAIL", Content: "x"})
				return err
			},
			want: "validator rejected",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := e.open()
			before := e.remote.tip()
			if err := tt.mutate(e, id); err != nil {
				t.Fatal(err)
			}
			msg := tt.message
			if msg == "" {
				msg = "change"
			}
			_, err := e.submit(id, msg)
			mustErr(t, err, tt.want)
			if e.remote.tip() != before {
				t.Fatal("a failed verification pushed")
			}
			rec := e.record(id)
			if rec.State != StateOpen || rec.Commit != nil || !strings.Contains(*rec.LastError, tt.want) {
				t.Fatalf("record after failure = %+v", rec)
			}
		})
	}
}

func TestSubmitRejectsEmptyAndBadInput(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	_, err := e.submit(id, "nothing")
	mustErr(t, err, "no edits")
	if _, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "a.md", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.submit(id, "  ")
	mustErr(t, err, "message must not be empty")
	_, err = e.svc.Submit(context.Background(), "chg_0000000000000001", "m", nil)
	mustErr(t, err, "unknown change")
}

func TestVerificationFixesRefusedBundle(t *testing.T) {
	e := newEnv(t)
	// Upstream pushes a broken skill; it is refused, and authoring can fix it.
	write(t, filepath.Join(e.remote.work, "skills/agent/broken/SKILL.md"), skillDoc("broken", "desc", "<%= bad %>\n"))
	e.remote.push("broken")
	if err := e.src.Refresh(context.Background(), "reg", sourceRefresh()); err != nil {
		t.Fatal(err)
	}
	if len(e.view(config.ClaudeCode).Diagnostics) != 1 {
		t.Fatal("broken skill not refused")
	}
	id := e.open()
	if _, err := e.svc.Edit(context.Background(), EditArgs{ChangeID: id, Bundle: "skills/agent/broken", OldString: "<%= bad %>", NewString: "fixed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.submit(id, "fix broken"); err != nil {
		t.Fatal(err)
	}
	if e.view(config.ClaudeCode).Skill("broken") == nil {
		t.Fatal("repaired skill not served")
	}
}

func TestProgressReported(t *testing.T) {
	e := newEnv(t)
	id := e.open()
	if _, err := e.svc.WriteFile(context.Background(), WriteArgs{ChangeID: id, Bundle: "skills/agent/golang", Path: "a.md", Content: "x"}); err != nil {
		t.Fatal(err)
	}
	var steps []string
	if _, err := e.svc.Submit(context.Background(), id, "m", func(s string) { steps = append(steps, s) }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(steps, "|")
	for _, want := range []string{"committing", "verifying: validate skills/agent/golang", "pushing (attempt 1)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("progress %q lacks %q", joined, want)
		}
	}
}

func TestDenyScanAndParsing(t *testing.T) {
	if _, err := parseNameStatus([]byte("R100\x00old\x00")); err == nil {
		t.Fatal("truncated rename parsed")
	}
	entries, err := parseNameStatus([]byte("M\x00a\x00R090\x00b\x00c\x00"))
	if err != nil || len(entries) != 2 || entries[1].status != "R" || entries[1].paths[1] != "c" {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	patch := "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1,2 @@\n+added\n+++ looks like a header\n-removed\n context\n"
	lines, err := addedLines([]byte(patch))
	if err != nil || len(lines) != 2 || lines[0] != "added" || lines[1] != "++ looks like a header" {
		t.Fatalf("addedLines = %q, %v", lines, err)
	}
	if _, err := addedLines([]byte(strings.Repeat("x", maxFileBytes+2048))); err == nil {
		t.Fatal("an oversized line was not reported")
	}
}
