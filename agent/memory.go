package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PlatoX-Type/monet-bot/providers"
)

// Memory manages MEMORY.md (facts) + HISTORY.md (log) with LLM-powered consolidation.
// All file operations are mutex-protected for goroutine safety.
type Memory struct {
	mu          sync.Mutex
	dir         string
	memoryPath  string
	historyPath string
}

// saveMemoryTool is the tool schema the consolidation LLM calls.
var saveMemoryTool = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "save_memory",
			"description": "Save the memory consolidation result to persistent storage.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"history_entry": map[string]any{
						"type": "string",
						"description": "A paragraph (2-5 sentences) summarizing key events/decisions/topics. " +
							"Start with [YYYY-MM-DD HH:MM]. Include detail useful for grep search.",
					},
					"memory_update": map[string]any{
						"type": "string",
						"description": "Full updated long-term memory as markdown. Include all existing " +
							"facts plus new ones. Return unchanged if nothing new.",
					},
				},
				"required": []string{"history_entry", "memory_update"},
			},
		},
	},
}

func NewMemory(workspace string) *Memory {
	dir := filepath.Join(workspace, "memory")
	os.MkdirAll(dir, 0o755)

	memPath := filepath.Join(dir, "MEMORY.md")
	histPath := filepath.Join(dir, "HISTORY.md")

	if _, err := os.Stat(memPath); os.IsNotExist(err) {
		os.WriteFile(memPath, []byte("# CCMonet Team Memory\n\n(No memories yet.)\n"), 0o644)
	}
	if _, err := os.Stat(histPath); os.IsNotExist(err) {
		os.WriteFile(histPath, []byte("# CCMonet History Log\n\n"), 0o644)
	}

	return &Memory{dir: dir, memoryPath: memPath, historyPath: histPath}
}

func (m *Memory) LoadMemory() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.memoryPath)
	if err != nil {
		return ""
	}
	return string(data)
}

func (m *Memory) SaveMemory(content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	os.WriteFile(m.memoryPath, []byte(content), 0o644)
}

func (m *Memory) AppendHistory(entry string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := os.OpenFile(m.historyPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n%s\n", strings.TrimRight(entry, "\n"))
}

// loadMemoryUnlocked reads MEMORY.md without locking. Caller must hold m.mu.
func (m *Memory) loadMemoryUnlocked() string {
	data, err := os.ReadFile(m.memoryPath)
	if err != nil {
		return ""
	}
	return string(data)
}

// Consolidate uses the LLM to extract knowledge from old session messages
// into MEMORY.md + HISTORY.md. Returns true on success.
func (m *Memory) Consolidate(provider *providers.Provider, messages []map[string]any, maxTokens int, temperature float64) bool {
	if len(messages) == 0 {
		return true
	}

	// Format messages for consolidation (no lock needed — only reads args)
	var lines []string
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		if content == "" {
			continue
		}
		ts, _ := msg["timestamp"].(string)
		if len(ts) > 16 {
			ts = ts[:16]
		}
		if ts == "" {
			ts = "?"
		}

		// Note tool usage
		toolSuffix := ""
		if toolCalls, ok := msg["tool_calls"]; ok {
			if tcList, ok := toolCalls.([]any); ok {
				var names []string
				for _, tc := range tcList {
					if tcMap, ok := tc.(map[string]any); ok {
						if fn, ok := tcMap["function"].(map[string]any); ok {
							if name, ok := fn["name"].(string); ok {
								names = append(names, name)
							}
						}
					}
				}
				if len(names) > 0 {
					toolSuffix = fmt.Sprintf(" [tools: %s]", strings.Join(names, ", "))
				}
			}
		}

		// Truncate very long content
		if len(content) > 500 {
			content = content[:500] + "... (truncated)"
		}

		lines = append(lines, fmt.Sprintf("[%s] %s%s: %s", ts, strings.ToUpper(role), toolSuffix, content))
	}

	if len(lines) == 0 {
		return true
	}

	// Read current memory under lock
	m.mu.Lock()
	currentMemory := m.loadMemoryUnlocked()
	m.mu.Unlock()

	prompt := fmt.Sprintf(`Process this conversation and call the save_memory tool with your consolidation.

## Current Long-term Memory
%s

## Conversation to Process
%s`, currentMemory, strings.Join(lines, "\n"))

	llmMessages := []map[string]any{
		{"role": "system", "content": "You are a memory consolidation agent. Call the save_memory tool with your consolidation of the conversation."},
		{"role": "user", "content": prompt},
	}

	// LLM call happens outside the lock (can be slow)
	resp, err := provider.Chat(llmMessages, saveMemoryTool, maxTokens, temperature)
	if err != nil {
		log.Printf("[memory] consolidation LLM error: %v", err)
		return false
	}

	if !resp.HasToolCalls() {
		log.Println("[memory] consolidation: LLM did not call save_memory, skipping")
		return false
	}

	args := resp.ToolCalls[0].Arguments

	// Write results under lock
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, ok := args["history_entry"].(string); ok && entry != "" {
		f, err := os.OpenFile(m.historyPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "\n%s\n", strings.TrimRight(entry, "\n"))
			f.Close()
		}
		log.Printf("[memory] appended history entry (%d chars)", len(entry))
	}

	if update, ok := args["memory_update"].(string); ok && update != "" {
		if update != currentMemory {
			os.WriteFile(m.memoryPath, []byte(update), 0o644)
			log.Printf("[memory] updated long-term memory (%d chars)", len(update))
		}
	}

	return true
}

// ConsolidateJSON is like Consolidate but takes raw JSONL message bytes
// (for when we need to consolidate from a session file).
func (m *Memory) ConsolidateJSON(provider *providers.Provider, rawMessages []json.RawMessage, maxTokens int, temperature float64) bool {
	var messages []map[string]any
	for _, raw := range rawMessages {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err == nil {
			if _, ok := msg["role"]; ok {
				messages = append(messages, msg)
			}
		}
	}
	return m.Consolidate(provider, messages, maxTokens, temperature)
}
