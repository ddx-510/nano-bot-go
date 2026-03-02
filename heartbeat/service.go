package heartbeat

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PlatoX-Type/monet-bot/bus"
	"github.com/PlatoX-Type/monet-bot/providers"
)

// heartbeatTool is the tool schema the LLM uses to decide skip or run.
var heartbeatTool = []map[string]any{
	{
		"type": "function",
		"function": map[string]any{
			"name":        "heartbeat",
			"description": "Report your decision on whether to run or skip this heartbeat tick.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action": map[string]any{
						"type":        "string",
						"enum":        []string{"skip", "run"},
						"description": "Whether to skip this tick or run the tasks.",
					},
					"tasks": map[string]any{
						"type":        "string",
						"description": "If action is 'run', a summary of the tasks to execute.",
					},
				},
				"required": []string{"action"},
			},
		},
	},
}

type Service struct {
	bus       *bus.MessageBus
	provider  *providers.Provider
	workspace string
	interval  time.Duration
	maxTokens int
	temp      float64
}

func New(mb *bus.MessageBus, provider *providers.Provider, workspace string, intervalMin int, maxTokens int, temp float64) *Service {
	return &Service{
		bus:       mb,
		provider:  provider,
		workspace: workspace,
		interval:  time.Duration(intervalMin) * time.Minute,
		maxTokens: maxTokens,
		temp:      temp,
	}
}

func (s *Service) Run() {
	log.Printf("[heartbeat] started (every %v)", s.interval)
	for {
		time.Sleep(s.interval)

		path := filepath.Join(s.workspace, "HEARTBEAT.md")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		content := strings.TrimSpace(string(data))
		if content == "" {
			continue
		}

		action, tasks := s.decide(content)
		if action != "run" {
			log.Println("[heartbeat] LLM decided to skip")
			continue
		}

		log.Printf("[heartbeat] LLM decided to run: %s", tasks)
		s.bus.Inbound <- bus.InboundMessage{
			Channel:   "system",
			ChatID:    "heartbeat",
			User:      "heartbeat",
			Text:      "[Heartbeat] Process the following tasks:\n\n" + tasks,
			Timestamp: time.Now(),
		}
	}
}

// decide asks the LLM whether to skip or run, using a tool call.
func (s *Service) decide(heartbeatContent string) (action, tasks string) {
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC (Monday)")

	messages := []map[string]any{
		{
			"role":    "system",
			"content": "You are a heartbeat agent. Your job is to decide whether scheduled tasks need to run right now. Call the heartbeat tool to report your decision. Consider the current time, day of week, and whether the tasks are relevant right now.",
		},
		{
			"role": "user",
			"content": "Current time: " + now + "\n\nScheduled tasks:\n\n" + heartbeatContent +
				"\n\nDecide whether to run these tasks now or skip this tick. Call the heartbeat tool with your decision.",
		},
	}

	resp, err := s.provider.Chat(messages, heartbeatTool, s.maxTokens, s.temp)
	if err != nil {
		log.Printf("[heartbeat] LLM decision error: %v — defaulting to run", err)
		return "run", heartbeatContent
	}

	// If no tool calls, default to skip (LLM didn't follow instructions)
	if !resp.HasToolCalls() {
		log.Println("[heartbeat] LLM did not call heartbeat tool — defaulting to skip")
		return "skip", ""
	}

	for _, tc := range resp.ToolCalls {
		if tc.Name != "heartbeat" {
			continue
		}
		a, _ := tc.Arguments["action"].(string)
		t, _ := tc.Arguments["tasks"].(string)
		if a == "" {
			a = "skip"
		}
		if t == "" {
			t = heartbeatContent
		}
		return a, t
	}

	return "skip", ""
}

// TriggerNow runs the decide+execute flow immediately, bypassing the interval.
// Returns the tasks string if action is "run", or empty if skipped.
func (s *Service) TriggerNow() string {
	path := filepath.Join(s.workspace, "HEARTBEAT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return ""
	}

	action, tasks := s.decide(content)
	if action != "run" {
		return ""
	}

	s.bus.Inbound <- bus.InboundMessage{
		Channel:   "system",
		ChatID:    "heartbeat",
		User:      "heartbeat",
		Text:      "[Heartbeat] Process the following tasks:\n\n" + tasks,
		Timestamp: time.Now(),
	}
	return tasks
}

