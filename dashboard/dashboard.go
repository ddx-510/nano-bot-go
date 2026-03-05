package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PlatoX-Type/monet-bot/bus"
	"github.com/PlatoX-Type/monet-bot/hooks"
)

//go:embed static/index.html
var staticFS embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

const maxRecentEvents = 200

// Dashboard implements both channels.Channel and hooks.Hook.
// It serves a web UI over HTTP and streams events via WebSocket.
type Dashboard struct {
	bus       *bus.MessageBus
	hub       *Hub
	port      int
	startTime time.Time

	mu     sync.Mutex
	recent []hooks.Event
}

// New creates a new Dashboard.
func New(mb *bus.MessageBus, port int) *Dashboard {
	if port == 0 {
		port = 8080
	}
	return &Dashboard{
		bus:       mb,
		hub:       newHub(),
		port:      port,
		startTime: time.Now(),
		recent:    make([]hooks.Event, 0, maxRecentEvents),
	}
}

// --- channels.Channel interface ---

func (d *Dashboard) Name() string { return "dashboard" }

// Start launches the HTTP server. Called as a goroutine by the channel manager.
func (d *Dashboard) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/ws", d.handleWS)
	mux.HandleFunc("/api/state", d.handleState)

	addr := fmt.Sprintf(":%d", d.port)
	log.Printf("[dashboard] listening on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("[dashboard] server error: %v", err)
	}
}

// Send forwards an outbound message to WebSocket clients.
func (d *Dashboard) Send(chatID, text string) {
	msg := map[string]any{
		"type": "outbound_message",
		"data": map[string]any{
			"chat_id": chatID,
			"text":    text,
		},
		"timestamp": time.Now(),
	}
	data, _ := json.Marshal(msg)
	d.hub.Broadcast(data)
}

// --- hooks.Hook interface (Name() already satisfies both) ---

// HandleEvent receives lifecycle events and broadcasts them to WebSocket clients.
func (d *Dashboard) HandleEvent(event hooks.Event) {
	// Store in ring buffer
	d.mu.Lock()
	if len(d.recent) >= maxRecentEvents {
		d.recent = d.recent[1:]
	}
	d.recent = append(d.recent, event)
	d.mu.Unlock()

	// Broadcast to all connected browsers
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	d.hub.Broadcast(data)
}

// --- HTTP handlers ---

func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	content, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index.html not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func (d *Dashboard) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[dashboard] ws upgrade error: %v", err)
		return
	}

	c := &client{conn: conn, send: make(chan []byte, 64)}
	d.hub.register(c)

	// Send recent events to the newly connected client
	d.mu.Lock()
	snapshot := make([]hooks.Event, len(d.recent))
	copy(snapshot, d.recent)
	d.mu.Unlock()

	for _, ev := range snapshot {
		data, _ := json.Marshal(ev)
		select {
		case c.send <- data:
		default:
		}
	}

	// Start write pump
	go d.hub.writePump(c)

	// Read pump — handle incoming messages from the browser
	go d.readPump(c)
}

func (d *Dashboard) readPump(c *client) {
	defer func() {
		d.hub.unregister(c)
		c.conn.Close()
	}()
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			break
		}

		var msg struct {
			Action string `json:"action"`
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg.Action == "send_message" && msg.Text != "" {
			chatID := msg.ChatID
			if chatID == "" {
				chatID = "dashboard"
			}
			d.bus.Inbound <- bus.InboundMessage{
				Channel:   "dashboard",
				ChatID:    chatID,
				User:      "dashboard-user",
				Text:      msg.Text,
				Timestamp: time.Now(),
			}
		}
	}
}

func (d *Dashboard) handleState(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	snapshot := make([]hooks.Event, len(d.recent))
	copy(snapshot, d.recent)
	d.mu.Unlock()

	state := map[string]any{
		"uptime_seconds": time.Since(d.startTime).Seconds(),
		"recent_events":  snapshot,
		"connected":      len(d.hub.clients),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}
