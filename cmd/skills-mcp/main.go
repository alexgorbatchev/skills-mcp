package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/alexgorbatchev/skills-mcp/internal/agent"
)

func main() {
	os.Exit(run(newRootCommand()))
}

func run(cmd *cobra.Command) int {
	if err := cmd.Execute(); err != nil {
		if agent.IsAgentMode() {
			fmt.Fprintf(cmd.ErrOrStderr(), "ERR: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "[ERROR] %v\n", err)
		}
		return 1
	}
	return 0
}
