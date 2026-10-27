package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/alexgorbatchev/skills-mcp/internal/agent"
	"github.com/alexgorbatchev/skills-mcp/internal/homepath"
)

func main() {
	os.Exit(run(newRootCommand()))
}

func run(cmd *cobra.Command) int {
	err := cmd.Execute()
	if err == nil {
		return 0
	}
	if !errors.Is(err, errReported) {
		msg := homepath.FoldText(err.Error())
		if agent.IsAgentMode() {
			fmt.Fprintf(cmd.ErrOrStderr(), "ERR: %s\n", msg)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "[ERROR] %s\n", msg)
		}
	}
	return 1
}
