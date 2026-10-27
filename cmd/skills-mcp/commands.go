package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	cobrahelptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	"github.com/spf13/cobra"

	"github.com/alexgorbatchev/skills-mcp/internal/agent"
	"github.com/alexgorbatchev/skills-mcp/internal/catalog"
	"github.com/alexgorbatchev/skills-mcp/internal/config"
	"github.com/alexgorbatchev/skills-mcp/internal/daemon"
	"github.com/alexgorbatchev/skills-mcp/internal/homepath"
	"github.com/alexgorbatchev/skills-mcp/internal/source"
	"github.com/alexgorbatchev/skills-mcp/internal/token"
)

// errReported marks a failure whose details were already printed.
var errReported = errors.New("problems found")

func newServerCommand(resolve resolver) *cobra.Command {
	server := &cobra.Command{Use: "server", Short: "Run the skills-mcp daemon", Args: cobra.NoArgs}
	server.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground until stopped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, cfgPath, err := resolve()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{ReplaceAttr: foldAttr}))
			return daemon.Run(ctx, daemon.Options{ConfigPath: cfgPath, Dirs: dirs, Logger: log, Version: version})
		},
	})
	return server
}

// foldAttr shows the home directory as "~" in every logged string.
func foldAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindString {
		a.Value = slog.StringValue(homepath.FoldText(a.Value.String()))
	}
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(homepath.FoldText(err.Error()))
		}
	}
	return a
}

func newCatalogCommand(resolve resolver) *cobra.Command {
	cat := &cobra.Command{Use: "catalog", Short: "Inspect the skills the daemon serves", Args: cobra.NoArgs}
	var harness string
	list := &cobra.Command{
		Use:   "list",
		Short: "List served skills per harness and the skills that were refused",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, cfgPath, err := resolve()
			if err != nil {
				return err
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			harnesses := config.Harnesses
			if harness != "" {
				if !containsHarness(harness) {
					return fmt.Errorf("unknown harness %q; use claude-code, codex, or pi", harness)
				}
				harnesses = []config.HarnessID{config.HarnessID(harness)}
			}
			m := source.NewManager(source.Options{Config: cfg, Dirs: dirs})
			if err := m.LoadIndexes(); err != nil {
				return err
			}
			snaps, err := m.CurrentSnapshots()
			if err != nil {
				return err
			}
			if len(snaps) == 0 {
				return errors.New("no snapshots yet; run `skills-mcp server run` first")
			}
			views, err := m.Cache().Views(snaps)
			if err != nil {
				return err
			}
			return printCatalog(cmd.OutOrStdout(), cmd.ErrOrStderr(), harnesses, views)
		},
	}
	list.Flags().StringVar(&harness, "harness", "", "only list this harness: claude-code, codex, or pi")
	cat.AddCommand(list)
	return cat
}

func containsHarness(h string) bool {
	for _, id := range config.Harnesses {
		if string(id) == h {
			return true
		}
	}
	return false
}

func printCatalog(out, errOut io.Writer, harnesses []config.HarnessID, views map[config.HarnessID]*catalog.View) error {
	diags := map[string]catalog.Diagnostic{}
	var b strings.Builder
	width := cobrahelptree.GetTerminalWidth()
	for _, h := range harnesses {
		v := views[h]
		if agent.IsAgentMode() {
			for _, s := range v.Skills {
				fmt.Fprintf(&b, "harness=%s name=%s invocation=%s source=%s revision=%s description=%s\n",
					h, s.Name, s.Invocation, s.Source, s.Revision, oneLine(s.Description))
			}
		} else {
			fmt.Fprintf(&b, "%s\n", h)
			for i, s := range v.Skills {
				branch := "├─ "
				if i == len(v.Skills)-1 {
					branch = "╰─ "
				}
				label := branch + s.Name
				if s.Invocation == config.User {
					label += " (prompt)"
				}
				fmt.Fprintf(&b, "%s\n", clip(label+"  "+oneLine(s.Description), width))
			}
		}
		for _, d := range v.Diagnostics {
			diags[d.Code+d.String()] = d
		}
	}
	if _, err := io.WriteString(out, b.String()); err != nil {
		return err
	}
	if len(diags) == 0 {
		return nil
	}
	var sorted []catalog.Diagnostic
	for _, d := range diags {
		sorted = append(sorted, d)
	}
	catalog.SortDiagnostics(sorted)
	for _, d := range sorted {
		prefix := "[WARN] refused: "
		if agent.IsAgentMode() {
			prefix = "ERR: refused code=" + d.Code + " "
		}
		fmt.Fprintln(errOut, prefix+homepath.FoldText(d.String()))
	}
	return errReported
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip truncates a line to the terminal width with a trailing ellipsis.
func clip(line string, width int) string {
	runes := []rune(line)
	if width <= 3 || len(runes) <= width {
		return line
	}
	return string(runes[:width-3]) + "..."
}

func newBundleCommand(resolve resolver) *cobra.Command {
	bundle := &cobra.Command{Use: "bundle", Short: "Check skill bundles before pushing them", Args: cobra.NoArgs}
	var sourceName string
	validate := &cobra.Command{
		Use:   "validate <bundle-dir>...",
		Short: "Check skill bundle directories against a source's rules",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfgPath, err := resolve()
			if err != nil {
				return err
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			name := sourceName
			if name == "" {
				name = cfg.DefaultWriteSource
			}
			src := cfg.SourceByName(name)
			if src == nil {
				return fmt.Errorf("no source %q; pass --source or set default_write_source", name)
			}
			failed := false
			for _, dir := range args {
				problems, err := catalog.ValidateDir(dir, src, os.LookupEnv)
				if err != nil {
					return err
				}
				shown := homepath.Fold(dir)
				if len(problems) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), okPrefix()+shown)
					continue
				}
				failed = true
				for _, p := range problems {
					fmt.Fprintln(cmd.ErrOrStderr(), errPrefix()+shown+": "+p)
				}
			}
			if failed {
				return errReported
			}
			return nil
		},
	}
	validate.Flags().StringVar(&sourceName, "source", "", "source whose rules apply (default: default_write_source)")
	bundle.AddCommand(validate)
	return bundle
}

func okPrefix() string {
	if agent.IsAgentMode() {
		return "OK: "
	}
	return "[OK] "
}

func errPrefix() string {
	if agent.IsAgentMode() {
		return "ERR: "
	}
	return "[ERROR] "
}

func newTokenCommand(resolve resolver) *cobra.Command {
	tok := &cobra.Command{Use: "token", Short: "Manage the bearer token clients send", Args: cobra.NoArgs}
	tok.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the bearer token, creating it when absent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, _, err := resolve()
			if err != nil {
				return err
			}
			t, err := token.Ensure(dirs.TokenFile())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), t)
			return err
		},
	})
	return tok
}
