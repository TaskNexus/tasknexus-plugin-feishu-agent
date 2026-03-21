// Package wsserver provides a WebSocket server that manages client connections
// and allows sending messages to specific clients by their unique ID.
package wsserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 4096
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client represents a connected WebSocket client.
type Client struct {
	ID   string
	conn *websocket.Conn
	send chan []byte
	hub  *Hub
}

// Hub maintains the set of active clients and dispatches messages to them.
type Hub struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

// NewHub creates a new Hub instance.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[string]*Client),
	}
}

// register adds a new client to the hub.
func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c.ID] = c
	h.mu.Unlock()
	fmt.Printf("[ws-server] client registered: %s (total: %d)\n", c.ID, h.clientCount())
}

// unregister removes a client from the hub and closes its send channel.
func (h *Hub) unregister(id string) {
	h.mu.Lock()
	if c, ok := h.clients[id]; ok {
		close(c.send)
		delete(h.clients, id)
	}
	h.mu.Unlock()
	fmt.Printf("[ws-server] client unregistered: %s (total: %d)\n", id, h.clientCount())
}

func (h *Hub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// SendToClient sends a JSON message to the client with the given ID.
// Returns an error if the client is not found.
func (h *Hub) SendToClient(id string, message interface{}) error {
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	h.mu.RLock()
	c, ok := h.clients[id]
	h.mu.RUnlock()

	if !ok {
		return fmt.Errorf("client %s not found", id)
	}

	select {
	case c.send <- data:
		return nil
	default:
		// send buffer full, drop the client
		h.unregister(id)
		return fmt.Errorf("client %s send buffer full, disconnected", id)
	}
}

// HandleWS upgrades an HTTP connection to WebSocket and registers the client.
func (h *Hub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Printf("[ws-server] upgrade error: %v\n", err)
		return
	}

	client := &Client{
		ID:   uuid.New().String(),
		conn: conn,
		send: make(chan []byte, 256),
		hub:  h,
	}

	h.register(client)

	// Send the assigned client ID to the newly connected client
	welcome := map[string]string{
		"type":      "connected",
		"client_id": client.ID,
	}
	welcomeData, _ := json.Marshal(welcome)
	client.send <- welcomeData

	// Start read/write pumps
	go client.writePump()
	go client.readPump()
}

// readPump reads messages from the WebSocket connection.
// It handles pong messages for keep-alive and cleans up on disconnect.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister(c.ID)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				fmt.Printf("[ws-server] read error for %s: %v\n", c.ID, err)
			}
			break
		}
		// Client messages are ignored (used only for keep-alive).
	}
}

// writePump writes messages from the send channel to the WebSocket connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				fmt.Printf("[ws-server] write error for %s: %v\n", c.ID, err)
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
