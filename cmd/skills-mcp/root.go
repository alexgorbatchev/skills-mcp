package main

import (
	"github.com/spf13/cobra"

	"github.com/alexgorbatchev/skills-mcp/internal/paths"
)

// Injected at build time with -ldflags "-X main.version=<version>".
var version = "dev"

func newRootCommand() *cobra.Command {
	var configPath string
	rootCmd := &cobra.Command{
		Use:          "skills-mcp",
		Short:        "Serve agent skills from a Git repository as MCP tools and prompts",
		Long:         "skills-mcp serves agent skills from Git repositories to Claude Code, Codex, and pi as MCP tools and prompts, and lets agents push skill changes back.",
		Version:      version,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	rootCmd.SetVersionTemplate("{{.Version}}\n")
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "config file (default $XDG_CONFIG_HOME/skills-mcp/config.yaml)")

	resolve := func() (paths.Dirs, string, error) {
		dirs, err := paths.Resolve()
		if err != nil {
			return dirs, "", err
		}
		if configPath == "" {
			return dirs, dirs.ConfigFile(), nil
		}
		return dirs, configPath, nil
	}
	rootCmd.AddCommand(
		newServerCommand(resolve),
		newCatalogCommand(resolve),
		newBundleCommand(resolve),
		newTokenCommand(resolve),
		newSkillCommand(),
	)
	setupHelp(rootCmd)
	return rootCmd
}

// resolver returns the application directories and the config file path.
type resolver func() (paths.Dirs, string, error)
