package channels

import "github.com/PlatoX-Type/monet-bot/bus"

// Channel is the interface for chat platform adapters.
type Channel interface {
	Name() string
	Start()
	Send(chatID, text string)
}

// Replier is an optional interface for channels that support replying to specific messages.
type Replier interface {
	Reply(chatID, text, replyTo string)
}

// Manager routes outbound messages to the correct channel.
type Manager struct {
	bus      *bus.MessageBus
	channels map[string]Channel
}

func NewManager(mb *bus.MessageBus) *Manager {
	return &Manager{bus: mb, channels: make(map[string]Channel)}
}

func (m *Manager) Register(ch Channel) {
	m.channels[ch.Name()] = ch
}

// StartAll starts all channels and the outbound router.
func (m *Manager) StartAll() {
	for _, ch := range m.channels {
		go ch.Start()
	}
	// Route outbound messages
	for msg := range m.bus.Outbound {
		if ch, ok := m.channels[msg.Channel]; ok {
			if msg.ReplyTo != "" {
				if r, ok := ch.(Replier); ok {
					r.Reply(msg.ChatID, msg.Text, msg.ReplyTo)
					continue
				}
			}
			ch.Send(msg.ChatID, msg.Text)
		}
	}
}
