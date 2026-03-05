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
						"description": "Full updated GLOBAL long-term memory as markdown. Only team-wide facts, " +
							"conventions, and knowledge that apply across all chats. Return unchanged if nothing new.",
					},
					"chat_memory_update": map[string]any{
						"type": "string",
						"description": "Context specific to THIS chat session — ongoing investigations, project " +
							"context, decisions in progress. Return empty string if nothing chat-specific.",
					},
				},
				"required": []string{"history_entry", "memory_update", "chat_memory_update"},
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

// chatMemoryPath returns the path for a per-chat memory file.
func (m *Memory) chatMemoryPath(channel, chatID string) string {
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(chatID)
	return filepath.Join(m.dir, fmt.Sprintf("chat_%s_%s.md", channel, safe))
}

// LoadChatMemory returns per-chat memory, or empty string if none.
func (m *Memory) LoadChatMemory(channel, chatID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := os.ReadFile(m.chatMemoryPath(channel, chatID))
	if err != nil {
		return ""
	}
	return string(data)
}

// SaveChatMemory writes per-chat memory.
func (m *Memory) SaveChatMemory(channel, chatID, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if content == "" {
		return
	}
	os.WriteFile(m.chatMemoryPath(channel, chatID), []byte(content), 0o644)
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

// ConsolidateOpts holds optional parameters for consolidation.
type ConsolidateOpts struct {
	MaxMemoryBytes int    // trigger compression if MEMORY.md exceeds this (0 = no limit)
	Channel        string // chat channel (for per-chat memory)
	ChatID         string // chat ID (for per-chat memory)
}

// Consolidate uses the LLM to extract knowledge from old session messages
// into MEMORY.md + HISTORY.md + optional per-chat memory. Returns true on success.
func (m *Memory) Consolidate(provider *providers.Provider, messages []map[string]any, maxTokens int, temperature float64, opts ...ConsolidateOpts) bool {
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

	// Resolve options
	var opt ConsolidateOpts
	if len(opts) > 0 {
		opt = opts[0]
	}
	maxBytes := opt.MaxMemoryBytes

	sizeHint := ""
	if maxBytes > 0 {
		sizeHint = fmt.Sprintf("\n\nIMPORTANT: Keep the memory_update under %d bytes. Be concise — merge duplicates, drop stale info, use short bullet points.", maxBytes)
	}

	// Load current chat memory if scoped
	chatMemory := ""
	if opt.Channel != "" && opt.ChatID != "" {
		m.mu.Lock()
		data, _ := os.ReadFile(m.chatMemoryPath(opt.Channel, opt.ChatID))
		chatMemory = string(data)
		m.mu.Unlock()
	}

	chatSection := ""
	if opt.Channel != "" && opt.ChatID != "" {
		chatSection = fmt.Sprintf(`

## Current Chat-Specific Memory (channel=%s, chat=%s)
%s

Separate team-wide facts (memory_update) from chat-specific context (chat_memory_update).
Team-wide: conventions, people, repos, services — applies to all chats.
Chat-specific: ongoing investigations, project context, decisions in progress — only this chat.`, opt.Channel, opt.ChatID, chatMemory)
	}

	prompt := fmt.Sprintf(`Process this conversation and call the save_memory tool with your consolidation.%s%s

## Current Long-term Memory
%s

## Conversation to Process
%s`, sizeHint, chatSection, currentMemory, strings.Join(lines, "\n"))

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
	var needsCompression bool
	var updatedMemory string

	// Write results under lock
	m.mu.Lock()
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
			log.Printf("[memory] updated long-term memory (%d bytes)", len(update))
			updatedMemory = update
			needsCompression = opt.MaxMemoryBytes > 0 && len(update) > opt.MaxMemoryBytes
		}
	}
	// Write chat-specific memory
	if chatUpdate, ok := args["chat_memory_update"].(string); ok && chatUpdate != "" && opt.Channel != "" && opt.ChatID != "" {
		os.WriteFile(m.chatMemoryPath(opt.Channel, opt.ChatID), []byte(chatUpdate), 0o644)
		log.Printf("[memory] updated chat memory for %s:%s (%d bytes)", opt.Channel, opt.ChatID, len(chatUpdate))
	}
	m.mu.Unlock()

	// Compression pass outside lock (slow LLM call)
	if needsCompression {
		log.Printf("[memory] memory too large (%d > %d bytes), running compression", len(updatedMemory), maxBytes)
		m.compress(provider, updatedMemory, maxBytes, maxTokens, temperature)
	}

	return true
}

// compress asks the LLM to make MEMORY.md more concise when it exceeds the size limit.
func (m *Memory) compress(provider *providers.Provider, current string, maxBytes, maxTokens int, temperature float64) {
	prompt := fmt.Sprintf(`You are a memory compression agent. The current memory is %d bytes but the limit is %d bytes.
Rewrite the memory to be more concise while preserving ALL important facts. Merge duplicates, use shorter phrasing, drop stale or obvious info.
Call the save_memory tool with:
- history_entry: a one-line note "[YYYY-MM-DD HH:MM] Memory compressed from %d to ~%d bytes"
- memory_update: the compressed memory (MUST be under %d bytes)

## Current Memory
%s`, len(current), maxBytes, len(current), maxBytes, maxBytes, current)

	llmMessages := []map[string]any{
		{"role": "system", "content": "You are a memory compression agent. Make the memory more concise. Call save_memory with the result."},
		{"role": "user", "content": prompt},
	}

	resp, err := provider.Chat(llmMessages, saveMemoryTool, maxTokens, temperature)
	if err != nil {
		log.Printf("[memory] compression LLM error: %v", err)
		return
	}
	if !resp.HasToolCalls() {
		log.Println("[memory] compression: LLM did not call save_memory")
		return
	}

	args := resp.ToolCalls[0].Arguments

	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, ok := args["history_entry"].(string); ok && entry != "" {
		f, err := os.OpenFile(m.historyPath, os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "\n%s\n", strings.TrimRight(entry, "\n"))
			f.Close()
		}
	}
	if update, ok := args["memory_update"].(string); ok && update != "" {
		os.WriteFile(m.memoryPath, []byte(update), 0o644)
		log.Printf("[memory] compressed memory: %d -> %d bytes", len(current), len(update))
	}
}

// ConsolidateJSON is like Consolidate but takes raw JSONL message bytes.
func (m *Memory) ConsolidateJSON(provider *providers.Provider, rawMessages []json.RawMessage, maxTokens int, temperature float64, opts ...ConsolidateOpts) bool {
	var messages []map[string]any
	for _, raw := range rawMessages {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err == nil {
			if _, ok := msg["role"]; ok {
				messages = append(messages, msg)
			}
		}
	}
	return m.Consolidate(provider, messages, maxTokens, temperature, opts...)
}
