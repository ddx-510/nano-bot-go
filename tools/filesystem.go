package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func resolveSafe(workspace, path string) (string, error) {
	full := filepath.Join(workspace, path)
	abs, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	wsAbs, _ := filepath.Abs(workspace)
	if !strings.HasPrefix(abs, wsAbs) {
		return "", fmt.Errorf("path escapes workspace: %s", path)
	}
	return abs, nil
}

// ReadFileTool reads file contents.
type ReadFileTool struct{ Workspace string }

func (t *ReadFileTool) Name() string        { return "read_file" }
func (t *ReadFileTool) Description() string { return "Read the contents of a file. Path is relative to workspace." }
func (t *ReadFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to workspace"},
			"offset": {"type": "integer", "description": "Start line (0-based)"},
			"limit": {"type": "integer", "description": "Max lines to read"}
		},
		"required": ["path"]
	}`)
}

func (t *ReadFileTool) Execute(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	resolved, err := resolveSafe(t.Workspace, path)
	if err != nil {
		return err.Error(), nil
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	lines := strings.Split(string(data), "\n")
	offset := intArg(args, "offset", 0)
	limit := intArg(args, "limit", 500)

	if offset > len(lines) {
		offset = len(lines)
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	selected := lines[offset:end]

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("(%d lines total, showing %d-%d)\n", len(lines), offset+1, end))
	for i, line := range selected {
		sb.WriteString(fmt.Sprintf("%5d\t%s\n", offset+i+1, line))
	}
	return sb.String(), nil
}

// WriteFileTool writes content to a file.
type WriteFileTool struct{ Workspace string }

func (t *WriteFileTool) Name() string        { return "write_file" }
func (t *WriteFileTool) Description() string { return "Write content to a file. Creates parent directories." }
func (t *WriteFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to workspace"},
			"content": {"type": "string", "description": "Content to write"}
		},
		"required": ["path", "content"]
	}`)
}

func (t *WriteFileTool) Execute(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)

	resolved, err := resolveSafe(t.Workspace, path)
	if err != nil {
		return err.Error(), nil
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	if err := os.WriteFile(resolved, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	return fmt.Sprintf("Wrote %d chars to %s", len(content), path), nil
}

// EditFileTool replaces text in a file.
type EditFileTool struct{ Workspace string }

func (t *EditFileTool) Name() string        { return "edit_file" }
func (t *EditFileTool) Description() string { return "Replace a specific string in a file. old_string must match exactly and be unique." }
func (t *EditFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File path relative to workspace"},
			"old_string": {"type": "string", "description": "Exact text to find"},
			"new_string": {"type": "string", "description": "Replacement text"}
		},
		"required": ["path", "old_string", "new_string"]
	}`)
}

func (t *EditFileTool) Execute(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	oldStr, _ := args["old_string"].(string)
	newStr, _ := args["new_string"].(string)

	resolved, err := resolveSafe(t.Workspace, path)
	if err != nil {
		return err.Error(), nil
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	content := string(data)
	count := strings.Count(content, oldStr)
	if count == 0 {
		return notFoundMessage(oldStr, content, path), nil
	}
	if count > 1 {
		return fmt.Sprintf("Error: old_string found %d times — must be unique", count), nil
	}

	newContent := strings.Replace(content, oldStr, newStr, 1)
	if err := os.WriteFile(resolved, []byte(newContent), 0o644); err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	return fmt.Sprintf("Edited %s — replaced 1 occurrence", path), nil
}

// notFoundMessage builds a helpful error showing the closest matching text
// so the LLM can see what it got wrong.
func notFoundMessage(oldStr, content, path string) string {
	oldLines := strings.Split(oldStr, "\n")
	contentLines := strings.Split(content, "\n")
	window := len(oldLines)
	if window > len(contentLines) {
		window = len(contentLines)
	}

	bestRatio := 0.0
	bestStart := 0

	for i := 0; i <= len(contentLines)-window; i++ {
		ratio := similarityRatio(oldLines, contentLines[i:i+window])
		if ratio > bestRatio {
			bestRatio = ratio
			bestStart = i
		}
	}

	if bestRatio > 0.5 {
		// Build a simple diff showing the mismatch
		actual := contentLines[bestStart : bestStart+window]
		var sb strings.Builder
		fmt.Fprintf(&sb, "Error: old_string not found in %s.\n", path)
		fmt.Fprintf(&sb, "Best match (%.0f%% similar) at line %d:\n", bestRatio*100, bestStart+1)
		sb.WriteString("--- old_string (provided)\n+++ " + path + " (actual)\n")
		for i := 0; i < len(oldLines) || i < len(actual); i++ {
			old := ""
			act := ""
			if i < len(oldLines) {
				old = oldLines[i]
			}
			if i < len(actual) {
				act = actual[i]
			}
			if old == act {
				sb.WriteString(" " + old + "\n")
			} else {
				if old != "" {
					sb.WriteString("-" + old + "\n")
				}
				if act != "" {
					sb.WriteString("+" + act + "\n")
				}
			}
		}
		return sb.String()
	}

	return fmt.Sprintf("Error: old_string not found in %s. No similar text found. Verify the file content.", path)
}

// similarityRatio computes a simple line-level similarity ratio (0.0 to 1.0).
func similarityRatio(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	matches := 0
	total := len(a)
	if len(b) > total {
		total = len(b)
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			matches++
		} else {
			// Partial match: count character-level similarity
			matches += charSimilarity(a[i], b[i])
		}
	}
	return float64(matches) / float64(total)
}

func charSimilarity(a, b string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	// Simple longest common subsequence ratio
	m := len(a)
	n := len(b)
	// Use two rows to save memory
	prev := make([]int, n+1)
	curr := make([]int, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
			} else if prev[j] > curr[j-1] {
				curr[j] = prev[j]
			} else {
				curr[j] = curr[j-1]
			}
		}
		prev, curr = curr, prev
		for j := range curr {
			curr[j] = 0
		}
	}
	lcs := prev[n]
	maxLen := m
	if n > maxLen {
		maxLen = n
	}
	if float64(lcs)/float64(maxLen) > 0.6 {
		return 1 // close enough to count as partial match
	}
	return 0
}

// ListDirTool lists directory contents.
type ListDirTool struct{ Workspace string }

func (t *ListDirTool) Name() string        { return "list_dir" }
func (t *ListDirTool) Description() string { return "List files and directories. Path relative to workspace." }
func (t *ListDirTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Directory path relative to workspace"}
		},
		"required": []
	}`)
}

func (t *ListDirTool) Execute(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}

	resolved, err := resolveSafe(t.Workspace, path)
	if err != nil {
		return err.Error(), nil
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}

	var sb strings.Builder
	for i, e := range entries {
		if i >= 100 {
			sb.WriteString(fmt.Sprintf("\n... (%d more entries)", len(entries)-100))
			break
		}
		prefix := "   "
		if e.IsDir() {
			prefix = "📁 "
		}
		sb.WriteString(fmt.Sprintf("%s%s\n", prefix, e.Name()))
	}
	if sb.Len() == 0 {
		return "(empty directory)", nil
	}
	return sb.String(), nil
}

func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return def
	}
}
