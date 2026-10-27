package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexgorbatchev/skills-mcp/internal/authoring"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

type openArgs struct {
	Source  string `json:"source,omitempty"`
	Summary string `json:"summary"`
}

type changeArgs struct {
	ChangeID string `json:"change_id"`
}

type submitArgs struct {
	ChangeID string `json:"change_id"`
	Message  string `json:"message"`
}

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	additive    = &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	destructive = &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
	publishing  = &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)}
)

var changeID = str("Change handle from skill_change_open.")

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return textResult(string(data)), nil, nil
}

func (s *Servers) addAuthoringTools(h config.HarnessID, srv *mcp.Server) {
	svc := s.opts.Authoring
	budget := func() int { return s.opts.Config.Harnesses[h].MaxResultBytes }
	frontmatterEntry := object([]string{"key", "value"}, map[string]*jsonschema.Schema{
		"key":   str("Frontmatter key."),
		"value": {Description: "Frontmatter value (any JSON value)."},
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_change_open",
		Description: "Open a change for creating or editing skills. Returns a change_id used by every other skill_ tool. Edits are pushed to the skill repository only by skill_change_submit.",
		InputSchema: object([]string{"summary"}, map[string]*jsonschema.Schema{
			"source":  str("Skill source to change; defaults to the configured write source."),
			"summary": str("One line describing the intended change."),
		}),
		Annotations: additive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a openArgs) (*mcp.CallToolResult, any, error) {
		res, err := svc.Open(ctx, h, a.Source, a.Summary)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_change_read",
		Description: "Read a skill bundle file's raw source text (before template rendering) from a change. Use this text for skill_edit old_string values.",
		InputSchema: object([]string{"change_id", "bundle"}, map[string]*jsonschema.Schema{
			"change_id": changeID,
			"bundle":    str("Repo-relative bundle directory, e.g. skills/agent/golang."),
			"path":      str("Bundle-relative file; defaults to SKILL.md."),
			"offset":    nonNegative("Byte offset from a continuation element. Pass it together with sha."),
			"sha":       str("Content hash from a continuation element. Pass it together with offset."),
		}),
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a authoring.ReadArgs) (*mcp.CallToolResult, any, error) {
		text, err := svc.Read(ctx, a, budget())
		if err != nil {
			return nil, nil, err
		}
		return textResult(text), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_create",
		Description: "Create a new skill bundle with a SKILL.md in a change.",
		InputSchema: object([]string{"change_id", "root", "name", "description", "body"}, map[string]*jsonschema.Schema{
			"change_id":   changeID,
			"root":        str("Skill root of the change's source, e.g. skills/agent."),
			"name":        str("Skill name: lowercase letters, digits, and single hyphens; also the directory name."),
			"description": str("When to use the skill; becomes the tool description."),
			"frontmatter": {Type: "array", Items: frontmatterEntry, Description: "Extra frontmatter keys in order, after name and description."},
			"body":        str("SKILL.md content after the frontmatter."),
		}),
		Annotations: additive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a authoring.CreateArgs) (*mcp.CallToolResult, any, error) {
		res, err := svc.Create(ctx, a)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_edit",
		Description: "Replace exact text in a skill bundle file of a change. old_string must occur exactly once unless replace_all is set.",
		InputSchema: object([]string{"change_id", "bundle", "old_string", "new_string"}, map[string]*jsonschema.Schema{
			"change_id":   changeID,
			"bundle":      str("Repo-relative bundle directory."),
			"path":        str("Bundle-relative file; defaults to SKILL.md."),
			"old_string":  str("Exact text to replace, as returned by skill_change_read."),
			"new_string":  str("Replacement text."),
			"replace_all": boolean("Replace every occurrence."),
		}),
		Annotations: destructive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a authoring.EditArgs) (*mcp.CallToolResult, any, error) {
		res, err := svc.Edit(ctx, a)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_write_file",
		Description: "Add or replace a text file in a skill bundle of a change.",
		InputSchema: object([]string{"change_id", "bundle", "path", "content"}, map[string]*jsonschema.Schema{
			"change_id": changeID,
			"bundle":    str("Repo-relative bundle directory."),
			"path":      str("Bundle-relative file path."),
			"content":   str("Full UTF-8 file content."),
		}),
		Annotations: destructive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a authoring.WriteArgs) (*mcp.CallToolResult, any, error) {
		res, err := svc.WriteFile(ctx, a)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_change_diff",
		Description: "Show the staged diff of a change against its base revision, including new files.",
		InputSchema: object([]string{"change_id"}, map[string]*jsonschema.Schema{
			"change_id": changeID,
			"offset":    nonNegative("Byte offset from a continuation element. Pass it together with sha."),
			"sha":       str("Content hash from a continuation element. Pass it together with offset."),
		}),
		Annotations: readOnly,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a authoring.DiffArgs) (*mcp.CallToolResult, any, error) {
		text, err := svc.Diff(ctx, a, budget())
		if err != nil {
			return nil, nil, err
		}
		return textResult(text), nil, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_change_submit",
		Description: "Commit, verify, and push a change to the skill repository's tracked branch. The change is live for every agent when this returns. Calling it again on a submitted change returns the same result.",
		InputSchema: object([]string{"change_id", "message"}, map[string]*jsonschema.Schema{
			"change_id": changeID,
			"message":   str("Commit message."),
		}),
		Annotations: publishing,
	}, func(ctx context.Context, req *mcp.CallToolRequest, a submitArgs) (*mcp.CallToolResult, any, error) {
		res, err := svc.Submit(ctx, a.ChangeID, a.Message, notifier(req, s.opts.Logger))
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(res)
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "skill_change_discard",
		Description: "Discard a change and its unpushed edits.",
		InputSchema: object([]string{"change_id"}, map[string]*jsonschema.Schema{"change_id": changeID}),
		Annotations: destructive,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, a changeArgs) (*mcp.CallToolResult, any, error) {
		if err := svc.Discard(ctx, a.ChangeID); err != nil {
			return nil, nil, err
		}
		return jsonResult(map[string]string{"change_id": a.ChangeID, "state": authoring.StateDiscarded})
	})
}
