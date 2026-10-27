package mcpserver

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/alexgorbatchev/skills-mcp/internal/config"
)

// Handler routes /mcp/<harness> to that harness's server behind bearer
// authentication and cross-origin protection, and serves /healthz. Unknown
// paths get 404 before authentication.
func (s *Servers) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok")) // a failed health response needs no handling
	})
	verifier := func(_ context.Context, presented string, _ *http.Request) (*auth.TokenInfo, error) {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{}, nil
	}
	requireToken := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	crossOrigin := http.NewCrossOriginProtection()
	for _, h := range config.Harnesses {
		srv := s.servers[h]
		streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
			Logger: s.opts.Logger,
			// Sessions stay open until DELETE or restart: an open GET stream
			// does not reset the idle timer.
			SessionTimeout: 0,
		})
		mux.Handle("/mcp/"+string(h), requireToken(crossOrigin.Handler(streamable)))
	}
	return mux
}
