package flashcards

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/henricktissink/loom/internal/memory"
)

const defaultTimeout = 90 * time.Second

// runClaude runs one hardened `claude -p` child. The argv is BINDING and copied
// from internal/memory/summarize.go (the sanctioned headless path): no tools, no
// MCP, no slash commands, no session, no settings, dynamic system-prompt sections
// excluded. Untrusted content travels on stdin; the child env is scrubbed of
// CLAUDECODE/CLAUDE_CODE_*. Returns trimmed stdout; errors on non-zero exit,
// timeout, or empty output.
func runClaude(ctx context.Context, binary, workDir, prompt, stdin string) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	args := []string{
		"-p", prompt,
		"--model", "haiku",
		"--no-session-persistence",
		"--tools", "",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--disable-slash-commands",
		"--setting-sources", "",
		"--exclude-dynamic-system-prompt-sections",
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workDir
	cmd.Env = memory.ScrubEnv(os.Environ())
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", fmt.Errorf("claude -p: empty output")
	}
	return out, nil
}
