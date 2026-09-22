package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsWriteWait      = 54 * time.Second
	wsPongWait       = 60 * time.Second
	wsPingPeriod     = (wsPongWait * 9) / 10
	wsMaxMessageSize = 512
	wsSendBufSize    = 256
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// TraceSummary is the lightweight payload sent over WebSocket
type TraceSummary struct {
	ID         string  `json:"id"`
	PhprayID   string  `json:"phpray_id"`
	Timestamp  int64   `json:"timestamp"`
	UID        uint32  `json:"uid"`
	Username   string  `json:"username,omitempty"`
	Host       string  `json:"host"`
	Method     string  `json:"method"`
	URI        string  `json:"uri"`
	Status     uint16  `json:"status"`
	DurationMs float64 `json:"duration_ms"`
	CPUUserMs  float64 `json:"cpu_user_ms"`
	CPUSysMs   float64 `json:"cpu_sys_ms"`
	MemoryMB   float64 `json:"memory_peak_mb"`
	DBCount    uint16  `json:"db_count"`
	DBMs       float64 `json:"db_ms"`
	HTTPCount  uint16  `json:"http_count"`
	HTTPMs     float64 `json:"http_ms"`
	FileCount  uint16  `json:"file_count"`
	FileMs     float64 `json:"file_ms"`
	WP         uint8   `json:"wp"`
	N1         uint8   `json:"n1"`
	Level      string  `json:"level"`
	ErrorCount int     `json:"error_count"`
	PhpVer     string  `json:"php_ver,omitempty"`
}

func traceToSummary(t *Trace) TraceSummary {
	s := TraceSummary{
		ID:         t.ID,
		PhprayID:   t.ID,
		Timestamp:  t.Ts,
		UID:        t.UID,
		Host:       t.Host,
		Method:     t.Method,
		URI:        t.URI,
		Status:     t.Status,
		DurationMs: t.DurationMs,
		CPUUserMs:  t.CPUUserMs,
		CPUSysMs:   t.CPUSysMs,
		MemoryMB:   t.MemoryMB,
		DBMs:       t.DBMs,
		HTTPMs:     t.HTTPMs,
		FileMs:     t.FileMs,
		WP:         t.WP,
		N1:         t.N1,
		Level:      t.Level,
		ErrorCount: len(t.Errors),
		PhpVer:     t.PhpVer,
	}
	if t.DBCount != nil {
		s.DBCount = *t.DBCount
	}
	if t.HTTPCount != nil {
		s.HTTPCount = *t.HTTPCount
	}
	if t.FileCount != nil {
		s.FileCount = *t.FileCount
	}
	return s
}

// Hub manages WebSocket clients and broadcasts traces
type Hub struct {
	clients    map[*wsClient]struct{}
	broadcast  chan Trace
	register   chan *wsClient
	unregister chan *wsClient
	users      *UserResolver
}

// NewHub creates a new WebSocket hub
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*wsClient]struct{}),
		broadcast:  make(chan Trace, 256),
		register:   make(chan *wsClient),
		unregister: make(chan *wsClient),
	}
}

// Run processes hub events — must run in its own goroutine
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			h.clients[c] = struct{}{}
			log.Printf("WS client connected (%d total)", len(h.clients))

		case c := <-h.unregister:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				close(c.send)
				log.Printf("WS client disconnected (%d total)", len(h.clients))
			}

		case t := <-h.broadcast:
			if len(h.clients) == 0 {
				continue
			}
			// Serialize once for all clients
			summary := traceToSummary(&t)
			// Enrich with username
			if h.users != nil {
				summary.Username = h.users.Resolve(t.UID)
			}
			msg, err := json.Marshal(summary)
			if err != nil {
				log.Printf("WS marshal error: %v", err)
				continue
			}
			for c := range h.clients {
				select {
				case c.send <- msg:
				default:
					// Client buffer full — drop
					delete(h.clients, c)
					close(c.send)
				}
			}
		}
	}
}

// Broadcast sends a trace to all connected clients
func (h *Hub) Broadcast(t Trace) {
	select {
	case h.broadcast <- t:
	default:
		// Broadcast channel full — drop
	}
}

// wsClient represents a single WebSocket connection
type wsClient struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// readPump reads from the WebSocket connection (for close detection)
func (c *wsClient) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(wsMaxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsPongWait))
		return nil
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			break
		}
	}
}

// writePump writes messages to the WebSocket connection
func (c *wsClient) writePump() {
	ticker := time.NewTicker(wsPingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
