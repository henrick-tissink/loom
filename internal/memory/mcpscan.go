package memory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
)

// ScanMCP counts the mcp__<server>__ tool calls in one transcript file
// without the full extraction: a line is parsed only when it contains the
// prefix, so a file with no MCP calls costs one sequential read. Used to
// backfill files indexed before the counts were recorded; a fresh index
// gets them from ExtractFile. mainFile applies ExtractFile's rule that a
// main session file's sidechain lines belong to the subagent files.
func ScanMCP(path string, mainFile bool) map[string]int {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out map[string]int
	needle := []byte(`"name":"mcp__`)
	needleSp := []byte(`"name": "mcp__`)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, needle) && !bytes.Contains(line, needleSp) {
			continue
		}
		var r record
		if json.Unmarshal(line, &r) != nil || r.Type != "assistant" || r.Message == nil {
			continue
		}
		if mainFile && r.IsSidechain {
			continue
		}
		_, _, mcp := extractAssistantBlocks(r.Message.Content)
		for _, sv := range mcp {
			if out == nil {
				out = map[string]int{}
			}
			out[sv]++
		}
	}
	return out
}
