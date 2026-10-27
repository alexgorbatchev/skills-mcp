// Package paths resolves the XDG base directories used by skills-mcp.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const appName = "skills-mcp"

// Dirs holds the application directories under each XDG base directory.
type Dirs struct {
	Config string
	Cache  string
	State  string
}

// Resolve returns the application directories, honoring XDG_CONFIG_HOME,
// XDG_CACHE_HOME, and XDG_STATE_HOME before the platform defaults.
func Resolve() (Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Dirs{}, fmt.Errorf("resolving home directory: %w", err)
	}
	config := xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cache := xdg("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	state := xdg("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	return Dirs{
		Config: filepath.Join(config, appName),
		Cache:  filepath.Join(cache, appName),
		State:  filepath.Join(state, appName),
	}, nil
}

// xdg returns the variable's value when it is an absolute path, as the XDG
// specification requires, and the fallback otherwise.
func xdg(name, fallback string) string {
	if v := os.Getenv(name); filepath.IsAbs(v) {
		return v
	}
	return fallback
}

// ConfigFile is the path of config.yaml.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.yaml") }

// TokenFile is the path of the bearer token file.
func (d Dirs) TokenFile() string { return filepath.Join(d.Config, "token") }

// LockFile is the path of the daemon lock file.
func (d Dirs) LockFile() string { return filepath.Join(d.State, "lock") }

// SourceState is the state directory of one source, holding repo.git.
func (d Dirs) SourceState(source string) string { return filepath.Join(d.State, "sources", source) }

// SourceCache is the cache directory of one source, holding snapshots.
func (d Dirs) SourceCache(source string) string { return filepath.Join(d.Cache, "sources", source) }

// Changes is the directory holding change records and worktrees.
func (d Dirs) Changes() string { return filepath.Join(d.State, "changes") }
