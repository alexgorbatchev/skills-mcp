package main

import (
	cobrahelptree "github.com/alexgorbatchev/cobra-help-tree/v4"
	"github.com/spf13/cobra"
)

var techCatalog = cobrahelptree.TechCatalog{
	"skills-mcp": {
		Summary:     "Serve agent skills from a Git repository as MCP tools and prompts",
		Description: "Runs a loopback MCP daemon serving skills to Claude Code, Codex, and pi from Git or local sources.",
	},
	"skills-mcp server run": {
		Summary:     "Run the daemon in the foreground until stopped",
		Description: "Loads config.yaml, takes the daemon lock, refreshes sources, and serves /mcp/<harness> on the configured loopback address until SIGTERM or SIGINT.",
		Metadata:    map[string]string{"stops_on": "SIGTERM, SIGINT"},
	},
	"skills-mcp catalog list": {
		Summary:     "List served skills per harness and the skills that were refused",
		Description: "Builds the catalog from the cached snapshots without contacting remotes; refused skills go to stderr and make the exit status 1.",
	},
	"skills-mcp bundle validate": {
		Summary:     "Check skill bundle directories against a source's rules",
		Description: "Applies the structural, symlink, and template rules of --source (default: default_write_source) to each directory.",
		Args:        []cobrahelptree.ArgSpec{{Name: "<bundle-dir>...", Description: "Skill bundle directories containing SKILL.md"}},
	},
	"skills-mcp token show": {
		Summary:     "Print the bearer token, creating it when absent",
		Description: "Prints the 64-character token from $XDG_CONFIG_HOME/skills-mcp/token, creating the file with mode 0600 when absent.",
		MutatesDB:   true,
	},
	"skills-mcp skill": {
		Summary:     "Print how to install the skills-mcp agent skill",
		Description: "Prints the Skills CLI install command and the skill URL.",
	},
}

func setupHelp(cmd *cobra.Command) {
	_ = cobrahelptree.SetupWithOptions(cmd, cobrahelptree.HelpOptions{ // the static options always validate
		Catalog: techCatalog,
		Tree:    cobrahelptree.TreeOptions{HideGeneratedCommands: true},
		Skill:   cobrahelptree.SkillSource{Repository: "alexgorbatchev/skills-mcp", Branch: "main"},
	})
}
