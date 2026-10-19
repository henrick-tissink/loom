package transcript

import (
	"encoding/json"
	"io"
	"os"
	"regexp"
)

// gitBranchField matches the gitBranch claude writes on every turn record,
// as a JSON string (escapes included).
var gitBranchField = regexp.MustCompile(`"gitBranch":("(?:[^"\\]|\\.)*")`)

// branchTailMax bounds how far back LastBranch looks: one record can be
// large (a tool result), so the window doubles until a branch turns up.
const branchTailMax = 4 << 20

// LastBranch returns the git branch the session last worked on, from the
// newest gitBranch in its transcript; "" when none is recorded (a session
// outside a repository, or a transcript not written yet). It reads only the
// file's tail.
func LastBranch(path string) (string, error) {
	return LastBranchSince(path, 0)
}

// LastBranchSince is LastBranch over the bytes from offset since onward, so
// a reader that already scanned a growing transcript reads only what was
// appended; "" means none in that range.
func LastBranchSince(path string, since int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	if since < 0 || since > size {
		since = 0
	}
	span := size - since
	for window := int64(64 << 10); ; window *= 2 {
		if window > span {
			window = span
		}
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, size-window); err != nil && err != io.EOF {
			return "", err
		}
		if ms := gitBranchField.FindAllSubmatch(buf, -1); len(ms) > 0 {
			var b string
			if err := json.Unmarshal(ms[len(ms)-1][1], &b); err != nil {
				return "", err
			}
			return b, nil
		}
		if window == span || window >= branchTailMax {
			return "", nil
		}
	}
}
