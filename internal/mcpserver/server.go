// Package mcpserver exposes the harness views as MCP servers: one tool per
// model skill, one prompt per user skill, and the authoring tools.
package mcpserver

import (
	"context"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexgorbatchev/skills-mcp/internal/authoring"
	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
)

const (
	instructionsBase = "Each tool named after a skill returns task instructions. Before starting work that matches a skill tool's description, call that tool and follow the instructions it returns. Read bundled files by calling the tool again with `path`, or read the absolute paths it returns."
	instructionsAuth = " Tools prefixed `skill_` create and edit skills and push them to the skill repository."
	instructionsDiag = " Report any catalog_diagnostics block to the user."
)

// Options configures the servers.
type Options struct {
	Config    *config.Config
	Sources   *source.Manager
	Authoring *authoring.Service // nil when no write source is configured
	Logger    *slog.Logger
	Version   string
}

// Servers holds one MCP server per harness.
type Servers struct {
	opts    Options
	servers map[config.HarnessID]*mcp.Server

	mu    sync.Mutex
	views map[config.HarnessID]*catalog.View
}

// New creates the servers with no skills registered.
func New(opts Options) *Servers {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Servers{opts: opts, servers: map[config.HarnessID]*mcp.Server{}, views: map[config.HarnessID]*catalog.View{}}
	instructions := instructionsBase
	if opts.Authoring != nil {
		instructions += instructionsAuth
	}
	instructions += instructionsDiag
	for _, h := range config.Harnesses {
		srv := mcp.NewServer(&mcp.Implementation{Name: "skills-mcp", Version: opts.Version}, &mcp.ServerOptions{
			Instructions: instructions,
			Logger:       opts.Logger,
			// Declared up front so clients subscribe to list changes even
			// when a view starts empty.
			Capabilities: &mcp.ServerCapabilities{
				Tools:   &mcp.ToolCapabilities{ListChanged: true},
				Prompts: &mcp.PromptCapabilities{ListChanged: true},
			},
		})
		if opts.Authoring != nil {
			s.addAuthoringTools(h, srv)
		}
		s.servers[h] = srv
	}
	return s
}

// Server returns a harness's MCP server.
func (s *Servers) Server(h config.HarnessID) *mcp.Server { return s.servers[h] }

// ApplyViews registers the differences between the current and new views.
// go-sdk sends list_changed for each change.
func (s *Servers) ApplyViews(views map[config.HarnessID]*catalog.View) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range config.Harnesses {
		next := views[h]
		d := catalog.DiffViews(s.views[h], next)
		srv := s.servers[h]
		if len(d.RemoveTools) > 0 {
			srv.RemoveTools(d.RemoveTools...)
		}
		if len(d.RemovePrompts) > 0 {
			srv.RemovePrompts(d.RemovePrompts...)
		}
		for _, sk := range d.AddTools {
			s.addSkillTool(h, srv, sk)
		}
		for _, sk := range d.AddPrompts {
			s.addSkillPrompt(h, srv, sk)
		}
		s.views[h] = next
	}
}

func (s *Servers) view(h config.HarnessID) *catalog.View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.views[h]
}

// CloseSessions closes every MCP session so standalone GET streams end.
func (s *Servers) CloseSessions() {
	for _, srv := range s.servers {
		for ss := range srv.Sessions() {
			if err := ss.Close(); err != nil {
				s.opts.Logger.Debug("closing session", "error", err)
			}
		}
	}
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// notifier returns a progress callback for a request with a progress token,
// or nil.
func notifier(req *mcp.CallToolRequest, log *slog.Logger) func(string) {
	token := req.Params.GetProgressToken()
	if token == nil || req.Session == nil {
		return nil
	}
	var mu sync.Mutex
	var n float64
	return func(msg string) {
		mu.Lock()
		n++
		progress := n
		mu.Unlock()
		err := req.Session.NotifyProgress(context.Background(), &mcp.ProgressNotificationParams{ProgressToken: token, Message: msg, Progress: progress})
		if err != nil {
			log.Debug("progress notification failed", "error", err)
		}
	}
}
