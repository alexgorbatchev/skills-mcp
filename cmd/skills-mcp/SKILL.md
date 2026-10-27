---
name: "skills-mcp"
description: "Use when operating the skills-mcp CLI."
author: "Alex Gorbatchev"
metadata:
  created_on: 2026-10-26 17:19
  last_modified: 2026-10-26 17:19
  status: current
---

## Execution rules

Read this entire skill before running operational commands. Set `AGENT=1`
on every invocation, or export it for the session. Select commands and
options from this reference. Read exit status as well as stdout; failures
exit nonzero and print `ERR:` diagnostics to stderr in agent mode.

Set `AGENT=0` or leave it unset for human output. Values `1`, `true`, and
`yes` enable agent output, with case and surrounding whitespace ignored.
Operational commands use the arguments and options documented below.

## `skills-mcp`

Invoke without a subcommand to print root help. All commands accept
`--help` / `-h`; the root also accepts `--version` / `-v`.
Agent help starts with an alert to read this skill. Root and operational
commands accept no positional arguments. Require no credentials or
network access for the commands below.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--help` | `-h` | `bool` | `false` | Print help for the selected command. |
| `--version` | `-v` | `bool` | `false` | Print only the raw version and a newline. |

## `skills-mcp skill`

Print this embedded SKILL.md verbatim to stdout, including frontmatter,
in both human and agent modes. Accept no command-specific flags or
positional arguments. Work from any directory without repository files.
Output-write failures return an error.

## `skills-mcp status`

Print `status: ready` and `version: VERSION` key-value lines in agent mode.
In human mode, print `[OK] System status: ready` and `Version: VERSION`.
Accept no positional arguments or command-specific flags. Report runtime
readiness and the binary version; leave persistent state unchanged.

## `skills-mcp help [command]`

Print root help with no argument, or supply a space-separated command path,
such as `skills-mcp help status`. Accept no command-specific flags.

## `skills-mcp completion`

Print completion group help with no verb. Use one of the four shell commands
below to print a completion script to stdout. Accept no positional arguments
or command-specific flags on the group. Redirect a script to a file to save it.

## `skills-mcp completion bash`

Print a Bash completion script. Accept no positional arguments. Load with
`source <(skills-mcp completion bash)` in Bash with `bash-completion` installed.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--no-descriptions` | — | `bool` | `false` | Omit completion descriptions. |

## `skills-mcp completion zsh`

Print a Zsh completion script. Accept no positional arguments. Enable
completion with `autoload -U compinit; compinit`, then load with
`source <(skills-mcp completion zsh)`.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--no-descriptions` | — | `bool` | `false` | Omit completion descriptions. |

## `skills-mcp completion fish`

Print a Fish completion script. Accept no positional arguments. Load with
`skills-mcp completion fish | source`.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--no-descriptions` | — | `bool` | `false` | Omit completion descriptions. |

## `skills-mcp completion powershell`

Print a PowerShell completion script. Accept no positional arguments. Load
with `skills-mcp completion powershell | Out-String | Invoke-Expression`.

| Flag | Short | Type | Default | Behavior |
| --- | --- | --- | --- | --- |
| `--no-descriptions` | — | `bool` | `false` | Omit completion descriptions. |

## Workflow

```sh
export AGENT=1
skills-mcp skill
skills-mcp status
skills-mcp --version
```
