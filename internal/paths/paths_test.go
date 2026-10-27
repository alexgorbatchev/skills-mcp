package paths

import (
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	home := t.TempDir()
	tests := []struct {
		name                 string
		config, cache, state string
		want                 Dirs
	}{
		{
			name:   "xdg variables",
			config: "/x/config", cache: "/x/cache", state: "/x/state",
			want: Dirs{Config: "/x/config/skills-mcp", Cache: "/x/cache/skills-mcp", State: "/x/state/skills-mcp"},
		},
		{
			name: "defaults when unset",
			want: Dirs{
				Config: filepath.Join(home, ".config", "skills-mcp"),
				Cache:  filepath.Join(home, ".cache", "skills-mcp"),
				State:  filepath.Join(home, ".local", "state", "skills-mcp"),
			},
		},
		{
			name:   "relative values ignored",
			config: "rel", cache: "rel", state: "rel",
			want: Dirs{
				Config: filepath.Join(home, ".config", "skills-mcp"),
				Cache:  filepath.Join(home, ".cache", "skills-mcp"),
				State:  filepath.Join(home, ".local", "state", "skills-mcp"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", tt.config)
			t.Setenv("XDG_CACHE_HOME", tt.cache)
			t.Setenv("XDG_STATE_HOME", tt.state)
			got, err := Resolve()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("Resolve() = %+v, want %+v", got, tt.want)
			}
			checks := map[string]string{
				got.ConfigFile():     filepath.Join(tt.want.Config, "config.yaml"),
				got.TokenFile():      filepath.Join(tt.want.Config, "token"),
				got.LockFile():       filepath.Join(tt.want.State, "lock"),
				got.SourceState("a"): filepath.Join(tt.want.State, "sources", "a"),
				got.SourceCache("a"): filepath.Join(tt.want.Cache, "sources", "a"),
				got.Changes():        filepath.Join(tt.want.State, "changes"),
			}
			for gotPath, wantPath := range checks {
				if gotPath != wantPath {
					t.Errorf("path = %q, want %q", gotPath, wantPath)
				}
			}
		})
	}
}

func TestResolveWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := Resolve(); err == nil {
		t.Fatal("Resolve() without HOME succeeded")
	}
}
