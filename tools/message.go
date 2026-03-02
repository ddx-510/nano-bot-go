package tools

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/PlatoX-Type/monet-bot/bus"
)

type MessageTool struct {
	Outbound chan<- bus.OutboundMessage

	mu          sync.Mutex
	sentInTurn  bool
	defChannel  string
	defChatID   string
}

func (t *MessageTool) Name() string        { return "message" }
func (t *MessageTool) Description() string { return "Send a message to a chat channel." }
func (t *MessageTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"channel": {"type": "string", "description": "Channel name (e.g. 'lark', 'cli')"},
			"chat_id": {"type": "string", "description": "Chat/group ID"},
			"text": {"type": "string", "description": "Message text"},
			"media": {"type": "array", "items": {"type": "string"}, "description": "File paths to attach (images, documents)"}
		},
		"required": ["text"]
	}`)
}

// SetContext sets the default channel/chat for this turn.
func (t *MessageTool) SetContext(channel, chatID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.defChannel = channel
	t.defChatID = chatID
}

// StartTurn resets per-turn tracking. Call before each message processing.
func (t *MessageTool) StartTurn() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sentInTurn = false
}

// SentInTurn returns true if the message tool was used during this turn.
func (t *MessageTool) SentInTurn() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sentInTurn
}

func (t *MessageTool) Execute(args map[string]any) (string, error) {
	channel, _ := args["channel"].(string)
	chatID, _ := args["chat_id"].(string)
	text, _ := args["text"].(string)

	// Parse media array
	var media []string
	if rawMedia, ok := args["media"].([]any); ok {
		for _, m := range rawMedia {
			if s, ok := m.(string); ok {
				media = append(media, s)
			}
		}
	}

	t.mu.Lock()
	if channel == "" {
		channel = t.defChannel
	}
	if chatID == "" {
		chatID = t.defChatID
	}
	t.mu.Unlock()

	if channel == "" || chatID == "" {
		return "Error: no target channel/chat_id specified", nil
	}

	t.Outbound <- bus.OutboundMessage{
		Channel: channel,
		ChatID:  chatID,
		Text:    text,
		Media:   media,
	}

	t.mu.Lock()
	t.sentInTurn = true
	t.mu.Unlock()

	mediaInfo := ""
	if len(media) > 0 {
		mediaInfo = fmt.Sprintf(" with %d attachment(s)", len(media))
	}
	return fmt.Sprintf("Message sent to %s:%s%s", channel, chatID, mediaInfo), nil
}
