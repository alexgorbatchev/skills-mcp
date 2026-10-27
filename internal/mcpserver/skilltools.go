package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/render"
)

// SkillArgs are a skill tool's arguments.
type SkillArgs struct {
	Path     string `json:"path,omitempty"`
	Offset   *int   `json:"offset,omitempty"`
	Revision string `json:"revision,omitempty"`
}

var skillInputSchema = object(nil, map[string]*jsonschema.Schema{
	"path":     str("Bundled file relative to the skill root. Omit to load the skill."),
	"offset":   nonNegative("Byte offset from a continuation element. Pass it together with revision."),
	"revision": str("Revision from a continuation element. Pass it together with offset."),
})

func (s *Servers) addSkillTool(h config.HarnessID, srv *mcp.Server, sk *catalog.Skill) {
	tool := &mcp.Tool{
		Name:        sk.Name,
		Description: sk.Description,
		InputSchema: skillInputSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)},
		Meta:        mcp.Meta{"anthropic/alwaysLoad": true},
	}
	name := sk.Name
	mcp.AddTool(srv, tool, func(_ context.Context, _ *mcp.CallToolRequest, args SkillArgs) (*mcp.CallToolResult, any, error) {
		text, err := s.callSkill(h, name, args)
		if err != nil {
			return nil, nil, err
		}
		return textResult(text), nil, nil
	})
}

var errReload = errors.New("this revision is no longer available; call the tool again without offset and revision to reload the skill")

func (s *Servers) callSkill(h config.HarnessID, name string, args SkillArgs) (string, error) {
	if (args.Offset == nil) != (args.Revision == "") {
		return "", errors.New("offset and revision must be given together")
	}
	view := s.view(h)
	sk := view.Skill(name)
	if sk == nil {
		return "", fmt.Errorf("skill %q is no longer served", name)
	}
	if args.Revision != "" && args.Revision != sk.Revision {
		var err error
		if sk, err = s.skillAt(h, sk, args.Revision); err != nil {
			return "", err
		}
	}
	offset := 0
	if args.Offset != nil {
		offset = *args.Offset
	}
	budget := s.opts.Config.Harnesses[h].MaxResultBytes
	if args.Path == "" {
		diags := ""
		if offset == 0 {
			diags = render.Diagnostics(view.Diagnostics)
		}
		return render.SkillLoad(sk, offset, budget, diags)
	}
	rel := args.Path
	if rel != path.Clean(rel) || strings.HasPrefix(rel, "/") || rel == "." {
		return "", fmt.Errorf("path %q must be a clean path relative to the skill root", rel)
	}
	root, err := os.OpenRoot(sk.BaseDir)
	if err != nil {
		return "", errReload
	}
	defer root.Close()
	content, err := root.ReadFile(rel)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", rel, err)
	}
	return render.SkillFile(sk, rel, filepath.Join(sk.BaseDir, filepath.FromSlash(rel)), content, offset, budget)
}

// skillAt resolves a skill in a retained revision of its source.
func (s *Servers) skillAt(h config.HarnessID, current *catalog.Skill, revision string) (*catalog.Skill, error) {
	if !s.opts.Sources.Retained(current.Source, revision) {
		return nil, errReload
	}
	snap, err := s.opts.Sources.Snapshot(current.Source, revision)
	if err != nil {
		return nil, errReload
	}
	views, err := s.opts.Sources.Cache().Views([]catalog.Snapshot{snap})
	if err != nil {
		return nil, errReload
	}
	sk := views[h].Skill(current.Name)
	if sk == nil {
		return nil, errReload
	}
	return sk, nil
}

func (s *Servers) addSkillPrompt(h config.HarnessID, srv *mcp.Server, sk *catalog.Skill) {
	prompt := &mcp.Prompt{
		Name:        sk.Name,
		Description: sk.Description,
		Arguments:   []*mcp.PromptArgument{{Name: "args", Description: "Additional context for the skill.", Required: false}},
	}
	name := sk.Name
	srv.AddPrompt(prompt, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		view := s.view(h)
		cur := view.Skill(name)
		if cur == nil {
			return nil, fmt.Errorf("skill %q is no longer served", name)
		}
		text, err := render.SkillLoad(cur, 0, int(^uint(0)>>1), render.Diagnostics(view.Diagnostics))
		if err != nil {
			return nil, err
		}
		if extra := req.Params.Arguments["args"]; extra != "" {
			text += "\n" + extra
		}
		return &mcp.GetPromptResult{
			Description: cur.Description,
			Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
		}, nil
	})
}
