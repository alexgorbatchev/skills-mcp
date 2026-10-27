---
created_on: 2026-10-26 10:13
last_modified: 2026-10-26 17:21
status: current
---

# skills-mcp: Skills Served as MCP Tools

Engineering design for `skills-mcp`, a Go MCP server that replaces harness-native skill loading with MCP tools and prompts backed by a Git repository, and lets agents author skill changes that are pushed back to that repository.

Audience: the engineer implementing `skills-mcp` and the owner reviewing its design decisions. [Appendix A](#appendix-a-decision-record) records the original request, every design question, and every answer verbatim so each decision traces to its source.

Sections marked **Blocked on Qn** depend on an open decision listed in [15. Decisions](#15-decisions). Do not implement a blocked section until its decision is recorded.

## 1. Objective and non-goals

### Objective

- Serve every model-invoked skill as one MCP tool whose name is the skill's served name and whose description is the skill's frontmatter `description`, copied verbatim.
- Serve every user-invoked skill as one MCP prompt, and as a `/library:<name>` command in pi through `pi-skill-library`.
- Return the skill body and bundled files on request, with absolute local paths so harness-native file reads and bundled scripts keep working.
- Use a Git repository hosted on GitHub as the storage of record. The server keeps a local cache and refreshes it.
- Let agents create skills and edit skill bundles through MCP tools, validate the result, and push it to the tracked branch.
- Report every skill the server refuses to load to the user.
- Serve Claude Code, Codex, and pi from one long-running local daemon.
- Keep each harness's project-level skill discovery working; only registry skills move from harness homes to the daemon.

### Non-goals

- Remote or multi-machine deployment. The daemon binds loopback only.
- A stdio transport.
- Skill deletion or renaming through MCP tools.
- Porting the ai-registry TypeScript build pipeline to Go, or embedding a general EJS engine.
- Executing bundled skill scripts on behalf of agents. Agents run them with their own shell tools.
- Serving ai-registry profile-local skills (`profiles/<profile>/skills`). The daemon has no profile concept; ai-registry keeps installing them.
- Serving opencode.
- Writable skill directories. Snapshots are read-only; dependencies a skill needs are installed into the snapshot by the source's `prepare` rules at extraction (Q11A2).
- Knowledge of any package manager in the daemon. `prepare` rules are configuration, like `validate`.
- Claude Code skill features that MCP cannot carry: `allowed-tools` permission grants and `` !`command` `` preload execution. Skill text containing them is served unchanged; the README lists the affected skills.

## 2. Current baseline

### This repository

- `ai/tools/skills-mcp` is a Git repository on branch `main` with no commits and no remote. Its only file is this document.

### Skill source: ai-registry

Verified in `ai/standards/ai-registry` at commit `429e35f` (remote `git@github.com:alexgorbatchev/ai-registry.git`):

- 48 `SKILL.md` bundles: 34 under `skills/agent/` (model-invoked) and 14 under `skills/user/` (user-invoked).
- Harness-specific skills live in `harnesses/<target>/skills/agent/<name>/`: `claude-code-config` (claude-code); `claude-remote-control`, `claude-review`, `codex-config` (codex); `opencode-config`, `opencode-conversation-analysis`, `opencode-sessions` (opencode); `pi-config`, `pi-debug` (pi). No `harnesses/<target>/skills/user/` directory exists. No harness skill shares a name with a generic skill; the registry build supports same-name replacement.
- Profile-specific skills exist: `profiles/personal-assistant/skills/movie-showtime-search`.
- The registry build supports hostname variants in `variants/<hostname>/SKILL.md`; no bundle has a `variants/` directory.
- `.registry-ignore` files (`packages/registry-cli/src/lib/harnessBuild.ts`, `getSourceCopyEntries`) apply only from the copied directory downward: `ignore-walk` applies each `.registry-ignore` at or below the skill directory with gitignore semantics, and lines starting with `./` are additionally treated as path prefixes relative to the file's directory (`getSupplementalIgnorePrefixes`). Ignore files in ancestor directories of a skill are not applied to it. `node_modules` paths and the `.registry-ignore` files themselves are always excluded. Bundle-level files exist in `skills/agent/github-fix-issues/` and `skills/user/disk-cleanup/` and exclude `scripts/node_modules/`, `scripts/__tests__/`, `scripts/bun.lock`, and `justfile`. `harnesses/<target>/.registry-ignore` files (for example `./skills/` in claude-code) govern the raw harness-directory copy, not harness skills.
- The build supports a private overlay repository through `AI_REGISTRY_OVERLAY` in the git-ignored `.env`, mirroring `skills/`, `commands/`, and `profiles/`. The local checkout has no `.env`, so no overlay is in use.
- Frontmatter keys in use: `name`, `description`, `author`, `metadata` (`created_on`, `last_modified`, `status`), `license`, `allowed-tools`.
- `skills/agent/skill-writer/scripts/quick-validate.ts` imports only `node:` modules. It requires `name`, `description`, `author`, and `metadata.created_on`, `metadata.last_modified`, and `metadata.status`; rejects `<` or `>` in `description`; and fails on linked files missing from the bundle. `skills/agent/archify` fails it at `429e35f`.
- The build renames skills by author (`packages/registry-cli/src/lib/userSkillUtils.ts`, `getOutputSkillName`): a skill with no `author`, or with `author` `alexgorbatchev` or `agorbatchev`, keeps its name; otherwise the prefix is `lowercase(author)` + `-`, added only when the name does not already start with it. 16 bundles are renamed, for example `mattpocock-grilling`, `tt-a1i-archify`, `shadcn-shadcn`, and `juliusbrussee-caveman` (`author: JuliusBrussee`). The build also injects `disable-model-invocation: true` into user skills.
- Three bundles ship a `package.json`: `skills/user/disk-cleanup/` (its `SKILL.md` runs `bun install --frozen-lockfile` from the skill directory), `skills/agent/github-fix-issues/scripts/` (`references/setup.md` runs `bun install --cwd "$reviewer_scripts" --ignore-scripts`), and `skills/agent/archify/`. Installing into a read-only directory fails with `EACCES` (verified with bun 1.4.4).
- `skills/agent/shadcn/SKILL.md` uses a Claude Code `` !`command` `` preload; `agent-browser` declares `allowed-tools`.
- Harness builds stage skills into `.output/<harness>/<profile>/skills/`, and bootstrap applies the default profile to the harness home (`~/.claude`, or `CLAUDE_CONFIG_DIR`, for Claude Code). The claude-code hook `harnesses/claude-code/hooks/syncProjectSkills.ts` copies a project's `.agents/skills` into its `.claude/skills`.

Templating (`packages/template-resolver/src/renderTemplate.ts`, `packages/registry-cli/src/commands/buildCommand.ts`):

- Every staged skill file is rendered with EJS. The context is `repo_root`, `overlay_dir`, `skills_dir`, `commands_dir`, `profiles_dir`, `output_dir` (`<repo_root>/.output`), plus `file_path` and `file_dir`, which are the source file's path and directory in the checkout. `env(name, fallback?)` returns the build environment's value, else `fallback`, and throws when both are missing.
- No skill file uses `include(...)` or any tag other than `<%- ... %>` raw output.
- Tags occur in `SKILL.md` files and in bundled files:

| Expression | Files |
|---|---|
| `env("DOCS_INTERNAL_DIR", "docs/internal")` | `skills/agent/docs-writer/SKILL.md`, `references/tickets.md`, `references/eng-designs.md`, `assets/eng-designs-template.md`; `skills/user/policy-audit/SKILL.md`, `references/policy-rubric.md` |
| `file_dir` | `skills/user/disk-cleanup/SKILL.md` (11); `harnesses/opencode/skills/agent/opencode-conversation-analysis/SKILL.md` |
| `skills_dir` | `skills/agent/ai-registry-integration/SKILL.md` (5); `skills/agent/typescript/references/storybook.md` |
| `repo_root` | `skills/agent/ai-registry-integration/SKILL.md`; `harnesses/codex/skills/agent/codex-config/SKILL.md`, `references/harness-maintenance.md`, `references/configuration.md`; `harnesses/opencode/skills/agent/opencode-conversation-analysis/SKILL.md`, `scripts/extract.ts` |
| `output_dir` | `harnesses/codex/skills/agent/codex-config/SKILL.md`, `references/harness-maintenance.md` |
| `env("PERSONAL_ASSISTANT_DEFAULT_MOVIE_AREA", "__ASK_USER__")` | `profiles/personal-assistant/skills/movie-showtime-search/SKILL.md` |

- `repo_root`, `skills_dir`, and `output_dir` tell agents where to edit registry sources and where build output lives, for example "edit Codex harness source under `<%- repo_root %>/harnesses/codex/`, then run `bun run build`". `file_dir` locates a bundle's own scripts. `skills/agent/ai-registry-integration/SKILL.md` documents all eight context names for authors.

### pi-skill-library

Verified in `ai/pi/pi-skill-library` (remote `git@github.com:alexgorbatchev/pi-skill-library.git`):

- A pi extension that discovers `skills-library` directories and registers one `/library:<skill-name>` command per skill, expanding into pi's `<skill ...>...</skill>` block format. Extra roots come from the pi settings key `@alexgorbatchev/pi-skills-library.paths`. It prints a discovery report on startup.
- `src/piSkillLibraryExtension.ts` intercepts `/library:<name>` input in `pi.on("input")`; for a name that local discovery does not know it reports "Unknown library skill" and returns `{ action: "handled" }`.
- It calls `pi.registerCommand()` again when it refreshes discovery after session start.
- It builds against `@mariozechner/pi-coding-agent@0.73.1`; the installed pi is `@earendil-works/pi-coding-agent@1.3.0`.
- The `@modelcontextprotocol/sdk` TypeScript client's `StreamableHTTPClientTransport.close()` does not send HTTP `DELETE`; `terminateSession()` does (sdk 1.32.1).

### Client behavior

| Harness | Version | Verified behavior | Source |
|---|---|---|---|
| Claude Code | current | Tool search is on by default: only tool names and server instructions load at session start. A tool with `_meta["anthropic/alwaysLoad"]: true` loads upfront. Upfront tool descriptions and server instructions are cut at 4,096 characters. Refetches tools, prompts, and resources on `list_changed`. MCP prompts appear as `/<server>:<prompt>`, with arguments split on whitespace. Default MCP output limit is 25,000 tokens; text results over 50,000 characters are saved to a file. Supports Streamable HTTP with `headers`. The per-server `timeout` (milliseconds) is a hard wall-clock limit per tool call that progress notifications do not extend; unset, `MCP_TOOL_TIMEOUT` applies (default about 28 hours). For HTTP servers the first response byte must arrive within max(60 s, the tool timeout, `MCP_TIMEOUT`), and a call with no response or progress notification for the idle window (default 5 minutes, never shorter than a per-server `timeout` of at least 1000 ms) is aborted. | code.claude.com/docs/en/mcp |
| Codex | 0.164.0 installed | Supports Streamable HTTP with `bearer_token_env_var`. Builds an "initial tool catalog" at startup. Per-tool `output_token_limit`. `tool_timeout_sec` defaults to 60. Server instructions: the first 512 characters are the part reliably used. Deferral, `list_changed`, and prompt support are not documented. | learn.chatgpt.com/docs/extend/mcp |
| pi | 1.3.0 installed | Built-in MCP client over stdio and Streamable HTTP. Default `exposure` is `codemode`: the model sees no tool declarations. `"exposure": "direct"` declares tools to the model. Text results over 20 KB have their middle removed. Adds and withdraws tools on a changed tool list. Per-server `timeout` is the per-request timeout in seconds (default 60), reset by progress notifications. Tool names are exposed as `mcp__<server>__<tool>` with `-` replaced by `_`. Docs cover tools and resources only, not prompts. Extensions register slash commands with `pi.registerCommand()` and send user messages with `pi.sendUserMessage()`. Skills load from user and project skill directories, including `~/.agents/skills` and project `.agents/skills`; `--no-skills` disables every discovered skill. | `@earendil-works/pi-coding-agent@1.3.0/docs/{mcp,extensions,skills,cli}.md` |

The MCP protocol version each harness negotiates, and whether it falls back from 2026-07-28, is not yet recorded (check V6).

### Protocol and SDK

- MCP spec 2026-07-28 removes `initialize` and protocol sessions. Clients send capabilities in each request's `_meta`; `io.modelcontextprotocol/clientInfo` is a per-request SHOULD. List results do not vary per connection. Change notifications are delivered over a `subscriptions/listen` stream.
- `github.com/modelcontextprotocol/go-sdk` v1.8.0 supports spec versions 2024-11-05 through 2026-07-28 and provides `Server.AddTool`, `RemoveTools`, `AddPrompt`, `RemovePrompts`, and `NewStreamableHTTPHandler`.
- A stateful `StreamableHTTPHandler` (`Stateless: false`) accepts only protocol versions before 2026-07-28. A call carrying 2026-07-28 receives JSON-RPC error `CodeUnsupportedProtocolVersion` (`-32022`) listing the legacy versions, so the client can renegotiate (`mcp/streamable.go`, `docs/mcpgodebug.md`); the go-sdk client falls back to the legacy `initialize` handshake.
- `StreamableHTTPOptions.Stateless: true` accepts 2026-07-28 but returns 405 for GET and DELETE, so legacy clients receive no `list_changed`.
- go-sdk `Server.notifySessions` (`mcp/server.go`) delivers `list_changed` to legacy sessions on the session's standalone stream (opened with HTTP GET).
- go-sdk advertises the `tools` and `prompts` capabilities only when items exist at initialization, unless `ServerOptions.Capabilities` declares them.
- go-sdk cancels a handler's context when the client sends `notifications/cancelled`.
- `StreamableHTTPOptions` enables DNS-rebinding protection for localhost listeners by default (non-localhost `Host` → 403). Its `CrossOriginProtection` field is deprecated in favor of wrapping the handler with `http.CrossOriginProtection.Handler`. `http.CrossOriginProtection` always allows `GET`, `HEAD`, and `OPTIONS`.
- `StreamableHTTPOptions.SessionTimeout` closes sessions that receive no HTTP request for the duration; only POST requests pause the idle timer, so an open standalone GET stream does not keep a session alive. Zero disables the timeout.

### Git behavior

Verified with git 2.43 against local bare repositories:

- `git status --porcelain` reports a new untracked directory as one `?? <dir>/` line; `git diff` excludes untracked files and prints `Binary files ... differ` for blobs containing NUL unless `-a` is given.
- `git worktree remove` exits 128 on a worktree with modified or untracked files unless `--force` is given.
- `git push --porcelain` exits 1 on a rejected ref (`!` flag) and 0 with the `=` flag when the remote ref already equals the pushed commit.
- `git archive` applies `export-ignore` and `export-subst` from in-tree `.gitattributes`.
- Porcelain `git diff` honors `diff.external` and `.gitattributes` textconv drivers, so its output can differ from the blob content; `--no-ext-diff --no-textconv` disables both.
- A global `core.hooksPath` runs hooks for `worktree add` (`post-checkout`), `rebase` (`pre-rebase`, `prepare-commit-msg`, `post-commit`, `post-rewrite`), `push` (`pre-push`), and ref updates (`reference-transaction`).
- A rebase whose change already exists upstream drops the commit and exits 0 with `HEAD` at the upstream tip.
- `-c gc.auto=0` or `-c maintenance.auto=false` makes `git maintenance run --auto` skip garbage collection.
- Fetching `+refs/heads/<branch>:refs/heads/<branch>` into a bare repository with worktrees, and rebasing inside such a worktree, both work.

### Measured resource cost

A go-sdk v1.8.0 stdio server with 50 registered tools uses 12.5 MB RSS after `tools/list`: 4.4 MB anonymous memory per process and 8.1 MB file-backed binary pages that instances share.

## 3. Non-negotiable constraints

- Every skill tool must carry `_meta["anthropic/alwaysLoad"]: true`. Without it, Claude Code defers descriptions and skills never trigger. The Claude Code client configuration also sets server-level `"alwaysLoad": true` so startup waits for the daemon.
- Exactly one daemon process serves all harnesses and all agent sessions on the machine, enforced by a lock file.
- The daemon must bind `127.0.0.1` only and require a bearer token. DNS-rebinding protection uses go-sdk's built-in localhost protection; cross-origin protection wraps the handler with `http.CrossOriginProtection.Handler`; bearer authentication uses go-sdk `auth.RequireBearerToken`. The only custom HTTP code is the token verifier.
- All Git operations use the `git` binary so the user's SSH agent, signing, credential helpers, and remotes apply. Git never prompts, never runs hooks, and every Git command has a timeout.
- Served skill files are immutable for the lifetime of a snapshot, and served files are rendered: an agent reading an absolute path sees the same text the tool returns.
- Snapshot content from the source contains no symbolic links, and extraction never follows a link. `prepare` output may contain symbolic links only when they resolve inside the directory the rule ran in, and shares no inode with any file outside the snapshot.
- Template rendering accepts only the tag forms in [Template](#template); `env()` reads only variables listed in the source's `template.env`.
- A skill bundle with any diagnostic is not served, and the diagnostic reaches the user through every first-chunk skill load and every prompt on the affected harness, the daemon log, and `skills-mcp list`.
- Authoring never writes into a served snapshot or into a user's working tree. Agents mutate change worktrees only through authoring tools.
- Every check that gates a push runs against the exact commit being pushed, and a submit, once started, runs to completion independently of the client request.
- Every JSON state file is written atomically (temporary file, `fsync`, rename).
- Validation commands run from the trusted base snapshot, never from agent-modified files, and authoring cannot change them.
- Content pushed to a source must pass that source's `deny_patterns` scan over added lines (binary included), added and renamed file paths, and the commit message. A source with `writable: true` and empty `deny_patterns` is a configuration error.
- Publication never force-pushes.
- Go 1.26, Cobra with `cobra-help-tree/v2`, `log/slog`, XDG directories, binaries in `bin/`, `--version` prints only the version string.
- 90% test coverage, with behavioral tests only.

## 4. Architecture

```
GitHub repo (storage of record)
   │  git clone --bare; git fetch origin +refs/heads/<branch>:refs/heads/<branch>
   ▼
$XDG_STATE_HOME/skills-mcp/sources/<source>/repo.git
   │  git ls-tree + cat-file → temp dir → render templates → chmod → rename to rev/<sha>
   ▼
$XDG_CACHE_HOME/skills-mcp/sources/<source>/rev/<sha>/   (+ rev/<sha>.render.json)
   │  parse SKILL.md, roots + harness roots + precedence, collect diagnostics
   ▼
catalog (one view per harness per retained revision set)
   │  diff against previous view → AddTool / RemoveTools / AddPrompt / RemovePrompts
   ▼
one mcp.Server per harness, mounted at /mcp/<harness> (stateful Streamable HTTP, legacy protocol versions)
   ├─ model skills → tools (alwaysLoad)
   ├─ user skills  → prompts ──► pi-skill-library /library:<name> (pi)
   └─ authoring    → skill_change_* tools
                      │
                      ▼
       $XDG_STATE_HOME/skills-mcp/changes/<change_id>/worktree  (git worktree on a change branch)
                      │  commit → verify the commit → push to refs/heads/<branch> (rebase and re-verify on rejection)
                      ▼
                   GitHub repo ──► synchronous refresh of the source
```

### Chosen design

- **One tool per model skill** with the skill description as the tool description, per the original request.
- **One local daemon over Streamable HTTP** (Q3A1). Per-agent stdio processes multiply catalogs, file watchers, fetch pollers, and Git writers, and an in-process lock cannot serialize writers across processes.
- **Stateful Streamable HTTP handler on legacy protocol versions** (`Stateless: false`, `SessionTimeout: 0`). Legacy sessions receive `list_changed` on their GET stream; go-sdk answers 2026-07-28 calls with `-32022` so clients renegotiate to a legacy version. A stateless handler would serve 2026-07-28 but deliver no `list_changed` to legacy clients. A nonzero timeout would close idle agent sessions, because an open GET stream does not reset the idle timer. Sessions end on client `DELETE` or daemon restart; sessions abandoned without `DELETE` stay in memory until restart (measured by V7).
- **Harness chosen by URL path** (`/mcp/claude-code`, `/mcp/codex`, `/mcp/pi`; Q9A1). Client identity is optional per request, and a distinct path makes each harness view a distinct server.
- **Durable Git state, disposable cache.** `repo.git` holds change branches and unpushed commits, so it lives under `$XDG_STATE_HOME`; rendered snapshots are rebuildable from it and live under `$XDG_CACHE_HOME`.
- **Immutable, rendered snapshots** (Q1A5). Each revision is materialized from `git ls-tree` and `git cat-file` (no `.gitattributes` processing), filtered by the source's ignore file, rendered, and made read-only before it becomes visible. Absolute-path reads return rendered text, and agents cannot bypass authoring tools by editing the cache. `path` sources are snapshotted the same way.
- **Fixed template context, configured per source** (Q1A5). The renderer replaces `<%- name %>` with a configured variable or the built-in `file_dir` and `file_path`, and `<%- env("NAME", "fallback") %>` with an allowlisted environment variable. Any other tag refuses the bundle. ai-registry's path variables point at the user's checkout, because skills using them instruct agents to edit registry sources there; `file_dir` and `file_path` point into the snapshot, where bundled scripts live.
- **Author-prefixed served names**, matching what the registry installs today: a bundle whose `author` is set and outside the source's `author_prefix_except` list is served as `lowercase(author)-<name>`, unless the name already starts with that prefix.
- **Refusals reported in-band.** Each harness view carries the diagnostics of the bundles it refused. First-chunk skill loads and prompts on that harness append them, so whichever agent the user is working with reports them, without changing tool definitions or prompt caches.
- **Change handles for authoring** (Q7A1). Multi-step edits are grouped by a server-minted `change_id` passed as a tool argument. Handles survive daemon restarts and work for clients on every protocol version. Each mutation stages its paths, and each change produces one commit.
- **Commit first, then verify the commit.** Submit creates the commit with hooks disabled, then runs every gate against that exact commit, materialized the same way a snapshot is: full catalog build for all harnesses (duplicates, template, structure), the source's `validate` command, scope, and deny scan.
- **Direct push to the tracked branch** (Q2A2). A verified commit is pushed to `refs/heads/<branch>`; on a non-fast-forward rejection the change is rebased onto the new tip and re-verified before the next attempt. After the push the source refreshes synchronously, so the change is live when submit returns.
- **User skills in pi through `pi-skill-library`** (Q5A1). pi's built-in MCP client does not expose prompts; the existing `/library:<name>` extension gains the daemon as a library source.
- **Registry skills leave harness homes; project skills stay native** (Q6). ai-registry stops installing served skills into the claude-code, codex, and pi homes. Each harness's project-level skill discovery stays enabled.

### Rejected alternatives

| Alternative | Reason rejected |
|---|---|
| Single `load_skill(name)` tool with an index | Loses per-skill descriptions as triggers, which is the core requirement. |
| Skill descriptions in server instructions | Instructions are cut at 4,096 characters in Claude Code and 512 effective characters in Codex. |
| stdio process per agent session | Duplicates fetch, watch, and catalog work per session; Git writers race across processes. |
| Stateless Streamable HTTP handler | Legacy clients never receive `list_changed`. |
| Stateless and stateful handlers side by side, routed by `Mcp-Protocol-Version` | Adds a custom dispatch layer in front of go-sdk for clients that go-sdk already steers to a legacy version with `-32022`. Revisit only if V6 finds a harness that does not fall back. |
| Harness detection from `clientInfo` | Optional per request under 2026-07-28; list results must not vary per connection. |
| Custom Host and Origin middleware | Duplicates go-sdk localhost protection and `http.CrossOriginProtection`. |
| `StreamableHTTPOptions.CrossOriginProtection` | Deprecated in go-sdk v1.8.0. |
| Nonzero `SessionTimeout` | Closes idle agent sessions and their `list_changed` stream. |
| go-git | Bypasses the user's SSH agent, signing, and credential helper configuration. |
| Unified-diff patch input | Models produce invalid hunks; exact `old_string` / `new_string` replacement fails loudly and precisely. |
| Commit per mutation call | Produces one commit per edit and cannot group a multi-file change (Q7A2). |
| Serving from the bare repo or a worktree | Mutable under fetch; agents can edit it directly. |
| `git archive` extraction | Applies in-tree `export-ignore` and `export-subst`, so served content can differ from the validated commit. |
| Validating the worktree before committing | Ignored files, commit hooks, and direct worktree edits make the validated tree differ from the pushed commit. |
| Deny scan over porcelain `git diff` | `diff.external` and textconv drivers can rewrite the scanned text. |
| Browser access through `allowed_origins` | Every request needs the bearer token, and a CORS preflight never carries it, so no browser origin can succeed. |
| Running the submit pipeline on the request context | Client timeouts and `notifications/cancelled` would kill a push or rebase midway. |
| Running `validate` from the change worktree | The agent can modify the validator inside its own change. |
| `repo.git` under `$XDG_CACHE_HOME` | Clearing the cache would destroy unpushed change commits. |
| ai-registry build target as the served tree (Q1A1) | Served files become build output; authoring needs a reverse mapping to sources. |
| Dedicated untemplated skills repository (Q1A4) | Moves skills out of ai-registry. |
| Rendering only at tool-call time | Absolute-path reads, which agents use for bundled files and scripts, would return raw tags. |
| Unrestricted `env()` | An authored skill could publish any daemon environment variable, including credentials, into served text. |
| Unprefixed vendored names | Renames 16 installed skills and loses the prefix that keeps vendored names from colliding. |
| Proposal branch and pull request (Q2A1) | Rejected by the Q2 answer. |
| Force push on rejection | Discards commits pushed by others. |
| Writable `path` sources (Q8A2) | Writes into a user's working tree with no history or review point. |
| User skills as tools in Codex and pi (Q5A2) | The model could invoke user-only skills. |

## 5. On-disk layout

| Path | Owner | Contents |
|---|---|---|
| `$XDG_CONFIG_HOME/skills-mcp/config.yaml` | user | Daemon configuration, see `Config`. Local only; never committed. |
| `$XDG_CONFIG_HOME/skills-mcp/env` | user | Optional `KEY=value` lines loaded by the systemd unit with `EnvironmentFile=-`: `PATH` (must include the `validate` and `prepare` interpreters, for example `bun` in `~/.bun/bin`), `SSH_AUTH_SOCK`, and values for `template.env` names. |
| `$XDG_CONFIG_HOME/skills-mcp/token` | daemon | Bearer token: exactly 64 lowercase hex characters (32 random bytes), mode `0600`. Created by `serve` or `token` when absent: the token is written and synced to a temporary file with mode `0600` in the same directory, then `link`ed to the final name; on `EEXIST` the temporary file is removed and the existing file is used. Every read rejects content that is not exactly 64 hex characters (one trailing newline allowed) or a mode other than `0600`, and exits with status 1. |
| `$XDG_STATE_HOME/skills-mcp/lock` | daemon | `flock` held by `serve` for its lifetime. |
| `$XDG_STATE_HOME/skills-mcp/sources/<source>/repo.git` | daemon | Bare clone of a `git` source, including change branches. Cloned into `repo.git.tmp-<random>` and renamed into place; leftover `repo.git.tmp-*` directories are deleted at startup. |
| `$XDG_STATE_HOME/skills-mcp/changes/.quarantine/` | daemon | Unparsable `change.json` files moved aside at startup, named `<change_id>.json`. |
| `$XDG_STATE_HOME/skills-mcp/changes/<change_id>/change.json` | daemon | `ChangeRecord`. |
| `$XDG_STATE_HOME/skills-mcp/changes/<change_id>/worktree/` | daemon | Git worktree of `repo.git` on branch `skills-mcp/change/<change_id>`. |
| `$XDG_CACHE_HOME/skills-mcp/sources/<source>/rev/<revision>/` | daemon | Rendered, read-only snapshot of `<revision>`. Directories `0555`, files `0444`; files with Git mode `100755` (or an executable bit on disk for `path` sources) are `0555`; `prepare` output files are `0555` when their owner-execute bit is set after `prepare`, `0444` otherwise. |
| `$XDG_CACHE_HOME/skills-mcp/sources/<source>/rev/<revision>.render.json` | daemon | `RenderRecord` for the snapshot. Written before the snapshot directory is renamed into place. |
| `$XDG_CACHE_HOME/skills-mcp/sources/<source>/rev/.tmp-<revision>-<random>/` | daemon | In-progress extraction from `os.MkdirTemp`; deleted at startup when present. |
| `$XDG_CACHE_HOME/skills-mcp/sources/<source>/rev/.tmp-path-<random>/` | daemon | In-progress `path` source copy before its revision is known; deleted at startup when present. |
| `$XDG_CACHE_HOME/skills-mcp/sources/<source>/rev/.tmp-verify-<random>/` | daemon | Submit verification tree; deleted after verification and at startup. |
| `$XDG_CACHE_HOME/skills-mcp/sources/<source>/snapshots.json` | daemon | `SnapshotIndex`. |

Removing a read-only tree first adds owner write permission to every directory in it. At startup, a `rev/<revision>.render.json` without a matching `rev/<revision>/` directory is deleted, a `rev/<revision>/` directory without its `RenderRecord` is deleted, and a `rev/<revision>/` that is neither `SnapshotIndex.current` nor a key of `superseded` is deleted with its `RenderRecord`.

## 6. Types and contracts

Wire and file contracts are written as TypeScript types. The Go implementation mirrors them with `json`/`yaml` struct tags using the same field names.

### Configuration (`config.yaml`)

```ts
type HarnessId = 'claude-code' | 'codex' | 'pi';
type Invocation = 'model' | 'user';

interface Config {
  listen: string;                 // default "127.0.0.1:7341"; host must be 127.0.0.1
  fetch_interval: string;         // Go duration, default "60s"
  submit_timeout: string;         // Go duration, default "600s"; deadline for one submit pipeline run
  snapshot_retention: string;     // Go duration, default "24h"
  change_ttl: string;             // Go duration, default "168h"; also how long submitted and discarded records are kept
  default_write_source: string | null; // default null; when null, authoring tools are not registered
  harnesses: Record<HarnessId, HarnessConfig>;
  sources: SourceConfig[];        // order defines precedence, first wins
}

interface HarnessConfig {
  max_result_bytes: number;       // defaults: claude-code 48000, codex 32000, pi 19000
}

interface RootConfig {
  path: string;                   // repo-relative; in harness_roots, "{harness}" is replaced by the HarnessId
  invocation: Invocation;
}

interface PrepareRule {
  when_file: string;              // file name; the rule runs in every source directory inside a bundle (at any depth) that contains it, computed before any rule runs
  run: string[];                  // argv, run with that directory as the working directory; non-empty
  keep: string[];                 // paths relative to that directory exempt from ignore_file filtering, e.g. a lockfile
  timeout: string;                // Go duration, default "300s"
}

interface TemplateConfig {
  vars: Record<string, string>;   // name ^[a-z][a-z0-9_]*$ → value; "~/" prefix expanded; "{snapshot}" replaced by the final snapshot root;
                                  // merged over the default { repo_root: "{snapshot}" }; "file_dir" and "file_path" are reserved
  env: string[];                  // ^[A-Z_][A-Z0-9_]*$; the only names env() may read; default []
}

interface SourceBase {
  name: string;                   // ^[a-z0-9]+(-[a-z0-9]+)*$, unique
  roots: RootConfig[];
  harness_roots: RootConfig[];
  template: TemplateConfig;
  ignore_file: string | null;     // gitignore-style per-directory ignore file name, e.g. ".registry-ignore"; default null
  author_prefix_except: string[] | null; // null: no prefixing; otherwise see "Served name" below
  prepare: PrepareRule[];         // default []; run at snapshot extraction and submit verification
}

interface GitSource extends SourceBase {
  kind: 'git';
  url: string;
  branch: string;
  writable: boolean;
  validate: string[];             // argv run per touched bundle on submit, from rev/<base_revision>/; "{bundle_dir}" is replaced; [] disables.
                                  // Snapshots contain only the served roots, so repo-relative scripts it names must live under a root.
  deny_patterns: string[];        // Go RE2 patterns; must be non-empty when writable is true
}

interface PathSource extends SourceBase {
  kind: 'path';
  path: string;                   // absolute, or ~-prefixed; resolved once at startup with filepath.EvalSymlinks, links below it are refused
}

type SourceConfig = GitSource | PathSource;
```

A source's `prepare` rules run in configuration order, once per matching directory; their output is never template-rendered.

Served name, when `author_prefix_except` is non-null: a bundle with no `author`, or whose `author` is in the list (exact match), is served under its frontmatter `name`. Otherwise the prefix `p` is `lowercase(author) + "-"`; the served name is `name` when `name` starts with `p`, else `p + name`.

Configuration errors (exit status 1, field-level message): `listen` host other than `127.0.0.1`; `submit_timeout` under `60s`; a `prepare` rule with empty `when_file` or `run`, or a non-positive `timeout`; `fetch_interval`, `snapshot_retention`, or `change_ttl` not positive; duplicate source names; a non-null `default_write_source` not naming a `git` source with `writable: true`; a `writable` source with empty `deny_patterns`; an invalid RE2 pattern; a template variable or env name not matching its pattern; a reserved template variable name; an unparsable duration.

Example for ai-registry:

```yaml
listen: 127.0.0.1:7341
default_write_source: ai-registry
harnesses:
  claude-code: { max_result_bytes: 48000 }
  codex: { max_result_bytes: 32000 }
  pi: { max_result_bytes: 19000 }
sources:
  - name: ai-registry
    kind: git
    url: git@github.com:alexgorbatchev/ai-registry.git
    branch: main
    writable: true
    roots:
      - { path: skills/agent, invocation: model }
      - { path: skills/user, invocation: user }
    harness_roots:
      - { path: "harnesses/{harness}/skills/agent", invocation: model }
    ignore_file: .registry-ignore
    author_prefix_except: [alexgorbatchev, agorbatchev]
    template:
      vars:
        repo_root: ~/development/workspace/ai/standards/ai-registry
        skills_dir: ~/development/workspace/ai/standards/ai-registry/skills
        commands_dir: ~/development/workspace/ai/standards/ai-registry/commands
        profiles_dir: ~/development/workspace/ai/standards/ai-registry/profiles
        output_dir: ~/development/workspace/ai/standards/ai-registry/.output
        overlay_dir: ""
      env: [DOCS_INTERNAL_DIR]
    prepare:
      - when_file: package.json
        run: [bun, install, --frozen-lockfile, --ignore-scripts, --backend=copyfile]
        keep: [bun.lock]
    validate: [bun, skills/agent/skill-writer/scripts/quick-validate.ts, "{bundle_dir}"]
    deny_patterns:
      - '<private-domain pattern>'   # one entry per private domain; config.yaml is never committed
```

### Snapshot index (`snapshots.json`)

```ts
interface SnapshotIndex {
  render_context: string;                       // RenderContextHash of the context the retained snapshots were rendered with
  current: string;                              // revision served as the source tip
  superseded: Record<string, string>;           // revision → RFC 3339 time it stopped being current or was extracted on demand
}
```

`RenderContextHash`: the first 12 hex characters of SHA-256 over the canonical JSON object `{ "renderer": <RendererVersion>, "roots": <roots as configured>, "harness_roots": <harness_roots as configured>, "vars": <vars after "~/" expansion with "{snapshot}" left literal, keys sorted>, "env": <name → value, or null when unset, keys sorted>, "ignore_file": <ignore_file>, "prepare": <prepare as configured> }`. `author_prefix_except` is not an input: it affects catalog building, not snapshot contents. `RendererVersion` is an integer constant in `internal/catalog/template.go`, incremented whenever rendering or ignore semantics change. At startup, when the hash differs from `SnapshotIndex.render_context`, every `rev/` entry of that source is deleted, `current` is cleared, `superseded` is emptied, and `render_context` is updated before extraction.

### Render record (`rev/<revision>.render.json`)

```ts
interface RenderRecord {
  revision: string;
  render_context: string;                       // RenderContextHash used for this snapshot
  bundles: string[];                            // repo-relative bundle directories found under all expanded roots, sorted
  files: Record<string, string[]>;              // bundle → bundle-relative source files after ignore filtering and before prepare, sorted
  prepare_retry: boolean;                       // a prepare rule failed; Refresh retries this revision with backoff
  diagnostics: CatalogDiagnostic[];             // extraction diagnostics (template, symlink, submodule, missing_skill_md, prepare_failed), sorted by bundle, file, line
}
```

### Catalog

```ts
interface SkillBundle {
  name: string;                   // served name: frontmatter name, prefixed "lowercase(author)-" per "Served name"
  skill_name: string;             // frontmatter name == bundle directory name
  description: string;
  invocation: Invocation;
  source: string;                 // SourceConfig.name
  revision: string;               // git sources: commit sha; path sources: see "Path source revision"
  bundle: string;                 // repo-relative bundle directory, e.g. "skills/agent/golang"
  base_dir: string;               // absolute snapshot directory served to agents
  body: string;                   // rendered SKILL.md without frontmatter
  files: string[];                // RenderRecord.files for the bundle, excluding SKILL.md
}

type DiagnosticCode =
  | 'invalid_frontmatter'         // missing delimiters or not a YAML mapping
  | 'invalid_name'                // frontmatter or served name fails the name pattern or length
  | 'name_mismatch'               // frontmatter name differs from the bundle directory name
  | 'invalid_description'         // empty, not a string, or over 1,024 characters
  | 'invalid_author'              // author_prefix_except is set and author is not a string, or lowercase(author) fails the name pattern
  | 'reserved_name'               // served name collides with an authoring tool name after "-" → "_"
  | 'duplicate_name'              // two bundles of one source share a served name within one harness view
  | 'symlink'                     // the bundle contains a symbolic link
  | 'submodule'                   // the bundle contains a submodule entry
  | 'missing_skill_md'            // ignore rules match the bundle's SKILL.md
  | 'prepare_failed'              // a prepare rule exited non-zero, timed out, or produced a link leaving its directory
  | 'unsupported_template_tag'    // a "<%" that does not start an accepted tag
  | 'unknown_template_variable'   // <%- name %> with name not in template.vars and not built-in
  | 'env_not_allowed'             // env("NAME") with NAME not in template.env
  | 'env_unset';                  // env("NAME") without fallback and NAME unset

interface CatalogDiagnostic {
  code: DiagnosticCode;
  source: string;
  revision: string;
  bundle: string;                 // repo-relative bundle directory
  file: string;                   // bundle-relative path
  line: number | null;            // 1-based; null when the diagnostic has no line
  message: string;                // one line, no trailing period, e.g. 'unknown template variable "output_dir"'
}

interface HarnessView {
  harness: HarnessId;
  skills: SkillBundle[];          // sorted by name; names unique within the view
  diagnostics: CatalogDiagnostic[]; // refused bundles visible to this harness, sorted by source, bundle, file, line
}
```

Path source revision: `"local-"` followed by the first 12 hex characters of SHA-256 over the sorted list of `(relative path, mode, SHA-256 of contents)` for every non-ignored regular file under the source's roots and harness roots, plus `(relative path, Lstat mode, link target)` for every recorded symlink or other non-regular entry.

### Change record (`change.json`)

```ts
type ChangeState = 'open' | 'submitted' | 'discarded';

interface ChangeRecord {
  change_id: string;              // "chg_" + 16 lowercase hex characters from crypto/rand
  source: string;
  harness: HarnessId;             // endpoint the change was opened on; governs {harness} expansion for the change's lifetime
  base_revision: string;          // sha the change is based on; updated by rebase
  branch: string;                 // "skills-mcp/change/<change_id>"
  summary: string;
  state: ChangeState;
  created_at: string;             // RFC 3339
  updated_at: string;             // RFC 3339, set on every mutation and submit attempt
  touched_bundles: string[];      // repo-relative bundle directories, sorted, unique
  message: string | null;         // commit message, set when commit is created
  commit: string | null;          // change commit; set by submit, replaced by rebase, cleared by a mutation, failed verification, an already-upstream rebase, or reconciliation
  rebase_onto: string | null;     // tip a rebase is moving the change onto; set before the Git command, cleared after the record update
  push_started: boolean;          // set and persisted before each push; cleared when the push result is recorded
  last_error: string | null;
  publication: Publication | null; // set only in state "submitted"
}

interface Publication {
  remote: 'origin';
  ref: string;                    // "refs/heads/<branch>"
  commit: string;                 // sha of the change commit as it exists on the remote branch
  attempts: number;               // pushes made by the submit call that published the change, 1–3; 0 when reconciliation detected the publication
  pushed_at: string;              // RFC 3339
  refresh_error: string | null;   // set when the post-push Refresh failed; the change is still published
}
```

## 7. File plan

Scaffold with `go-scaffold`; module path `github.com/alexgorbatchev/skills-mcp`. Gitignore pattern matching for `ignore_file` uses `github.com/go-git/go-git/v5/plumbing/format/gitignore` (`ParsePattern`, `NewMatcher`) as a pattern matcher only; all Git operations still use the `git` binary. The `./` prefix rule is implemented in `internal/source/ignore.go`. Per-revision extraction deduplication uses `golang.org/x/sync/singleflight`.

### Add

| File | Responsibility |
|---|---|
| `go.mod` | Module, `go 1.26`, `tool` directives for dev tools. |
| `justfile` | `setup`, `run` (daemon), `cli` (binary with args), `build`, `check`, `test`, `coverage`. |
| `.gitignore` | `bin/`, coverage output. |
| `AGENTS.md` | Project instructions, written with `docs-writer`. |
| `README.md` | CLI README, written with `docs-writer`. Includes client configuration with timeouts, systemd environment, native skill loading, the project-skill name overlap, prompt argument quoting in Claude Code, and the unsupported `allowed-tools` and `` !`command` `` features with the affected skills. |
| `contrib/systemd/skills-mcp.service` | systemd user unit running `skills-mcp serve` with `EnvironmentFile=-%E/skills-mcp/env`, `KillMode=mixed` (SIGTERM to the daemon only), and `TimeoutStopSec=60`. |
| `cmd/skills-mcp/main.go` | Entry point; calls `internal/cli`. |
| `internal/cli/root.go` | Cobra root, `cobra-help-tree/v2`, `--version`, `--config`. |
| `internal/cli/serve.go` | `serve`: take the lock, load config, ensure token, start source manager and HTTP server, handle SIGTERM and SIGINT per [Shutdown](#shutdown). |
| `internal/cli/list.go` | `list [--harness <id>]`: build the catalog from the cache and print one line per skill, then every diagnostic on stderr; exits 1 when any diagnostic exists. |
| `internal/cli/validate.go` | `validate [--source <name>] <bundle-dir>...`: run the structural, template, and symlink rules except the bundle-directory rule, using the configuration of `--source` (default `default_write_source`, required when that is null); exits 1 on failure. Cross-bundle uniqueness is checked by `list`. |
| `internal/cli/token.go` | `token`: create the token file when absent and print the token to stdout. |
| `internal/config/config.go` | Parse `config.yaml`, apply defaults, validate fields. |
| `internal/paths/paths.go` | XDG config, cache, and state directories for the app. |
| `internal/gitx/git.go` | `git` exec wrapper: fixed environment and `-c` options, per-command timeouts, stderr capture, per-source mutex registry, `git push --porcelain` result parsing. |
| `internal/source/git.go` | Bare clone, refspec fetch, tree materialization from `git ls-tree -r -z` and `git cat-file --batch`. |
| `internal/source/path.go` | `path` sources: recursive fsnotify watch with a 500 ms debounce, path source revision, materialization by copy. |
| `internal/source/prepare.go` | `prepare` rule execution in extraction and verification trees: matching directories, argv execution through the shared process wrapper, link and inode checks, `prepare_failed` diagnostics. |
| `internal/source/ignore.go` | `ignore_file` evaluation: gitignore patterns from ignore files at or below the bundle directory, the `./` prefix rule, `node_modules`, and the ignore files themselves. |
| `internal/source/snapshot.go` | Shared extraction: ignore filtering, symlink and submodule diagnostics, render, `RenderRecord`, permissions, atomic rename, singleflight, startup cleanup, `RenderContextHash`. Also materializes a commit into `rev/.tmp-verify-*` for submit verification. |
| `internal/source/manager.go` | `Refresh(source)`, refresh loop, `SnapshotIndex` persistence under the index mutex, snapshot retention, catalog rebuild requests, hourly `git maintenance run --auto` per source. |
| `internal/catalog/parse.go` | Frontmatter split and YAML decode, file listing. |
| `internal/catalog/template.go` | `RendererVersion`, tag scanner, and renderer for the accepted tag forms; returns rendered bytes and template diagnostics. |
| `internal/catalog/build.go` | Root and harness-root expansion, author prefixing, precedence, `HarnessView` construction with diagnostics, per-revision catalog cache. |
| `internal/catalog/validate.go` | Structural validation rules from section 9, producing `CatalogDiagnostic` values. |
| `internal/catalog/diff.go` | Compare two `HarnessView`s into added, changed, and removed tools and prompts. |
| `internal/render/render.go` | Skill and file result rendering, `<catalog_diagnostics>` element, byte-budget chunking, argument validation for `offset` and `revision`. |
| `internal/mcpserver/http.go` | HTTP mux, `/mcp/<harness>` routing for GET, POST, and DELETE, `auth.RequireBearerToken` with a `crypto/subtle.ConstantTimeCompare` verifier returning `TokenInfo` without expiration and `RequireBearerTokenOptions.AllowMissingExpiration: true`, `StreamableHTTPOptions`, `http.CrossOriginProtection` wrapper. |
| `internal/mcpserver/server.go` | One `mcp.Server` per harness with `ServerOptions.Capabilities` declaring `Tools` and `Prompts` with `ListChanged: true`, server instructions, view rebuilds and diff application under the view mutex, diagnostic logging. |
| `internal/mcpserver/skilltools.go` | Skill tool definitions and handler, registered with generic `mcp.AddTool[In, Out]` and an explicit `InputSchema` so go-sdk validates arguments. |
| `internal/mcpserver/prompts.go` | User-skill prompt definitions and handler. |
| `internal/mcpserver/authoringtools.go` | Authoring tool definitions and handlers, registered the same way as skill tools. |
| `internal/authoring/store.go` | Atomic `ChangeRecord` persistence, in-memory index of open changes' `base_revision` values, quarantine of unparsable records, TTL sweep. |
| `internal/authoring/reconcile.go` | Change reconciliation at startup and at the start of every submit and mutation, and its reduced form for discards: rebase abort, `HEAD` repair, published detection, orphaned worktree and branch removal. |
| `internal/authoring/change.go` | Open, discard, worktree lifecycle, lock ordering, commit reset on mutation. |
| `internal/authoring/mutate.go` | Read, create, edit, and write operations confined with `os.Root`, staging each written path. |
| `internal/authoring/submit.go` | Submit pipeline on a detached context with `submit_timeout`, progress notifications, commit, verification of the commit, idempotent result for submitted changes. |
| `internal/authoring/verify.go` | Commit verification: materialize, catalog diagnostics against base, `validate`, protected paths, scope, deny scan. |
| `internal/authoring/deny.go` | `deny_patterns` scan. |
| `internal/authoring/publish.go` | Push, already-published detection, rejection handling, rebase, re-verification loop, post-push cleanup and refresh. |

Each Go file above has a sibling `_test.go` file with the same base name. Integration tests live in `internal/mcpserver/integration_test.go` and use local bare repositories over `file://` URLs.

### Changes in other repositories

Each change is made, tested, and committed in its own repository under that repository's conventions.

| Repository | Change |
|---|---|
| `ai/standards/ai-registry` | The claude-code and pi harness builds stop staging bundles from `skills/agent`, `skills/user`, and `harnesses/<target>/skills/agent` into profile output. The codex build stops staging `skills/agent` and `harnesses/codex/skills/agent`, and keeps staging `skills/user` until V4 passes or Q10 is answered. Profile-local skills (`profiles/<profile>/skills`) are still staged. The opencode build and `syncProjectSkills.ts` are unchanged. Each harness's removal is gated on its client checks (section 11, step 11). |
| `ai/pi/pi-skill-library` | Moves its pi dependency to `@earendil-works/pi-coding-agent` 1.3.0 and adds the daemon as a library source, per the contract below. |

`pi-skill-library` contract:

```ts
// Pi settings block "@alexgorbatchev/pi-skills-library"
interface PiSkillsLibrarySettings {
  paths?: string[];                       // existing
  server?: {
    url: string;                          // "http://127.0.0.1:7341/mcp/pi"
    token_env: string;                    // environment variable holding the bearer token, e.g. "SKILLS_MCP_TOKEN"
  };
}
```

- On session start, when `server` is set, the extension creates one `@modelcontextprotocol/sdk` Streamable HTTP client for the pi session, sending `Authorization: Bearer <value of token_env>`. Every discovery refresh calls `prompts/list` on that client. A client is replaced only after a connection error, which includes any `404` response to a request or to the GET stream and any failed stream; the replaced client is closed with `terminateSession()` followed by `close()`.
- Daemon prompts join local library skills in one name table. A local library skill wins over a daemon prompt of the same name; the collision is listed in the discovery report.
- The extension registers `/library:<name>` with `pi.registerCommand()` for every name in the table, and its `input` handler resolves `/library:<name>` against the same table, so daemon names are never reported as unknown.
- Invoking a daemon name calls `prompts/get` with `{ name, arguments: { args } }`, where `args` is the text after the command, omitted when empty, and sends the returned message text with `pi.sendUserMessage()`.
- On `notifications/prompts/list_changed`, the extension re-runs discovery and re-registers commands, the same way its existing refresh does.
- The discovery report lists the server URL, its prompts, and any connection or listing error. A server error leaves local library commands working.
- On session shutdown the client is closed with `terminateSession()` followed by `close()`, so the daemon releases the session.

## 8. Runtime behavior

### Git execution

- Every `git` invocation runs with `GIT_TERMINAL_PROMPT=0`, `SSH_ASKPASS_REQUIRE=never`, and `-c core.hooksPath=/dev/null`. When neither `GIT_SSH_COMMAND` nor `core.sshCommand` is set, it also sets `GIT_SSH_COMMAND="ssh -o BatchMode=yes"`. Every invocation except the hourly maintenance command also passes `-c gc.auto=0 -c maintenance.auto=false`. The maintenance command runs as `git -c gc.autoDetach=false -c maintenance.autoDetach=false maintenance run --auto` with a 600-second timeout, so it stays in its process group under the source mutex; a killed run leaves only temporary packs. Every invocation except `git check-ignore` sets `GIT_LITERAL_PATHSPECS=1`, so agent-supplied paths are never globs. `git check-ignore` rejects literal pathspec magic (exit 128 in git 2.43) and already matches its inputs as literal pathnames; it runs as `git check-ignore --stdin -z` with the path on stdin.
- Commands that produce diffs for scanning use plumbing with `--no-ext-diff --no-textconv --no-color`. Every command whose output lists paths runs with `-z` (or `--porcelain -z`), so no path is quoted.
- Timeouts: 120 seconds for `clone`, `fetch`, `push`, and `ls-remote`; 60 seconds for every other command; each also bounded by the caller's deadline. Each command runs in its own process group (`SysProcAttr.Setpgid`); a timeout sends SIGTERM to the whole group from `cmd.Cancel`, and after a 5-second grace period the wrapper's own timer sends `syscall.Kill(-pgid, SIGKILL)` to the group; `cmd.WaitDelay` is 6 seconds, and the operation fails with the command name and the timeout. The `validate` command runs the same way.
- Commits made by the daemon also pass `--no-verify`. Commit signing follows the user's Git configuration.
- Git commands run on daemon-owned contexts: the submit pipeline's detached context, the refresh loop's context, the startup context, or, for every other authoring tool, `context.WithoutCancel(requestCtx)` with a 120-second deadline held under the change mutex. They never run on an MCP request context. Cleanup after a failed or expired step (`git rebase --abort`, `git reset --soft`) runs on a fresh `context.WithoutCancel` with a 60-second deadline.

### Locks

- **Refresh mutex** (per source): held for the whole of a `Refresh` run, so runs on one source never overlap.
- **View mutex** (process-wide): serializes harness view rebuilds and diff application.
- **Index mutex** (per source): serializes every read and read-modify-write of `SnapshotIndex` and its file.
- **Catalog mutex** (process-wide, leaf): guards the per-revision catalog cache.
- **Source mutex** (per source): held only around individual Git commands, or short fixed sequences of them, that read or write refs, objects, or the index of `repo.git` or a change worktree: clone, fetch plus the following `rev-parse`, each `ls-tree` and `cat-file` read during materialization, worktree add, remove, and prune, add, commit, reset, rebase plus abort, push, branch deletion, and maintenance.
- **Change mutex** (per change): covers the change's record and worktree files for the whole of a mutation or submit.
- Lock order is change mutex, refresh mutex, source mutex, index mutex, view mutex, catalog mutex. No code path acquires a lock that precedes one it holds. In particular, view-mutex holders never acquire the index or source mutex (they use `current` values read under the index mutex beforehand), and no code path waits on a singleflight key or on the view mutex while holding a source mutex.
- Retention reads open changes' `base_revision` values from the store's in-memory index, which the store updates atomically on every record write; it takes no change mutex.
- Snapshot extraction is deduplicated per (source, revision) with `singleflight`; each extraction writes to its own `os.MkdirTemp` directory and renames it into place, discarding its own copy when the target already exists.

### Startup

1. Take an exclusive `flock` on `$XDG_STATE_HOME/skills-mcp/lock`; when it is held, exit with status 1 naming the lock file.
2. Load `config.yaml`; exit with status 1 and a field-level error on invalid config.
3. Create or read the token file.
4. For each `git` source: delete stale Git lock files (`*.lock`) under `repo.git`, including `repo.git/worktrees/*/`; the daemon lock guarantees no daemon-owned Git process is running. For each source: delete `repo.git.tmp-*`, `rev/.tmp-*` directories, orphaned `RenderRecord` files, snapshot directories without a `RenderRecord`, and snapshot directories that are neither `current` nor in `superseded`; compute `RenderContextHash` and apply the reset rule in [Snapshot index](#snapshot-index-snapshotsjson).
5. Load the change store from directories under `changes/` whose name matches `^chg_[0-9a-f]{16}$`: move unparsable `change.json` files to `changes/.quarantine/` with a `warn` log; delete `submitted` and `discarded` records older than `change_ttl`; build the open-change `base_revision` index.
6. For each `git` source: run `Refresh(source)` with view rebuilds suppressed. A failure is logged with Git's stderr. A source without `repo.git` contributes no skills until a later `Refresh` clones it.
7. For each writable `git` source with a `repo.git`: run `git ls-remote origin refs/heads/<branch>` and log a warning with Git's stderr on failure.
8. For each `path` source: run `Refresh(source)` with view rebuilds suppressed.
9. Reconcile every open change (see [Change reconciliation](#change-reconciliation)), with any `Refresh` it calls running with view rebuilds suppressed. A reconciliation failure is logged at `warn` and leaves the change open; a TTL discard still applies to it. Then discard the other open changes whose `updated_at` is older than `change_ttl`.
10. Delete directories under `changes/` whose name matches `^chg_[0-9a-f]{16}$` and that have no `change.json`, run `git worktree prune`, then delete `skills-mcp/change/<change_id>` branches without a record. A failure in this step is logged at `warn` and does not stop startup.
11. Build all harness views, log diagnostics, register tools and prompts, and start listening. Then start the `path` source watchers, the refresh loop, the hourly sweep, and the hourly maintenance timer; no view rebuild runs before this step.

### Shutdown

On SIGTERM or SIGINT, within the unit's 60-second `TimeoutStopSec`:

1. Cancel the process-wide shutdown context. Every pipeline and refresh checks it before starting each Git or `validate` command; once it is cancelled, a pipeline records `last_error` "interrupted by shutdown" under the change mutex it owns and starts nothing new.
2. Close the listener, close every MCP session through each harness server's `Sessions()` and `ServerSession.Close()` so standalone GET streams end, and call `http.Server.Shutdown` with a 5-second deadline followed by `http.Server.Close()`. Stop the refresh loop, watchers, sweep, and maintenance timer.
3. Wait up to 30 seconds for running submit pipelines and Git commands.
4. Kill the process group of every remaining child and wait up to 10 more seconds for the pipeline goroutines to record their own failures.
5. Release the lock and exit 0.

Shutdown itself writes no change record. Every child also gets `SysProcAttr.Pdeathsig = SIGKILL` on Linux, so children die with a crashed daemon; the next startup's reconciliation repairs the change.

### Snapshot extraction

1. Populate a new `os.MkdirTemp(rev, ".tmp-<revision>-")` directory. For `git` sources, list the tree with `git ls-tree -r -z <sha> -- <expanded roots and harness roots>` and write each blob from `git cat-file --batch`, so nothing outside the served roots (for example a repository-root `package.json` that declares bundles as workspace members) reaches the snapshot; entries with mode `120000` (symlink) or `160000` (submodule) are not written and are recorded. For `path` sources, walk the expanded roots with `Lstat` and copy regular files into `rev/.tmp-path-<random>/`; symbolic links are not followed or copied and are recorded. The path source revision is computed from the copied tree after step 3's ignore evaluation, and the remaining steps use it.
2. Find bundles: every directory directly under an expanded root or harness root (harness roots expanded for all three `HarnessId` values) that contains `SKILL.md` or a recorded entry named `SKILL.md`, and every recorded symlink or submodule entry directly under such a root, which becomes a bundle with a `symlink` or `submodule` diagnostic. A recorded symlink inside a bundle produces a `symlink` diagnostic, and a recorded submodule a `submodule` diagnostic, for that bundle.
3. When `ignore_file` is set, evaluate it per bundle (`internal/source/ignore.go`): each ignore file at or below the bundle directory applies gitignore patterns to paths below its own directory, and each of its lines starting with `./` also excludes that path prefix relative to its own directory. Ignore files above the bundle directory are not read. Every `node_modules` path and every ignore file is excluded. Paths listed in a `prepare` rule's `keep`, relative to a directory that contains its `when_file`, are exempt. Excluded files are deleted from the temporary directory. When the rules match the bundle's `SKILL.md`, it is kept and the bundle gets a `missing_skill_md` diagnostic.
4. Render every remaining regular file inside each bundle, except binary files (NUL byte in the first 8,000 bytes), with the source's template context (see [Template](#template)). `file_dir`, `file_path`, and `{snapshot}` render to the final `rev/<revision>/` paths, never the temporary directory. A bundle with any extraction diagnostic keeps its raw files.
5. Prepare: before any rule runs, compute each rule's matching directories once from the filtered source tree: every directory inside a bundle without diagnostics that contains `when_file`. Directories created by `prepare` are never matched. Then, for each rule in configuration order, run `run` in each of its matching directories with the daemon's environment, a timeout of the smaller of the rule's `timeout` and the caller's remaining deadline, the shutdown context checked before each start, and the process handling in [Git execution](#git-execution). When the shutdown context or the caller's deadline ends a run, the extraction is aborted: the temporary directory is deleted, no `RenderRecord` is written, and the cancellation error is returned. Afterwards, walking the directory with `Lstat`: a symbolic link whose resolved target leaves that directory, or a non-zero exit or timeout, gives the bundle a `prepare_failed` diagnostic carrying the last line of stderr, and the `RenderRecord` is marked `prepare_retry`; every regular file with a link count above 1 is replaced by a private copy, so no inode is shared with a package cache.
6. Set permissions per section 5, walking with `Lstat`; symbolic links are left unchanged.
7. Write `rev/<revision>.render.json` atomically.
8. Rename the temporary directory to `rev/<revision>/`. When the target exists, delete the temporary directory instead.
9. When the extraction was not requested by `Refresh`: under the index mutex, when `<revision>` is neither `current` nor in `superseded`, add it to `superseded` with the extraction time (a submit base or a rebase tip) and persist the index. `Refresh` adds a new `current` itself.

### Refresh

`Refresh(source)` is the only routine that moves a source's `current`. It runs on every `fetch_interval` tick, at startup, from the publish path, and after a successful push, always under the source's refresh mutex. It returns every error it meets, joined: the fetch or clone error after completing the remaining steps, and any extraction, index persistence, or view rebuild error, which stops the run at that step.

1. Under the source mutex: when `repo.git` is absent, clone `--bare` into `repo.git.tmp-<random>` and rename it to `repo.git`; otherwise, when `git remote get-url origin` differs from the configured `url`, run `git remote set-url origin <url>`, then run `git fetch origin +refs/heads/<branch>:refs/heads/<branch>`. When `repo.git` exists afterwards, read `refs/heads/<branch>`, whether or not the fetch succeeded. Release the source mutex. Without `repo.git`, stop and return the error.
2. When that commit differs from `SnapshotIndex.current`, the snapshot of `current` is missing, or `current`'s `RenderRecord` has `prepare_retry` and its retry backoff (starting at `fetch_interval`, doubling per failed retry up to one hour) has elapsed: extract its snapshot; a retry of an existing revision extracts into a temporary directory and replaces `rev/<revision>/` by renaming the old directory aside, renaming the new one into place, and deleting the old one; then, under the index mutex, record the previous `current` in `superseded` with the current time when its snapshot directory exists, set `current`, and persist the index.
3. Unless view rebuilds are suppressed (startup), read every source's `current` under its index mutex, then take the view mutex. When the revision of this source that the current views were built from differs from the `current` just read, rebuild all harness views from those `current` values, diff each view against its previous view, apply the diff, and log diagnostics. Release the view mutex. A Refresh that returns has therefore made the source's `current` live.
4. Diff semantics: a tool or prompt is **changed** when its served name, description, or invocation changed. Body-only and bundled-file edits change no definitions and send no `list_changed`, so client prompt caches stay valid. Diagnostics changes send no `list_changed`. The diff is applied with `AddTool`, `RemoveTools`, `AddPrompt`, and `RemovePrompts`; go-sdk sends `list_changed`.
5. Each diagnostic is logged at `warn` once per (source, revision, bundle, file, line, code).
6. Retention, on every run whether or not `current` changed, under the index mutex: delete snapshots whose `superseded` time is older than `snapshot_retention`, except `current` and any revision in the open-change `base_revision` index, then remove them and their `RenderRecord` from the index and, under the catalog mutex, drop their cached catalogs.

`path` sources run steps 2–6 on a debounced fsnotify event and on every tick, with the path source revision in place of a sha. The watcher adds every directory under the expanded roots at start and each directory reported by a `Create` event.

The source manager runs `git maintenance run --auto` for each `git` source hourly, under the source mutex.

### Skill tool call

1. Argument rules: arguments are validated against the tool's input schema by go-sdk (tools are registered with the generic `mcp.AddTool[In, Out]` and an explicit `InputSchema`), so unknown properties and wrong types are rejected before the handler runs. `offset` is a non-negative integer. `offset` and `revision` are given together or not at all. `offset` must not exceed the content length and must fall on a UTF-8 rune boundary. Violations return a tool error naming the rule.
2. Select the revision: the `revision` argument when present, otherwise the revision of that source the endpoint's current view was built from. A `revision` that is not retained returns a tool error telling the agent to reload the skill without `offset` and `revision`.
3. Resolve the skill in that revision's catalog for the endpoint's harness. Catalogs for retained snapshots are built on first use and cached.
4. Without `path`, the content is the rendered body. With `path`, confine the path to `base_dir` with `os.Root`; the content is the file's bytes. A file with a NUL byte in its first 8,000 bytes is binary: the result carries metadata and `abs_path` only.
5. Chunking: the opening and closing wrapper elements appear on every chunk. The `<bundled_files>` block and the `<catalog_diagnostics>` element (when the endpoint's current view has diagnostics) appear only on a load result whose `offset` is absent or 0. The continuation element appears when content remains. All of these count toward `max_result_bytes`. The content slice starts at `offset` and ends at the last newline that fits the remaining budget. When no newline fits, the cut falls on the last UTF-8 rune boundary that fits. When the fixed elements leave room for less than one rune of content, the call returns a tool error naming the overhead and the budget.

The same chunking applies to `skill_change_read` and `skill_change_diff` results, with `sha` (first 12 hex characters of SHA-256 of the full content) in place of `revision`: a continuation call whose `sha` no longer matches the content returns a tool error telling the agent to read again from offset 0.

### Change lifecycle

```ts
type ChangeTransition =
  | { from: 'open'; to: 'open'; on: 'skill_create' | 'skill_edit' | 'skill_write_file' | 'submit_failed' | 'reconciled' }
  | { from: 'open'; to: 'submitted'; on: 'push_succeeded' | 'found_published' }
  | { from: 'open'; to: 'discarded'; on: 'skill_change_discard' | 'ttl_expired' | 'worktree_missing' | 'worktree_add_failed' };
```

- `skill_change_open` fails with "source <name> has no snapshot" when the source has no `repo.git` or no `current`. Otherwise it adds a worktree on a new change branch from the source's current tip and records the endpoint's harness. The record is written before the worktree is added. When `git worktree add` fails or times out, the change is set to `discarded` with `last_error` carrying Git's stderr, lifecycle cleanup runs, and the tool returns the error. A `change_id` may be used from any harness endpoint; the recorded harness governs `{harness}` expansion.
- Every mutation first reconciles the change (see [Change reconciliation](#change-reconciliation)). A discard, including a TTL discard, runs only reconciliation steps 1 and 4 (missing worktree, published detection) and then the lifecycle cleanup even when other repairs would fail. A mutation then validates its input and resulting content, adds its bundle to `touched_bundles`, and persists the record before writing any file.
- Every mutation writes through an `os.Root` opened at the worktree root, using the repo-relative path, to a temporary file in the target directory that is renamed over the target, and then runs `git add -- <path>` for the written path. When `git add` fails, the write is undone with `git restore --worktree -- <path>` for a path with an index entry, or removal of a new file, and the tool returns the error. A temporary file is created with the target's existing permission bits, or `0644` for a new file, so the Git mode is preserved. A mutation targeting a path that `git check-ignore` reports as ignored, or a file named `.gitattributes`, is rejected.
- A mutation on a change whose `commit` is set first runs `git reset --soft <base_revision>` and clears `commit` and `message`; the staged content is kept.
- Mutations update `updated_at`.
- On `submitted` or `discarded`, the record is persisted first, then `git worktree remove -f -f` (which also removes a locked worktree), `git worktree prune`, and `git branch -D <branch>` run. Each cleanup step tolerates an already-removed target. A cleanup failure is logged and retried by the hourly sweep.
- The hourly sweep discards open changes whose `updated_at` is older than `change_ttl`, deletes terminal records older than `change_ttl`, and retries pending cleanup.

### Change reconciliation

Runs for an open change at startup (step 9) and at the start of every submit, mutation, and discard, under the change mutex.

1. When the worktree is missing, not listed by `git worktree list --porcelain`, or listed as locked with reason `initializing`, discard the change with `last_error` naming the worktree state.
2. When a rebase is in progress in the worktree, run `git rebase --abort`.
3. When `rebase_onto` is set: if `HEAD^` equals `rebase_onto`, set `base_revision` to `rebase_onto` and `commit` to `HEAD`; if `HEAD` equals `rebase_onto`, set `base_revision` to `rebase_onto` and clear `commit` and `message`. Then clear `rebase_onto` and persist the record.
4. When `push_started` is set, call `Refresh(source)` first; when it fails, stop reconciliation and fail the calling operation with "publication state unknown; retry when the remote is reachable". Clear `push_started`. Then, when `commit` is set and `git merge-base --is-ancestor <commit> refs/heads/<branch>` succeeds, the change is published: record `publication` with `attempts: 0`, set `state` to `submitted`, and run the lifecycle cleanup.
5. When `commit` is set and `HEAD` differs from it, or `HEAD^` differs from `base_revision`: run `git reset --soft <base_revision>` and clear `commit` and `message`.
6. When `commit` is null and `HEAD` differs from `base_revision`: run `git reset --soft <base_revision>`.
7. Set `touched_bundles` to the union of its recorded value and the bundle directories of `git diff --cached --name-only -z <base_revision>`.
8. Discard unstaged work in touched bundles: run `git restore --worktree -- <bundles>` for the touched bundles that have index entries (`git ls-files -z -- <bundle>` is non-empty), skipped when none do, then `git clean -fdx -- <touched bundles>`. The index is the record of applied mutations.

Every repair is logged at `info` with the change ID.

### Submit pipeline

The tool handler acquires the change mutex and starts the pipeline in a goroutine on `context.WithoutCancel(requestCtx)` with a `submit_timeout` deadline. The goroutine owns the change mutex from then on and releases it when the pipeline finishes; the handler never unlocks it. The handler waits for the pipeline or for the request to end. When the request is cancelled or the client times out, the pipeline keeps running and records its outcome in the change record. A later submit call waits for the change mutex: when the change is then `submitted` it returns the recorded result, and otherwise it runs the pipeline again from step 1. When the request carries a `progressToken`, the handler sends `notifications/progress` at the start of each step below and every 10 seconds while a step runs.

1. Reconcile the change. When it is `submitted`, return its recorded `ChangeSubmitResult` without side effects. When it is `discarded`, fail with "change is discarded".
2. When `commit` is null: reject the submit when `touched_bundles` is empty or `git diff --cached --quiet <base_revision>` reports no staged change; otherwise commit the staged content with the user's Git identity, the agent-supplied message, and a trailer `Skills-MCP-Change: <change_id>`, and record `commit` and `message`. When `commit` is set, the `message` argument is ignored and the result reports the recorded message.
3. Require `git rev-parse HEAD` to equal `commit`, `git rev-parse HEAD^` to equal `base_revision`, and `git status --porcelain -z --ignored -uall` to be empty; otherwise fail with the mismatch or the unexpected paths.
4. Verify the commit (`internal/authoring/verify.go`):
   1. Materialize `commit` into `rev/.tmp-verify-<random>/` with snapshot extraction steps 1–6, rendered in that directory. `prepare` runs only in touched bundles, bounded by the pipeline's remaining deadline, so a `prepare` failure caused by the change fails verification before the push; untouched bundles skip `prepare` and take their `prepare_failed` diagnostics from the base revision's `RenderRecord`.
   2. Build the three harness views from it without the per-revision catalog cache. Fail on any diagnostic that is not also present, with the same code, bundle, and file, in the views of `base_revision`. Fail when a touched bundle that is served in a base view has a different served name or invocation in that view (bundles refused or shadowed in the base views are exempt, so authoring can repair them), or when a skill served from another source in the base views is no longer served.
   3. Changed paths are `git diff-tree -r -z -M --name-status --no-ext-diff --no-textconv <base_revision> <commit>`. Fail on any `D` status, on any `R` status whose old path is outside `touched_bundles`, and when any changed path is outside `touched_bundles` or is a protected path: an argv element of the source's `validate` that names an existing file in `base_revision`, any path in that file's directory, or any `ignore_file` in that directory or its ancestors up to the bundle directory.
   4. Scan the added lines of `git diff-tree -r -p -a -U0 -M --no-ext-diff --no-textconv --no-color <base_revision> <commit>`, the added and renamed paths, and the commit message from `git log -1 --format=%B <commit>` against `deny_patterns`.
   5. Run `validate` once per touched bundle with `rev/<base_revision>/` as the working directory (extracted on demand), a timeout of the smaller of 120 seconds and the remaining deadline, and `{bundle_dir}` replaced by the bundle's path inside the verification directory. Nonzero exit fails with the combined output.
   6. Delete the verification directory after adding owner write permission.
   7. On any verification failure, run `git reset --soft <base_revision>`, clear `commit` and `message`, and fail.
5. Publish, with at most three pushes per submit call:
   1. Set `push_started` and persist the record. Run `git -C repo.git push --porcelain origin <commit>:refs/heads/<branch>`, then clear `push_started` once a ref status line has been parsed and its outcome below recorded. When no ref status line is parsed (timeout or transport failure), run 5.2's `Refresh` and `is-ancestor` check: when the commit is on the branch, clear `push_started` and go to 5.4; otherwise leave `push_started` set and fail the submit with Git's stderr. A `*`, ` `, `+`, or `=` flag for the ref is success: go to 5.4. A `!` flag with `(non-fast-forward)` or `(fetch first)` is a rejection: go to 5.2. Anything else fails the submit with Git's stderr.
   2. Call `Refresh(source)`. When it returns an error, fail the submit with that error. When `git merge-base --is-ancestor <commit> refs/heads/<branch>` succeeds, the change is already published: go to 5.4.
   3. Set `rebase_onto` to the commit at `refs/heads/<branch>` and persist the record. Run `git -C <worktree> rebase --onto <rebase_onto> <base_revision>`, which replays only the change commit even when the upstream branch was force-pushed. On a nonzero exit, capture conflicting paths from `git diff --name-only -z --diff-filter=U` and stderr, run `git rebase --abort` when a rebase is in progress, clear `rebase_onto`, and fail the submit with `last_error` naming the new tip, the paths, and stderr; the agent discards the change and opens a new one. When the rebase exits 0 with `HEAD` equal to the new tip, the change already exists upstream: set `base_revision` to the new tip, clear `commit`, `message`, and `rebase_onto`, persist the record, and fail with `last_error` "change already applied upstream at <new tip>; discard it". Otherwise set `base_revision` to the new tip and `commit` to the rebased `HEAD`, clear `rebase_onto`, persist the record, repeat steps 3–4, and return to 5.1. When the third push of this call is rejected, the submit fails.
   4. Record `publication` with the pushes made in this call, set `state` to `submitted`, persist the record, and run the lifecycle cleanup. Then call `Refresh(source)` with its own 180-second deadline; when it fails, record its error in `publication.refresh_error`. Return success.
6. Any failure keeps the change `open`, records `last_error`, and returns it to the caller. A retry resumes at step 1.

## 9. Validation rules

### Structural (on every mutation, on submit verification, and on every view build)

- `SKILL.md` starts with a `---` line, contains a closing `---` line, and the block between them decodes as a YAML mapping.
- `name` is 1–64 characters, matches `^[a-z0-9]+(-[a-z0-9]+)*$`, and equals the bundle directory name. With `author_prefix_except` set, a present `author` is a string whose lowercase form matches the same pattern (`invalid_author`), and the served name satisfies the length and pattern rule (`invalid_name`).
- The served name with `-` replaced by `_` is none of the authoring tool names (`reserved_name`).
- `description` is a non-empty string of at most 1,024 characters.
- The bundle's extraction produced no `symlink`, `submodule`, or `missing_skill_md` diagnostic. View builds read these from the `RenderRecord` and never walk the snapshot for links, so `prepare` output links are not re-judged.

### Authoring-only (on every mutation)

- A bundle directory is exactly `<expanded root path>/<name>` for one of the source's `roots` or `harness_roots`, expanded with the change's harness. Served bundles are found by the same rule, so it holds for every served bundle by construction. The `validate` CLI skips this rule.
- Bundle-relative paths are relative, contain no `..` element, contain no `.git` element, do not name a `.gitattributes` file, and are joined to the bundle directory and opened through an `os.Root` at the worktree root, so no symlink in any path component can leave the worktree.
- Written content is valid UTF-8 without NUL bytes and is at most 1 MiB.
- A mutation that leaves `SKILL.md` structurally invalid is rejected and leaves the file unchanged.

### Template

Applied to every non-binary, non-ignored file in a bundle at snapshot extraction, at submit verification, and by the `validate` CLI.

- Every occurrence of `<%` must start one of these tags, with any run of spaces and tabs where one space is shown:
  - `<%- name %>`, where `name` matches `^[a-z][a-z0-9_]*$`. It renders to `template.vars[name]` after `~/` and `{snapshot}` expansion, or to the built-in `file_dir` (absolute snapshot directory of the file being rendered) or `file_path` (absolute snapshot path of that file). Other names produce `unknown_template_variable`.
  - `<%- env("NAME") %>` or `<%- env("NAME", "fallback") %>`, where `NAME` matches `^[A-Z_][A-Z0-9_]*$` and `fallback` is a double-quoted string without `"` or `\`. It renders to the daemon's value of `NAME`, else `fallback`. A `NAME` outside `template.env` produces `env_not_allowed`; an unset `NAME` without fallback produces `env_unset`.
- Any other `<%` produces `unsupported_template_tag`, including `<%=`, `<%_`, `<%#`, `include(...)`, and a literal `<%` in prose.
- Rendering is a single pass: rendered values are not scanned again.
- Each diagnostic carries the file and the 1-based line of the `<%`.

### Catalog (on every view build, at submit verification, and in `list`)

- A bundle with any structural or extraction diagnostic is excluded from every view and listed in the diagnostics of each view that would have included it.
- Served names are unique across a source's `roots`. When two bundles of one source share a served name within one harness view, both are excluded with `duplicate_name`. A `harness_roots` skill with the same served name as a `roots` skill replaces it in that harness's view and is not a duplicate.
- Across sources, the first source in `sources` order wins, including when its bundle is refused. A bundle's precedence key is its served name; for a refused bundle whose served name cannot be computed, the key is the bundle directory name, prefixed with `lowercase(author) + "-"` when `author` parses and prefixing applies. A refused bundle still shadows the same key in later sources, and a refused `harness_roots` bundle still replaces the `roots` bundle with the same key in that harness's view. Each shadowed skill is logged at `warn` and is not a diagnostic.

### Request

Checks run in this order; the first failure determines the response.

1. A path other than `/healthz` and `/mcp/<harness>` for a known harness returns `404`.
2. go-sdk `auth.RequireBearerToken` returns `401` for a missing or wrong `Authorization: Bearer <token>`, for every method including `GET`. The verifier compares with `crypto/subtle.ConstantTimeCompare`.
3. `http.CrossOriginProtection`, with no trusted origins, returns `403` for a cross-origin `POST` or `DELETE`.
4. go-sdk localhost protection returns `403` for a non-localhost `Host`.
5. go-sdk answers a 2026-07-28 call with JSON-RPC error `-32022`.

### Authoring

- Authoring tools reject a `change_id` that is unknown or not `open`, except that `skill_change_submit` returns the recorded result for a `submitted` change.
- `skill_change_open` rejects a source that is missing, is a `path` source, or has `writable: false` (Q8A1).
- `skill_edit`, `skill_write_file`, and `skill_change_read` reject a `bundle` that fails the bundle-directory rule or has no `SKILL.md` in the worktree.

## 10. API surface

### HTTP

| Method and path | Behavior |
|---|---|
| `GET`, `POST`, `DELETE` `/mcp/claude-code`, `/mcp/codex`, `/mcp/pi` | `auth.RequireBearerToken`, then `http.CrossOriginProtection.Handler` with no trusted origins, then go-sdk `StreamableHTTPHandler` for that harness's `mcp.Server`, with `Stateless: false` and `SessionTimeout: 0`. |
| `GET /healthz` | `200` with body `ok`; no auth. |

### Server instructions (identical for all harnesses, under 512 characters; the `skill_` sentence is omitted when `default_write_source` is null)

```text
Each tool named after a skill returns task instructions. Before starting work that matches a skill tool's description, call that tool and follow the instructions it returns. Read bundled files by calling the tool again with `path`, or read the absolute paths it returns. Tools prefixed `skill_` create and edit skills and push them to the skill repository. Report any catalog_diagnostics block to the user.
```

### Skill tool (one per model skill)

```ts
interface SkillToolDefinition {
  name: string;                                   // served name
  description: string;                            // skill description, verbatim
  inputSchema: {
    type: 'object';
    properties: {
      path: { type: 'string'; description: 'Bundled file relative to the skill root. Omit to load the skill.' };
      offset: { type: 'integer'; minimum: 0; description: 'Byte offset from a continuation element. Pass it together with revision.' };
      revision: { type: 'string'; description: 'Revision from a continuation element. Pass it together with offset.' };
    };
    additionalProperties: false;
  };
  annotations: { readOnlyHint: true; idempotentHint: true; openWorldHint: false };
  _meta: { 'anthropic/alwaysLoad': true };
}

interface SkillToolArgs {
  path?: string;
  offset?: number;
  revision?: string;
}
```

`SkillLoadResult`, returned as a single text content block:

```text
<skill name="golang" source="ai-registry" bundle="skills/agent/golang" revision="3f2a91c..." base_dir="/home/<user>/.cache/skills-mcp/sources/ai-registry/rev/3f2a91c.../skills/agent/golang">
...rendered SKILL.md body...
</skill>
<bundled_files>
references/setup.md
references/parameter-design.md
</bundled_files>
```

`SkillFileResult`:

```text
<skill_file skill="golang" path="references/setup.md" revision="3f2a91c..." abs_path="/home/<user>/.cache/skills-mcp/sources/ai-registry/rev/3f2a91c.../skills/agent/golang/references/setup.md">
...file contents...
</skill_file>
```

Continuation element appended to a truncated result. `next_offset` and `total_bytes` count content bytes only: the rendered body for a load, the file bytes for a file.

```text
<continuation revision="3f2a91c..." next_offset="17950" total_bytes="29840">Call this tool again with the same arguments plus revision "3f2a91c..." and offset 17950 to read the rest.</continuation>
```

Diagnostics element, appended after `<bundled_files>` on a load whose `offset` is absent or 0, when the endpoint's current view has diagnostics. One line per diagnostic in `<bundle directory name> <source>:<bundle>/<file>:<line> <message>` form (`:<line>` omitted when `line` is null). The element is at most 2,048 bytes; lines that do not fit are replaced by one `and <n> more` line.

```text
<catalog_diagnostics count="1">
disk-cleanup ai-registry:skills/user/disk-cleanup/SKILL.md:12 unsupported template tag "<%= file_dir %>"
These skills are not loaded. Tell the user; `skills-mcp list` shows details.
</catalog_diagnostics>
```

### Prompt (one per user skill)

```ts
interface SkillPromptDefinition {
  name: string;                                   // served name
  description: string;                            // skill description, verbatim
  arguments: [{ name: 'args'; description: 'Additional context for the skill.'; required: false }];
}
```

`prompts/get` returns one `user` message whose text is `SkillLoadResult`, followed by the `<catalog_diagnostics>` element when the view has diagnostics, then a blank line and `args` when present. Prompts are not chunked.

User skill delivery per harness:

| Harness | Delivery |
|---|---|
| Claude Code | MCP prompt, invoked as `/skills:<name>`. Claude Code splits arguments on whitespace, so multi-word `args` must be quoted (V9). |
| pi | `/library:<name>` through `pi-skill-library` (Q5A1). |
| Codex | MCP prompt when V4 confirms Codex surfaces prompts; otherwise **Blocked on Q10**. |

### Authoring tools

Registered only when `default_write_source` is set. Authoring tools do not set per-tool `anthropic/alwaysLoad`; under the Claude Code configuration's server-level `alwaysLoad` they load upfront with the skill tools. `skill_change_read` and `skill_change_diff` return chunked text (format below); every other result is a single text content block containing the JSON-encoded response.

| Tool | Annotations |
|---|---|
| `skill_change_read`, `skill_change_diff` | `readOnlyHint: true`, `openWorldHint: false` |
| `skill_change_open`, `skill_create` | `readOnlyHint: false`, `destructiveHint: false`, `openWorldHint: false` |
| `skill_edit`, `skill_write_file`, `skill_change_discard` | `readOnlyHint: false`, `destructiveHint: true`, `openWorldHint: false` |
| `skill_change_submit` | `readOnlyHint: false`, `destructiveHint: true`, `idempotentHint: true`, `openWorldHint: true` |

```ts
// skill_change_open
interface ChangeOpenArgs { source?: string; summary: string }   // source defaults to default_write_source
interface ChangeOpenResult { change_id: string; source: string; base_revision: string }

// skill_change_read: raw source text from the change worktree, before template rendering; result is ChangeReadText
interface ChangeReadArgs {
  change_id: string;
  bundle: string;
  path?: string;                  // defaults to "SKILL.md"
  offset?: number;                // from a continuation element; given together with sha
  sha?: string;                   // from a continuation element; given together with offset
}

// skill_create
interface SkillCreateArgs {
  change_id: string;
  root: string;                   // a RootConfig.path of the change's source, with {harness} expanded to the change's harness
  name: string;
  description: string;
  frontmatter?: Array<{ key: string; value: unknown }>;  // extra keys written after name and description, in array order; keys unique, excluding name and description
  body: string;                   // SKILL.md content after the frontmatter
}
interface SkillCreateResult { bundle: string; served_name: string }

// skill_edit: matches against raw source text, as returned by skill_change_read
interface SkillEditArgs {
  change_id: string;
  bundle: string;                 // repo-relative bundle directory
  path?: string;                  // bundle-relative, default "SKILL.md"
  old_string: string;
  new_string: string;
  replace_all?: boolean;          // default false; without it old_string must occur exactly once
}
interface SkillEditResult { bundle: string; path: string; replacements: number }

// skill_write_file
interface SkillWriteFileArgs { change_id: string; bundle: string; path: string; content: string }
interface SkillWriteFileResult { bundle: string; path: string; bytes: number }

// skill_change_diff: git diff --cached -M --no-ext-diff --no-textconv <base_revision>, including new files; result is ChangeDiffText
interface ChangeDiffArgs { change_id: string; offset?: number; sha?: string }

// skill_change_submit
interface ChangeSubmitArgs { change_id: string; message: string }
interface ChangeSubmitResult { change_id: string; commit: string; message: string; publication: Publication }

// skill_change_discard
interface ChangeDiscardArgs { change_id: string }
interface ChangeDiscardResult { change_id: string; state: 'discarded' }
```

`ChangeReadText` and `ChangeDiffText`, chunked per [Skill tool call](#skill-tool-call) with `sha` in place of `revision`:

```text
<skill_source change_id="chg_..." bundle="skills/agent/golang" path="SKILL.md" sha="9b1c0d2e4f6a">
...raw file content...
</skill_source>
<continuation sha="9b1c0d2e4f6a" next_offset="17950" total_bytes="30285">Call this tool again with the same arguments plus sha "9b1c0d2e4f6a" and offset 17950 to read the rest.</continuation>
```

```text
<skill_change_diff change_id="chg_..." sha="4e5f6a7b8c9d">
...diff...
</skill_change_diff>
```

`skill_create` writes `SKILL.md` with frontmatter keys `name` and `description` followed by `frontmatter` entries in array order, then `body`. It fails when the bundle directory exists.

`skill_change_submit` returns after the push succeeds and the source refresh has been attempted; `publication.refresh_error` reports a failed refresh. A failure returns a tool error carrying `last_error`. Calling it again on a submitted change returns the same result. The worst-case duration is `submit_timeout` plus the post-push refresh (at most 180 seconds).

### Client configuration

Export the token in the shell profile:

```bash
export SKILLS_MCP_TOKEN="$(skills-mcp token)"
```

Every client's tool-call timeout must exceed the worst-case submit duration with the default `submit_timeout` (600 seconds plus up to 180 seconds of post-push refresh), so each is set to 900 seconds. Claude Code's per-server `timeout` is a hard limit in milliseconds that also raises its first-byte timer and idle floor to 900 seconds; submit's progress notifications reset pi's timeout and Claude Code's idle window.

Claude Code (`.mcp.json` or user config). Server-level `alwaysLoad` makes startup wait (up to 5 seconds) for the daemon, so the first turn, including `claude -p`, has the skill tools; per-tool `_meta` alone does not delay startup.

```json
{ "mcpServers": { "skills": { "type": "http", "url": "http://127.0.0.1:7341/mcp/claude-code", "alwaysLoad": true, "timeout": 900000, "headers": { "Authorization": "Bearer ${SKILLS_MCP_TOKEN}" } } } }
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.skills]
url = "http://127.0.0.1:7341/mcp/codex"
bearer_token_env_var = "SKILLS_MCP_TOKEN"
tool_timeout_sec = 900
```

pi (`~/.pi/agent/mcp.json`, `timeout` in seconds, and `~/.pi/agent/settings.json`):

```json
{ "mcpServers": { "skills": { "url": "http://127.0.0.1:7341/mcp/pi", "exposure": "direct", "timeout": 900, "headers": { "Authorization": "Bearer ${SKILLS_MCP_TOKEN}" } } } }
```

```json
{ "@alexgorbatchev/pi-skills-library": { "server": { "url": "http://127.0.0.1:7341/mcp/pi", "token_env": "SKILLS_MCP_TOKEN" } } }
```

systemd: `systemctl --user import-environment SSH_AUTH_SOCK` before starting the unit, or `SSH_AUTH_SOCK` and `PATH` in `$XDG_CONFIG_HOME/skills-mcp/env`.

### Native skill loading

- After the ai-registry change in [Changes in other repositories](#changes-in-other-repositories), the claude-code, codex, and pi homes contain no registry model skills; the daemon serves them. Codex keeps registry user skills until V4 passes or Q10 is answered.
- Project-level skill discovery stays enabled in every harness. No flag that disables all discovered skills, such as pi `--no-skills`, is used.
- The daemon has no project context, so it cannot detect a project skill with the same name as a served skill. Both appear to the model: the native skill and the MCP tool. The README documents this overlap.

## 11. Implementation order

1. Scaffold, `internal/paths`, `internal/config`, Cobra root, `token`.
2. `internal/catalog` parse, template, validate, build (with author prefixing), and diff against fixture bundles; `list` and `validate` commands.
3. `internal/gitx` and `internal/source` with `file://` bare fixtures: clone into a temporary directory, refspec fetch, tree materialization, ignore evaluation, symlink handling, rendered snapshots, `RenderRecord`, `RenderContextHash` reset, singleflight, snapshot index, retention, offline startup, path source snapshots and recursive watching.
4. `internal/render` with chunking, argument rules, and the diagnostics element.
5. `internal/mcpserver`: HTTP options, request check order, `auth.RequireBearerToken`, cross-origin wrapper, per-harness servers, skill tools, prompts, server instructions, startup lock.
6. Refresh loop, `Refresh`, diff, and `list_changed` delivery.
7. `internal/authoring`: change handles, staged mutations, detached submit pipeline with progress, commit verification, publication with already-published detection, rejection, rebase, and re-verification; reconciliation, startup recovery, and sweep.
8. Client verification: run each check in [Unverified client behavior](#unverified-client-behavior) against the installed harness versions and record results in this document.
9. `AGENTS.md`, `README.md`, systemd unit.
10. `pi-skill-library` daemon source, in that repository.
11. ai-registry harness build change, in that repository, after steps 1–10 pass with the daemon running against ai-registry. Each harness's registry skills are removed from its home only when the checks it depends on pass: Claude Code V6, V11, V12; Codex V1, V2, V6, V11 (user skills additionally V4); pi V6, V8, V11. A negative result keeps that harness's native registry skills and opens a new decision in section 15.

### Unverified client behavior

| ID | Check |
|---|---|
| V1 | Codex 0.164.0 shows all MCP tool descriptions to the model without deferral. |
| V2 | Codex 0.164.0 refreshes tools after `list_changed`. |
| V3 | Codex 0.164.0 default MCP output truncation budget; adjust `harnesses.codex.max_result_bytes` to fit it. |
| V4 | Codex 0.164.0 handling of MCP prompts. A negative result opens Q10. |
| V5 | pi 1.3.0 handling of MCP prompts through the built-in client. |
| V6 | MCP protocol version negotiated by Claude Code, Codex 0.164.0, and pi 1.3.0 against the daemon, including fallback after a `-32022` response to 2026-07-28, and that each opens the GET stream and receives `list_changed`. A harness that does not fall back reopens the side-by-side handler alternative. |
| V7 | Whether each harness sends `DELETE` when a session ends, and daemon session count and RSS after 100 agent sessions start and exit. |
| V8 | `pi-skill-library` re-registration on `notifications/prompts/list_changed` under pi 1.3.0. |
| V9 | How Claude Code passes a multi-word prompt argument: quoted text in one `args` value, or only the first word. |
| V10 | A 780-second submit sending progress notifications completes in Claude Code (`timeout: 900000`), Codex 0.164.0 (`tool_timeout_sec = 900`), and pi 1.3.0 (`timeout: 900`). |
| V11 | After a daemon restart, Claude Code, Codex 0.164.0, pi 1.3.0, and `pi-skill-library` re-initialize on a `404` session-not-found response to a POST and to the GET stream, and receive later `list_changed`. |
| V12 | `claude -p` with the specified configuration has the skill tools on its first turn. |

## 12. Testing plan

- **config:** defaults, each configuration error including `submit_timeout` under 60 seconds, `~/` and `{snapshot}` expansion, `template.vars` merging over the default, authoring tools absent when `default_write_source` is null.
- **catalog:** frontmatter edge cases, invalid names, description length, name and directory mismatch, author prefixing with and without `author_prefix_except`, a missing author (unprefixed), a mixed-case author (`JuliusBrussee` → `juliusbrussee-caveman`), a name already carrying the prefix, an invalid author, prefixed-name length overflow, reserved names such as `skill-create`, precedence across sources, harness same-name replacement, duplicate served names excluding both bundles, deterministic ordering, per-revision catalog cache.
- **prepare:** a repository-root `package.json` declaring the bundle as a workspace member not reaching the snapshot, matching directories computed before rules run and never inside `node_modules`, executable bits of installed binaries kept, a `prepare_failed` snapshot retried with backoff and replaced after the environment is fixed, shutdown or deadline during `prepare` aborting the extraction without a `RenderRecord`, verification running `prepare` only in touched bundles, a rule running in a nested matching directory, `keep` exempting a lockfile from `ignore_file`, prepare output excluded from rendering and from `files`, non-zero exit and timeout producing `prepare_failed`, a link leaving its directory refused, hardlinked files replaced by private copies, symlinks inside the directory kept, `RenderContextHash` changing with the rules, a change that breaks `prepare` failing verification, the configured bun rule installing `disk-cleanup` dependencies so its own `bun install --frozen-lockfile` exits 0 on the read-only snapshot.
- **template:** each accepted tag form with varied spacing, `file_dir` and `file_path` per file pointing at the final snapshot path (no `.tmp-` in output), `{snapshot}` and `~/` in variables, `env` with and without fallback, unset and non-allowlisted env names, `<%=`, `<%_`, `<%#`, `include(...)`, and literal `<%` refused, single-pass rendering, binary files untouched, line numbers in diagnostics.
- **source:** clone, refspec fetch moving `refs/heads/<branch>`, fetch failure with and without a snapshot, offline startup after a `RenderContextHash` change re-extracting from `repo.git`, interrupted extraction cleanup, orphaned `RenderRecord` cleanup, concurrent extraction of one revision, snapshot permissions and removal of read-only trees, `.gitattributes` `export-ignore` and `export-subst` having no effect, symlink and submodule entries producing diagnostics and never being followed, `ignore_file` evaluation against copies of the real ai-registry `.registry-ignore` files (bundle-level patterns, nested ignore files, `./` prefix lines, ancestor ignore files not applied, `SKILL.md` never removed), `node_modules` exclusion, adding a root changing `RenderContextHash`, unindexed snapshot directories deleted at startup, clone into a temporary directory and leftover cleanup, startup offline with no `repo.git` and a later Refresh cloning, rendered files on disk, refused bundles left raw, `SnapshotIndex` persistence across restart, retention on a tick with no new commit, retention skipping `current` and open-change bases, on-demand snapshots entering retention, path source content-hash revision, recursive watch of a newly created directory.
- **render:** `skill_change_read` and `skill_change_diff` chunking with `sha` mismatch errors; budget boundaries including wrapper bytes, newline cut, single line longer than the budget, multi-byte UTF-8 at the cut, continuation offsets that reassemble the original content exactly, `offset` without `revision` and the reverse, out-of-range and mid-rune `offset`, retired revision error, overhead larger than the budget, `<bundled_files>` and diagnostics only on the first chunk, the diagnostics 2,048-byte cap with the `and <n> more` line.
- **mcpserver:** go-sdk client over `httptest`: `tools/list` contents and `_meta`, tool calls with and without `path`, path traversal rejection, binary files, prompts with and without diagnostics, request check order including unauthenticated unknown paths, bad `Host` without a token, cross-origin `POST`, and unauthenticated `GET`; a 2026-07-28 client receiving `-32022` and completing on a legacy version; `list_changed` on a description change over the GET stream delivered to a session idle for longer than any client timeout; none on a body-only or diagnostics-only change; a refused bundle disappearing from `tools/list` and appearing in diagnostics; `tools` and `prompts` capabilities advertised by a server that starts with no tools or prompts, followed by `list_changed` when they appear; a second `serve` failing on the lock; concurrent Refresh calls on one source serialized by the refresh mutex so `current` never moves backwards.
- **authoring:** each mutation with valid and invalid input, staging of new files visible in `skill_change_diff`, `old_string` zero and multiple matches, `bundle` outside configured roots, `os.Root` escape attempts through `..` and symlinks, ignored-path and `.gitattributes` rejection, `os.Root` at the worktree root refusing a symlinked ancestor directory, NUL and invalid UTF-8 rejection, mutation after commit resetting `commit`, submit of a new skill whose body contains a deny pattern, deny pattern in a renamed path and in the message, deny scan unaffected by `diff.external` and textconv configuration, duplicate served name introduced by a change, template failures, protected validator paths, `validate` failure and timeout, validator edits not affecting validation, dirty or ignored worktree files blocking submit, no hook running for any daemon Git command under a global `core.hooksPath`, empty-change rejection, submit on a submitted change returning the recorded result, TTL sweep, terminal record deletion, reconciliation after a crash between commit and record write, during a rebase, after a rebase before the record write (`rebase_onto` applied), after an upstream-applied rebase, between `git add` and the record write (`touched_bundles` recovered), a mutation after a `submit_timeout` expiry during rebase, a crash between file write and `git add` (worktree restored from the index), a failed `git add` undoing the write, a failed `worktree add` discarding the change, reconciliation with a touched bundle that has no index entries, a force-pushed upstream whose removed commit is not replayed by the rebase, executable bits preserved by edits, a push timeout after the remote accepted the ref, an offline startup with `push_started` set, a changed `url` applied to `origin`, stale `index.lock` files removed at startup, authoring that repairs a bundle refused in `base_revision`, a symlinked bundle directly under a root reported as a diagnostic, a symlinked path source root resolved once, adding a symlink to a path source changing its revision, a negative or non-integer `offset` rejected by schema validation, maintenance not detaching, a grandchild ignoring SIGTERM killed by the group SIGKILL, a glob character in an agent-supplied path treated literally by `add`, `restore`, and `check-ignore`, a watcher event during startup not rebuilding views before servers exist, a discard succeeding when other repairs fail, a non-ASCII bundle file name in `touched_bundles` recovery, shutdown cancelling a pipeline before its next Git command, a locked `initializing` worktree treated as missing, a push accepted remotely before a crash detected through `push_started`, verification rejecting deletions, served-name changes through `author` edits, and changes to a protected `ignore_file`, verification catalogs never entering the per-revision cache, quarantine directories surviving startup, and after a push before the record write (published detection), quarantine of an unparsable record, orphaned change directories and branches removed at startup, startup recovery of a change with a missing worktree, `skill_change_open` on a source with no snapshot, `worktree remove` on a dirty worktree, concurrent submits on one source.
- **publish:** against a `file://` bare remote: fast-forward push and synchronous refresh before submit returns; non-fast-forward rejection from a concurrent push, rebase, re-verification, and second push; rebase conflict reporting paths and leaving the change open; non-conflict rebase failure aborting the rebase; verification failure after rebase; third rejection failing; crash after push detected as already published on retry; rebase dropping a change that already exists upstream; `=` push result treated as success; non-rejection push failure; Git timeout; client cancellation and request timeout not interrupting the pipeline, with a later submit returning its outcome; progress notifications during a long `validate`; `submit_timeout` expiry; post-push Refresh failure reported in `refresh_error`; Refresh failure in 5.2 failing the submit without a rebase; a `validate` grandchild process killed with its process group on timeout; a refused bundle in the first source shadowing a later source by its precedence key; Refresh returning extraction errors into `refresh_error`; shutdown with a submit in flight; no force push in any path.
- **CLI:** `list` exit status and stderr diagnostics, `validate --source` template rules, `token` racing `serve` on first creation, malformed and over-permissive token files rejected.
- **Coverage:** at least 90% line coverage per package, enforced by `just coverage`.
- **pi-skill-library:** in its repository: command registration from a test MCP server, `input` handler resolving daemon names, local-name precedence, `prompts/get` with and without `args`, re-registration on `list_changed` over a single client, server error reporting, `terminateSession()` on shutdown and on client replacement.

## 13. Out of scope / rejection list

Reject implementations that:

- Add a stdio transport, non-loopback bind, unauthenticated MCP endpoint, stateless HTTP handler, or nonzero `SessionTimeout`.
- Use the deprecated `StreamableHTTPOptions.CrossOriginProtection` field.
- Put skill descriptions in server instructions or omit `anthropic/alwaysLoad` on skill tools.
- Select the harness from `clientInfo`.
- Reimplement Host, Origin, or bearer-token checks that go-sdk (`auth.RequireBearerToken`, localhost protection) and `http.CrossOriginProtection` provide.
- Serve files from `repo.git`, a change worktree, a `path` source directory, or any other mutable directory.
- Extract snapshots with `git archive`, follow or materialize symbolic links, or serve ignored files.
- Serve unrendered files from a bundle that rendered cleanly, or serve a bundle with diagnostics.
- Read environment variables outside `template.env` while rendering.
- Embed a general EJS engine in Go.
- Hard-code any package manager, lockfile name, or install command in the daemon.
- Serve `prepare` output whose files share inodes with a package cache, or whose links leave their directory.
- Validate the worktree instead of the commit, run any Git hook, or scan porcelain diff output.
- Run the submit pipeline or any Git command on an MCP request context.
- Run `validate` from the change worktree, or let a change modify a protected path.
- Write to `path` sources.
- Use go-git for any Git operation (its `plumbing/format/gitignore` package is the only permitted import), or accept unified diffs.
- Commit once per mutation instead of once per change.
- Force-push, open pull requests, or push to any ref other than `refs/heads/<branch>`.
- Push a commit that has not passed every step of submit verification.
- Store `repo.git` under `$XDG_CACHE_HOME`.
- Run Git without the prompt-suppressing environment or without a timeout.
- Write JSON state without temporary-file-and-rename atomicity.
- Accept a token file that is not exactly 64 hex characters with mode `0600`.
- Apply ignore files from above a bundle directory, or let ignore rules remove `SKILL.md`.
- Delete or rename skills.
- Disable project-level skill discovery in any harness.

## 14. Definition of done

- Q10 is answered or closed by a positive V4 result.
- `disk-cleanup` and `github-fix-issues` run their documented `bun install` commands and scripts from the read-only snapshot without modification.
- `just check` and `just test` pass; `just coverage` reports at least 90% per package.
- With the daemon running against ai-registry: Claude Code, Codex, and pi each list every model skill as a tool under its served name with its description visible to the model, load a skill, read a bundled file by `path` and by absolute path with rendered content, read a skill larger than their budget through continuations, and run a bundled script from `base_dir`.
- A skill with an unsupported template tag pushed to the tracked branch disappears from all harnesses, and the next skill load in each harness reports it in `<catalog_diagnostics>`.
- A description edit pushed to the tracked branch reaches a running Claude Code session without a restart.
- An agent opens a change, creates a skill, edits it, submits it, the commit appears on the remote tracked branch, and the new skill tool appears in a running Claude Code session without a restart.
- pi invokes a user skill through `/library:<name>`, and Claude Code through `/skills:<name>`.
- After the ai-registry change, every harness whose gating checks passed has no registry model skills in its home, every harness with a failed check keeps them under a recorded decision, and a project `.agents/skills` skill still loads in each harness.
- Checks V1–V12 are recorded.
- A review subagent reviews this document and the implementation against it, and every finding is resolved until a review comes back clean.

## 15. Decisions

| ID | Decision | Outcome |
|---|---|---|
| Q1 | How skill source templating is handled. | Q1A5: the server renders a fixed tag set against a per-source context and refuses any other tag; refusals are reported to the user. Specified in sections 4, 6, 8, 9, 10. |
| Q2 | How submitted changes land. | Q2A2: push to the tracked branch (`main` for ai-registry). Specified in sections 4, 6, 8. |
| Q3 | One loopback daemon over Streamable HTTP. | Q3A1: confirmed. |
| Q5 | How user skills reach pi and Codex. | Q5A1: `pi-skill-library` serves daemon prompts as `/library:<name>` in pi. Codex depends on V4 and Q10. |
| Q6 | Native skill loading. | Registry skills leave the claude-code, codex, and pi homes; project-level skill discovery stays. Specified in sections 7 and 10. |
| Q7 | Change handles for authoring. | Q7A1: confirmed. |
| Q8 | `path` sources read-only. | Q8A1: confirmed. |
| Q9 | Harness selection by URL path. | Q9A1: confirmed. |
| Q10 | **Open, contingent on V4.** How Codex users invoke user skills when Codex does not surface MCP prompts. | Blocks Codex user-skill delivery and removal of user skills from the Codex home. |
| Q11 | How skills that install dependencies into their own directory work from read-only snapshots. | Q11A2: per-source `prepare` rules in configuration install dependencies into each snapshot at extraction and verification; the daemon has no package-manager knowledge. Specified in sections 1, 3, 6, 7, 8. |

## Appendix A: Decision record

### A.1 Original request

> this is a new project, we will make it in golang... it will be an mcp server, the idea here is that it will completely replace harness skills with mcp tools... tools will serve skill files that agent request, skill descriptions become tool descriptions... i should be able to point the server at a repo and also allow agents to patch and make new skills that will go back, lets design this

### A.2 Round 1: initial proposal and answers

Each entry quotes the proposal, then the answer as given.

**Item 1: Tool per skill**

> Tool per skill: the tool name is the skill name and the description is copied as-is. Every skill tool sets `anthropic/alwaysLoad`. Without it, Claude Code never shows the model the descriptions, nothing fires on its own, and the whole idea fails without any error. The context cost matches what harness skills cost today.

Answer:

> yes

Outcome: accepted.

**Item 2: Input schema**

> Input schema: one optional `path` argument. With no argument, the tool returns the `SKILL.md` body without frontmatter, the absolute bundle `base_dir`, and a list of bundled files. With `path`, it returns that one file. That way you don't need a separate generic "read file" tool.

Answer:

> show me what you have in mind

Outcome: example provided in round 2; now specified in [10. API surface](#10-api-surface) with added `offset` and `revision` arguments.

**Item 3: Absolute paths**

> Absolute paths: the bundle is copied to a local cache and the server returns absolute paths, so the agent's own Read tool and `bun scripts/...` keep working. This is why **stdio first**: scripts need a local filesystem.

Answer:

> so i think the skills are sourced from github repo remotely and stored locally, github is basically external storage

Outcome: accepted as GitHub-as-storage with a local cache; stdio replaced by the local daemon (Q3).

**Item 4: Server instructions**

> Server instructions: one short rule, roughly "these tools return instructions; call the matching tool before starting that kind of task and follow what it returns." All 49 descriptions won't fit in the 4,096-character cap, so they don't go there.

Answer:

> i dont understand, explain... arent all tools from a single mcp server exposed all the time?

Outcome: explained in round 2. The "49" count was wrong; the verified count is 48 (see A.4).

**Item 5: User skills as prompts**

> User skills become MCP prompts, not tools. That's the protocol's own primitive for "the user invokes this," so you don't need to inject `disable-model-invocation`.

Answer: none recorded.

Outcome: kept; non-Claude-Code delivery became Q5.

**Item 6: Harness overrides**

> Harness overrides: the server reads the harness from `clientInfo.name` at initialize and applies the matching `harnesses/<target>/skills` override.

Answer:

> yes

Outcome: overrides accepted; harness selection by URL path proposed in round 2 (Q9).

**Hot reload**

> fsnotify on local sources, periodic `git fetch` on remote ones. The server rebuilds the catalog, diffs it, and calls `AddTool`/`RemoveTools`.

Answer:

> yes

> One cost to accept: changing an always-loaded tool mid-session invalidates the prompt cache. That's fine for occasional edits and bad if something rewrites skills in a loop.

Answer:

> agreed

**Authoring**

> - `skill_create(name, description, body)`: scaffolds the bundle.
> - `skill_edit(name, path, old_string, new_string)`: works like the Edit tool. LLMs produce valid unified diffs about as reliably as they count brackets, so no diff format.
> - `skill_write_file(name, path, content)`: adds or replaces bundled files.
> - Pipeline: a per-repo mutex, then the agentskills spec checks in Go (kebab-case name matching its directory, description ≤1024, frontmatter parses), then an optional per-source validate command (your `quick-validate.ts`), then commit, push and an optional PR.
> - Use the `git` binary, not go-git: you get your SSH agent, signing, credential helpers and Gitea remotes for free.

Answer:

> ok

Outcome: accepted; grouped under change handles (Q7).

**Repo layout**

> - `cmd/skills-mcp/` contains `serve`, `list` and `validate`.
> - `internal/source` handles clone, fetch, cache and watch.
> - `internal/catalog` handles parsing, variants and precedence.
> - `internal/mcpserver` handles registration and diff sync.
> - `internal/authoring` handles validation and git.
> - `internal/config` holds configuration.
> - Libraries: `go-sdk`, `fsnotify`, a YAML frontmatter parser.

Answer: none recorded.

Outcome: expanded in [7. File plan](#7-file-plan).

**Q1: What does "a repo" mean?**

> This is the big one. Writes going into generated files are exactly what the registry's "generated-edit recovery" exists to clean up.
> - **Q1A1 (recommended):** the server reads plain Agent Skills directories and handles hostname variants and harness overrides itself, which is cheap. For EJS includes, you add a build target to ai-registry that writes includes out as plain files. The server can then run the registry's own build as a configured hook after writes. Write-back always goes to *source*, never to build output.
> - **Q1A2:** port the registry build to Go. That's two implementations of the same build in two languages, and they will drift.
> - **Q1A3:** support plain skill directories only and drop includes from the registry.

Answer: none recorded.

Outcome: the premise about includes was wrong (see A.4); Q1A5 added and chosen in round 4 (A.5).

**Q2: How should agent writes land?**

> A skill is a prompt that every future session trusts. An agent that reads one poisoned web page and then calls `skill_edit` leaves an injection behind for good.
> - **Q2A1 (recommended):** commit to a `skills/proposals/<session>` branch and open a PR. Changes go live after you merge.
> - **Q2A2:** commit and push directly to the tracked branch, live immediately through `list_changed`.
> - **Q2A3:** make it a per-source setting.

Answer (given on Q2A2):

> what is list_changed?

Outcome: explained in round 2; Q2A2 chosen in round 4 (A.5).

**Q3: Deployment**

> Local stdio only, or also a shared Streamable HTTP server later? With HTTP, `base_dir` and scripts stop working for remote clients. That's workable if `path` serving is a full fallback, but bundles that depend on scripts would become read-only.

Answer:

> i run A LOT of agents locally so RAM is a concern here

Outcome: one local daemon over Streamable HTTP proposed in round 2; confirmed in round 4 (A.5).

**Q4: Which harnesses must work?**

> - Claude Code: confirmed above.
> - Codex: I couldn't confirm from primary sources how it handles `list_changed` or tool deferral.
> - pi: to my knowledge pi doesn't ship an MCP client, so "completely replace harness skills" there means writing an adapter extension. Is that in scope?

Answers:

> Claude Code: yes

> Codex: yes

> pi: it does now

Outcome: all three required; pi's built-in client verified at 1.3.0.

### A.3 Round 2: follow-up findings and revisions

| Topic | Finding or revision | Status |
|---|---|---|
| Item 2 | Showed the tool definition and result format, plus `offset` chunking: pi removes the middle of results over 20 KB, and three ai-registry skills exceed that. | Specified in section 10 |
| Item 3 | The daemon keeps a bare clone and serves immutable read-only snapshots per commit; authoring uses a separate worktree. | Specified in sections 4, 5, 8 |
| Item 4 | `tools/list` returns every tool, but each client decides what the model sees: Claude Code shows names only unless `alwaysLoad` is set; pi shows nothing unless `exposure` is `direct`; Codex appears to show everything (unverified, V1). Server instructions carry one rule, not skill listings. | Specified in sections 2, 10 |
| Q1 | Restated Q1A1 and added Q1A4: a dedicated skills repository with no build step, kept separate from ai-registry. | Superseded by Q1A5 (A.5) |
| Q2 | `list_changed` is the server notifying the client that its tool list changed so the client refetches it; without it, new skills appear only after a restart. Q2A2 makes an edit live in every running session within seconds, including an injected edit. | Q2A2 chosen (A.5) |
| Q3 | Measured 12.5 MB RSS per go-sdk stdio process (4.4 MB private). The larger cost of per-agent processes is duplicated catalogs, watchers, fetch pollers, and racing Git writers. Proposed one loopback daemon with bearer auth. | Confirmed (A.5) |
| Item 6 | Harness selection by URL path in place of `clientInfo`, because spec 2026-07-28 removes `initialize` and makes `clientInfo` optional per request. | Confirmed as Q9A1 (A.5) |
| Q5 | New: how user skills reach Codex and pi, since prompt support there is undocumented. | Q5A1 chosen (A.5) |
| Q6 | New: whether disabling native skill loading in each harness is in scope. | Answered (A.5) |

### A.4 Round 3: introduced while writing and reviewing this document

| Topic | Revision | Status |
|---|---|---|
| Q7 | Authoring grouped under server-minted change handles, because per-call commits fragment history and cannot group a multi-file change. | Confirmed as Q7A1 (A.5) |
| Q8 | `path` sources are read-only; authoring targets `git` sources only, so the daemon never commits into a user's working tree. | Confirmed as Q8A1 (A.5) |
| Baseline correction | Rounds 1 and 2 stated 49 skills, 6,534 description characters, and EJS includes. Verified at `27e2a8c`: 48 skills, 10,992 description characters, no includes; only four fixed template expressions under `skills/`. Q1 reframed and Q1A5 added. | Corrected in section 2; extended in A.5 |
| Transport | A stateless go-sdk handler would deliver no `list_changed` to legacy clients; the handler is stateful with GET, POST, and DELETE routed. | Specified in sections 4, 10; protocol scope corrected in A.6 |
| Fetch | `git clone --bare` sets no fetch refspec; fetch uses `+refs/heads/<branch>:refs/heads/<branch>`. | Specified in section 8 |
| Snapshots | Atomic extraction through a temporary directory, persisted `SnapshotIndex` for retention across restarts. | Specified in sections 5, 6, 8 |
| Chunking | Continuations pin `revision`; offsets count content bytes; wrappers repeat on every chunk; rune-boundary fallback for long lines. | Specified in sections 8, 10 |
| Authoring safety | `bundle` confined to configured roots; `validate` runs from the base snapshot; only touched bundles are committed; deny scan covers file paths; `commit` recorded for idempotent retries; `frontmatter` added to `skill_create`; `skill_change_read` added for raw source edits; lock ordering defined. | Specified in sections 6, 8, 9, 10 |
| HTTP protection | Host and Origin checks use go-sdk localhost protection and `http.CrossOriginProtection`; only the bearer check is custom. `skills-mcp token` prints the token for client configuration. | Bearer check superseded by A.12; rest specified in sections 3, 7, 10 |
| Authoring tool loading | Authoring tools are not `alwaysLoad`, so they cost no upfront context in Claude Code. | Superseded by A.13 (server-level `alwaysLoad`) |
| Body edits | Body-only edits send no `list_changed`, because tool definitions carry only name and description; client prompt caches stay valid. | Specified in section 8 |
| Review round 2 | Cross-origin protection wraps the handler (the options field is deprecated) and covers non-safe methods only; `SessionTimeout: 0` because GET streams do not reset the idle timer; snapshot retention keeps base revisions of open changes; `frontmatter` is an ordered array; chunking errors when overhead exceeds the budget; Q1, Q3, and Q9 blocking markers completed. | Specified in sections 2–4, 8–11, 15 |

### A.5 Round 4: decisions

Options presented (condensed from the round-4 message):

> **Q1:** How are the template tags in ai-registry skills handled? **Q1A1:** a build target in ai-registry writes plain, already-rendered skills, and the server serves that output. **Q1A4:** a dedicated skills repo with no templating at all. **Q1A5 (recommended):** the server fills in those four expressions itself, using the paths of its cached copy, refuses to load a skill that uses any other template tag, and agents edit the raw source text.
>
> **Q2:** **Q2A1 (recommended):** push a branch and open a PR. **Q2A2:** push straight to the tracked branch. **Q2A3:** a setting per skill source.
>
> **Q3:** **Q3A1 (recommended):** one long-running local server on loopback HTTP that every agent shares. **Q3A2:** a separate stdio process per agent.
>
> **Q5:** **Q5A1:** a pi extension that maps `/library:<name>` to the server's prompts. **Q5A2:** expose user skills as tools in Codex and pi as well.
>
> **Q6:** **Q6A1:** the setup turns off built-in skill loading in each harness. **Q6A2:** no; the user handles each harness's config.
>
> **Q7:** **Q7A1 (recommended):** change handles. **Q7A2:** each edit tool call commits directly.
>
> **Q8:** **Q8A1 (recommended):** folder sources are read-only. **Q8A2:** edits are written straight into the folder.
>
> **Q9:** **Q9A1 (recommended):** URL path per harness. **Q9A2:** the `clientInfo` client name.

Answer:

> q1a5 refusal has to be known the user somehow, q2 - push to main, q3a1, q5a1, q6 -- should still be able to load skills from project directories, q7a1, q8a1, q9a1

| ID | Answer | Outcome |
|---|---|---|
| Q1 | "q1a5 refusal has to be known the user somehow" | Q1A5. Refused bundles carry diagnostics that first-chunk skill loads and prompts on the affected harness append, that the daemon logs, and that `skills-mcp list` prints. |
| Q2 | "push to main" | Q2A2 with the tracked branch `main`. Rejection handling: rebase, re-verify, retry up to three pushes, never force. |
| Q3 | "q3a1" | Confirmed. |
| Q5 | "q5a1" | `pi-skill-library` gains the daemon as a library source. Codex delivery depends on V4; Q10 opened for a negative result. |
| Q6 | "should still be able to load skills from project directories" | Registry skills are removed from the claude-code, codex, and pi homes by an ai-registry build change; project-level discovery stays enabled. |
| Q7 | "q7a1" | Confirmed. |
| Q8 | "q8a1" | Confirmed. |
| Q9 | "q9a1" | Confirmed. |

Revisions made while specifying round 4:

| Topic | Revision | Status |
|---|---|---|
| Baseline correction | Re-verified at `429e35f`. Template tags also occur in bundled files (`references/`, `assets/`, `scripts/`), and harness skills use `output_dir` and `file_dir`. The build context has eight names plus `env()`. Section 2 lists every occurrence. | Corrected in section 2 |
| Rendering location | Rendering happens at snapshot extraction, so absolute-path reads return rendered text. `path` sources are snapshotted too. | Specified in sections 4, 5, 8 |
| Template context | Per-source `template.vars`, with ai-registry's path variables pointing at the user's checkout; `file_dir` and `file_path` built in; `env()` limited to `template.env`, so authored skills cannot publish daemon credentials. | Specified in sections 6, 9 |
| Render context changes | `RenderContextHash` in `SnapshotIndex` wipes snapshots rendered with a different context. | Specified in sections 6, 8 |
| Direct-push safety | A `writable` source requires non-empty `deny_patterns`, because pushes reach GitHub with no review step. | Specified in sections 3, 6 |
| Systemd environment | `template.env` values come from `$XDG_CONFIG_HOME/skills-mcp/env` under systemd. | Specified in sections 5, 7 |

### A.6 Review round 3: findings applied after round 4

| Topic | Revision | Status |
|---|---|---|
| Protocol scope | go-sdk v1.8.0 rejects 2026-07-28 calls on a stateful handler with `-32022`, so the daemon serves legacy protocol versions and relies on client fallback. Earlier text claiming `subscriptions/listen` delivery was wrong. V6 extended to verify fallback per harness. | Specified in sections 2, 4, 9, 11 |
| Commit verification | Submit commits first (hooks disabled) and verifies that exact commit: full catalog build for duplicates and template rules, protected validator paths, scope, deny scan with `-a` over a staged diff that includes new files, `validate` against the materialized commit. Deny scan source superseded by A.7 (`git diff-tree` of the commit). Mutations stage paths; written content must be UTF-8 without NUL; `worktree` removed from `ChangeOpenResult`; clean-tree check before push. | Specified in sections 3, 8, 9, 10 |
| Validator protection | Changes to the `validate` script and its directory are rejected, so a pushed change cannot disable validation for later submits. | Specified in sections 8, 13 |
| Extraction | `git ls-tree` and `git cat-file` replace `git archive` (no `.gitattributes` effects); symlinks and submodules are never materialized and refuse their bundle; `ignore_file` honors `.registry-ignore`; unique temporary directories with singleflight; read-only trees made writable before removal; `RenderContextHash` includes a renderer version and leaves `{snapshot}` literal. | Specified in sections 5, 6, 8 |
| Durable state | `repo.git` moved to `$XDG_STATE_HOME`; startup prunes worktrees and discards changes whose worktree is missing; offline startup extracts `current` from `repo.git`. | Specified in sections 5, 8 |
| Locks and refresh | Single `Refresh(source)` routine compares the branch ref against `SnapshotIndex.current`; refresh after push is synchronous and outside the source mutex; retention reads an in-memory base-revision index; view mutex added; ref-moving commands listed under the source mutex. | Specified in section 8 |
| Publication | Already-published detection with `merge-base --is-ancestor`; `=` push result is success; any rebase failure aborts; `worktree remove --force` (superseded by `-f -f` in A.9) with idempotent cleanup; submit on a submitted change returns the recorded result. | Specified in sections 8, 9, 10 |
| Git execution | No prompts, per-command timeouts, `gc.auto=0`, `ls-remote` preflight, systemd `SSH_AUTH_SOCK` and `PATH`. | Specified in sections 5, 8, 10 |
| Retention | Runs on every refresh tick; on-demand snapshots enter retention; terminal change records deleted after `change_ttl`. | Specified in section 8 |
| Served names | Author prefixing matches the registry's installed names (`author_prefix_except`). | Specified in sections 4, 6, 9 |
| Harness limits | Codex `tool_timeout_sec` and pi `timeout` in client config (V10); Claude Code splits prompt arguments on whitespace (V9); `allowed-tools` and `` !`command` `` preloads declared out of scope. | Specified in sections 1, 2, 10, 11 |
| pi-skill-library | Contract covers the existing `input` interceptor and re-registration on `list_changed`. | Specified in section 7 |
| Codex user skills | Codex keeps registry user skills in its home until V4 passes or Q10 is answered. | Specified in sections 7, 10 |
| Configuration and HTTP | `default_write_source` optional; `template.vars` merges over the default; invalid `allowed_origins` is an error; request check order defined; constant-time token compare; `O_EXCL` token creation; daemon lock file. | `allowed_origins` and `O_EXCL` superseded by A.7; rest specified in sections 5, 6, 9 |
| Chunking | `offset` and `revision` required together; out-of-range and mid-rune offsets rejected; `<bundled_files>` only on the first chunk. | Specified in sections 8, 10 |
| Path sources | Content-hash revision; recursive watch registration. | Specified in sections 6, 8 |

### A.7 Review round 4: findings applied

| Topic | Revision | Status |
|---|---|---|
| Served names | Prefix is `lowercase(author)`, skipped when the name already carries it, with `agorbatchev` also exempt; a missing author means no prefix; `invalid_author` replaces `missing_author`; `reserved_name` keeps skills from colliding with authoring tools in pi's `-` → `_` naming. | Specified in sections 2, 4, 6, 9 |
| Ignore files | Corrected baseline: `.registry-ignore` applies only at or below the copied directory, with a custom `./` prefix rule. Evaluation implemented in `internal/source/ignore.go` over go-git's gitignore matcher; `SKILL.md` is never removed (`missing_skill_md`). | Specified in sections 2, 7, 8 |
| Render context | `RenderContextHash` covers `roots` and `harness_roots`. | Specified in section 6 |
| Deny scan | Plumbing `git diff-tree` with `--no-ext-diff --no-textconv`; `.gitattributes` writes rejected. | Specified in sections 8, 9 |
| Crash recovery | Change reconciliation at startup and submit entry; `HEAD^ == base_revision` check; orphaned change directories and branches removed; atomic JSON writes; quarantine of unparsable records; unindexed snapshots deleted. | Specified in sections 3, 5, 8 |
| Submit execution | Detached context with `submit_timeout`, progress notifications, outcome recorded for a later call; client timeouts set to 900 seconds; pi `timeout` is in seconds. | Specified in sections 6, 8, 10 |
| Refresh and locks | `Refresh` clones when `repo.git` is absent (temporary directory, rename); source mutex held only around Git commands, never while waiting on singleflight or the view mutex; index mutex added; view rebuild compares the views' revision against `current`, so a returning Refresh has made `current` live; startup loads changes before the first Refresh and suppresses view rebuilds until servers exist. | Specified in section 8 |
| Hooks and maintenance | `core.hooksPath=/dev/null` on every Git command; maintenance runs without the `gc.auto` and `maintenance.auto` overrides, owned by the source manager. | Specified in section 8 |
| Rebase | A rebase that drops the change as already upstream fails with a discard instruction instead of recording a foreign commit. `attempts` counts pushes in the publishing call. | Specified in sections 6, 8 |
| Authoring results | `skill_change_read` and `skill_change_diff` are chunked with a content `sha`. | Specified in sections 8, 10 |
| Token | Created through a temporary file and `link`; malformed or over-permissive token files are rejected. | Specified in section 5 |
| HTTP | `allowed_origins` removed; `ServerOptions.Capabilities` declares tools and prompts with `listChanged`. | Specified in sections 6, 7, 9, 10 |
| Path confinement | `os.Root` opened at the worktree root. | Specified in sections 8, 9 |
| pi-skill-library | One client per pi session, `terminateSession()` on shutdown and replacement, dependency moved to `@earendil-works/pi-coding-agent`. | Specified in sections 2, 7 |

### A.8 Review round 5: findings applied

| Topic | Revision | Status |
|---|---|---|
| Refresh concurrency | Per-source refresh mutex; `Refresh` serves the local branch ref when the fetch fails, so startup no longer sets `current` itself; path sources refresh at startup. | Specified in section 8 |
| Rebase crash recovery | `rebase_onto` persisted before a rebase and applied by reconciliation. | Specified in sections 6, 8 |
| Mutations during failed submits | Reconciliation runs before every mutation and discard; cleanup after an expired step runs on a fresh 60-second context. | Specified in section 8 |
| Submit ownership | The pipeline goroutine owns the change mutex; a later call returns a recorded success or reruns. | Specified in section 8 |
| Startup cleanup | `git worktree prune` before branch deletion; failures are warnings. | Specified in section 8 |
| Publish | A failed Refresh in 5.2 fails the submit without rebasing. | Specified in section 8 |
| Locks | Full order: change, refresh, source, index, view, catalog. View-mutex holders use `current` values read beforehand. | Specified in section 8 |
| Snapshot reset | The reset clears `current`; `superseded` only records revisions whose snapshot exists. | Specified in sections 6, 8 |
| Path sources | Revision computed from the copied tree. | Specified in section 8 |
| `touched_bundles` | Persisted before the file write and recovered from the staged diff. | Specified in section 8 |
| Git execution | Authoring tools run Git on a detached 120-second context; process-group kill and `WaitDelay`; durations must be positive. | Specified in sections 6, 8 |
| Precedence | A refused bundle still shadows later sources. | Specified in section 9 |
| Baseline | Claude Code timeouts restated from current docs (per-server `timeout` in milliseconds, hard limit); `quick-validate.ts` rules expanded, `archify` fails them today. Token mode exactly `0600`; systemd env path uses `%E`. | Specified in sections 2, 5, 7, 10 |

### A.9 Review round 6: findings applied

| Topic | Revision | Status |
|---|---|---|
| Change directories | Startup only treats `chg_<16 hex>` directories as changes, so `.quarantine/` survives. | Specified in section 8 |
| Path sources | Revision hashed after ignore evaluation; `rev/.tmp-path-*` temporary directory. | Specified in sections 5, 8 |
| Worktree creation | A failed `worktree add` discards the change; locked `initializing` worktrees count as missing; cleanup uses `worktree remove -f -f`. | Specified in section 8 |
| Verification scope | Deletions, out-of-scope renames, served-name or invocation changes, and lost cross-source skills fail verification; verification catalogs bypass the cache; ignore files above the validator are protected. | Specified in section 8 |
| Mutation atomicity | Writes go through temporary files; a failed `git add` undoes the write; reconciliation restores touched bundles from the index. | Specified in section 8 |
| Publication detection | `push_started` makes reconciliation fetch before deciding whether a change is published. | Specified in sections 6, 8 |
| Refresh errors | `Refresh` returns extraction, persistence, and rebuild errors too. | Specified in section 8 |
| Precedence key | Defined for refused bundles, including harness-root replacement. | Specified in section 9 |
| Restarts | V11 covers client re-initialization after a restart; `pi-skill-library` treats `404` as a connection error. | Specified in sections 7, 11 |
| Shutdown | Bounded wait, process-group kill, `Pdeathsig`. | Specified in section 8 |
| `attempts` | 1–3 for pushes in the publishing call, 0 only for reconciliation-detected publication. | Specified in section 6 |

### A.10 Review round 7: findings applied

| Topic | Revision | Status |
|---|---|---|
| Restore commands | `git restore --worktree` without `--source`, limited to bundles with index entries; mutations validate before persisting `touched_bundles`. | Specified in section 8 |
| Rebase scope | `git rebase --onto <rebase_onto> <base_revision>` replays only the change commit, so force-pushed upstream removals are not resurrected. | Specified in section 8 |
| Unknown push outcome | `push_started` stays set unless a ref status line was parsed or the commit is found on the branch. | Specified in section 8 |
| Startup reconciliation | Rebuilds suppressed; failures are warnings and skip the TTL discard. | TTL exemption superseded by A.11; rest specified in section 8 |
| Remote URL | `origin` follows the configured `url`. | Specified in section 8 |
| File modes | Temporary files keep the target's permission bits. | Specified in section 8 |
| Shutdown | Pipeline goroutines record their own interruption; shutdown writes no change record. | Specified in section 8 |

### A.11 Review round 8: findings applied

| Topic | Revision | Status |
|---|---|---|
| Dependency-bearing skills | Read-only snapshots break in-bundle `bun install` (`disk-cleanup`, `github-fix-issues`; `archify` also ships `package.json`). Recorded as Q11. | Decided as Q11A2 (A.15) |
| Stale Git locks | SIGTERM before SIGKILL; stale `*.lock` files removed at startup; discards need only published detection before cleanup. | Specified in section 8 |
| systemd stop | `KillMode=mixed`, `TimeoutStopSec=60`; a shutdown context stops pipelines from starting new commands. | Specified in sections 7, 8 |
| Path quoting | Path-listing Git commands use `-z`. | Specified in section 8 |
| Record coherence | Superseded A.6 entries marked; `reconcile.go` scope corrected. | Corrected in section 7 and A.6 |

### A.12 Review round 9: findings applied

| Topic | Revision | Status |
|---|---|---|
| Bearer authentication | go-sdk `auth.RequireBearerToken` with a constant-time static-token verifier replaces a custom check. | Specified in sections 3, 7, 9, 10 |
| Startup ordering | Watchers, refresh loop, sweep, and maintenance start after server registration. | Specified in section 8 |
| Symlinked bundles | A symlink or submodule directly under a root is a refused bundle with a diagnostic; a symlinked `path` source root is resolved once. | Specified in sections 6, 8 |
| Repairs | Served-name and invocation comparison applies only to bundles served in the base views. | Specified in section 8 |
| Path quoting | `-z` added to the remaining path-listing commands. | Specified in section 8 |

### A.13 Review round 10: findings applied

| Topic | Revision | Status |
|---|---|---|
| Maintenance | Runs without auto-detach, under the source mutex, with a 600-second timeout. | Specified in section 8 |
| Process kill | The wrapper sends SIGKILL to the whole group after the grace period. | Specified in section 8 |
| Path source revision | Includes symlinks and other non-regular entries. | Specified in section 6 |
| Claude Code first turn | Server-level `alwaysLoad` in the client configuration; authoring tools load upfront there; V12 added. | Specified in sections 10, 11 |
| Argument validation | Tools registered with generic `mcp.AddTool` and explicit schemas; `offset` must be non-negative. | Specified in sections 7, 8 |
| Annotations | `destructiveHint: true` on edit, write, discard, and submit. | Specified in section 10 |
| Smaller items | Instructions omit the authoring sentence without a write source; `worktree_add_failed` transition; view-built revision for skill calls; literal pathspecs. | Specified in sections 6, 8, 10 |

### A.14 Review round 11: findings applied

| Topic | Revision | Status |
|---|---|---|
| `check-ignore` | Exempt from `GIT_LITERAL_PATHSPECS` (git 2.43 rejects it with exit 128, verified); runs with `--stdin -z`. | Specified in section 8 |
| Removal gating | Registry skills leave each harness home only when that harness's client checks pass; a failure keeps native skills and opens a decision. | Specified in sections 7, 11, 14 |
| Shutdown | Sessions are closed so GET streams end; the full sequence fits the 60-second stop timeout. | Specified in section 8 |

### A.15 Round 5: Q11

Exchange (verbatim):

> q11 -- you recommendation is to break the skills? why is that a problem

Answered: snapshots are shared by every agent, replaced on every revision, and read-only so served skills change only through authoring. Measured with bun 1.4.4 on read-only copies: the skills' own `bun install` fails without installed dependencies and without `bun.lock`, and exits 0 with "no changes" when both are present. Recommendation changed to Q11A2.

> bun.lock?

> i know what bun lock is, arent we writing this in golang?

Outcome: Q11A2 restated as a generic, configuration-driven `prepare` step (like `validate`), so the Go daemon carries no package-manager logic.

> Q11A2

| Topic | Revision | Status |
|---|---|---|
| Q11 | Q11A2 with per-source `prepare` rules. | Specified in sections 1, 3, 6, 7, 8 |
| Package cache inodes | bun's default Linux backend hardlinks installed files from its global cache (37 links measured); the example rule uses `--backend=copyfile`, and the daemon replaces any multiply-linked file with a private copy before setting permissions. | Specified in sections 6, 8 |
| Install links | `node_modules/.bin` symlinks are allowed only when they resolve inside the rule's directory. | Specified in sections 3, 8 |

### A.16 Review round 12: `prepare` findings applied

| Topic | Revision | Status |
|---|---|---|
| Workspace roots | Git snapshots materialize only the served roots; ai-registry's root `package.json` lists `skills/user/disk-cleanup` and `skills/agent/github-fix-issues/scripts` as workspace members, and bun otherwise installs the whole workspace at the snapshot root. | Specified in section 8 |
| Source file list | `RenderRecord.files` records each bundle's source files before `prepare`; `files` and the link rule use it. | Specified in sections 6, 9 |
| Installed modes | `prepare` output keeps the owner-execute bit as `0555`. | Specified in section 5 |
| Transient failures | `prepare_retry` makes Refresh re-extract with backoff; the env file names the `prepare` interpreter. | Specified in sections 5, 6, 8 |
| Cancellation | Shutdown or deadline during `prepare` aborts the extraction. | Specified in section 8 |
| Matching | Matching directories are computed once before rules run and never inside `prepare` output. | Specified in sections 6, 8 |
| Verification | `prepare` runs only in touched bundles, bounded by the pipeline deadline. | Specified in section 8 |
