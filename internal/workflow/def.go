// Package workflow implements Phase 3 Workflows (spec docs/superpowers/
// specs/2026-07-03-workflows-design.md): saved multi-step chains of real
// interactive claude sessions with explicit context topology. This file
// (def.go) is the definitions half — JSON files under ~/.loom/workflows/,
// loaded and validated on every workflows-view open (spec §2.1); run.go is
// the CAS-guarded Runner that executes them.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/henricktissink/loom/internal/registry"
)

// Step is one entry of a Definition's "steps" array (spec §3). Field names
// match the on-disk JSON keys via the tags below.
//
// Project deserves a callout: on disk it is a registry repo LABEL (e.g.
// "parallax"). The JSON key stays "project" — renaming it would invalidate
// every saved workflow on disk. LoadAll resolves and VALIDATES it against
// the registry at load time and overwrites it in place with the repo's
// resolved ABSOLUTE PATH (registry.Repo.Path) — never a bare label past
// LoadAll. This is a deliberate design choice, not an oversight: Runner
// (run.go) has no registry of its own (by the Produces contract its only
// fields are Store/Launcher/ClaudeConfigDir), so label→path resolution can only
// happen once, here, while a registry is in hand. An empty Project (valid
// for any step but step 1) means "inherit the resolved previous step's
// cwd/label" (spec §2.12) — resolved at Advance time from the previous
// step's live SessionRow, not from the registry. Callers that need the
// short label back for display can recover it via filepath.Base(Project).
type Step struct {
	Label    string `json:"label"`
	Project  string `json:"project"`
	Model    string `json:"model"`
	Mode     string `json:"mode"`
	Seed     string `json:"seed"`
	Relation string `json:"relation"`
}

// Definition is one loaded, validated workflow file.
type Definition struct {
	Name  string `json:"name"`
	Steps []Step `json:"steps"`
	Path  string `json:"-"` // the definition FILE's path; not part of the on-disk format, not persisted into a run's def_json snapshot
}

// LoadError is a malformed/invalid definition file, reported instead of
// panicking (spec §2.1: "malformed files listed dim-red with their error").
type LoadError struct {
	Path, Err string
}

// Known-set validation (spec §3 "Load validation"). Mirrors
// internal/ui/launcher.go's modelOptions/modeOptions exactly — the same
// sets the interactive launcher offers.
var (
	validModels = map[string]bool{"": true, "opus": true, "sonnet": true, "fable": true}
	validModes  = map[string]bool{"": true, "plan": true, "acceptEdits": true, "auto": true, "bypassPermissions": true}
	// validRelations deliberately excludes "" — every step but step 1 (whose
	// relation is ignored, spec §2.2) must name one of the three relations
	// explicitly; a typo here is exactly what load-time validation exists
	// to catch.
	validRelations = map[string]bool{"fresh": true, "fork": true, "continue": true}
)

// templateTokenRe finds every `{{...}}` occurrence in a seed string, valid
// or not — used at LOAD time to reject any token outside the whitelist
// (spec §2.3: "typos surface at load, never ship literal braces").
var templateTokenRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// seedNewlineRe matches runs of newlines/carriage-returns/tabs authored
// (deliberately or via copy-paste) inside a step's seed JSON string. A
// literal \n surviving into SendLiteral on a continue delivery (run.go's
// sendPendingSeed) acts as a premature Enter mid-seed, submitting the
// prompt early — collapsing at LOAD time, once, is cheaper and safer than
// trying to catch it at every send site.
var seedNewlineRe = regexp.MustCompile(`[\r\n\t]+`)

var templateWhitelist = map[string]bool{
	"{{prev.outcome}}": true,
	"{{prev.title}}":   true,
	"{{prev.ask}}":     true,
}

// LoadAll loads every *.json regular file at the top level of dir (spec
// §1/§3). A missing dir is not an error (empty result). Malformed/invalid
// files produce a LoadError, never a panic, and are excluded from the
// returned Definitions. Both slices are sorted for deterministic output
// (Definitions by Name, LoadErrors by Path).
func LoadAll(dir string, repos []registry.Repo) ([]Definition, []LoadError) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []LoadError{{Path: dir, Err: err.Error()}}
	}

	known := make(map[string]registry.Repo, len(repos))
	for _, r := range repos {
		known[r.Label] = r
	}

	var defs []Definition
	var errs []LoadError
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue // symlinks/devices/etc. excluded (spec §3: "regular files" only)
		}
		path := filepath.Join(dir, e.Name())
		d, derr := loadOne(path, known)
		if derr != nil {
			errs = append(errs, LoadError{Path: path, Err: derr.Error()})
			continue
		}
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	sort.Slice(errs, func(i, j int) bool { return errs[i].Path < errs[j].Path })
	return defs, errs
}

// loadOne parses and validates one definition file (spec §3 "Load
// validation"): name == filename stem; ≥1 step; step-1 repo label known to
// the registry; relations/models/modes in known sets; template-token
// whitelist. Only ever returns an error for the caller to wrap as a
// LoadError — never panics on malformed input.
func loadOne(path string, known map[string]registry.Repo) (Definition, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Definition{}, err
	}
	var d Definition
	if err := json.Unmarshal(raw, &d); err != nil {
		return Definition{}, fmt.Errorf("invalid JSON: %w", err)
	}

	stem := strings.TrimSuffix(filepath.Base(path), ".json")
	if d.Name != stem {
		return Definition{}, fmt.Errorf("name %q does not match filename %q", d.Name, stem)
	}
	if len(d.Steps) == 0 {
		return Definition{}, errors.New("must have at least 1 step")
	}

	for i := range d.Steps {
		s := &d.Steps[i]
		if s.Label == "" {
			return Definition{}, fmt.Errorf("step %d: label required", i+1)
		}
		if !validModels[s.Model] {
			return Definition{}, fmt.Errorf("step %d: unknown model %q", i+1, s.Model)
		}
		if !validModes[s.Mode] {
			return Definition{}, fmt.Errorf("step %d: unknown mode %q", i+1, s.Mode)
		}
		if i == 0 {
			if s.Project == "" {
				return Definition{}, fmt.Errorf("step 1 must name a project")
			}
			// Step 1's relation is ignored at runtime (always fresh, spec
			// §2.2) but a non-empty value is still validated so a typo
			// surfaces at load rather than silently doing nothing.
			if s.Relation != "" && !validRelations[s.Relation] {
				return Definition{}, fmt.Errorf("step 1: unknown relation %q", s.Relation)
			}
		} else if !validRelations[s.Relation] {
			return Definition{}, fmt.Errorf("step %d: relation must be one of fresh/fork/continue, got %q", i+1, s.Relation)
		}
		if s.Project != "" {
			repo, ok := known[s.Project]
			if !ok {
				return Definition{}, fmt.Errorf("step %d: unknown project %q", i+1, s.Project)
			}
			s.Project = repo.Path // bake the resolved absolute path in place (see Step's doc comment)
		}
		s.Seed = seedNewlineRe.ReplaceAllString(s.Seed, " ") // normalize BEFORE template-token validation, see seedNewlineRe
		for _, tok := range templateTokenRe.FindAllString(s.Seed, -1) {
			if !templateWhitelist[tok] {
				return Definition{}, fmt.Errorf("step %d: unknown template token %q", i+1, tok)
			}
		}
	}

	d.Path = path
	return d, nil
}
