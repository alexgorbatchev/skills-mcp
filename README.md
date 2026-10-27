skills-mcp is a command-line tool for MCP server that serves agent skills from a Git repository as tools and prompts.

# What It Does

- Provides fast and intuitive command-line workflows.
- Supports dual-mode human-friendly and token-conservative agent output via `AGENT=1`.
- Prints a complete embedded command and option guide with `skills-mcp skill`.
- Offers structured subject-verb command hierarchies and aligned tree-view help screens.

# How It Works

- Parses command-line flags and subcommands using [Cobra](https://github.com/spf13/cobra).
- Formats visual tree hierarchies and status summaries for terminal users.
- Adjusts verbosity and formatting automatically when invoked by automated AI agents.

# How it Really Works

- Detects the `AGENT` environment variable to switch between human TTY output and structured agent output.
- Utilizes [cobra-help-tree](https://github.com/alexgorbatchev/cobra-help-tree) to render terminal-width-aware command tree listings.
- Follows XDG Base Directory specifications for persistent data and configuration storage.

# Prerequisites

- [Go](https://go.dev/) 1.26.2 or higher (for development)
- [Just](https://github.com/casey/just) task runner (for project automation)

# Installation

Download the latest prebuilt binary from GitHub Releases:

```bash
# Using GitHub CLI
gh release download --repo github.com/alexgorbatchev/skills-mcp --pattern 'skills-mcp_*_darwin_arm64.tar.gz'
tar -xzf skills-mcp_*_darwin_arm64.tar.gz
chmod +x skills-mcp
mv skills-mcp ~/.local/bin/
```

Or via direct download:

```bash
curl -fsSL https://github.com/alexgorbatchev/skills-mcp/releases/latest/download/skills-mcp_darwin_arm64.tar.gz | tar -xz
chmod +x skills-mcp
mv skills-mcp ~/.local/bin/
```

# Quick Start

```bash
# Show help and available commands
skills-mcp --help

# Check binary version
skills-mcp --version

# Run status check
skills-mcp status

# Read the complete guide before agent operations
AGENT=1 skills-mcp skill
```

# Options & Flags

| Flag | Short | Default | Description |
| :--- | :--- | :--- | :--- |
| `--help` | `-h` | `false` | Display help and command tree |
| `--version` | `-v` | `false` | Display binary version |

# License

MIT License. Copyright (c) 2026 Alex Gorbatchev.
