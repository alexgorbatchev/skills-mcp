package main

import (
	"fmt"

	cobrahelptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	"github.com/spf13/cobra"
	"github.com/alexgorbatchev/skills-mcp/internal/agent"
)

var techCatalog = cobrahelptree.TechCatalog{
	"skills-mcp": {
		Summary:     "MCP server that serves agent skills from a Git repository as tools and prompts",
		Description: "MCP server that serves agent skills from a Git repository as tools and prompts - command-line interface.",
	},
	"skills-mcp status": {
		Summary:     "Display system and environment status",
		Description: "Outputs current runtime readiness and version information.",
	},
}

func setupHelp(cmd *cobra.Command) {
	_ = cobrahelptree.SetupWithOptions(cmd, cobrahelptree.HelpOptions{
		Catalog: techCatalog,
	})
	help := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		if agent.IsAgentMode() {
			if _, err := fmt.Fprintln(c.OutOrStdout(), "ALERT: Agents must read `AGENT=1 skills-mcp skill` before using this tool."); err != nil {
				c.PrintErrln(err)
				return
			}
		}
		help(c, args)
	})
}
