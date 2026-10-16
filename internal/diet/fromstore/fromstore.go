// Package fromstore fills diet.Inputs from loom's store: the visible
// projects with their repos, an attribution that respects hide and solo,
// and the MCP usage the transcript index recorded. It is the one place
// the diet touches loom.db, kept apart so internal/discipline (which
// carries the diet types in its report) never depends on the store.
package fromstore

import (
	"github.com/henricktissink/loom/internal/diet"
	"github.com/henricktissink/loom/internal/projects"
	"github.com/henricktissink/loom/internal/store"
)

// Fill sets Projects, Attribute, Usage, Unscanned and HasIndex on in.
// Hidden projects and the sessions under them are left out, like
// everywhere else in loom. A store error leaves the field empty.
func Fill(in *diet.Inputs, st *store.Store) {
	ps, err := st.ListProjects()
	if err != nil {
		return
	}
	repos, _ := st.ListAllProjectRepos()
	res := projects.NewResolver(ps, repos)
	byRoot := map[string][]string{}
	for _, r := range repos {
		byRoot[r.ProjectRoot] = append(byRoot[r.ProjectRoot], r.Path)
	}
	in.Projects = nil
	for _, p := range ps {
		if p.Root == "" || !res.ProjectVisible(p.Root) {
			continue
		}
		in.Projects = append(in.Projects, diet.Project{Root: p.Root, Name: p.Name, Repos: byRoot[p.Root]})
	}
	in.Attribute = func(cwd string) (string, bool) {
		a, ok := res.Attribute(cwd)
		if !ok || a.Root == "" || !res.Visible(cwd) {
			return "", false
		}
		return a.Root, true
	}
	in.HasIndex = true
	in.Usage = nil
	if usage, err := st.MCPUsage(); err == nil {
		for _, u := range usage {
			in.Usage = append(in.Usage, diet.Usage{Cwd: u.Cwd, LastTS: u.LastTS, Servers: u.Servers})
		}
	}
	if n, err := st.UnscannedMCPCount(); err == nil {
		in.Unscanned = int(n)
	}
}
