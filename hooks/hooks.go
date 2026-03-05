package hooks

import (
	"sync"
	"time"
)

// EventType identifies a lifecycle event in the agent.
type EventType string

const (
	// Message flow
	EventMessageReceived EventType = "message.received"
	EventMessageSent     EventType = "message.sent"

	// Agent processing
	EventSessionStarted   EventType = "session.started"
	EventSessionCompleted EventType = "session.completed"
	EventSessionCancelled EventType = "session.cancelled"

	// ReAct loop
	EventLLMRequest  EventType = "llm.request"
	EventLLMResponse EventType = "llm.response"
	EventToolStart   EventType = "tool.start"
	EventToolEnd     EventType = "tool.end"

	// Memory & state
	EventMemoryUpdated  EventType = "memory.updated"
	EventCommandExecuted EventType = "command.executed"

	// System
	EventSubagentStarted   EventType = "subagent.started"
	EventSubagentCompleted EventType = "subagent.completed"
)

// Event is a single lifecycle event emitted by the agent.
type Event struct {
	Type      EventType      `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	SessionID string         `json:"session_id,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// Hook receives lifecycle events from the agent.
type Hook interface {
	Name() string
	HandleEvent(event Event)
}

// Emitter dispatches events to registered hooks.
// All methods are nil-safe — callers can use a nil *Emitter without checks.
type Emitter struct {
	mu    sync.RWMutex
	hooks []Hook
}

// NewEmitter creates a new Emitter.
func NewEmitter() *Emitter {
	return &Emitter{}
}

// Register adds a hook. Safe to call concurrently.
func (e *Emitter) Register(h Hook) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.hooks = append(e.hooks, h)
	e.mu.Unlock()
}

// Emit dispatches an event to all registered hooks.
// Each hook runs in its own goroutine so the agent loop is never blocked.
func (e *Emitter) Emit(event Event) {
	if e == nil {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	e.mu.RLock()
	snapshot := make([]Hook, len(e.hooks))
	copy(snapshot, e.hooks)
	e.mu.RUnlock()

	for _, h := range snapshot {
		go h.HandleEvent(event)
	}
}
