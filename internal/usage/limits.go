// Package usage reports plan usage: the rate-limit windows claude shows in
// /usage, and the tokens spent per model and project.
//
// The limits reach exactly one place loom can observe without credentials:
// the JSON claude pipes to the statusLine command on every render
// (rate_limits.five_hour / seven_day: used_percentage, resets_at in unix
// seconds; verified on 2.1.303). So loom offers an opt-in tap: the
// statusLine command is wrapped by a small script that saves that JSON under
// ~/.loom/usage and then runs the original command on the same input, whose
// output the status line shows unchanged. Off restores the original command.
// The tokens come from the transcripts (tokens.go) and need no setup.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/jsonedit"
)

// Window is one rate-limit window as claude reports it.
type Window struct {
	Name     string  // five_hour, seven_day, or any window claude adds
	UsedPct  float64 `json:"used_percentage"`
	ResetsAt int64   `json:"resets_at"` // unix seconds
}

// Limits is the newest rate-limit report any claude session rendered.
type Limits struct {
	Windows    []Window
	CapturedAt time.Time // when the tap last wrote it
}

// StatusFile is where the tap saves the status line JSON, under loomDir.
func StatusFile(loomDir string) string { return filepath.Join(loomDir, "usage", "statusline.json") }

// ReadLimits reads the tap's last capture; ok is false when there is none
// or it carried no rate limits (an API-key login has no plan windows).
func ReadLimits(path string) (Limits, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Limits{}, false, nil
	}
	if err != nil {
		return Limits{}, false, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Limits{}, false, err
	}
	var d struct {
		RateLimits map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return Limits{}, false, fmt.Errorf("usage: unreadable status capture: %w", err)
	}
	l := Limits{CapturedAt: fi.ModTime()}
	for name, raw := range d.RateLimits {
		var w struct {
			UsedPct  *float64 `json:"used_percentage"`
			ResetsAt int64    `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.UsedPct == nil {
			continue // not a window (spend_limit has its own shape)
		}
		l.Windows = append(l.Windows, Window{Name: name, UsedPct: *w.UsedPct, ResetsAt: w.ResetsAt})
	}
	// the short window first, then by name
	rank := map[string]int{"five_hour": 0, "seven_day": 1}
	sort.Slice(l.Windows, func(i, j int) bool {
		ri, iok := rank[l.Windows[i].Name]
		rj, jok := rank[l.Windows[j].Name]
		if iok != jok {
			return iok
		}
		if ri != rj {
			return ri < rj
		}
		return l.Windows[i].Name < l.Windows[j].Name
	})
	return l, len(l.Windows) > 0, nil
}

// tapScript saves the status line input for loom, then hands the same input
// to the original command ($1), whose output and exit status are the status
// line's. It writes through a temp file so a reader never sees half a file,
// and a failed save never stops the original command.
const tapScript = `#!/bin/sh
# loom usage tap: saves claude's status line JSON (its rate limits) for
# loom's usage view, then runs your original status line command on it.
# Turn it off in loom's usage view; that restores the original command.
in=$(cat)
d="$HOME/.loom/usage"
# a render without rate limits (an API-key session, a session before its
# first reply) keeps the last capture
case $in in *'"rate_limits"'*)
mkdir -p "$d" 2>/dev/null && printf '%s' "$in" > "$d/statusline.json.$$" 2>/dev/null && mv -f "$d/statusline.json.$$" "$d/statusline.json" 2>/dev/null ;;
esac
[ -n "$1" ] || exit 0
printf '%s' "$in" | sh -c "$1"
`

// WriteTapScript writes (or refreshes) the tap script and returns its path.
func WriteTapScript(loomDir string) (string, error) {
	script := TapScript(loomDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		return "", err
	}
	return script, jsonedit.WriteBytes(script, []byte(tapScript))
}

// TapScript is the tap's path under loomDir.
func TapScript(loomDir string) string { return filepath.Join(loomDir, "usage", "statusline-tap.sh") }

const tapPrefix = "sh "

// TapCommand wraps orig in the tap.
func TapCommand(script, orig string) string {
	return tapPrefix + shQuote(script) + " " + shQuote(orig)
}

// Untap returns the original command inside a tapped one, and whether cmd
// was tapped at all.
func Untap(cmd string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, tapPrefix)
	if !ok {
		return cmd, false
	}
	script, rest, ok := cutQuoted(rest)
	if !ok || filepath.Base(script) != "statusline-tap.sh" {
		return cmd, false
	}
	orig, tail, ok := cutQuoted(strings.TrimPrefix(rest, " "))
	if !ok || tail != "" {
		return cmd, false
	}
	return orig, true
}

// Retap swaps the original command inside a tapped command, keeping the
// tap; ok is false when cmd is not tapped.
func Retap(cmd, orig string) (string, bool) {
	rest, ok := strings.CutPrefix(cmd, tapPrefix)
	if !ok {
		return "", false
	}
	script, _, ok := cutQuoted(rest)
	if _, tapped := Untap(cmd); !ok || !tapped {
		return "", false
	}
	return TapCommand(script, orig), true
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// cutQuoted reads one shQuote'd word off the front of s.
func cutQuoted(s string) (word, rest string, ok bool) {
	var b strings.Builder
	for {
		if !strings.HasPrefix(s, "'") {
			return "", "", false
		}
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		b.WriteString(s[1 : 1+end])
		s = s[2+end:]
		if !strings.HasPrefix(s, `\''`) {
			return b.String(), s, true
		}
		b.WriteByte('\'')
		s = s[2:] // leave the opening quote of the next run
	}
}

// TapState reports the settings' status line command and whether it is
// tapped.
func TapState(settingsPath string) (cmd string, tapped bool, err error) {
	doc, _, err := jsonedit.ReadFile(settingsPath)
	if err != nil {
		return "", false, err
	}
	cmd = doc.Path("statusLine", "command").Str()
	_, tapped = Untap(cmd)
	return cmd, tapped, nil
}

// SetTap turns the tap on or off in a claude settings file. On writes the
// tap script and wraps the current statusLine command (none: the tap only
// saves and shows nothing, as before); off puts the original back. The
// settings file is backed up before it is changed.
func SetTap(settingsPath, loomDir string, on bool, now time.Time) error {
	doc, exists, err := jsonedit.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	cmd := doc.Path("statusLine", "command").Str()
	orig, tapped := Untap(cmd)
	if on == tapped {
		return nil
	}
	if on {
		script, err := WriteTapScript(loomDir)
		if err != nil {
			return err
		}
		sl := doc.Ensure("statusLine")
		sl.Set("type", jsonedit.String("command"))
		sl.Set("command", jsonedit.String(TapCommand(script, orig)))
	} else if orig == "" {
		doc.Delete("statusLine") // the tap was the whole status line
	} else {
		doc.Path("statusLine").Set("command", jsonedit.String(orig))
	}
	if exists {
		b, err := os.ReadFile(settingsPath)
		if err != nil {
			return err
		}
		bak := settingsPath + ".bak-loom-usage-" + now.Format("20060102150405")
		if err := os.WriteFile(bak, b, 0o600); err != nil {
			return fmt.Errorf("usage: back up settings: %w", err)
		}
	}
	return jsonedit.WriteFile(settingsPath, doc)
}
