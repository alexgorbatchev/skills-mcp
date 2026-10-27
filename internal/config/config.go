// Package config loads and validates config.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// HarnessID names a supported agent harness.
type HarnessID string

// Supported harnesses, in a fixed order used for views and routing.
const (
	ClaudeCode HarnessID = "claude-code"
	Codex      HarnessID = "codex"
	Pi         HarnessID = "pi"
)

// Harnesses lists every supported harness.
var Harnesses = []HarnessID{ClaudeCode, Codex, Pi}

// Invocation says who invokes a skill.
type Invocation string

// Skill invocation kinds.
const (
	Model Invocation = "model"
	User  Invocation = "user"
)

// Source kinds.
const (
	KindGit  = "git"
	KindPath = "path"
)

var (
	namePattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	varNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	envNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Reserved template variables are built into the renderer.
var reservedVars = []string{"file_dir", "file_path"}

const defaultMaxResultBytes = 48000

var defaultMaxResult = map[HarnessID]int{ClaudeCode: 48000, Codex: 32000, Pi: 19000}

// Config is the parsed and validated config.yaml.
type Config struct {
	Listen             string
	FetchInterval      time.Duration
	SubmitTimeout      time.Duration
	SnapshotRetention  time.Duration
	ChangeTTL          time.Duration
	DefaultWriteSource string // empty when authoring is disabled
	Harnesses          map[HarnessID]Harness
	Sources            []*Source
}

// Harness holds per-harness settings.
type Harness struct {
	MaxResultBytes int
}

// Root is a repo-relative skill root.
type Root struct {
	Path       string     `yaml:"path"`
	Invocation Invocation `yaml:"invocation"`
}

// Template is the template rendering context of a source.
type Template struct {
	Vars map[string]string // values after "~/" expansion; "{snapshot}" left literal
	Env  []string
}

// PrepareRule installs dependencies into snapshot directories.
type PrepareRule struct {
	WhenFile string
	Run      []string
	Keep     []string
	Timeout  time.Duration
}

// Source is one skill source.
type Source struct {
	Name               string
	Kind               string
	Roots              []Root
	HarnessRoots       []Root
	Template           Template
	IgnoreFile         string
	AuthorPrefix       bool // author_prefix_except is non-null
	AuthorPrefixExcept []string
	Prepare            []PrepareRule

	// git sources
	URL          string
	Branch       string
	Writable     bool
	Validate     []string
	DenyPatterns []*regexp.Regexp

	// path sources
	Path string
}

// ExpandedRoots returns the source's roots and harness roots expanded for
// the given harness, roots first.
func (s *Source) ExpandedRoots(h HarnessID) []Root {
	roots := slices.Clone(s.Roots)
	for _, r := range s.HarnessRoots {
		roots = append(roots, Root{Path: ExpandHarness(r.Path, h), Invocation: r.Invocation})
	}
	return roots
}

// AllRootPaths returns every root path, with harness roots expanded for all
// harnesses, deduplicated and sorted.
func (s *Source) AllRootPaths() []string {
	var out []string
	for _, r := range s.Roots {
		out = append(out, r.Path)
	}
	for _, h := range Harnesses {
		for _, r := range s.HarnessRoots {
			out = append(out, ExpandHarness(r.Path, h))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ExpandHarness replaces "{harness}" in a root path.
func ExpandHarness(path string, h HarnessID) string {
	return strings.ReplaceAll(path, "{harness}", string(h))
}

// SourceByName returns the named source or nil.
func (c *Config) SourceByName(name string) *Source {
	for _, s := range c.Sources {
		if s.Name == name {
			return s
		}
	}
	return nil
}

type rawConfig struct {
	Listen             string                `yaml:"listen"`
	FetchInterval      string                `yaml:"fetch_interval"`
	SubmitTimeout      string                `yaml:"submit_timeout"`
	SnapshotRetention  string                `yaml:"snapshot_retention"`
	ChangeTTL          string                `yaml:"change_ttl"`
	DefaultWriteSource *string               `yaml:"default_write_source"`
	Harnesses          map[string]rawHarness `yaml:"harnesses"`
	Sources            []rawSource           `yaml:"sources"`
}

type rawHarness struct {
	MaxResultBytes int `yaml:"max_result_bytes"`
}

type rawTemplate struct {
	Vars map[string]string `yaml:"vars"`
	Env  []string          `yaml:"env"`
}

type rawPrepare struct {
	WhenFile string   `yaml:"when_file"`
	Run      []string `yaml:"run"`
	Keep     []string `yaml:"keep"`
	Timeout  string   `yaml:"timeout"`
}

type rawSource struct {
	Name               string       `yaml:"name"`
	Kind               string       `yaml:"kind"`
	Roots              []Root       `yaml:"roots"`
	HarnessRoots       []Root       `yaml:"harness_roots"`
	Template           rawTemplate  `yaml:"template"`
	IgnoreFile         *string      `yaml:"ignore_file"`
	AuthorPrefixExcept *[]string    `yaml:"author_prefix_except"`
	Prepare            []rawPrepare `yaml:"prepare"`
	URL                string       `yaml:"url"`
	Branch             string       `yaml:"branch"`
	Writable           bool         `yaml:"writable"`
	Validate           []string     `yaml:"validate"`
	DenyPatterns       []string     `yaml:"deny_patterns"`
	Path               string       `yaml:"path"`
}

// Load reads and validates the configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

// Parse decodes and validates configuration bytes.
func Parse(data []byte) (*Config, error) {
	var raw rawConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	b := builder{home: home}
	cfg := b.build(raw)
	if len(b.errs) > 0 {
		return nil, errors.Join(b.errs...)
	}
	return cfg, nil
}

type builder struct {
	home string
	errs []error
}

func (b *builder) fail(field, format string, args ...any) {
	b.errs = append(b.errs, fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...)))
}

func (b *builder) duration(field, value, def string, minimum time.Duration) time.Duration {
	if value == "" {
		value = def
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		b.fail(field, "invalid duration %q", value)
		return 0
	}
	if d < minimum || d <= 0 {
		if minimum > 0 {
			b.fail(field, "must be at least %s", minimum)
		} else {
			b.fail(field, "must be positive")
		}
	}
	return d
}

func (b *builder) build(raw rawConfig) *Config {
	cfg := &Config{
		Listen:            raw.Listen,
		FetchInterval:     b.duration("fetch_interval", raw.FetchInterval, "60s", 0),
		SubmitTimeout:     b.duration("submit_timeout", raw.SubmitTimeout, "600s", time.Minute),
		SnapshotRetention: b.duration("snapshot_retention", raw.SnapshotRetention, "24h", 0),
		ChangeTTL:         b.duration("change_ttl", raw.ChangeTTL, "168h", 0),
		Harnesses:         map[HarnessID]Harness{},
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:7341"
	}
	b.checkListen(cfg.Listen)
	b.buildHarnesses(cfg, raw.Harnesses)
	names := map[string]bool{}
	for i, rs := range raw.Sources {
		s := b.buildSource(fmt.Sprintf("sources[%d]", i), rs)
		if names[s.Name] {
			b.fail(fmt.Sprintf("sources[%d].name", i), "duplicate source name %q", s.Name)
		}
		names[s.Name] = true
		cfg.Sources = append(cfg.Sources, s)
	}
	if raw.DefaultWriteSource != nil {
		cfg.DefaultWriteSource = *raw.DefaultWriteSource
		s := cfg.SourceByName(cfg.DefaultWriteSource)
		if s == nil || s.Kind != KindGit || !s.Writable {
			b.fail("default_write_source", "%q is not a writable git source", cfg.DefaultWriteSource)
		}
	}
	return cfg
}

func (b *builder) checkListen(listen string) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		b.fail("listen", "invalid address %q", listen)
		return
	}
	if host != "127.0.0.1" {
		b.fail("listen", "host must be 127.0.0.1, got %q", host)
	}
}

func (b *builder) buildHarnesses(cfg *Config, raw map[string]rawHarness) {
	for name := range raw {
		if !slices.Contains(Harnesses, HarnessID(name)) {
			b.fail("harnesses."+name, "unknown harness")
		}
	}
	for _, h := range Harnesses {
		max := defaultMaxResult[h]
		if r, ok := raw[string(h)]; ok && r.MaxResultBytes != 0 {
			max = r.MaxResultBytes
		}
		if max <= 0 {
			b.fail("harnesses."+string(h)+".max_result_bytes", "must be positive")
			max = defaultMaxResultBytes
		}
		cfg.Harnesses[h] = Harness{MaxResultBytes: max}
	}
}

func (b *builder) buildSource(field string, rs rawSource) *Source {
	s := &Source{
		Name:         rs.Name,
		Kind:         rs.Kind,
		Roots:        rs.Roots,
		HarnessRoots: rs.HarnessRoots,
		URL:          rs.URL,
		Branch:       rs.Branch,
		Writable:     rs.Writable,
		Validate:     rs.Validate,
	}
	if !namePattern.MatchString(s.Name) {
		b.fail(field+".name", "invalid source name %q", s.Name)
	}
	if rs.IgnoreFile != nil {
		s.IgnoreFile = *rs.IgnoreFile
	}
	if rs.AuthorPrefixExcept != nil {
		s.AuthorPrefix = true
		s.AuthorPrefixExcept = *rs.AuthorPrefixExcept
	}
	b.checkRoots(field+".roots", s.Roots)
	b.checkRoots(field+".harness_roots", s.HarnessRoots)
	s.Template = b.buildTemplate(field+".template", rs.Template)
	s.Prepare = b.buildPrepare(field+".prepare", rs.Prepare)
	switch s.Kind {
	case KindGit:
		if s.URL == "" {
			b.fail(field+".url", "required for git sources")
		}
		if s.Branch == "" {
			b.fail(field+".branch", "required for git sources")
		}
		for j, p := range rs.DenyPatterns {
			re, err := regexp.Compile(p)
			if err != nil {
				b.fail(fmt.Sprintf("%s.deny_patterns[%d]", field, j), "invalid pattern: %v", err)
				continue
			}
			s.DenyPatterns = append(s.DenyPatterns, re)
		}
		if s.Writable && len(rs.DenyPatterns) == 0 {
			b.fail(field+".deny_patterns", "must be non-empty for a writable source")
		}
	case KindPath:
		if rs.Path == "" {
			b.fail(field+".path", "required for path sources")
		}
		s.Path = b.expandHome(rs.Path)
		if s.Path != "" && !filepath.IsAbs(s.Path) {
			b.fail(field+".path", "must be absolute or ~-prefixed")
		}
		if s.Writable {
			b.fail(field+".writable", "path sources are read-only")
		}
	default:
		b.fail(field+".kind", "must be %q or %q", KindGit, KindPath)
	}
	return s
}

func (b *builder) checkRoots(field string, roots []Root) {
	for i, r := range roots {
		f := fmt.Sprintf("%s[%d]", field, i)
		clean := path.Clean(r.Path)
		switch {
		case r.Path == "" || filepath.IsAbs(r.Path) || clean == "." || strings.HasPrefix(clean, "../") || clean == "..":
			b.fail(f+".path", "must be a repo-relative path inside the repository")
		case clean != r.Path:
			b.fail(f+".path", "must be written in clean form, e.g. %q", clean)
		}
		if r.Invocation != Model && r.Invocation != User {
			b.fail(f+".invocation", "must be %q or %q", Model, User)
		}
	}
}

func (b *builder) buildTemplate(field string, rt rawTemplate) Template {
	t := Template{Vars: map[string]string{"repo_root": "{snapshot}"}, Env: rt.Env}
	for name, value := range rt.Vars {
		if !varNamePattern.MatchString(name) {
			b.fail(field+".vars."+name, "invalid variable name")
		}
		if slices.Contains(reservedVars, name) {
			b.fail(field+".vars."+name, "reserved variable name")
		}
		t.Vars[name] = b.expandHome(value)
	}
	for _, name := range rt.Env {
		if !envNamePattern.MatchString(name) {
			b.fail(field+".env", "invalid environment variable name %q", name)
		}
	}
	return t
}

func (b *builder) buildPrepare(field string, raw []rawPrepare) []PrepareRule {
	var rules []PrepareRule
	for i, rp := range raw {
		f := fmt.Sprintf("%s[%d]", field, i)
		if rp.WhenFile == "" || strings.ContainsRune(rp.WhenFile, '/') {
			b.fail(f+".when_file", "must be a file name")
		}
		if len(rp.Run) == 0 {
			b.fail(f+".run", "must not be empty")
		}
		rules = append(rules, PrepareRule{
			WhenFile: rp.WhenFile,
			Run:      rp.Run,
			Keep:     rp.Keep,
			Timeout:  b.duration(f+".timeout", rp.Timeout, "300s", 0),
		})
	}
	return rules
}

func (b *builder) expandHome(p string) string {
	if p == "~" {
		return b.home
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(b.home, rest)
	}
	return p
}
