package internal

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSMessage is the envelope sent to all websocket clients.
type WSMessage struct {
	Event   string      `json:"event"`
	Payload interface{} `json:"payload"`
	Time    time.Time   `json:"time"`
}

// Hub manages websocket connections and broadcasts events.
type Hub struct {
	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
	upgrader websocket.Upgrader
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

func (h *Hub) Broadcast(event string, payload interface{}) {
	msg := WSMessage{Event: event, Payload: payload, Time: time.Now()}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
			h.remove(c)
		}
	}
}

func (h *Hub) remove(c *websocket.Conn) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		c.Close()
	}
	h.mu.Unlock()
}

func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()

	// initial snapshot
	snapshot := map[string]interface{}{
		"workers": getStoreSnapshot(),
	}
	init := WSMessage{Event: "init", Payload: snapshot, Time: time.Now()}
	if data, err := json.Marshal(init); err == nil {
		conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		conn.WriteMessage(websocket.TextMessage, data)
	}

	// read loop (just to detect close)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			h.remove(conn)
			return
		}
	}
}

// package-level accessor used by HandleWS — set by Server via SetStore.
var storeRef *Store

func SetStoreRef(s *Store) { storeRef = s }

func getStoreSnapshot() map[string]interface{} {
	if storeRef == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"workers": storeRef.ListWorkers(),
		"tasks":   storeRef.ListTasks(),
	}
}
