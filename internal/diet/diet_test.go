package diet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// layout builds a config dir with two user-scope plugins (one enabled, with
// a skill, a command and an MCP server; one disabled), one project-scope
// plugin that must not be listed, and a ~/.claude.json with a user server,
// per-directory settings and plugin usage.
func layout(t *testing.T) (ccd, cj string) {
	t.Helper()
	home := t.TempDir()
	ccd = filepath.Join(home, ".claude")
	cj = ClaudeJSONPath(ccd, home)
	notion := filepath.Join(ccd, "plugins", "cache", "mk", "notion", "1.0")
	write(t, filepath.Join(notion, "skills", "find", "SKILL.md"), "# find")
	write(t, filepath.Join(notion, "commands", "search.md"), "# search")
	write(t, filepath.Join(notion, ".mcp.json"), `{"mcpServers": {"notion": {"type": "http"}}}`)
	write(t, filepath.Join(ccd, "plugins", "installed_plugins.json"), `{"version": 2, "plugins": {
		"notion@mk": [{"scope": "user", "installPath": "`+notion+`", "version": "1.0"}],
		"slack@mk": [{"scope": "user", "installPath": "/nowhere", "version": "2.0"}],
		"local@mk": [{"scope": "project", "installPath": "/nowhere", "projectPath": "/w/a"}]}}`)
	write(t, filepath.Join(ccd, "settings.json"), `{"enabledPlugins": {"notion@mk": true, "slack@mk": false}}`)
	write(t, cj, `{"mcpServers": {"lava": {}}, "pluginUsage": {"notion@mk": {"usageCount": 3, "lastUsedAt": 1790000000000}},
		"projects": {"/w/a": {"disabledMcpServers": ["claude.ai Didit"], "disabledMcpjsonServers": ["hoard"], "mcpServers": {"hoard": {}}},
		             "/w/a/sub": {"disabledMcpServers": ["claude.ai Didit"]},
		             "/w/hidden": {"disabledMcpServers": ["claude.ai Gmail"]}}}`)
	return ccd, cj
}

func inputs(t *testing.T) Inputs {
	ccd, cj := layout(t)
	repo := t.TempDir()
	return Inputs{
		ConfigDir: ccd, ClaudeJSON: cj, Now: time.Unix(1790100000, 0), HasIndex: true,
		Usage: []Usage{
			{Cwd: "/w/a", LastTS: 100, Servers: map[string]int{"lava": 2, "claude_ai_Gmail": 1}},
			{Cwd: "/w/a/sub", LastTS: 200, Servers: map[string]int{"plugin_notion_notion": 4}},
			{Cwd: "/w/hidden", LastTS: 300, Servers: map[string]int{"lava": 9}},
		},
		Projects:   []Project{{Root: "/w/a", Name: "A", Repos: []string{repo}}},
		Attribute:  func(cwd string) (string, bool) { return "/w/a", cwd == "/w/a" || cwd == "/w/a/sub" },
		GitTracked: func(repo, rel string) bool { return true },
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			return "Projected token cost\n  Always-on:   ~1.2k tok   added to every session\n", nil
		},
	}
}

func TestBuildPluginsProjectsAndServers(t *testing.T) {
	in := inputs(t)
	write(t, filepath.Join(in.Projects[0].Repos[0], ".claude", "settings.local.json"), "{}")
	r := Build(context.Background(), in)

	if len(r.PluginsUserScope) != 2 || !r.PluginsUserScope[0].Enabled || r.PluginsUserScope[0].Key != "notion@mk" {
		t.Fatalf("plugins: %+v", r.PluginsUserScope)
	}
	p := r.PluginsUserScope[0]
	if p.Skills != 1 || p.Commands != 1 || p.Agents != 0 || len(p.MCPServers) != 1 || p.MCPServers[0] != "notion" {
		t.Fatalf("inventory: %+v", p)
	}
	if p.AlwaysOnTokens != 1200 || p.UsageCount != 3 || p.LastUsedAt != 1790000000 {
		t.Fatalf("measure/usage: %+v", p)
	}
	if p.Command != "claude plugin disable notion@mk -s user" || r.PluginsUserScope[1].Command != "claude plugin enable slack@mk -s user" {
		t.Fatalf("commands: %q %q", p.Command, r.PluginsUserScope[1].Command)
	}
	if r.PluginsUserScope[1].AlwaysOnTokens != 0 {
		t.Fatal("a disabled plugin is not measured")
	}

	if len(r.Projects) != 1 || r.Unattributed != 1 {
		t.Fatalf("projects %+v unattributed %d", r.Projects, r.Unattributed)
	}
	a := r.Projects[0]
	if a.Sessions != 2 || a.LastTS != 200 || a.Calls["lava"] != 2 || a.Calls["plugin_notion_notion"] != 4 || a.Calls["claude_ai_Gmail"] != 1 {
		t.Fatalf("row: %+v", a)
	}
	if len(a.Disabled) != 2 || a.Disabled[0] != "claude.ai Didit" || a.Disabled[1] != "hoard" || len(a.LocalServers) != 1 || a.LocalServers[0] != "hoard" {
		t.Fatalf("disabled/local: %+v", a)
	}
	if len(a.TrackedLocalSettings) != 1 {
		t.Fatalf("tracked: %+v", a.TrackedLocalSettings)
	}

	by := map[string]Server{}
	for _, s := range r.Servers {
		by[s.Name] = s
	}
	if by["lava"].Scope != "user" || by["lava"].Calls != 2 || by["lava"].Projects != 1 || by["lava"].Command != "" {
		t.Fatalf("lava: %+v", by["lava"])
	}
	if by["claude_ai_Gmail"].Scope != "connector" || by["claude_ai_Gmail"].Label != "claude.ai Gmail" {
		t.Fatalf("gmail: %+v", by["claude_ai_Gmail"])
	}
	if by["plugin_notion_notion"].Scope != "plugin" || by["plugin_notion_notion"].Source != "notion@mk" || by["plugin_notion_notion"].Label != "notion (plugin notion)" {
		t.Fatalf("plugin server: %+v", by["plugin_notion_notion"])
	}
	if by["hoard"].Scope != "project" || by["hoard"].Source != "/w/a" {
		t.Fatalf("hoard: %+v", by["hoard"])
	}
	if r.Servers[0].Name != "plugin_notion_notion" {
		t.Fatalf("sorted by calls: %+v", r.Servers)
	}
	if b, _ := json.Marshal(r); !json.Valid(b) {
		t.Fatal("json")
	}
}

func TestRemovalSuggestedOnlyWhenIndexComplete(t *testing.T) {
	in := inputs(t)
	in.Usage = nil
	in.Unscanned = 5
	r := Build(context.Background(), in)
	for _, s := range r.Servers {
		if s.Command != "" {
			t.Fatalf("half-scanned index must not suggest removal: %+v", s)
		}
	}
	in.Unscanned = 0
	r = Build(context.Background(), in)
	found := false
	for _, s := range r.Servers {
		if s.Name == "lava" && s.Command == "claude mcp remove lava -s user" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unused user server gets the removal command: %+v", r.Servers)
	}
	// A call from a hidden project or an unattributed directory is still a
	// call: the command is withheld, while the visible count stays 0.
	in.Usage = []Usage{{Cwd: "/w/hidden", Servers: map[string]int{"lava": 1}}}
	r = Build(context.Background(), in)
	for _, s := range r.Servers {
		if s.Name == "lava" && (s.Command != "" || s.Calls != 0) {
			t.Fatalf("hidden use: %+v", s)
		}
	}
	in.Usage = nil
	in.HasIndex = false
	if r = Build(context.Background(), in); r.UsageSource != "" || r.Servers[0].Command != "" {
		t.Fatalf("no index: %+v", r.Servers)
	}
}

func TestMissingFilesBecomeWarnings(t *testing.T) {
	r := Build(context.Background(), Inputs{ConfigDir: t.TempDir(), ClaudeJSON: "/nonexistent/.claude.json"})
	if len(r.Warnings) != 1 || len(r.PluginsUserScope) != 0 || len(r.Projects) != 0 || len(r.Servers) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestClaudeJSONPathAndParse(t *testing.T) {
	if p := ClaudeJSONPath("/h/.claude", "/h"); p != "/h/.claude.json" {
		t.Fatal(p)
	}
	if p := ClaudeJSONPath("/elsewhere/cfg", "/h"); p != "/elsewhere/cfg/.claude.json" {
		t.Fatal(p)
	}
	if n := parseAlwaysOn("  Always-on:   ~177 tok   added"); n != 177 {
		t.Fatal(n)
	}
	if n := parseAlwaysOn("nothing"); n != 0 {
		t.Fatal(n)
	}
	if Label("claude_ai_Claude_Docs") != "claude.ai Claude Docs" || Label("claude-in-chrome") != "claude-in-chrome" {
		t.Fatal("labels")
	}
}
