package dashboard

import (
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

// client is a single WebSocket connection.
type client struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub manages WebSocket clients and broadcasts messages to all of them.
type Hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
}

func newHub() *Hub {
	return &Hub{clients: make(map[*client]struct{})}
}

func (h *Hub) register(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

// Broadcast sends data to all connected clients (non-blocking per client).
func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			// Client is too slow — drop and disconnect
			go h.unregister(c)
		}
	}
}

// writePump pumps messages from the send channel to the WebSocket connection.
func (h *Hub) writePump(c *client) {
	defer func() {
		c.conn.Close()
		h.unregister(c)
	}()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Printf("[dashboard] ws write error: %v", err)
			return
		}
	}
}
