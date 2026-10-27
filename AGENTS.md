# skills-mcp

Go daemon and CLI that serves agent skills from Git repositories as MCP tools and prompts, and lets agents push skill changes back. The design of record is `docs/internal/eng-designs/skills-mcp.md`.

## Commands

- Set up dependencies: `just setup`
- Run the daemon: `just run` (uses `$XDG_CONFIG_HOME/skills-mcp/config.yaml`)
- Run the CLI: `just cli <args>`; agent mode: `just run-ai <args>`
- Test: `just test` (race detector)
- Coverage gate: `just coverage` (fails when any package is under 90%)
- Lint: `just lint` (`go mod tidy -diff`, `gofmt`, `go vet`)
- Everything: `just check`
- Build: `just build` (writes `bin/skills-mcp`)

## Layout

- `cmd/skills-mcp/` — Cobra commands (`server run`, `catalog list`, `bundle validate`, `token show`, `skill`).
- `internal/daemon/` — startup, HTTP server, refresh/sweep loops, shutdown.
- `internal/source/` — clones, snapshot extraction (ignore, render, prepare, permissions), refresh, retention.
- `internal/catalog/` — frontmatter, template rendering, validation, per-harness views, diffs.
- `internal/authoring/` — change handles, mutations, reconciliation, submit verification, push.
- `internal/mcpserver/` — MCP servers, skill tools, prompts, authoring tools, HTTP routing.
- `internal/render/` — result formatting and budget chunking.
- `internal/gitx/`, `internal/proc/` — the only ways to run `git` and child processes.
- `skills/skills-mcp/SKILL.md` — the agent skill for operating the CLI.

## Conventions

- Run `git` only through `internal/gitx`: it disables prompts and hooks, sets literal pathspecs (except `check-ignore`), and applies timeouts. Run every other child process through `internal/proc`, which kills the whole process group.
- Parse git output only from plumbing or `-z` forms; never parse porcelain diff output for content checks.
- Snapshots are read-only after extraction; remove them with `removeTree`, which restores write permission first.
- Tests use real git repositories over `file://` URLs and isolate git with `GIT_CONFIG_GLOBAL` and `GIT_CONFIG_NOSYSTEM`; snapshot fixtures must be unlocked in `t.Cleanup` before `t.TempDir` removal.

## Boundaries

- Always: for code changes that alter executable behavior, add or update meaningful behavioral tests. Keep at least 90% line and function coverage across the non-test source of each affected package (`just coverage`).
- Always: update `skills/skills-mcp/SKILL.md` in the same change as any command, argument, option, default, environment variable, output, or side-effect change, and update its `last_modified`.
- Always: keep `docs/internal/eng-designs/skills-mcp.md` consistent with the implementation when behavior or contracts change.
- Never: force-push (the base-revision `--force-with-lease` compare-and-swap is the only allowed push form), push to a ref other than the source's tracked branch, or run git hooks from daemon code.
- Never: commit or push private internal domains to github.com repositories.
- Never: publish releases, tags, or packages without explicit user authorization.
