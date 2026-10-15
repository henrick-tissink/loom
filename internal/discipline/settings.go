package discipline

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/henricktissink/loom/internal/jsonedit"
)

// The settings.json merge. Loom touches exactly four things in that file:
// its own hook registrations (matched by event + matcher + normalised
// script path), the statusLine (only when none is set), the optional env
// keys and the hook-directory deny entries (both opt-in). Everything else,
// including keys loom has never heard of, is left byte-for-byte alone.

// HookChange is what MergeHooks did or found for one registration.
type HookChange struct {
	ID     string `json:"id"`
	Action string `json:"action"` // added | fixed | present | duplicate
	Detail string `json:"detail,omitempty"`
}

// HookID identifies a registration independent of path spelling.
func HookID(event, matcher, script string) string {
	return event + "|" + normMatcher(matcher) + "|" + filepath.Base(script)
}

// hookCommand is the command loom writes for a registration: the bare
// script path under the hooks dir, which stays valid across upgrades
// because hooks/enforce is a symlink to the current revision.
func (l Layout) hookCommand(reg Registration) string {
	return filepath.Join(l.HooksDir(), filepath.Base(reg.Script))
}

// matches reports whether a settings hook entry is this registration.
func (l Layout) matches(entryCmd string, reg Registration) bool {
	return l.NormalizeCommand(entryCmd) == l.hookCommand(reg)
}

// MergeHooks ensures every bundle registration is present once with the
// bundle's timeout and onFailure. Duplicates are reported, never removed.
func MergeHooks(doc *jsonedit.Node, l Layout, regs Registrations) []HookChange {
	var out []HookChange
	hooks := doc.Ensure("hooks")
	for _, reg := range regs.Hooks {
		id := HookID(reg.Event, reg.Matcher, reg.Script)
		var found []*jsonedit.Node
		groups := hooks.Get(reg.Event)
		if groups != nil {
			for _, g := range groups.Items {
				if normMatcher(g.Get("matcher").Str()) != normMatcher(reg.Matcher) {
					continue
				}
				if hs := g.Get("hooks"); hs != nil {
					for _, h := range hs.Items {
						if l.matches(h.Get("command").Str(), reg) {
							found = append(found, h)
						}
					}
				}
			}
		}
		switch {
		case len(found) > 1:
			out = append(out, HookChange{ID: id, Action: "duplicate", Detail: "registered " + strconv.Itoa(len(found)) + " times; Claude Code runs every copy"})
		case len(found) == 1:
			h := found[0]
			var fixes []string
			if h.Get("timeout").IntVal() != reg.Timeout {
				h.Set("timeout", jsonedit.Int(reg.Timeout))
				fixes = append(fixes, "timeout")
			}
			if h.Get("onFailure").Str() != reg.OnFailure {
				if reg.OnFailure == "" {
					h.Delete("onFailure")
				} else {
					h.Set("onFailure", jsonedit.String(reg.OnFailure))
				}
				fixes = append(fixes, "onFailure")
			}
			if len(fixes) > 0 {
				out = append(out, HookChange{ID: id, Action: "fixed", Detail: strings.Join(fixes, ", ")})
			} else {
				out = append(out, HookChange{ID: id, Action: "present"})
			}
		default:
			entry := jsonedit.NewObject()
			entry.Set("type", jsonedit.String("command"))
			entry.Set("command", jsonedit.String(l.hookCommand(reg)))
			entry.Set("timeout", jsonedit.Int(reg.Timeout))
			if reg.OnFailure != "" {
				entry.Set("onFailure", jsonedit.String(reg.OnFailure))
			}
			group := jsonedit.NewObject()
			if reg.Matcher != "" {
				group.Set("matcher", jsonedit.String(reg.Matcher))
			}
			arr := jsonedit.NewArray()
			arr.Append(entry)
			group.Set("hooks", arr)
			hooks.EnsureArray(reg.Event).Append(group)
			out = append(out, HookChange{ID: id, Action: "added"})
		}
	}
	return out
}

// UnmergeHooks removes the registrations whose IDs are listed (the ones
// the receipt says loom added), then drops emptied groups and events.
func UnmergeHooks(doc *jsonedit.Node, l Layout, regs Registrations, ids []string) int {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	hooks := doc.Get("hooks")
	if hooks == nil {
		return 0
	}
	removed := 0
	for _, reg := range regs.Hooks {
		if !want[HookID(reg.Event, reg.Matcher, reg.Script)] {
			continue
		}
		groups := hooks.Get(reg.Event)
		if groups == nil {
			continue
		}
		for _, g := range groups.Items {
			if normMatcher(g.Get("matcher").Str()) != normMatcher(reg.Matcher) {
				continue
			}
			if hs := g.Get("hooks"); hs != nil {
				removed += hs.Filter(func(h *jsonedit.Node) bool { return !l.matches(h.Get("command").Str(), reg) })
			}
		}
		groups.Filter(func(g *jsonedit.Node) bool { return g.Get("hooks").Len() > 0 })
		if groups.Len() == 0 {
			hooks.Delete(reg.Event)
		}
	}
	if hooks.Len() == 0 {
		doc.Delete("hooks")
	}
	return removed
}

// StatusLineCommand is the bundle's status line for this layout.
func (l Layout) StatusLineCommand(regs Registrations) string {
	return strings.ReplaceAll(regs.StatusLine.Command, "<ccd>", l.ConfigDir)
}

// MergeStatusLine sets the status line only when none is set. Returns the
// command written, or "" when nothing changed.
func MergeStatusLine(doc *jsonedit.Node, cmd string) string {
	if sl := doc.Get("statusLine"); sl != nil && sl.Get("command").Str() != "" {
		return ""
	}
	sl := jsonedit.NewObject()
	sl.Set("type", jsonedit.String("command"))
	sl.Set("command", jsonedit.String(cmd))
	doc.Set("statusLine", sl)
	return cmd
}

// UnmergeStatusLine removes the status line if it still is the one loom wrote.
func UnmergeStatusLine(doc *jsonedit.Node, written string) bool {
	if written == "" {
		return false
	}
	if sl := doc.Get("statusLine"); sl != nil && sl.Get("command").Str() == written {
		return doc.Delete("statusLine")
	}
	return false
}

// MergeEnv sets the keys that are absent; present keys are the user's.
// Returns what was written.
func MergeEnv(doc *jsonedit.Node, kv map[string]string) map[string]string {
	written := map[string]string{}
	env := doc.Ensure("env")
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if env.Get(k) != nil {
			continue
		}
		env.Set(k, jsonedit.String(kv[k]))
		written[k] = kv[k]
	}
	if env.Len() == 0 {
		doc.Delete("env")
	}
	return written
}

// UnmergeEnv removes keys whose value is still what loom wrote.
func UnmergeEnv(doc *jsonedit.Node, written map[string]string) int {
	env := doc.Get("env")
	if env == nil {
		return 0
	}
	n := 0
	for k, v := range written {
		if env.Get(k).Str() == v && env.Delete(k) {
			n++
		}
	}
	if env.Len() == 0 {
		doc.Delete("env")
	}
	return n
}

// DenyEntries are the bundle's permissions.deny lines for this layout.
func (l Layout) DenyEntries(regs Registrations) []string {
	out := make([]string, 0, len(regs.PermissionsDeny))
	for _, d := range regs.PermissionsDeny {
		out = append(out, strings.ReplaceAll(d, "<ccd>", l.ConfigDir))
	}
	return out
}

// MergeDeny appends the entries that are not present. Returns what was added.
func MergeDeny(doc *jsonedit.Node, entries []string) []string {
	perms := doc.Ensure("permissions")
	deny := perms.EnsureArray("deny")
	have := map[string]bool{}
	for _, it := range deny.Items {
		have[it.Str()] = true
	}
	var added []string
	for _, e := range entries {
		if have[e] {
			continue
		}
		deny.Append(jsonedit.String(e))
		added = append(added, e)
	}
	return added
}

// UnmergeDeny removes the listed entries.
func UnmergeDeny(doc *jsonedit.Node, entries []string) int {
	deny := doc.Path("permissions", "deny")
	if deny == nil {
		return 0
	}
	drop := map[string]bool{}
	for _, e := range entries {
		drop[e] = true
	}
	n := deny.Filter(func(it *jsonedit.Node) bool { return !drop[it.Str()] })
	if deny.Len() == 0 {
		doc.Get("permissions").Delete("deny")
		if doc.Get("permissions").Len() == 0 {
			doc.Delete("permissions")
		}
	}
	return n
}
