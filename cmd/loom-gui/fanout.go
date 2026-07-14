package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// FanResultDTO summarizes a fan-out launch for the frontend: how many sessions
// started, how many failed, the shared group tag, and the first session name
// (so the UI can jump to it).
type FanResultDTO struct {
	Group    string `json:"group"`
	Launched int    `json:"launched"`
	Failed   int    `json:"failed"`
	First    string `json:"first"`
	Error    string `json:"error"`
}

// newFanGroupID mints a 6-hex fan-out group id (matches the TUI's tag shape).
func newFanGroupID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}

// Fanout launches the same recipe (model/mode/seed) across every selected
// project, tagging each session "fan:<group>" so they read as one batch.
// Sequential and best-effort: a project that fails to resolve or launch is
// counted in Failed but never aborts the rest.
func (a *App) Fanout(projectPaths []string, model, mode, seed string) FanResultDTO {
	if a.launcher == nil {
		return FanResultDTO{Error: "launcher unavailable"}
	}
	if len(projectPaths) == 0 {
		return FanResultDTO{Error: "no projects selected"}
	}
	group := newFanGroupID()
	res := FanResultDTO{Group: group}
	for _, path := range projectPaths {
		r, err := buildRecipe(a.projects, path, model, mode, seed)
		if err != nil {
			res.Failed++
			continue
		}
		name, err := a.launcher.Launch(r, launchCols, launchRows, a.now())
		if err != nil {
			res.Failed++
			continue
		}
		if a.st != nil {
			_ = a.st.SetTags(name, "fan:"+group)
		}
		res.Launched++
		if res.First == "" {
			res.First = name
		}
	}
	if res.Launched == 0 && res.Error == "" {
		res.Error = fmt.Sprintf("all %d launches failed", res.Failed)
	}
	return res
}
