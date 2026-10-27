package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexgorbatchev/skills-mcp/internal/authoring"
	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/gitx"
	"github.com/alexgorbatchev/skills-mcp/internal/paths"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func skillDoc(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
}

type fixture struct {
	t       *testing.T
	work    string // working clone pushing to the bare upstream
	src     *source.Manager
	servers *Servers
	url     string
	cfg     *config.Config
}

func isolateGit(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	write(t, empty, "")
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "tester")
	t.Setenv("GIT_COMMITTER_NAME", "tester")
	t.Setenv("GIT_AUTHOR_EMAIL", "tester@example.com")
	t.Setenv("GIT_COMMITTER_EMAIL", "tester@example.com")
}

func newFixture(t *testing.T, withAuthoring bool, budget int) *fixture {
	t.Helper()
	isolateGit(t)
	root := t.TempDir()
	bare := filepath.Join(root, "up.git")
	work := filepath.Join(root, "work")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, root, "clone", "-q", bare, work)
	git(t, work, "checkout", "-q", "-b", "main")
	write(t, filepath.Join(work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", strings.Repeat("go line\n", 200)))
	write(t, filepath.Join(work, "skills/agent/golang/references/setup.md"), "setup <%- repo_root %>\n")
	if err := os.WriteFile(filepath.Join(work, "skills/agent/golang/logo.png"), []byte("\x89PNG\x00\x01"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(work, "skills/user/commit/SKILL.md"), skillDoc("commit", "Commit changes", "commit body\n"))
	write(t, filepath.Join(work, "skills/agent/broken/SKILL.md"), skillDoc("broken", "Broken", "<%= nope %>\n"))
	pushAll(t, work, "init")

	src := &config.Source{
		Name: "reg", Kind: config.KindGit, URL: "file://" + bare, Branch: "main", Writable: withAuthoring,
		Roots: []config.Root{
			{Path: "skills/agent", Invocation: config.Model},
			{Path: "skills/user", Invocation: config.User},
		},
		Template:     config.Template{Vars: map[string]string{"repo_root": "{snapshot}"}},
		DenyPatterns: []*regexp.Regexp{regexp.MustCompile(`secret\.example`)},
	}
	cfg := &config.Config{
		FetchInterval: time.Minute, SubmitTimeout: 5 * time.Minute, SnapshotRetention: time.Hour, ChangeTTL: time.Hour,
		Harnesses: map[config.HarnessID]config.Harness{config.ClaudeCode: {MaxResultBytes: budget}, config.Codex: {MaxResultBytes: budget}, config.Pi: {MaxResultBytes: budget}},
		Sources:   []*config.Source{src},
	}
	dirs := paths.Dirs{Config: filepath.Join(root, "config"), Cache: filepath.Join(root, "cache"), State: filepath.Join(root, "state")}
	g := gitx.New(context.Background())
	f := &fixture{t: t, work: work, cfg: cfg}
	var svc *authoring.Service
	f.src = source.NewManager(source.Options{
		Config: cfg, Dirs: dirs, Git: g,
		BaseRevisions: func() map[string]map[string]bool {
			if svc == nil {
				return nil
			}
			return svc.BaseRevisions()
		},
		OnViews: func(v map[config.HarnessID]*catalog.View) { f.servers.ApplyViews(v) },
	})
	if withAuthoring {
		cfg.DefaultWriteSource = "reg"
		svc = authoring.NewService(authoring.Options{Config: cfg, Dirs: dirs, Git: g, Sources: f.src})
	}
	f.servers = New(Options{Config: cfg, Sources: f.src, Authoring: svc, Version: "test"})
	if err := f.src.Init(); err != nil {
		t.Fatal(err)
	}
	if err := f.src.Refresh(context.Background(), "reg", source.RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(f.servers.Handler(testToken))
	t.Cleanup(func() {
		f.servers.CloseSessions()
		ts.Close()
		unlock(dirs.Cache)
	})
	f.url = ts.URL
	return f
}

func pushAll(t *testing.T, work, msg string) {
	t.Helper()
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", msg)
	git(t, work, "push", "-q", "origin", "main")
}

func unlock(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) connect(h config.HarnessID, opts *mcp.ClientOptions) *mcp.ClientSession {
	f.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, opts)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   f.url + "/mcp/" + string(h),
		HTTPClient: &http.Client{Transport: bearer{testToken}},
	}
	cs, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestHTTPChecks(t *testing.T) {
	f := newFixture(t, false, 48000)
	tests := []struct {
		name, method, path, token, host, origin string
		want                                    int
	}{
		{name: "healthz", method: "GET", path: "/healthz", want: 200},
		{name: "unknown path without token", method: "POST", path: "/mcp/vim", want: 404},
		{name: "missing token", method: "POST", path: "/mcp/claude-code", want: 401},
		{name: "missing token on GET", method: "GET", path: "/mcp/claude-code", want: 401},
		{name: "wrong token", method: "POST", path: "/mcp/claude-code", token: strings.Repeat("f", 64), want: 401},
		{name: "bad host without token", method: "POST", path: "/mcp/claude-code", host: "evil.example", want: 401},
		{name: "bad host with token", method: "POST", path: "/mcp/claude-code", token: testToken, host: "evil.example", want: 403},
		{name: "cross-origin POST", method: "POST", path: "/mcp/claude-code", token: testToken, origin: "https://evil.example", want: 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, f.url+tt.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			if tt.host != "" {
				req.Host = tt.host
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestToolsAndPrompts(t *testing.T) {
	f := newFixture(t, false, 48000)
	cs := f.connect(config.ClaudeCode, nil)
	ctx := context.Background()
	if !strings.Contains(cs.InitializeResult().Instructions, "Report any catalog_diagnostics block") ||
		strings.Contains(cs.InitializeResult().Instructions, "skill_") {
		t.Fatalf("instructions = %q", cs.InitializeResult().Instructions)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "golang" || tools.Tools[0].Description != "Go rules" {
		t.Fatalf("tools = %+v", tools.Tools)
	}
	if tools.Tools[0].Meta["anthropic/alwaysLoad"] != true || !tools.Tools[0].Annotations.ReadOnlyHint {
		t.Fatalf("tool meta = %+v / %+v", tools.Tools[0].Meta, tools.Tools[0].Annotations)
	}

	load, isErr := callText(t, cs, "golang", nil)
	if isErr || !strings.HasPrefix(load, `<skill name="golang"`) || !strings.Contains(load, "<bundled_files>\nlogo.png\nreferences/setup.md\n</bundled_files>") {
		t.Fatalf("load = %s", load)
	}
	if !strings.Contains(load, `<catalog_diagnostics count="1">`) || !strings.Contains(load, "broken reg:skills/agent/broken/SKILL.md:5") {
		t.Fatalf("diagnostics missing:\n%s", load)
	}
	file, _ := callText(t, cs, "golang", map[string]any{"path": "references/setup.md"})
	if !strings.Contains(file, "setup "+f.src.SnapshotDir("reg", f.src.Current("reg"))) {
		t.Fatalf("file = %s", file)
	}
	bin, _ := callText(t, cs, "golang", map[string]any{"path": "logo.png"})
	if !strings.Contains(bin, `binary="true"`) {
		t.Fatalf("binary = %s", bin)
	}
	errorCases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"path": "../commit/SKILL.md"}, "path escapes"},
		{map[string]any{"path": "/etc/passwd"}, "must be a clean path"},
		{map[string]any{"path": "missing.md"}, "reading missing.md"},
		{map[string]any{"offset": 5}, "offset and revision must be given together"},
		{map[string]any{"offset": -1, "revision": "x"}, "validating"},
		{map[string]any{"bogus": 1}, "validating"},
		{map[string]any{"offset": 1, "revision": strings.Repeat("0", 40)}, "no longer available"},
	}
	for _, ec := range errorCases {
		text, isErr := callText(t, cs, "golang", ec.args)
		if !isErr || !strings.Contains(text, ec.want) {
			t.Errorf("args %v: %q (isError=%v), want %q", ec.args, text, isErr, ec.want)
		}
	}

	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "commit" {
		t.Fatalf("prompts = %+v, %v", prompts, err)
	}
	got, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "commit", Arguments: map[string]string{"args": "only staged files"}})
	if err != nil {
		t.Fatal(err)
	}
	text := got.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(text, "commit body") || !strings.HasSuffix(text, "</catalog_diagnostics>\n\nonly staged files") {
		t.Fatalf("prompt = %q", text)
	}
}

var contRe = regexp.MustCompile(`<continuation revision="([0-9a-f]+)" next_offset="(\d+)"`)

func TestContinuationsAndRetiredRevisions(t *testing.T) {
	f := newFixture(t, false, 1000)
	cs := f.connect(config.Pi, nil)
	first, _ := callText(t, cs, "golang", nil)
	m := contRe.FindStringSubmatch(first)
	if m == nil {
		t.Fatalf("no continuation:\n%s", first)
	}
	rev := m[1]
	// Upstream changes the body; the pinned revision still serves the old one.
	write(t, filepath.Join(f.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", strings.Repeat("new line\n", 50)))
	pushAll(t, f.work, "body change")
	if err := f.src.Refresh(context.Background(), "reg", source.RefreshOptions{}); err != nil {
		t.Fatal(err)
	}
	next, isErr := callText(t, cs, "golang", map[string]any{"offset": atoi(m[2]), "revision": rev})
	if isErr || !strings.Contains(next, "go line") || strings.Contains(next, "new line") {
		t.Fatalf("pinned continuation = %s", next)
	}
	if strings.Contains(next, "<bundled_files>") {
		t.Fatal("later chunk repeats bundled files")
	}
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}

func TestListChanged(t *testing.T) {
	f := newFixture(t, false, 48000)
	var mu sync.Mutex
	changes := 0
	cs := f.connect(config.ClaudeCode, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			mu.Lock()
			changes++
			mu.Unlock()
		},
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return changes
	}
	refresh := func() {
		if err := f.src.Refresh(context.Background(), "reg", source.RefreshOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// A body-only change sends nothing.
	write(t, filepath.Join(f.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules", "different body\n"))
	pushAll(t, f.work, "body")
	refresh()
	time.Sleep(300 * time.Millisecond)
	if count() != 0 {
		t.Fatal("list_changed on a body-only change")
	}
	// A description change reaches the idle session.
	write(t, filepath.Join(f.work, "skills/agent/golang/SKILL.md"), skillDoc("golang", "Go rules, revised", "different body\n"))
	pushAll(t, f.work, "description")
	refresh()
	deadline := time.Now().Add(5 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if count() == 0 {
		t.Fatal("no list_changed after a description change")
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || tools.Tools[0].Description != "Go rules, revised" {
		t.Fatalf("tools = %+v, %v", tools, err)
	}
	// A removed skill disappears and its tool reports so.
	if err := os.RemoveAll(filepath.Join(f.work, "skills/agent/golang")); err != nil {
		t.Fatal(err)
	}
	pushAll(t, f.work, "remove")
	refresh()
	tools, _ = cs.ListTools(context.Background(), nil)
	if len(tools.Tools) != 0 {
		t.Fatalf("removed skill still listed: %+v", tools.Tools)
	}
}

func TestAuthoringOverMCP(t *testing.T) {
	f := newFixture(t, true, 48000)
	var mu sync.Mutex
	var progress []string
	cs := f.connect(config.Codex, &mcp.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Message)
			mu.Unlock()
		},
	})
	if !strings.Contains(cs.InitializeResult().Instructions, "Tools prefixed `skill_`") {
		t.Fatal("authoring sentence missing from instructions")
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 9 {
		t.Fatalf("tools = %d", len(tools.Tools))
	}
	opened, isErr := callText(t, cs, "skill_change_open", map[string]any{"summary": "add fresh"})
	if isErr {
		t.Fatal(opened)
	}
	id := regexp.MustCompile(`"change_id": "(chg_[0-9a-f]+)"`).FindStringSubmatch(opened)[1]
	steps := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"skill_create", map[string]any{"change_id": id, "root": "skills/agent", "name": "fresh", "description": "Fresh", "body": "fresh body\n", "frontmatter": []any{map[string]any{"key": "metadata", "value": map[string]any{"status": "current"}}}}, `"served_name": "fresh"`},
		{"skill_write_file", map[string]any{"change_id": id, "bundle": "skills/agent/fresh", "path": "notes.md", "content": "notes"}, `"bytes": 5`},
		{"skill_edit", map[string]any{"change_id": id, "bundle": "skills/agent/fresh", "old_string": "fresh body", "new_string": "fresh body edited"}, `"replacements": 1`},
		{"skill_change_read", map[string]any{"change_id": id, "bundle": "skills/agent/fresh"}, "fresh body edited"},
		{"skill_change_diff", map[string]any{"change_id": id}, "+notes"},
	}
	for _, st := range steps {
		text, isErr := callText(t, cs, st.tool, st.args)
		if isErr || !strings.Contains(text, st.want) {
			t.Fatalf("%s = %s (isError=%v)", st.tool, text, isErr)
		}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "skill_change_submit", Arguments: map[string]any{"change_id": id, "message": "add fresh"},
		Meta: mcp.Meta{"progressToken": "p1"},
	})
	if err != nil || res.IsError {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	mu.Lock()
	gotProgress := strings.Join(progress, "|")
	mu.Unlock()
	if !strings.Contains(gotProgress, "pushing (attempt 1)") {
		t.Fatalf("progress = %q", gotProgress)
	}
	load, isErr := callText(t, cs, "fresh", nil)
	if isErr || !strings.Contains(load, "fresh body edited") {
		t.Fatalf("new skill not live: %s", load)
	}

	// Errors surface as tool errors.
	text, isErr := callText(t, cs, "skill_change_discard", map[string]any{"change_id": id})
	if !isErr || !strings.Contains(text, "not open") {
		t.Fatalf("discard of submitted = %s", text)
	}
	other, _ := callText(t, cs, "skill_change_open", map[string]any{"summary": "discard me"})
	otherID := regexp.MustCompile(`"change_id": "(chg_[0-9a-f]+)"`).FindStringSubmatch(other)[1]
	text, isErr = callText(t, cs, "skill_change_discard", map[string]any{"change_id": otherID})
	if isErr || !strings.Contains(text, `"state": "discarded"`) {
		t.Fatalf("discard = %s", text)
	}
	for _, tool := range []string{"skill_change_read", "skill_create", "skill_edit", "skill_write_file", "skill_change_diff", "skill_change_submit"} {
		args := map[string]any{"change_id": "chg_0000000000000000", "bundle": "skills/agent/x", "root": "skills/agent", "name": "x", "description": "d", "body": "b", "old_string": "a", "new_string": "b", "path": "p", "content": "c", "message": "m"}
		schemaKeys := map[string][]string{
			"skill_change_read":   {"change_id", "bundle"},
			"skill_create":        {"change_id", "root", "name", "description", "body"},
			"skill_edit":          {"change_id", "bundle", "old_string", "new_string"},
			"skill_write_file":    {"change_id", "bundle", "path", "content"},
			"skill_change_diff":   {"change_id"},
			"skill_change_submit": {"change_id", "message"},
		}
		filtered := map[string]any{}
		for _, k := range schemaKeys[tool] {
			filtered[k] = args[k]
		}
		text, isErr := callText(t, cs, tool, filtered)
		if !isErr || !strings.Contains(text, "unknown change") {
			t.Errorf("%s with unknown change = %s", tool, text)
		}
	}
	text, isErr = callText(t, cs, "skill_change_open", map[string]any{"summary": "s", "source": "missing"})
	if !isErr || !strings.Contains(text, "unknown source") {
		t.Fatalf("open unknown source = %s", text)
	}
}

func TestPromptAndToolForRemovedSkill(t *testing.T) {
	f := newFixture(t, false, 48000)
	if text, err := f.servers.callSkill(config.ClaudeCode, "missing", SkillArgs{}); err == nil {
		t.Fatalf("missing skill = %s", text)
	}
	notify, stop := notifier(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}, nil)
	stop()
	if notify != nil {
		t.Fatal("notifier without a token")
	}
	if f.servers.Server(config.Pi) == nil {
		t.Fatal("no pi server")
	}
}
