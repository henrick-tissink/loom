// Package diet builds the context-diet report (docs/DISCIPLINE.md phase 5):
// what is loaded into every session on this machine and what each project
// actually uses, with a copyable command per row and nothing applied.
//
// It reads Claude Code's own files read-only (~/.claude.json, settings.json,
// plugins/installed_plugins.json, the plugin cache) and takes the usage
// rows loom's transcript index recorded. It never writes and never imports
// loom's store: the callers pass what they know.
package diet

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Plugin is one plugin installed at user scope and what it costs.
type Plugin struct {
	Key         string   `json:"key"` // name@marketplace, as claude plugin takes it
	Name        string   `json:"name"`
	Marketplace string   `json:"marketplace"`
	Version     string   `json:"version,omitempty"`
	Enabled     bool     `json:"enabled"`
	Skills      int      `json:"skills"`
	Commands    int      `json:"commands"`
	Agents      int      `json:"agents"`
	Hooks       bool     `json:"hooks"`
	MCPServers  []string `json:"mcpServers"`
	// AlwaysOnTokens is `claude plugin details`' "Always-on" estimate: what
	// the plugin adds to every session. 0 when not measured.
	AlwaysOnTokens int `json:"alwaysOnTokens"`
	UsageCount     int `json:"usageCount"`
	// LastUsedAt is unix seconds; 0 = never recorded.
	LastUsedAt int64  `json:"lastUsedAt"`
	Command    string `json:"command"`
}

// Server is one MCP server sessions can call, with its calls across the
// visible projects.
type Server struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Scope  string `json:"scope"` // user | project | plugin | connector | builtin
	Source string `json:"source,omitempty"`
	Calls  int    `json:"calls"`
	// Projects is how many visible projects called it.
	Projects int    `json:"projects"`
	Command  string `json:"command,omitempty"`
}

// ProjectRow is one visible project: what it called and what it disabled.
type ProjectRow struct {
	Root     string         `json:"root"`
	Name     string         `json:"name"`
	Calls    map[string]int `json:"calls"`
	Sessions int            `json:"sessions"`
	LastTS   int64          `json:"lastTS"`
	// Disabled are the servers and connectors Claude Code has off for this
	// project's directories (disabledMcpServers + disabledMcpjsonServers).
	Disabled []string `json:"disabled"`
	// LocalServers are the project-scope servers configured for its cwds.
	LocalServers []string `json:"localServers"`
	// TrackedLocalSettings lists repos whose .claude/settings.local.json is
	// committed: a per-machine file that would carry this machine's choices
	// to everyone who clones it.
	TrackedLocalSettings []string `json:"trackedLocalSettings"`
}

// Report is the diet group of the doctor's report.
type Report struct {
	PluginsUserScope []Plugin     `json:"pluginsUserScope"`
	Servers          []Server     `json:"servers"`
	Projects         []ProjectRow `json:"projects"`
	// Unattributed counts usage sessions outside every visible project.
	Unattributed int `json:"unattributed"`
	// UnscannedFiles is how many indexed files have no MCP data yet.
	UnscannedFiles int      `json:"unscannedFiles"`
	Warnings       []string `json:"warnings"`
	// UsageSource says where the grid came from: "index" or "" (no store).
	UsageSource string `json:"usageSource"`
}

// Usage is one session's MCP calls, as the transcript index recorded them.
type Usage struct {
	Cwd     string
	LastTS  int64
	Servers map[string]int
}

// Project is one visible loom project with its repo checkouts.
type Project struct {
	Root, Name string
	Repos      []string
}

// Inputs is everything Build needs. Attribute maps a cwd to a visible
// project root; nil means no projects. Run executes a command and returns
// its output; nil skips the token measurement. GitTracked reports whether
// rel is committed in repo; nil uses git.
type Inputs struct {
	ConfigDir  string
	ClaudeJSON string
	Usage      []Usage
	Projects   []Project
	Attribute  func(cwd string) (root string, ok bool)
	Unscanned  int
	HasIndex   bool
	Run        func(ctx context.Context, name string, args ...string) (string, error)
	GitTracked func(repo, rel string) bool
	Now        time.Time
}

// ClaudeJSONPath is where Claude Code keeps ~/.claude.json for a config
// dir: beside the default dir in $HOME, inside a custom CLAUDE_CONFIG_DIR.
func ClaudeJSONPath(configDir, home string) string {
	if filepath.Clean(configDir) == filepath.Join(home, ".claude") {
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(configDir, ".claude.json")
}

// Build assembles the report. It never fails: anything unreadable becomes
// a warning and an empty section.
func Build(ctx context.Context, in Inputs) Report {
	r := Report{PluginsUserScope: []Plugin{}, Servers: []Server{}, Projects: []ProjectRow{}, Warnings: []string{}, UnscannedFiles: in.Unscanned}
	if in.HasIndex {
		r.UsageSource = "index"
	}
	cj := readJSON(in.ClaudeJSON)
	if cj == nil {
		r.Warnings = append(r.Warnings, "could not read "+in.ClaudeJSON+": the per-project MCP settings and plugin usage are unknown")
	}
	settings := readJSON(filepath.Join(in.ConfigDir, "settings.json"))
	r.PluginsUserScope = plugins(ctx, in, cj, settings)
	r.Projects, r.Unattributed = projects(in, cj)
	r.Servers = servers(in, cj, r.PluginsUserScope, r.Projects)
	return r
}

// --- plugins -------------------------------------------------------------

type installed struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
}

var alwaysOnRe = regexp.MustCompile(`Always-on:\s+~?([\d.]+)(k?)\s+tok`)

func plugins(ctx context.Context, in Inputs, cj, settings map[string]any) []Plugin {
	reg := readJSON(filepath.Join(in.ConfigDir, "plugins", "installed_plugins.json"))
	enabled := map[string]bool{}
	if settings != nil {
		if m, ok := settings["enabledPlugins"].(map[string]any); ok {
			for k, v := range m {
				enabled[k], _ = v.(bool)
			}
		}
	}
	usage := map[string]any{}
	if cj != nil {
		usage, _ = cj["pluginUsage"].(map[string]any)
	}
	var out []Plugin
	if reg != nil {
		if m, ok := reg["plugins"].(map[string]any); ok {
			for key, raw := range m {
				b, _ := json.Marshal(raw)
				var entries []installed
				if json.Unmarshal(b, &entries) != nil {
					var one installed
					if json.Unmarshal(b, &one) == nil {
						entries = []installed{one}
					}
				}
				for _, e := range entries {
					if e.Scope != "user" {
						continue
					}
					p := Plugin{Key: key, Version: e.Version, Enabled: enabled[key], MCPServers: []string{}}
					if i := strings.LastIndex(key, "@"); i > 0 {
						p.Name, p.Marketplace = key[:i], key[i+1:]
					} else {
						p.Name = key
					}
					inventory(&p, e.InstallPath)
					if u, ok := usage[key].(map[string]any); ok {
						p.UsageCount = intOf(u["usageCount"])
						if ms := int64Of(u["lastUsedAt"]); ms > 0 {
							p.LastUsedAt = ms / 1000
						}
					}
					if p.Enabled {
						p.Command = "claude plugin disable " + key + " -s user"
					} else {
						p.Command = "claude plugin enable " + key + " -s user"
					}
					out = append(out, p)
					break
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Enabled != out[j].Enabled {
			return out[i].Enabled
		}
		return out[i].Key < out[j].Key
	})
	if in.Run != nil {
		measure(ctx, in.Run, out)
	}
	if out == nil {
		out = []Plugin{}
	}
	return out
}

// inventory counts what a plugin's install dir loads.
func inventory(p *Plugin, dir string) {
	if dir == "" {
		return
	}
	skills, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
	p.Skills = len(skills)
	cmds, _ := filepath.Glob(filepath.Join(dir, "commands", "*.md"))
	p.Commands = len(cmds)
	agents, _ := filepath.Glob(filepath.Join(dir, "agents", "*.md"))
	p.Agents = len(agents)
	if _, err := os.Stat(filepath.Join(dir, "hooks", "hooks.json")); err == nil {
		p.Hooks = true
	}
	if m := readJSON(filepath.Join(dir, ".mcp.json")); m != nil {
		servers, _ := m["mcpServers"].(map[string]any)
		if servers == nil {
			servers = m
		}
		for name := range servers {
			p.MCPServers = append(p.MCPServers, name)
		}
		sort.Strings(p.MCPServers)
	}
}

// measure asks `claude plugin details` for each enabled plugin's always-on
// cost, concurrently, each call on its own deadline.
func measure(ctx context.Context, run func(context.Context, string, ...string) (string, error), ps []Plugin) {
	var wg sync.WaitGroup
	for i := range ps {
		if !ps[i].Enabled {
			continue
		}
		wg.Add(1)
		go func(p *Plugin) {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			out, err := run(c, "claude", "plugin", "details", p.Key)
			if err != nil && out == "" {
				return
			}
			p.AlwaysOnTokens = parseAlwaysOn(out)
		}(&ps[i])
	}
	wg.Wait()
}

func parseAlwaysOn(out string) int {
	m := alwaysOnRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	if m[2] == "k" {
		f *= 1000
	}
	return int(f)
}

// --- projects ------------------------------------------------------------

func projects(in Inputs, cj map[string]any) ([]ProjectRow, int) {
	rows := map[string]*ProjectRow{}
	order := []string{}
	for _, p := range in.Projects {
		rows[p.Root] = &ProjectRow{Root: p.Root, Name: p.Name, Calls: map[string]int{}, Disabled: []string{}, LocalServers: []string{}, TrackedLocalSettings: []string{}}
		order = append(order, p.Root)
		for _, repo := range p.Repos {
			rel := filepath.Join(".claude", "settings.local.json")
			if _, err := os.Stat(filepath.Join(repo, rel)); err != nil {
				continue
			}
			tracked := in.GitTracked
			if tracked == nil {
				tracked = gitTracked
			}
			if tracked(repo, rel) {
				rows[p.Root].TrackedLocalSettings = append(rows[p.Root].TrackedLocalSettings, repo)
			}
		}
	}
	attr := in.Attribute
	if attr == nil {
		attr = func(string) (string, bool) { return "", false }
	}
	unattributed := 0
	for _, u := range in.Usage {
		root, ok := attr(u.Cwd)
		row := rows[root]
		if !ok || row == nil {
			unattributed++
			continue
		}
		row.Sessions++
		if u.LastTS > row.LastTS {
			row.LastTS = u.LastTS
		}
		for k, v := range u.Servers {
			row.Calls[k] += v
		}
	}
	// Per-directory MCP settings from ~/.claude.json, folded onto the
	// project each directory belongs to.
	if cj != nil {
		if pm, ok := cj["projects"].(map[string]any); ok {
			for cwd, raw := range pm {
				m, _ := raw.(map[string]any)
				if m == nil {
					continue
				}
				root, ok := attr(cwd)
				row := rows[root]
				if !ok || row == nil {
					continue
				}
				row.Disabled = appendUnique(row.Disabled, strs(m["disabledMcpServers"])...)
				row.Disabled = appendUnique(row.Disabled, strs(m["disabledMcpjsonServers"])...)
				if local, ok := m["mcpServers"].(map[string]any); ok {
					for name := range local {
						row.LocalServers = appendUnique(row.LocalServers, name)
					}
				}
			}
		}
	}
	out := make([]ProjectRow, 0, len(order))
	for _, root := range order {
		r := rows[root]
		sort.Strings(r.Disabled)
		sort.Strings(r.LocalServers)
		sort.Strings(r.TrackedLocalSettings)
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ci, cj := total(out[i].Calls), total(out[j].Calls)
		if ci != cj {
			return ci > cj
		}
		return out[i].Name < out[j].Name
	})
	return out, unattributed
}

// --- servers -------------------------------------------------------------

// servers lists every server configured or called, with its scope.
func servers(in Inputs, cj map[string]any, plugins []Plugin, rows []ProjectRow) []Server {
	by := map[string]*Server{}
	add := func(name, scope, source string) *Server {
		if s := by[name]; s != nil {
			if s.Scope == "" {
				s.Scope, s.Source = scope, source
			}
			return s
		}
		s := &Server{Name: name, Label: Label(name), Scope: scope, Source: source}
		by[name] = s
		return s
	}
	if cj != nil {
		if m, ok := cj["mcpServers"].(map[string]any); ok {
			for name := range m {
				add(name, "user", "")
			}
		}
	}
	for _, p := range plugins {
		if !p.Enabled {
			continue
		}
		for _, sv := range p.MCPServers {
			add("plugin_"+p.Name+"_"+sv, "plugin", p.Key)
		}
	}
	for _, r := range rows {
		for _, sv := range r.LocalServers {
			add(sv, "project", r.Root)
		}
		for name, n := range r.Calls {
			s := add(name, scopeOf(name), "")
			s.Calls += n
			s.Projects++
		}
	}
	out := make([]Server, 0, len(by))
	// A removal is suggested only when the index is complete and no
	// session at all called the server: the visible rows leave out hidden
	// projects and unattributed directories, but a server used there is
	// still in use. Only the gate sees those calls; the counts shown stay
	// the visible ones.
	complete := in.HasIndex && in.Unscanned == 0
	anyCalls := map[string]bool{}
	for _, u := range in.Usage {
		for name, n := range u.Servers {
			if n > 0 {
				anyCalls[name] = true
			}
		}
	}
	for _, s := range by {
		if s.Scope == "user" && complete && !anyCalls[s.Name] {
			s.Command = "claude mcp remove " + s.Name + " -s user"
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// scopeOf classifies a server by the name Claude Code gives its tools.
func scopeOf(name string) string {
	switch {
	case strings.HasPrefix(name, "claude_ai_"):
		return "connector"
	case strings.HasPrefix(name, "plugin_"):
		return "plugin"
	case name == "claude-in-chrome":
		return "builtin"
	}
	return ""
}

// Label is the human name of a server: "claude.ai Gmail" for a connector,
// "slack (plugin)" for a plugin's server, the name otherwise.
func Label(name string) string {
	switch {
	case strings.HasPrefix(name, "claude_ai_"):
		return "claude.ai " + strings.ReplaceAll(strings.TrimPrefix(name, "claude_ai_"), "_", " ")
	case strings.HasPrefix(name, "plugin_"):
		rest := strings.TrimPrefix(name, "plugin_")
		if i := strings.LastIndex(rest, "_"); i > 0 {
			return rest[i+1:] + " (plugin " + rest[:i] + ")"
		}
		return rest + " (plugin)"
	}
	return name
}

// --- helpers -------------------------------------------------------------

func gitTracked(repo, rel string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "ls-files", "--error-unmatch", rel)
	return cmd.Run() == nil
}

func readJSON(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func strs(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func appendUnique(dst []string, vals ...string) []string {
	for _, v := range vals {
		seen := false
		for _, d := range dst {
			if d == v {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, v)
		}
	}
	return dst
}

func intOf(v any) int {
	f, _ := v.(float64)
	return int(f)
}

func int64Of(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
