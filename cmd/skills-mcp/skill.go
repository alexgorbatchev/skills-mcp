package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

const skillText = `Install the skills-mcp skill using the Skills CLI:

  npx skills add alexgorbatchev/skills-mcp --skill skills-mcp

Skill:

  https://github.com/alexgorbatchev/skills-mcp/tree/main/skills/skills-mcp
`

func newSkillCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "skill",
		Short: "Print how to install the skills-mcp agent skill",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := io.WriteString(cmd.OutOrStdout(), skillText); err != nil {
				return fmt.Errorf("printing skill: %w", err)
			}
			return nil
		},
	}
}
