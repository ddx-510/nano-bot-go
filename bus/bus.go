package bus

import "time"

type InboundMessage struct {
	Channel   string    `json:"channel"`
	ChatID    string    `json:"chat_id"`
	User      string    `json:"user"`
	Text      string    `json:"text"`
	MessageID string    `json:"message_id,omitempty"` // original message ID for replies
	Images    []string  `json:"images,omitempty"`     // file paths to attached images
	Timestamp time.Time `json:"timestamp"`
}

type OutboundMessage struct {
	Channel string   `json:"channel"`
	ChatID  string   `json:"chat_id"`
	Text    string   `json:"text"`
	ReplyTo string   `json:"reply_to,omitempty"` // message ID to reply to
	Media   []string `json:"media,omitempty"`    // file paths to attach (images, docs)
}

type MessageBus struct {
	Inbound  chan InboundMessage
	Outbound chan OutboundMessage
}

func New() *MessageBus {
	return &MessageBus{
		Inbound:  make(chan InboundMessage, 100),
		Outbound: make(chan OutboundMessage, 100),
	}
}
