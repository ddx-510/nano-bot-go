package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PlatoX-Type/monet-bot/bus"
	"github.com/PlatoX-Type/monet-bot/hooks"
	"github.com/PlatoX-Type/monet-bot/providers"
)

// SubagentManager tracks running subagents and supports cancellation.
type SubagentManager struct {
	Provider    *providers.Provider
	Workspace   string
	Bus         *bus.MessageBus
	MaxTokens   int
	Temperature float64
	Emitter     *hooks.Emitter // nil when dashboard is disabled

	mu     sync.Mutex
	nextID atomic.Int64
	// sessionKey -> []*runningTask
	tasks map[string][]*runningTask
}

type runningTask struct {
	id     string
	label  string
	cancel context.CancelFunc
}

func NewSubagentManager(provider *providers.Provider, workspace string, mb *bus.MessageBus, maxTokens int, temperature float64) *SubagentManager {
	return &SubagentManager{
		Provider:    provider,
		Workspace:   workspace,
		Bus:         mb,
		MaxTokens:   maxTokens,
		Temperature: temperature,
		tasks:       make(map[string][]*runningTask),
	}
}

// CancelBySession cancels all subagents for the given session. Returns count cancelled.
func (m *SubagentManager) CancelBySession(sessionKey string) int {
	m.mu.Lock()
	tasks := m.tasks[sessionKey]
	delete(m.tasks, sessionKey)
	m.mu.Unlock()

	for _, t := range tasks {
		t.cancel()
	}
	return len(tasks)
}

func (m *SubagentManager) removeTask(sessionKey, taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tasks := m.tasks[sessionKey]
	for i, t := range tasks {
		if t.id == taskID {
			m.tasks[sessionKey] = append(tasks[:i], tasks[i+1:]...)
			break
		}
	}
}

// Spawn launches a subagent in the background.
func (m *SubagentManager) Spawn(task, label, channel, chatID string) string {
	id := fmt.Sprintf("sub-%d", m.nextID.Add(1))
	if label == "" {
		label = task
		if len(label) > 30 {
			label = label[:30] + "..."
		}
	}

	sessionKey := channel + ":" + chatID
	ctx, cancel := context.WithCancel(context.Background())

	rt := &runningTask{id: id, label: label, cancel: cancel}
	m.mu.Lock()
	m.tasks[sessionKey] = append(m.tasks[sessionKey], rt)
	m.mu.Unlock()

	go m.run(ctx, id, task, label, channel, chatID, sessionKey)

	log.Printf("[spawn] started subagent [%s]: %s", id, label)
	m.Emitter.Emit(hooks.Event{
		Type:      hooks.EventSubagentStarted,
		SessionID: sessionKey,
		Data:      map[string]any{"id": id, "label": label, "task": task},
	})
	return fmt.Sprintf("Subagent [%s] started (id: %s). I'll notify you when it completes.", label, id)
}

func (m *SubagentManager) run(ctx context.Context, id, task, label, channel, chatID, sessionKey string) {
	defer m.removeTask(sessionKey, id)

	log.Printf("[spawn] subagent [%s] running: %s", id, label)

	// Build sub-agent tools
	registry := NewRegistry()
	registry.Register(&ReadFileTool{Workspace: m.Workspace})
	registry.Register(&WriteFileTool{Workspace: m.Workspace})
	registry.Register(&EditFileTool{Workspace: m.Workspace})
	registry.Register(&ListDirTool{Workspace: m.Workspace})
	registry.Register(&ShellTool{Workspace: m.Workspace})
	registry.Register(&WebFetchTool{})

	system := m.buildSystemPrompt()
	messages := []map[string]any{
		{"role": "system", "content": system},
		{"role": "user", "content": task},
	}

	thinkRe := regexp.MustCompile(`(?s)<think>.*?</think>`)
	var finalResult string

	for i := 0; i < 15; i++ {
		// Check cancellation
		if ctx.Err() != nil {
			finalResult = "Task was cancelled."
			break
		}

		resp, err := m.Provider.Chat(messages, registry.Schemas(), m.MaxTokens, m.Temperature)
		if err != nil {
			finalResult = fmt.Sprintf("LLM error: %v", err)
			break
		}

		if !resp.HasToolCalls() {
			content := thinkRe.ReplaceAllString(resp.Content, "")
			content = strings.TrimSpace(content)
			if content == "" {
				content = "(sub-agent produced no response)"
			}
			finalResult = content
			break
		}

		// Build assistant message
		assistantMsg := map[string]any{"role": "assistant", "content": resp.Content}
		var tcList []map[string]any
		for _, tc := range resp.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			tcList = append(tcList, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(argsJSON),
				},
			})
		}
		assistantMsg["tool_calls"] = tcList
		messages = append(messages, assistantMsg)

		// Execute tools
		for _, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				break
			}
			log.Printf("[spawn:%s] tool: %s", id, tc.Name)
			result := registry.Execute(tc.Name, tc.Arguments)
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"content":      result,
			})
		}
	}

	if finalResult == "" {
		finalResult = "Task completed but no final response was generated."
	}

	// Announce result back to the main agent via message bus
	status := "completed successfully"
	if ctx.Err() != nil {
		status = "was cancelled"
	}
	log.Printf("[spawn] subagent [%s] %s", id, status)
	m.Emitter.Emit(hooks.Event{
		Type:      hooks.EventSubagentCompleted,
		SessionID: sessionKey,
		Data:      map[string]any{"id": id, "label": label, "status": status},
	})

	announceContent := fmt.Sprintf(`[Subagent '%s' %s]

Task: %s

Result:
%s

Summarize this naturally for the user. Keep it brief (1-2 sentences). Do not mention technical details like "subagent" or task IDs.`, label, status, task, finalResult)

	m.Bus.Inbound <- bus.InboundMessage{
		Channel: "system",
		ChatID:  channel + ":" + chatID,
		User:    "subagent",
		Text:    announceContent,
	}
}

func (m *SubagentManager) buildSystemPrompt() string {
	var parts []string

	soulPath := filepath.Join(m.Workspace, "SOUL.md")
	if data, err := os.ReadFile(soulPath); err == nil {
		parts = append(parts, strings.TrimSpace(string(data)))
	}

	parts = append(parts, `## Sub-Agent Instructions
You are a focused sub-agent spawned to complete a specific task.
- Complete the task thoroughly and return a clear, concise result.
- You have access to: read_file, write_file, edit_file, list_dir, exec, web_fetch.
- Do NOT spawn further sub-agents.
- When done, respond with your findings/results directly.

## Workspace
Your workspace is at: `+m.Workspace)

	return strings.Join(parts, "\n\n---\n\n")
}

// SpawnTool is the tool the LLM calls to spawn a subagent.
type SpawnTool struct {
	Manager *SubagentManager
	// Current context — set per request.
	Channel string
	ChatID  string
}

func (t *SpawnTool) Name() string { return "spawn" }
func (t *SpawnTool) Description() string {
	return "Spawn a subagent to handle a task in the background. " +
		"Use this for complex or time-consuming tasks that can run independently. " +
		"The subagent will complete the task and report back when done."
}
func (t *SpawnTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task": {"type": "string", "description": "The task for the subagent to complete"},
			"label": {"type": "string", "description": "Optional short label for the task (for display)"}
		},
		"required": ["task"]
	}`)
}

func (t *SpawnTool) Execute(args map[string]any) (string, error) {
	task, _ := args["task"].(string)
	if task == "" {
		return "Error: task is required", nil
	}
	label, _ := args["label"].(string)
	return t.Manager.Spawn(task, label, t.Channel, t.ChatID), nil
}
