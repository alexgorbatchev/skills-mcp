---
name: "skills-mcp"
description: "Use when operating the skills-mcp CLI: running the skills daemon, configuring sources, listing served skills, validating skill bundles, or connecting Claude Code, Codex, or pi to it."
author: "alexgorbatchev"
metadata:
  created_on: 2026-10-26 17:19
  last_modified: 2026-10-26 21:20
  status: current
---

## Execution rules

Read this entire skill before running operational commands. Set `AGENT=1`
on every invocation, or export it for the session. Select commands and
options from this reference. Read exit status as well as stdout; failures
exit 1 and print `ERR:` diagnostics to stderr in agent mode (`[ERROR]` in
human mode).

Set `AGENT=0` or leave it unset for human output. Values `1`, `true`, and
`yes` enable agent output, with case and surrounding whitespace ignored.
Paths printed by every command show the home directory as `~`.

## Files and environment

| Path or variable | Purpose |
| --- | --- |
| `$XDG_CONFIG_HOME/skills-mcp/config.yaml` | Configuration (default `~/.config/skills-mcp/config.yaml`); `--config` overrides it. |
| `$XDG_CONFIG_HOME/skills-mcp/token` | Bearer token, 64 hex characters, mode `0600`; created by `token show` or `server run`. |
| `$XDG_CONFIG_HOME/skills-mcp/env` | `KEY=value` lines for the systemd unit: `PATH` (must reach `git` when any `git` source is configured, and the programs `prepare` and `validate` run, such as `bun`), `SSH_AUTH_SOCK`, and `template.env` values. |
| `$XDG_STATE_HOME/skills-mcp/` | Daemon lock, bare clones (`sources/<name>/repo.git`), and change records (`changes/`). |
| `$XDG_CACHE_HOME/skills-mcp/` | Rendered, read-only snapshots (`sources/<name>/rev/<revision>/`). |
| `AGENT` | Output mode, see above. |
| `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME` | Absolute base directories; relative or unset values use `~/.config`, `~/.local/state`, and `~/.cache`. |

`config.yaml` keys: `listen` (default `127.0.0.1:7341`, host must be
`127.0.0.1`), `fetch_interval` (default `60s`), `submit_timeout` (default
`600s`, at least `60s`), `snapshot_retention` (default `24h`), `change_ttl`
(default `168h`), `default_write_source` (enables the authoring tools),
`harnesses.<claude-code|codex|pi>.max_result_bytes` (defaults 48000, 32000,
19000), and `sources`. Each source has `name`, `kind` (`git` or `path`),
`roots` and `harness_roots` (`path`, `invocation: model|user`; `{harness}`
expands per harness), `template` (`vars`, `env`), `ignore_file`,
`author_prefix_except`, and `prepare` (`when_file`, `run`, `keep`,
`timeout`). Git sources add `url`, `branch`, `writable`, `validate` (argv with
`{bundle_dir}`), and `deny_patterns` (required when writable). Path sources
add `path`. Configuration errors exit 1 with one `field: problem` line each.

## `skills-mcp`

Invoke without a subcommand to print root help. All commands accept
`--help` / `-h`; the root also accepts `--version` / `-v`, which prints only
the version. Agent help starts with an alert to read this skill.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--config` | — | `string` | `""` | Config file; empty uses `$XDG_CONFIG_HOME/skills-mcp/config.yaml`. |
| `--help` | `-h` | `bool` | `false` | Print help for the selected command. |
| `--version` | `-v` | `bool` | `false` | Print only the raw version and a newline. |

## `skills-mcp server run`

Run the daemon in the foreground until SIGTERM or SIGINT. It takes an
exclusive lock (`$XDG_STATE_HOME/skills-mcp/lock`; a second daemon exits 1),
loads the config, creates the token when absent, clones or fetches every git
source, extracts and renders snapshots, and serves `GET /healthz` plus
`/mcp/claude-code`, `/mcp/codex`, and `/mcp/pi` on `listen`. It logs to
stderr. Sources refresh every `fetch_interval`; path sources also refresh on
file changes. Shutdown closes sessions, waits up to 30 seconds for running
submits, then stops remaining git and validate processes.

## `skills-mcp catalog list`

Print the skills each harness is served, from the cached snapshots, without
contacting remotes or the daemon. Human output is one tree per harness with
user skills marked `(prompt)`; agent output is one
`harness=<h> name=<n> invocation=<model|user> source=<s> revision=<sha> description=<text>`
line per skill. Refused skills print to stderr (`[WARN] refused: <location> <message>` or
`ERR: refused code=<code> <location> <message>`) and make the exit status 1.
Fails when no snapshot exists yet.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--harness` | — | `string` | `""` | Only list `claude-code`, `codex`, or `pi`. |

## `skills-mcp bundle validate <bundle-dir>...`

Check skill bundle directories before committing them: `SKILL.md`
frontmatter (`name` matching the directory, `description` of 1–1024
characters, valid `author` when prefixing applies), symbolic links, and
template tags against the source's `template` settings. Prints `OK: <dir>`
(`[OK] <dir>`) per clean directory to stdout and one
`ERR: <dir>: <code>: <file>[:<line>]: <message>` line per problem to stderr;
exits 1 when any problem exists. `node_modules` directories are skipped.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--source` | — | `string` | `""` | Source whose rules apply; empty uses `default_write_source`. |

## `skills-mcp token show`

Print the bearer token, creating `$XDG_CONFIG_HOME/skills-mcp/token` with a
new random token when absent. A token file that is not 64 hex characters or
not mode `0600` is an error.

## `skills-mcp skill`

Print the Skills CLI install command and the URL of this skill.

## `skills-mcp help [command]`

Print help for a command, the same as `<command> --help`.

## Workflows

Start the daemon under systemd and connect the harnesses:

```bash
systemctl --user enable --now skills-mcp
export SKILLS_MCP_TOKEN="$(skills-mcp token show)"
```

Claude Code (`.mcp.json` or user config):

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

pi (`~/.pi/agent/mcp.json`):

```json
{ "mcpServers": { "skills": { "url": "http://127.0.0.1:7341/mcp/pi", "exposure": "direct", "timeout": 900, "headers": { "Authorization": "Bearer ${SKILLS_MCP_TOKEN}" } } } }
```

Check what is served and why a skill is missing:

```bash
AGENT=1 skills-mcp catalog list --harness codex
```

Validate a bundle before pushing it:

```bash
AGENT=1 skills-mcp bundle validate skills/agent/my-skill
```
