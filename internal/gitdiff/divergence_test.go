package gitdiff

import (
	"reflect"
	"testing"
)

func TestDiffMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"db/migrations/**", "db/migrations/0007_account.sql", true},
		{"db/migrations/**", "db/migrations/sub/deep/x.sql", true},
		{"db/migrations/**", "db/migrations", true},
		{"db/migrations/**", "db/other/x.sql", false},
		{"internal/account/schema.go", "internal/account/schema.go", true},
		{"internal/account/schema.go", "internal/account/schema_test.go", false},
		// A bare directory means the directory.
		{"internal/auth", "internal/auth/token.go", true},
		{"internal/auth", "internal/authz/token.go", false},
		// `*` must not cross a separator — the failure filepath.Match has.
		{"internal/*", "internal/a/b.go", false},
		{"internal/*", "internal/a.go", true},
		{"internal/*.go", "internal/a.go", true},
		{"**/*_test.go", "a/b/x_test.go", true},
		{"**/*_test.go", "x_test.go", true},
		{"**", "anything/at/all.go", true},
		{"src/**/*.ts", "src/clients/auth/index.ts", true},
		{"src/**/*.ts", "src/index.ts", true},
		{"src/**/*.ts", "lib/index.ts", false},
		// Normalisation: both sides land in the same alphabet.
		{"./internal/auth/**", "internal/auth/x.go", true},
		{"internal/auth/", "internal/auth/x.go", true},
	}
	for _, c := range cases {
		if got := Match([]string{c.pattern}, c.file); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
}

func TestDiffDiverge(t *testing.T) {
	siblings := map[string][]string{
		"auth-api": {"internal/auth/**"},
		"ballista": {"src/clients/auth/**"},
	}
	cases := []struct {
		name     string
		files    []string
		declared []string
		siblings map[string][]string
		want     Divergence
	}{
		{
			name:     "everything inside the declared set",
			files:    []string{"db/migrations/0007.sql", "internal/account/schema.go"},
			declared: []string{"db/migrations/**", "internal/account/schema.go"},
			want:     Divergence{},
		},
		{
			name:     "one file outside",
			files:    []string{"db/migrations/0007.sql", "cmd/loom/main.go"},
			declared: []string{"db/migrations/**"},
			want:     Divergence{Outside: []string{"cmd/loom/main.go"}},
		},
		{
			name:     "outside and inside a sibling's paths — the conflict predictor",
			files:    []string{"db/migrations/0007.sql", "internal/auth/token.go"},
			declared: []string{"db/migrations/**"},
			siblings: siblings,
			want: Divergence{
				Outside:  []string{"internal/auth/token.go"},
				Siblings: map[string][]string{"auth-api": {"internal/auth/token.go"}},
			},
		},
		{
			name:     "a sibling hit that is inside the task's OWN declared paths still reports",
			files:    []string{"internal/auth/token.go"},
			declared: []string{"internal/auth/**"},
			siblings: siblings,
			want:     Divergence{Siblings: map[string][]string{"auth-api": {"internal/auth/token.go"}}},
		},
		{
			name:     "no declared paths means everything is outside",
			files:    []string{"a.go", "b.go"},
			declared: nil,
			want:     Divergence{Outside: []string{"a.go", "b.go"}},
		},
		{
			name:     "duplicates collapse and output is sorted",
			files:    []string{"z.go", "a.go", "z.go"},
			declared: []string{"src/**"},
			want:     Divergence{Outside: []string{"a.go", "z.go"}},
		},
		{
			name:     "no touched files diverges not at all",
			files:    nil,
			declared: nil,
			want:     Divergence{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Diverge(c.files, c.declared, c.siblings)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Diverge = %+v, want %+v", got, c.want)
			}
			if got.Empty() != (len(c.want.Outside) == 0 && len(c.want.Siblings) == 0) {
				t.Errorf("Empty() = %v for %+v", got.Empty(), got)
			}
		})
	}
}

// End to end: a real branch's committed work classified against declared paths.
// This is the shape the merge gate reads — capture base→branch, then diverge.
func TestDiffDiverge_overRealBranch(t *testing.T) {
	dir := initRepo(t)
	write(t, dir, "internal/account/schema.go", "package account\n")
	write(t, dir, "cmd/loom/main.go", "package main\n")
	base := commit(t, dir, "seed")

	run(t, dir, "checkout", "-q", "-b", "loom/run/schema")
	write(t, dir, "internal/account/schema.go", "package account\n\n// v2\n")
	write(t, dir, "db/migrations/0007_account.sql", "-- x\n")
	write(t, dir, "cmd/loom/main.go", "package main\n\n// oops\n")
	commit(t, dir, "task work")

	d := SinceBase(dir, base, "loom/run/schema")
	if d.Error != "" {
		t.Fatalf("capture: %+v", d)
	}
	got := Diverge(d.Files,
		[]string{"db/migrations/**", "internal/account/schema.go"},
		map[string][]string{"cli": {"cmd/**"}})

	want := Divergence{
		Outside:  []string{"cmd/loom/main.go"},
		Siblings: map[string][]string{"cli": {"cmd/loom/main.go"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Diverge = %+v, want %+v (files %v)", got, want, d.Files)
	}
}
