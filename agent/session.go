package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Session manages JSONL conversation persistence with consolidation tracking.
// All file operations are serialized per session path for goroutine safety.
type Session struct {
	dir   string
	locks sync.Map // path -> *sync.Mutex
}

func NewSession(workspace string) *Session {
	dir := filepath.Join(workspace, "sessions")
	os.MkdirAll(dir, 0o755)
	return &Session{dir: dir}
}

func (s *Session) path(channel, chatID string) string {
	safe := strings.NewReplacer("/", "_", ":", "_").Replace(chatID)
	return filepath.Join(s.dir, fmt.Sprintf("%s_%s.jsonl", channel, safe))
}

// fileLock returns a per-path mutex for serializing file access.
func (s *Session) fileLock(channel, chatID string) *sync.Mutex {
	p := s.path(channel, chatID)
	mu, _ := s.locks.LoadOrStore(p, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// SessionMeta stores metadata at the top of the JSONL file.
type SessionMeta struct {
	Type             string `json:"_type"`
	Key              string `json:"key"`
	LastConsolidated int    `json:"last_consolidated"`
	UpdatedAt        string `json:"updated_at"`
}

// loadMeta reads the metadata line (first line) from a session file.
func (s *Session) loadMeta(channel, chatID string) *SessionMeta {
	p := s.path(channel, chatID)
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	if scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		var meta SessionMeta
		if json.Unmarshal([]byte(line), &meta) == nil && meta.Type == "metadata" {
			return &meta
		}
	}
	return nil
}

// Load returns the last `limit` unconsolidated messages for a session.
func (s *Session) Load(channel, chatID string, limit int) []map[string]any {
	mu := s.fileLock(channel, chatID)
	mu.Lock()
	defer mu.Unlock()

	messages, lastConsolidated := s.loadAll(channel, chatID)

	// Only return unconsolidated messages
	if lastConsolidated > 0 && lastConsolidated < len(messages) {
		messages = messages[lastConsolidated:]
	}

	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}

	// Drop leading non-user messages to avoid orphaned tool results
	for i, m := range messages {
		if role, _ := m["role"].(string); role == "user" {
			messages = messages[i:]
			break
		}
	}

	return messages
}

// LoadAll returns ALL messages (including consolidated ones) for a session.
func (s *Session) LoadAll(channel, chatID string) ([]map[string]any, int) {
	mu := s.fileLock(channel, chatID)
	mu.Lock()
	defer mu.Unlock()
	return s.loadAll(channel, chatID)
}

// loadAll is the internal unlocked version.
func (s *Session) loadAll(channel, chatID string) ([]map[string]any, int) {
	p := s.path(channel, chatID)
	f, err := os.Open(p)
	if err != nil {
		return nil, 0
	}
	defer f.Close()

	var lastConsolidated int
	var messages []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var meta SessionMeta
		if json.Unmarshal([]byte(line), &meta) == nil && meta.Type == "metadata" {
			lastConsolidated = meta.LastConsolidated
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if _, ok := msg["role"]; ok {
			messages = append(messages, msg)
		}
	}
	return messages, lastConsolidated
}

// Append adds a message to the session, truncating large tool results.
func (s *Session) Append(channel, chatID string, msg map[string]any) {
	mu := s.fileLock(channel, chatID)
	mu.Lock()
	defer mu.Unlock()

	p := s.path(channel, chatID)
	msg["timestamp"] = time.Now().UTC().Format(time.RFC3339)

	// Truncate large tool result content to keep sessions lean
	if role, _ := msg["role"].(string); role == "tool" {
		if content, ok := msg["content"].(string); ok && len(content) > 500 {
			msg["content"] = content[:500] + "\n... (truncated)"
		}
	}

	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	data, _ := json.Marshal(msg)
	fmt.Fprintln(f, string(data))
}

// MessageCount returns the total number of messages and the last_consolidated pointer.
func (s *Session) MessageCount(channel, chatID string) (total int, lastConsolidated int) {
	mu := s.fileLock(channel, chatID)
	mu.Lock()
	defer mu.Unlock()
	allMsgs, lc := s.loadAll(channel, chatID)
	return len(allMsgs), lc
}

// UpdateConsolidated rewrites the session file with an updated last_consolidated pointer.
func (s *Session) UpdateConsolidated(channel, chatID string, lastConsolidated int) {
	mu := s.fileLock(channel, chatID)
	mu.Lock()
	defer mu.Unlock()
	allMsgs, _ := s.loadAll(channel, chatID)
	s.rewriteUnlocked(channel, chatID, allMsgs, lastConsolidated)
}

// Clear archives and resets a session.
func (s *Session) Clear(channel, chatID string) {
	mu := s.fileLock(channel, chatID)
	mu.Lock()
	defer mu.Unlock()

	p := s.path(channel, chatID)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return
	}

	archive := filepath.Join(s.dir, "archive")
	os.MkdirAll(archive, 0o755)

	ts := time.Now().UTC().Format("20060102_150405")
	base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	os.Rename(p, filepath.Join(archive, fmt.Sprintf("%s_%s.jsonl", base, ts)))
}

// rewriteUnlocked saves all messages with updated metadata. Caller must hold the file lock.
func (s *Session) rewriteUnlocked(channel, chatID string, messages []map[string]any, lastConsolidated int) {
	p := s.path(channel, chatID)
	f, err := os.Create(p)
	if err != nil {
		return
	}
	defer f.Close()

	// Write metadata line
	meta := SessionMeta{
		Type:             "metadata",
		Key:              channel + ":" + chatID,
		LastConsolidated: lastConsolidated,
		UpdatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	metaJSON, _ := json.Marshal(meta)
	fmt.Fprintln(f, string(metaJSON))

	// Write messages
	for _, msg := range messages {
		data, _ := json.Marshal(msg)
		fmt.Fprintln(f, string(data))
	}
}
