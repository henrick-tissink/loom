package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const limitRecord = `{"isSidechain":false,"type":"assistant","message":{"model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"You've hit your weekly limit · resets Oct 2 at 4pm (Europe/Bucharest)"}]},"quotaLimits":{"status":"rejected","resetsAt":1790946000,"rateLimitType":"seven_day"},"error":"rate_limit","isApiErrorMessage":true,"apiErrorStatus":429}`

func TestLastLimit(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		return p
	}
	user := `{"type":"user","message":{"role":"user","content":"go on"}}`
	asst := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`
	other := `{"type":"attachment","attachment":{}}`

	lim, ok, err := LastLimit(write(user, limitRecord, other, `{"type":"api-request","x":1}`))
	if err != nil || !ok || lim.ResetsAt.Unix() != 1790946000 || lim.Type != "seven_day" || !strings.HasPrefix(lim.Text, "You've hit") {
		t.Fatalf("limit = %+v, %v, %v", lim, ok, err)
	}
	for name, p := range map[string]string{
		"typed after":    write(limitRecord, user),
		"answered after": write(limitRecord, user, asst),
		"no limit":       write(user, asst),
		"subagent":       write(user, strings.Replace(limitRecord, `"isSidechain":false`, `"isSidechain":true`, 1)),
		"other error":    write(user, strings.Replace(limitRecord, `"rate_limit"`, `"server_error"`, 1)),
	} {
		if _, ok, _ := LastLimit(p); ok {
			t.Fatalf("%s: reported a limit", name)
		}
	}
	if _, ok, err := LastLimit(filepath.Join(dir, "none.jsonl")); ok || err != nil {
		t.Fatalf("missing file: %v %v", ok, err)
	}
	// a long tail of other records: the window grows past 64 KB
	pad := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		pad = append(pad, `{"type":"attachment","attachment":{"pad":"`+strings.Repeat("x", 60)+`"}}`)
	}
	if _, ok, _ := LastLimit(write(append([]string{user, limitRecord}, pad...)...)); !ok {
		t.Fatal("limit behind 140 KB of other records not found")
	}
}
