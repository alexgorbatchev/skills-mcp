`skills-mcp` serves agent skills from a Git repository to Claude Code, Codex, and pi as MCP tools and prompts, from one local daemon shared by every agent session. Agents can also create and edit skills through MCP tools; their changes are verified and pushed back to the repository, and every running session sees them.

# What It Does

- **One tool per skill**: each model-invoked skill is an MCP tool named after the skill, with the skill's description as the tool description, so harnesses trigger skills the way they trigger any tool.
- **User skills as prompts**: user-invoked skills are MCP prompts (`/skills:<name>` in Claude Code).
- **Git as storage**: the daemon clones the skill repository, follows its branch, and serves new revisions to running sessions without restarts.
- **Per-harness views**: `harnesses/<harness>/skills` roots add or replace skills for one harness only.
- **Rendered skills**: the fixed template tags ai-registry uses (`<%- repo_root %>`, `<%- file_dir %>`, `<%- env("NAME", "fallback") %>`, …) are filled in, in `SKILL.md` and in bundled files alike.
- **Refusals you can see**: a skill that fails validation is not served, and every skill load reports it so the agent tells you.
- **Agent authoring**: `skill_change_*` tools let an agent create and edit skills in a change, then commit, verify, and push it.
- **One process for all agents**: every session on the machine shares one daemon, one clone, and one set of snapshots.

# How It Works

- You point `config.yaml` at one or more skill sources: Git repositories or local folders.
- `skills-mcp server run` (usually under systemd) clones each source, renders the skills, and serves `http://127.0.0.1:7341/mcp/<harness>` for `claude-code`, `codex`, and `pi`.
- Each harness lists one tool per skill. When a task matches a skill's description, the agent calls the tool and gets the skill's instructions plus the list of bundled files, with absolute paths it can read and run.
- Every minute the daemon fetches the branch; new or changed skills reach running sessions through MCP list-change notifications.
- An agent that wants to change a skill opens a change, edits files, and submits it. The daemon commits, checks the commit, pushes it to the tracked branch, and serves it before the submit call returns.

# How it Really Works

- **Files on disk**: configuration and the bearer token live in `$XDG_CONFIG_HOME/skills-mcp/`; bare clones, the daemon lock, and change worktrees live in `$XDG_STATE_HOME/skills-mcp/`; rendered snapshots live in `$XDG_CACHE_HOME/skills-mcp/` and can be deleted at any time.
- **Snapshots are read-only**: each commit becomes its own read-only directory. Paths handed to an agent stay valid while newer commits arrive; superseded snapshots are deleted after `snapshot_retention` (24 hours by default).
- **Only the skill roots are copied**: files outside the configured roots never reach a snapshot, and `.registry-ignore` rules inside a bundle are applied the way ai-registry applies them.
- **Dependencies are installed once per revision**: `prepare` rules (for example `bun install --frozen-lockfile`) run in each bundle that has the trigger file, so a skill's own install command finds everything in place.
- **Large skills arrive in pieces**: results larger than a harness's budget (48 KB Claude Code, 32 KB Codex, 19 KB pi) end with a continuation that names the next offset; continuations stay on the revision they started on.
- **Description edits change the tool list; body edits do not**: only a changed name, description, or invocation sends a list-change notification, so harness prompt caches survive routine edits.
- **Pushes are checked first**: a submit verifies the exact commit it will push — duplicate and invalid skills, template tags, changes outside the edited bundles, changes to the validator, the source's `deny_patterns`, and the source's `validate` command — then pushes only if the remote branch is still exactly the commit the change was built on, so it never overwrites or resurrects anyone else's history. If the branch moved, the change is rebased onto the new tip and checked again, up to three attempts.
- **Network use**: `git fetch` every `fetch_interval`, `git push` on submit, and whatever `prepare` commands download. Nothing else leaves the machine; the daemon listens on `127.0.0.1` only and requires a bearer token.
- **Output modes**: CLI commands print human output by default and compact `key=value` lines with `AGENT=1`; errors go to stderr and exit 1.

# Installation

Download the archive for your platform from the [latest release](https://github.com/alexgorbatchev/skills-mcp/releases/latest), then unpack it onto your `PATH`:

```bash
curl -fsSL -o skills-mcp.tar.gz https://github.com/alexgorbatchev/skills-mcp/releases/download/v0.1.0/skills-mcp_0.1.0_darwin_arm64.tar.gz
tar -xzf skills-mcp.tar.gz skills-mcp && mv skills-mcp ~/.local/bin/
```

# Setup

- [Git](https://git-scm.com/) runs every operation on `git` sources; pushes use your SSH agent and Git configuration, so the daemon needs `SSH_AUTH_SOCK` for writable sources. `path` sources alone need no Git.
- [Bun](https://bun.sh/) is needed only when a source's `prepare` or `validate` commands call it, as ai-registry's do.
- Under systemd, the daemon reads `PATH`, `SSH_AUTH_SOCK`, and template environment variables from `~/.config/skills-mcp/env`. Copy `contrib/systemd/skills-mcp.service` to `~/.config/systemd/user/`.

# Quick Start

Create `~/.config/skills-mcp/config.yaml`:

```yaml
default_write_source: ai-registry
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
      env: [DOCS_INTERNAL_DIR]
    prepare:
      - when_file: package.json
        run: [bun, install, --frozen-lockfile, --ignore-scripts, --backend=copyfile]
        keep: [bun.lock]
    validate: [bun, skills/agent/skill-writer/scripts/quick-validate.ts, "{bundle_dir}"]
    deny_patterns: ['internal\.example\.com']
```

Start the daemon and export the token for the harnesses:

```bash
systemctl --user enable --now skills-mcp
export SKILLS_MCP_TOKEN="$(skills-mcp token show)"
```

List what each harness is served:

```bash
skills-mcp catalog list --harness claude-code
```

Sample Output:

```text
claude-code
├─ agent-browser  Use agent-browser for browser automation against real rendered web pages or web apps...
├─ golang  Apply Go coding rules, design principles, and project conventions for maintainable Go code...
├─ juliusbrussee-caveman (prompt)  Terse caveman voice: answer first, fluff gone, every technical fact...
╰─ vhs  Use when operating Charmbracelet VHS, writing or debugging .tape files, or creating terminal...
```

Connect Claude Code (`.mcp.json` or user configuration):

```json
{ "mcpServers": { "skills": { "type": "http", "url": "http://127.0.0.1:7341/mcp/claude-code", "alwaysLoad": true, "timeout": 900000, "headers": { "Authorization": "Bearer ${SKILLS_MCP_TOKEN}" } } } }
```

Connect Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.skills]
url = "http://127.0.0.1:7341/mcp/codex"
bearer_token_env_var = "SKILLS_MCP_TOKEN"
tool_timeout_sec = 900
```

Connect pi (`~/.pi/agent/mcp.json`):

```json
{ "mcpServers": { "skills": { "url": "http://127.0.0.1:7341/mcp/pi", "exposure": "direct", "timeout": 900, "headers": { "Authorization": "Bearer ${SKILLS_MCP_TOKEN}" } } } }
```

Check a bundle before pushing it yourself:

```bash
skills-mcp bundle validate skills/agent/my-skill
```

# Options & Flags

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--config <path>` | — | `~/.config/skills-mcp/config.yaml` | Configuration file |
| `--version` | `-v` | — | Print the version |
| `--help` | `-h` | — | Print help |

`skills-mcp catalog list`

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--harness <name>` | — | all harnesses | Only list `claude-code`, `codex`, or `pi` |

`skills-mcp bundle validate <bundle-dir>...`

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--source <name>` | — | `default_write_source` | Source whose rules apply |

# Configuration

| Key | Default | Description |
| :--- | :--- | :--- |
| `listen` | `127.0.0.1:7341` | Address to serve; the host must be `127.0.0.1` |
| `fetch_interval` | `60s` | How often sources are fetched |
| `submit_timeout` | `600s` | Limit for one submit, at least `60s` |
| `snapshot_retention` | `24h` | How long superseded snapshots are kept |
| `change_ttl` | `168h` | How long idle changes and finished change records are kept |
| `default_write_source` | none | Source agents write to; without it the authoring tools are off |
| `harnesses.<name>.max_result_bytes` | 48000 / 32000 / 19000 | Result budget for `claude-code` / `codex` / `pi` |
| `sources[].kind` | — | `git` or `path` (a local folder, read-only) |
| `sources[].roots`, `harness_roots` | — | Skill directories and their invocation (`model` or `user`); `{harness}` expands per harness |
| `sources[].template.vars`, `template.env` | `repo_root: {snapshot}` | Values for `<%- name %>` tags; environment variables `env()` may read |
| `sources[].ignore_file` | none | Per-bundle ignore file name, such as `.registry-ignore` |
| `sources[].author_prefix_except` | none | Serve skills by other authors as `<author>-<name>` |
| `sources[].prepare` | none | Commands run in bundle folders containing `when_file`; `keep` survives the ignore file |
| `sources[].validate` | none | Command run per edited bundle before a push; `{bundle_dir}` is replaced |
| `sources[].deny_patterns` | none | Patterns that block a push; required for writable sources |

## Known Limits

- A project skill with the same name as a served skill appears twice to the model: once as the harness's own skill and once as the MCP tool.
- Claude Code splits prompt arguments on spaces; quote multi-word text after `/skills:<name>`.
- Claude Code's `allowed-tools` grants and `` !`command` `` preloads do not apply to skills served over MCP (`agent-browser` and `shadcn-shadcn` in ai-registry).

# License

[MIT](LICENSE)
