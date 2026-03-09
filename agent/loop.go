package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PlatoX-Type/monet-bot/bus"
	"github.com/PlatoX-Type/monet-bot/config"
	"github.com/PlatoX-Type/monet-bot/cron"
	"github.com/PlatoX-Type/monet-bot/hooks"
	"github.com/PlatoX-Type/monet-bot/providers"
	"github.com/PlatoX-Type/monet-bot/tools"
)

type Loop struct {
	Config          *config.Config
	Provider        *providers.Provider
	Bus             *bus.MessageBus
	Tools           *tools.Registry
	Memory          *Memory
	Sessions        *Session
	Skills          []Skill
	Workspace       string
	CronService     *cron.Service
	SubagentManager *tools.SubagentManager
	Emitter         *hooks.Emitter // nil when dashboard is disabled

	// Concurrency: each message spawns its own goroutine.
	// /stop cancels all active tasks for a session.
	mu             sync.Mutex
	activeTasks    map[string]map[int64]*taskInfo // sessionKey -> taskID -> info
	taskCounter    atomic.Int64                   // global task ID counter
	consolidating  sync.Map                       // sessionKey -> bool (prevents double-consolidation)
}

type taskInfo struct {
	Cancel  context.CancelFunc
	Text    string    // original message text (truncated)
	Started time.Time
}

func NewLoop(cfg *config.Config, provider *providers.Provider, mb *bus.MessageBus, cronSvc *cron.Service) *Loop {
	workspace := cfg.Workspace

	subMgr := tools.NewSubagentManager(provider, workspace, mb, cfg.LLM.MaxTokens, cfg.LLM.Temperature)

	l := &Loop{
		Config:          cfg,
		Provider:        provider,
		Bus:             mb,
		Tools:           tools.NewRegistry(),
		Memory:          NewMemory(workspace),
		Sessions:        NewSession(workspace),
		Skills:          LoadSkills(workspace),
		Workspace:       workspace,
		CronService:     cronSvc,
		SubagentManager: subMgr,
		activeTasks: make(map[string]map[int64]*taskInfo),
	}
	l.registerTools()
	return l
}

func (l *Loop) registerTools() {
	l.Tools.Register(&tools.ReadFileTool{Workspace: l.Workspace})
	l.Tools.Register(&tools.WriteFileTool{Workspace: l.Workspace})
	l.Tools.Register(&tools.EditFileTool{Workspace: l.Workspace})
	l.Tools.Register(&tools.ListDirTool{Workspace: l.Workspace})
	l.Tools.Register(&tools.ShellTool{Workspace: l.Workspace})
	l.Tools.Register(&tools.WebFetchTool{})
	l.Tools.Register(&tools.MessageTool{Outbound: l.Bus.Outbound})

	if l.Config.BraveAPIKey != "" {
		l.Tools.Register(&tools.WebSearchTool{APIKey: l.Config.BraveAPIKey})
	}

	// Query API (read-only)
	services := make(map[string]tools.ServiceInfo)
	for _, svc := range l.Config.Services {
		services[svc.Name] = tools.ServiceInfo{BaseURL: svc.BaseURL, Token: svc.Token}
	}
	if len(services) > 0 {
		l.Tools.Register(&tools.QueryAPITool{Services: services})
	}

	// Spawn sub-agent (async)
	l.Tools.Register(&tools.SpawnTool{Manager: l.SubagentManager})

	// Dynamic cron tool
	if l.CronService != nil {
		l.Tools.Register(&tools.CronTool{Service: l.CronService})
	}

	// MCP — connect to servers and register each tool natively
	var mcpServers []tools.McpServer
	for _, svc := range l.Config.Services {
		if svc.McpURL != "" || svc.McpCmd != "" {
			mcpServers = append(mcpServers, tools.McpServer{
				Name: svc.Name,
				URL:  svc.McpURL,
				Cmd:  svc.McpCmd,
			})
		}
	}
	if len(mcpServers) > 0 {
		for _, t := range tools.ConnectMcpServers(mcpServers) {
			l.Tools.Register(t)
		}
	}
}

// Run routes inbound messages. Every message spawns its own goroutine —
// both cross-chat and same-chat messages run fully in parallel.
// /stop cancels all active tasks for the session.
func (l *Loop) Run() {
	log.Println("[agent] loop started (fully concurrent)")
	for msg := range l.Bus.Inbound {
		// System messages (subagent results)
		if msg.Channel == "system" {
			go l.processSystemMessage(msg)
			continue
		}

		// Emit message.received for non-system messages
		l.Emitter.Emit(hooks.Event{
			Type:      hooks.EventMessageReceived,
			SessionID: msg.Channel + ":" + msg.ChatID,
			Data: map[string]any{
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
				"user":    msg.User,
				"text":    msg.Text,
			},
		})

		// /stop needs immediate execution
		text := strings.TrimSpace(msg.Text)
		if strings.EqualFold(text, "/stop") {
			go l.handleStop(msg)
			continue
		}

		// Spawn a goroutine for every message — fully concurrent
		go l.process(msg)
	}
}

func (l *Loop) process(msg bus.InboundMessage) {
	// Handle commands (lightweight, no LLM needed)
	text := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(text, "/") {
		l.handleCommand(msg)
		return
	}

	// Immediate ack — reply to the original message
	l.sendOutbound(bus.OutboundMessage{
		Channel: msg.Channel,
		ChatID:  msg.ChatID,
		Text:    "\U0001f504 收到，处理中...",
		ReplyTo: msg.MessageID,
	})

	// Check if memory consolidation is needed
	l.maybeConsolidate(msg.Channel, msg.ChatID)

	sessionKey := msg.Channel + ":" + msg.ChatID

	l.Emitter.Emit(hooks.Event{
		Type:      hooks.EventSessionStarted,
		SessionID: sessionKey,
		Data:      map[string]any{"channel": msg.Channel, "chat_id": msg.ChatID, "text": msg.Text},
	})

	// Set up cancellable context. Each task gets a unique ID so /stop can cancel all.
	ctx, cancel := context.WithCancel(context.Background())
	taskID := l.taskCounter.Add(1)
	taskText := msg.Text
	if len(taskText) > 80 {
		taskText = taskText[:80] + "..."
	}
	l.mu.Lock()
	if l.activeTasks[sessionKey] == nil {
		l.activeTasks[sessionKey] = make(map[int64]*taskInfo)
	}
	l.activeTasks[sessionKey][taskID] = &taskInfo{
		Cancel:  cancel,
		Text:    taskText,
		Started: time.Now(),
	}
	l.mu.Unlock()
	defer func() {
		cancel()
		l.mu.Lock()
		delete(l.activeTasks[sessionKey], taskID)
		if len(l.activeTasks[sessionKey]) == 0 {
			delete(l.activeTasks, sessionKey)
		}
		l.mu.Unlock()
	}()

	// Create per-request tool registry to avoid shared session state.
	// Stateless tools (read_file, exec, etc.) are shared safely.
	// Session-dependent tools (message, spawn, cron) get fresh instances.
	reqTools := l.Tools.Clone()

	reqMsgTool := &tools.MessageTool{Outbound: l.Bus.Outbound}
	reqMsgTool.SetContext(msg.Channel, msg.ChatID)
	reqMsgTool.StartTurn()
	reqTools.Register(reqMsgTool)

	reqTools.Register(&tools.SpawnTool{
		Manager: l.SubagentManager,
		Channel: msg.Channel,
		ChatID:  msg.ChatID,
	})

	if l.CronService != nil {
		reqTools.Register(&tools.CronTool{
			Service: l.CronService,
			Channel: msg.Channel,
			ChatID:  msg.ChatID,
		})
	}

	// Load session history
	history := l.Sessions.Load(msg.Channel, msg.ChatID, l.Config.MemoryWindow)

	// Build system prompt
	system := BuildSystemPrompt(l.Workspace, l.Memory, l.Skills, msg.Channel, msg.ChatID)

	// Assemble messages
	messages := []map[string]any{
		{"role": "system", "content": system},
	}
	messages = append(messages, history...)
	messages = append(messages, map[string]any{"role": "user", "content": BuildRuntimeContext(msg.Channel, msg.ChatID)})
	messages = append(messages, buildUserMessage(msg.Text, msg.Images, l.Workspace))

	// Save user message
	l.Sessions.Append(msg.Channel, msg.ChatID, map[string]any{"role": "user", "content": msg.Text})

	// ReAct loop with per-request tools
	final := l.reactLoop(ctx, messages, msg.Channel, msg.ChatID, reqTools)

	// If cancelled by /stop, don't save or send the response
	if ctx.Err() != nil {
		log.Printf("[agent] task cancelled for session %s", sessionKey)
		l.Emitter.Emit(hooks.Event{
			Type:      hooks.EventSessionCancelled,
			SessionID: sessionKey,
		})
		return
	}

	// Save assistant response
	l.Sessions.Append(msg.Channel, msg.ChatID, map[string]any{"role": "assistant", "content": final})

	// Skip sending if the message tool already sent output this turn
	if !reqMsgTool.SentInTurn() {
		l.sendOutbound(bus.OutboundMessage{
			Channel: msg.Channel,
			ChatID:  msg.ChatID,
			Text:    final,
		})
	}

	l.Emitter.Emit(hooks.Event{
		Type:      hooks.EventSessionCompleted,
		SessionID: sessionKey,
		Data:      map[string]any{"response": final},
	})
}

// processSystemMessage handles subagent result announcements.
func (l *Loop) processSystemMessage(msg bus.InboundMessage) {
	// Parse origin from chat_id ("channel:chat_id")
	parts := strings.SplitN(msg.ChatID, ":", 2)
	if len(parts) != 2 {
		log.Printf("[agent] system message with invalid chat_id: %s", msg.ChatID)
		return
	}
	channel, chatID := parts[0], parts[1]

	log.Printf("[agent] processing system message from %s for %s:%s", msg.User, channel, chatID)

	// Per-request tools for system messages
	reqTools := l.Tools.Clone()
	reqMsgTool := &tools.MessageTool{Outbound: l.Bus.Outbound}
	reqMsgTool.SetContext(channel, chatID)
	reqMsgTool.StartTurn()
	reqTools.Register(reqMsgTool)

	// Load session history
	history := l.Sessions.Load(channel, chatID, l.Config.MemoryWindow)

	// Build system prompt
	system := BuildSystemPrompt(l.Workspace, l.Memory, l.Skills, channel, chatID)

	messages := []map[string]any{
		{"role": "system", "content": system},
	}
	messages = append(messages, history...)
	messages = append(messages, map[string]any{"role": "user", "content": BuildRuntimeContext(channel, chatID)})
	messages = append(messages, map[string]any{"role": "user", "content": msg.Text})

	ctx := context.Background()
	final := l.reactLoop(ctx, messages, channel, chatID, reqTools)

	// Save to session
	l.Sessions.Append(channel, chatID, map[string]any{"role": "assistant", "content": final})

	if !reqMsgTool.SentInTurn() {
		l.sendOutbound(bus.OutboundMessage{
			Channel: channel,
			ChatID:  chatID,
			Text:    final,
		})
	}
}

func (l *Loop) reactLoop(ctx context.Context, messages []map[string]any, channel, chatID string, reqTools *tools.Registry) string {
	thinkRe := regexp.MustCompile(`(?s)<think>.*?</think>`)
	schemas := reqTools.Schemas()

	for i := 0; i < l.Config.MaxIter; i++ {
		// Check cancellation
		if ctx.Err() != nil {
			return "\u23f9 Task stopped."
		}

		sessionID := channel + ":" + chatID
		l.Emitter.Emit(hooks.Event{
			Type:      hooks.EventLLMRequest,
			SessionID: sessionID,
			Data:      map[string]any{"iteration": i},
		})

		resp, err := l.Provider.Chat(messages, schemas, l.Config.LLM.MaxTokens, l.Config.LLM.Temperature)
		if err != nil {
			// If the error is about image support, retry without images
			if i == 0 && strings.Contains(err.Error(), "image") {
				log.Printf("[agent] LLM image error, retrying without images: %v", err)
				messages = stripImages(messages)
				resp, err = l.Provider.Chat(messages, schemas, l.Config.LLM.MaxTokens, l.Config.LLM.Temperature)
			}
			if err != nil {
				log.Printf("[agent] LLM error: %v", err)
				return fmt.Sprintf("LLM error: %v", err)
			}
		}

		l.Emitter.Emit(hooks.Event{
			Type:      hooks.EventLLMResponse,
			SessionID: sessionID,
			Data: map[string]any{
				"has_tool_calls": resp.HasToolCalls(),
				"content_length": len(resp.Content),
			},
		})

		if !resp.HasToolCalls() {
			content := thinkRe.ReplaceAllString(resp.Content, "")
			content = strings.TrimSpace(content)
			if content == "" {
				content = "(no response)"
			}
			return content
		}

		// Send progress hint (gated by config)
		if channel != "" && chatID != "" && l.Config.ProgressEnabled() {
			hints := make([]string, 0, len(resp.ToolCalls))
			for _, tc := range resp.ToolCalls {
				hints = append(hints, toolHint(tc.Name, tc.Arguments))
			}
			var text string
			if l.Config.ToolHintsEnabled() {
				text = "\xe2\x9a\x99\xef\xb8\x8f [working]  " + strings.Join(hints, "  \xe2\x86\x92  ")
			} else {
				text = "\xe2\x9a\x99\xef\xb8\x8f [working...]"
			}
			l.sendOutbound(bus.OutboundMessage{
				Channel: channel,
				ChatID:  chatID,
				Text:    text,
			})
		}

		// Build assistant message with tool calls
		assistantMsg := map[string]any{"role": "assistant", "content": resp.Content}
		var tcList []map[string]any
		for _, tc := range resp.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			tcEntry := map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(argsJSON),
				},
			}
			if tc.ThoughtSignature != "" {
				tcEntry["extra_content"] = map[string]any{
					"google": map[string]any{
						"thought_signature": tc.ThoughtSignature,
					},
				}
			}
			tcList = append(tcList, tcEntry)
		}
		assistantMsg["tool_calls"] = tcList
		messages = append(messages, assistantMsg)

		// Execute tools
		for _, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				return "\u23f9 Task stopped."
			}
			log.Printf("[agent] tool: %s(%v)", tc.Name, tc.Arguments)

			l.Emitter.Emit(hooks.Event{
				Type:      hooks.EventToolStart,
				SessionID: sessionID,
				Data:      map[string]any{"tool": tc.Name, "args": tc.Arguments},
			})

			result := reqTools.Execute(tc.Name, tc.Arguments)

			l.Emitter.Emit(hooks.Event{
				Type:      hooks.EventToolEnd,
				SessionID: sessionID,
				Data:      map[string]any{"tool": tc.Name, "result_length": len(result)},
			})

			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": tc.ID,
				"content":      result,
			})
		}
	}

	return "(max iterations reached)"
}

// sendOutbound sends an outbound message and emits a message.sent event.
func (l *Loop) sendOutbound(msg bus.OutboundMessage) {
	l.Bus.Outbound <- msg
	l.Emitter.Emit(hooks.Event{
		Type:      hooks.EventMessageSent,
		SessionID: msg.Channel + ":" + msg.ChatID,
		Data: map[string]any{
			"channel": msg.Channel,
			"chat_id": msg.ChatID,
			"text":    msg.Text,
		},
	})
}

func (l *Loop) handleCommand(msg bus.InboundMessage) {
	cmd := strings.ToLower(strings.TrimSpace(msg.Text))
	var reply string

	switch cmd {
	case "/new":
		// Consolidate memory before clearing session
		allMsgs, lastConsolidated := l.Sessions.LoadAll(msg.Channel, msg.ChatID)
		unconsolidated := allMsgs[lastConsolidated:]
		if len(unconsolidated) > 0 {
			log.Printf("[memory] /new: consolidating %d messages before clear", len(unconsolidated))
			ok := l.Memory.Consolidate(l.Provider, unconsolidated, l.Config.LLM.MaxTokens, l.Config.LLM.Temperature, ConsolidateOpts{
				MaxMemoryBytes: l.Config.MaxMemoryBytes,
				Channel:        msg.Channel,
				ChatID:         msg.ChatID,
			})
			if ok {
				reply = "\U0001f9e0 Memory saved. Session cleared."
			} else {
				reply = "\u26a0\ufe0f Memory consolidation failed, but session cleared."
			}
		} else {
			reply = "Session cleared."
		}
		l.Sessions.Clear(msg.Channel, msg.ChatID)

	case "/stop":
		// /stop is handled in handleStop() which bypasses the queue.
		// This branch only triggers if /stop somehow ended up in the queue.
		reply = "Use /stop directly (it bypasses the queue)."

	case "/memory":
		global := l.Memory.LoadMemory()
		chatMem := l.Memory.LoadChatMemory(msg.Channel, msg.ChatID)
		reply = fmt.Sprintf("**Global Memory:**\n```\n%s\n```", global)
		if chatMem != "" {
			reply += fmt.Sprintf("\n\n**Chat Memory:**\n```\n%s\n```", chatMem)
		}

	case "/skills":
		if len(l.Skills) == 0 {
			reply = "No skills loaded."
		} else {
			lines := make([]string, len(l.Skills))
			for i, s := range l.Skills {
				lines[i] = fmt.Sprintf("- **%s**: %s", s.Name, s.Description)
			}
			reply = "Available skills:\n" + strings.Join(lines, "\n")
		}

	case "/queue":
		l.mu.Lock()
		var lines []string
		total := 0
		for session, tasks := range l.activeTasks {
			for _, ti := range tasks {
				elapsed := time.Since(ti.Started).Truncate(time.Second)
				lines = append(lines, fmt.Sprintf("- [%s] `%s` (%s)", session, ti.Text, elapsed))
				total++
			}
		}
		l.mu.Unlock()
		if total == 0 {
			reply = "No active tasks."
		} else {
			reply = fmt.Sprintf("\U0001f4cb **%d active task(s):**\n%s", total, strings.Join(lines, "\n"))
		}

	case "/help":
		reply = "\U0001f916 **CCMonet Bot Commands:**\n" +
			"- `/new` \u2014 Save memory & clear session\n" +
			"- `/stop` \u2014 Stop active tasks\n" +
			"- `/queue` \u2014 Show active tasks\n" +
			"- `/memory` \u2014 Show team memory\n" +
			"- `/skills` \u2014 List skills\n" +
			"- `/help` \u2014 Show this help"

	default:
		reply = fmt.Sprintf("Unknown command: %s. Try /help", cmd)
	}

	l.Emitter.Emit(hooks.Event{
		Type:      hooks.EventCommandExecuted,
		SessionID: msg.Channel + ":" + msg.ChatID,
		Data:      map[string]any{"command": cmd},
	})

	l.sendOutbound(bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Text: reply})
}

// handleStop processes /stop immediately.
// Cancels ALL active tasks for the session and subagents.
func (l *Loop) handleStop(msg bus.InboundMessage) {
	sessionKey := msg.Channel + ":" + msg.ChatID
	cancelled := 0

	// Cancel all active LLM tasks for this session
	l.mu.Lock()
	if tasks, ok := l.activeTasks[sessionKey]; ok {
		for id, ti := range tasks {
			ti.Cancel()
			delete(tasks, id)
			cancelled++
		}
		delete(l.activeTasks, sessionKey)
	}
	l.mu.Unlock()

	// Cancel subagents
	cancelled += l.SubagentManager.CancelBySession(sessionKey)

	var reply string
	if cancelled > 0 {
		reply = fmt.Sprintf("\u23f9 Stopped %d task(s).", cancelled)
	} else {
		reply = "No active task to stop."
	}
	l.sendOutbound(bus.OutboundMessage{Channel: msg.Channel, ChatID: msg.ChatID, Text: reply})
}

// maybeConsolidate checks if the session has accumulated enough unconsolidated
// messages and fires async LLM memory consolidation if so.
// Non-blocking — runs in a background goroutine so the user isn't waiting.
func (l *Loop) maybeConsolidate(channel, chatID string) {
	sessionKey := channel + ":" + chatID

	// Skip if already consolidating this session
	if _, running := l.consolidating.Load(sessionKey); running {
		return
	}

	total, lastConsolidated := l.Sessions.MessageCount(channel, chatID)
	unconsolidated := total - lastConsolidated

	if unconsolidated < l.Config.MemoryWindow {
		return
	}

	allMsgs, _ := l.Sessions.LoadAll(channel, chatID)
	keepCount := l.Config.MemoryWindow / 2
	if lastConsolidated >= len(allMsgs) {
		return
	}
	endIdx := len(allMsgs) - keepCount
	if endIdx <= lastConsolidated {
		return
	}
	toConsolidate := make([]map[string]any, len(allMsgs[lastConsolidated:endIdx]))
	copy(toConsolidate, allMsgs[lastConsolidated:endIdx])

	// Mark as consolidating and fire async
	l.consolidating.Store(sessionKey, true)
	log.Printf("[memory] async consolidation started: %d messages (window=%d)", len(toConsolidate), l.Config.MemoryWindow)

	go func() {
		defer l.consolidating.Delete(sessionKey)
		ok := l.Memory.Consolidate(l.Provider, toConsolidate, l.Config.LLM.MaxTokens, l.Config.LLM.Temperature, ConsolidateOpts{
			MaxMemoryBytes: l.Config.MaxMemoryBytes,
			Channel:        channel,
			ChatID:         chatID,
		})
		if ok {
			l.Sessions.UpdateConsolidated(channel, chatID, endIdx)
			log.Printf("[memory] async consolidation done: %d messages, new pointer=%d", len(toConsolidate), endIdx)
			l.Emitter.Emit(hooks.Event{
				Type:      hooks.EventMemoryUpdated,
				SessionID: sessionKey,
				Data:      map[string]any{"messages_consolidated": len(toConsolidate)},
			})
		} else {
			log.Printf("[memory] async consolidation failed for %s", sessionKey)
		}
	}()
}

func toolHint(name string, args map[string]any) string {
	switch name {
	case "exec":
		if cmd, ok := args["command"].(string); ok {
			cmd = strings.TrimSpace(cmd)
			if strings.HasPrefix(cmd, "cd ") {
				if idx := strings.Index(cmd, "&&"); idx != -1 {
					cmd = strings.TrimSpace(cmd[idx+2:])
				}
			}
			if len(cmd) > 50 {
				cmd = cmd[:50] + "..."
			}
			return "\xf0\x9f\x96\xa5 " + cmd
		}
	case "read_file":
		if p, ok := args["path"].(string); ok {
			return "\xf0\x9f\x93\x84 reading " + p
		}
	case "write_file":
		if p, ok := args["path"].(string); ok {
			return "\xe2\x9c\x8f\xef\xb8\x8f writing " + p
		}
	case "edit_file":
		if p, ok := args["path"].(string); ok {
			return "\xe2\x9c\x8f\xef\xb8\x8f editing " + p
		}
	case "list_dir":
		p, _ := args["path"].(string)
		if p == "" {
			p = "."
		}
		return "\xf0\x9f\x93\x82 listing " + p
	case "query_api":
		svc, _ := args["service"].(string)
		path, _ := args["path"].(string)
		return "\xf0\x9f\x94\x8c GET " + svc + path
	case "web_search":
		q, _ := args["query"].(string)
		if len(q) > 40 {
			q = q[:40] + "..."
		}
		return "\xf0\x9f\x94\x8d searching: " + q
	case "web_fetch":
		return "\xf0\x9f\x8c\x90 fetching URL"
	case "spawn":
		return "\xf0\x9f\xa4\x96 spawning sub-agent"
	case "cron":
		action, _ := args["action"].(string)
		return "\u23f0 cron " + action
	case "message":
		return "\xf0\x9f\x92\xac sending message"
	}
	// MCP tools have mcp_ prefix
	if strings.HasPrefix(name, "mcp_") {
		return "\xf0\x9f\x94\x97 " + name
	}
	return "\xf0\x9f\x94\xa7 " + name
}

// buildUserMessage creates a user message, using the multipart content array
// format when images are attached (OpenAI vision API compatible).
func buildUserMessage(text string, images []string, workspace string) map[string]any {
	if len(images) == 0 {
		return map[string]any{"role": "user", "content": text}
	}

	content := []map[string]any{
		{"type": "text", "text": text},
	}

	for _, img := range images {
		imgPath := img
		if !filepath.IsAbs(imgPath) {
			imgPath = filepath.Join(workspace, img)
		}

		data, err := os.ReadFile(imgPath)
		if err != nil {
			log.Printf("[agent] failed to read image %s: %v", img, err)
			continue
		}

		mime := http.DetectContentType(data)
		b64 := base64.StdEncoding.EncodeToString(data)
		dataURL := fmt.Sprintf("data:%s;base64,%s", mime, b64)

		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": dataURL,
			},
		})

		log.Printf("[agent] attached image: %s (%d bytes, %s)", img, len(data), mime)
	}

	return map[string]any{"role": "user", "content": content}
}

// stripImages converts multipart image messages back to text-only.
// Used as a fallback when the LLM provider doesn't support image input.
func stripImages(messages []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		// Check if content is a multipart array (vision format)
		if parts, ok := msg["content"].([]map[string]any); ok {
			var text string
			imgCount := 0
			for _, part := range parts {
				if t, _ := part["type"].(string); t == "text" {
					text, _ = part["text"].(string)
				} else if t == "image_url" {
					imgCount++
				}
			}
			if imgCount > 0 {
				clean := copyMsg(msg)
				if imgCount > 0 {
					text += fmt.Sprintf(" [%d image(s) attached but not supported by this model]", imgCount)
				}
				clean["content"] = text
				result = append(result, clean)
				continue
			}
		}
		result = append(result, msg)
	}
	return result
}

func copyMsg(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
