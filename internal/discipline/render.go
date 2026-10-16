package discipline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/diet"
)

// Render prints a report the way the GUI's Checks tab will show it: one
// group per line, the health first, issues last.
func Render(r Report) string {
	var b strings.Builder
	mark := map[string]string{"green": "●", "amber": "●", "red": "✗"}[r.Health]
	installed := r.Rev.Installed
	if r.Rev.Layout == LayoutLegacy {
		installed += " (legacy layout)"
	}
	fmt.Fprintf(&b, "Discipline  %s %s · bundle %s · installed %s · checked %s\n",
		mark, r.Health, r.Rev.Bundled, installed, r.CheckedAt.Format("15:04"))
	tool := func(name string, t Tool) string {
		if !t.Found {
			return "✗ " + name
		}
		s := "✓ " + name
		if t.Version != "" {
			s += " " + t.Version
		}
		if name == "python3" && !t.Runs {
			s = "✗ python3 (does not run)"
		}
		if t.Tested == "newer" {
			s += " (newer than tested)"
		}
		return s
	}
	fmt.Fprintf(&b, "PREREQS  %s  %s  %s  %s  %s\n", tool("python3", r.Prereq.Python3), tool("jq", r.Prereq.Jq),
		tool("git", r.Prereq.Git), tool("tmux", r.Prereq.Tmux), tool("claude", r.Prereq.Claude))
	counts := map[string]int{}
	for _, f := range r.Files {
		counts[f.State]++
	}
	fmt.Fprintf(&b, "FILES    current %d · stale %d · modified %d · missing %d · absent %d · inline %d\n",
		counts[StateCurrent], counts[StateStale], counts[StateModified], counts[StateMissing], counts[StateAbsent], counts[StateInline])
	for _, f := range r.Files {
		if f.State == StateStale || f.State == StateModified || f.State == StateMissing {
			extra := ""
			if f.Matches != "" {
				extra = " (matches " + f.Matches + ")"
			}
			fmt.Fprintf(&b, "         %-9s %s%s\n", f.State, f.InstalledPath, extra)
		}
	}
	fmt.Fprintf(&b, "HOOKS    %d/%d registrations", r.Registrations.Matched, r.Registrations.Expected)
	if len(r.Registrations.Others) > 0 {
		fmt.Fprintf(&b, " · %d other hooks seen, not managed", len(r.Registrations.Others))
	}
	b.WriteString("\n")
	sl := "✗ no statusLine"
	if r.Settings.StatusLineSet {
		sl = "✓ statusLine"
		if !r.Settings.StatusLineOurs {
			sl += " (another tool's)"
		}
	}
	dl := "✗ deadline"
	switch {
	case r.Path.Login.Deadline:
		dl = "✓ deadline on login PATH"
	case r.Path.SettingsEnv.Deadline || r.Path.Tmux.Deadline:
		dl = "◐ deadline only in settings/tmux PATH"
	}
	fmt.Fprintf(&b, "HELPERS  %s  %s\n", dl, sl)
	deny := "○ deny Claude writes to the hook dir (yours)"
	if r.Operate.DenyRule {
		deny = "✓ deny Claude writes to the hook dir"
	}
	fmt.Fprintf(&b, "OPERATE  %s\n", deny)
	rules := "✓ rules imported"
	switch {
	case !r.Rules.ClaudeMD:
		rules = "✗ CLAUDE.md missing"
	case r.Rules.ImportLines == 0 && r.Rules.Inline:
		rules = "✓ rules inline in CLAUDE.md"
	case r.Rules.ImportLines == 0:
		rules = "✗ rules not in CLAUDE.md"
	}
	fmt.Fprintf(&b, "RULES    %s\n", rules)
	if r.Firing.LogsFound {
		q := "all hooks recorded in the last 7 days"
		if len(r.Firing.Quiet) > 0 {
			q = "quiet: " + strings.Join(r.Firing.Quiet, ", ")
		}
		fmt.Fprintf(&b, "FIRING   %s (logs in %s)\n", q, r.Layout.LogDir)
	}
	if r.Suites != nil && r.Suites.Ran {
		res := "PASS"
		if !r.Suites.Passed {
			res = "FAIL"
		}
		fmt.Fprintf(&b, "SUITES   %s\n", res)
		if !r.Suites.Passed {
			b.WriteString(indent(r.Suites.Output))
		}
	}
	if r.Diet != nil {
		b.WriteString(RenderDiet(*r.Diet))
	}
	if len(r.Issues) > 0 {
		b.WriteString("\n")
		for _, is := range r.Issues {
			fmt.Fprintf(&b, "%-5s %-13s %s\n", is.Severity, is.Group, is.Message)
			if is.Fix != "" {
				fmt.Fprintf(&b, "      %-13s → %s\n", "", is.Fix)
			}
		}
	}
	return b.String()
}

// RenderDiet prints the diet group: plugins, servers, the per-project grid
// and the commands, none of which loom runs.
func RenderDiet(d diet.Report) string {
	var b strings.Builder
	enabled := 0
	for _, p := range d.PluginsUserScope {
		if p.Enabled {
			enabled++
		}
	}
	fmt.Fprintf(&b, "DIET     plugins %d enabled of %d at user scope", enabled, len(d.PluginsUserScope))
	if d.UsageSource == "" {
		b.WriteString(" · MCP usage unknown (no index)")
	} else if d.UnscannedFiles > 0 {
		fmt.Fprintf(&b, " · %d indexed files not yet scanned for MCP calls", d.UnscannedFiles)
	}
	b.WriteString("\n")
	for _, p := range d.PluginsUserScope {
		if !p.Enabled {
			continue
		}
		var loads []string
		if p.Skills > 0 {
			loads = append(loads, fmt.Sprintf("%d skills", p.Skills))
		}
		if p.Commands > 0 {
			loads = append(loads, fmt.Sprintf("%d commands", p.Commands))
		}
		if p.Agents > 0 {
			loads = append(loads, fmt.Sprintf("%d agents", p.Agents))
		}
		if p.Hooks {
			loads = append(loads, "hooks")
		}
		if len(p.MCPServers) > 0 {
			loads = append(loads, "mcp "+strings.Join(p.MCPServers, ","))
		}
		cost := ""
		if p.AlwaysOnTokens > 0 {
			cost = fmt.Sprintf(" · ~%d tok always-on", p.AlwaysOnTokens)
		}
		used := "never used"
		if p.UsageCount > 0 {
			used = fmt.Sprintf("used %d×", p.UsageCount)
		}
		fmt.Fprintf(&b, "         ✓ %-44s %s%s · %s\n", p.Key, strings.Join(loads, ", "), cost, used)
	}
	for _, s := range d.Servers {
		scope := s.Scope
		if scope == "" {
			scope = "server"
		}
		fmt.Fprintf(&b, "         %-9s %-32s %d calls in %d projects", scope, s.Label, s.Calls, s.Projects)
		if s.Command != "" {
			b.WriteString("  → " + s.Command)
		}
		b.WriteString("\n")
	}
	for _, p := range d.Projects {
		var parts []string
		for name, n := range p.Calls {
			parts = append(parts, fmt.Sprintf("%s %d", diet.Label(name), n))
		}
		sort.Strings(parts)
		line := strings.Join(parts, ", ")
		if line == "" {
			line = "no MCP calls indexed"
		}
		fmt.Fprintf(&b, "         %-28s %s", p.Name, line)
		if len(p.Disabled) > 0 {
			fmt.Fprintf(&b, " · off: %s", strings.Join(p.Disabled, ", "))
		}
		for _, repo := range p.TrackedLocalSettings {
			fmt.Fprintf(&b, "\n         %-28s ! .claude/settings.local.json is git-tracked in %s", "", repo)
		}
		b.WriteString("\n")
	}
	if d.Unattributed > 0 {
		fmt.Fprintf(&b, "         %d sessions outside the listed projects\n", d.Unattributed)
	}
	for _, w := range d.Warnings {
		fmt.Fprintf(&b, "         ! %s\n", w)
	}
	return b.String()
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "         " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
