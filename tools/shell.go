package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var denyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+-[rf]{1,2}\b`),           // rm -r, rm -rf, rm -fr
	regexp.MustCompile(`\bdel\s+/[fq]\b`),               // del /f, del /q
	regexp.MustCompile(`\brmdir\s+/s\b`),                // rmdir /s
	regexp.MustCompile(`(?:^|[;&|]\s*)format\b`),        // format (as standalone command)
	regexp.MustCompile(`\b(mkfs|diskpart)\b`),           // disk operations
	regexp.MustCompile(`\bdd\s+if=`),                    // dd
	regexp.MustCompile(`>\s*/dev/sd`),                   // write to disk
	regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`), // system power
	regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),           // fork bomb
	regexp.MustCompile(`chmod\s+-R\s+777\s+/`),         // chmod -R 777 /
}

type ShellTool struct {
	Workspace string
	Timeout   time.Duration
}

func (t *ShellTool) Name() string { return "exec" }
func (t *ShellTool) Description() string {
	return "Execute a shell command in the workspace. Use for git log, git blame, grep, curl (GET only), and other read operations."
}
func (t *ShellTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command to execute"},
			"working_dir": {"type": "string", "description": "Subdirectory within workspace"}
		},
		"required": ["command"]
	}`)
}

func (t *ShellTool) Execute(args map[string]any) (string, error) {
	command, _ := args["command"].(string)
	if command == "" {
		return "Error: command is required", nil
	}

	cmdLower := strings.ToLower(command)
	for _, p := range denyPatterns {
		if p.MatchString(cmdLower) {
			return fmt.Sprintf("Error: command blocked — matched deny pattern '%s'", p.String()), nil
		}
	}

	cwd := t.Workspace
	if wd, ok := args["working_dir"].(string); ok && wd != "" {
		cwd = filepath.Join(t.Workspace, wd)
	}

	// Ensure we stay within workspace
	abs, _ := filepath.Abs(cwd)
	wsAbs, _ := filepath.Abs(t.Workspace)
	if !strings.HasPrefix(abs, wsAbs) {
		return "Error: cannot execute outside workspace", nil
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()

	result := string(out)
	if len(result) > 20000 {
		result = result[:20000] + fmt.Sprintf("\n... (truncated, %d total chars)", len(out))
	}

	if err != nil {
		result += fmt.Sprintf("\n(error: %v)", err)
	}
	if result == "" {
		result = "(no output)"
	}
	return result, nil
}
