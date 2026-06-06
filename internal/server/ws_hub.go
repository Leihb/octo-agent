package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsHub manages all active WebSocket connections and routes events to
// subscribed sessions.
type wsHub struct {
	// connections is the set of registered connections.
	connections map[*wsConn]struct{}

	// sessions maps session_id → set of subscribed connections.
	sessions map[string]map[*wsConn]struct{}

	// register sends new connections to the hub.
	register chan *wsConn

	// unregister removes closed connections.
	unregister chan *wsConn

	// events carries outbound events that the server wants to broadcast.
	events chan wsEventEnvelope

	// s is the owning Server, set via init().
	s wsHubOwner

	mu sync.Mutex
}

// wsEventEnvelope wraps an event with optional session targeting.
type wsEventEnvelope struct {
	SessionID string
	Event     any // will be JSON-marshalled
}

// wsConn represents a single WebSocket connection.
type wsConn struct {
	hub        *wsHub
	conn       *websocket.Conn
	send       chan []byte
	subscribed map[string]struct{} // subscribed session IDs
}

const (
	wsWriteWait      = 10 * time.Second
	wsPongWait       = 60 * time.Second
	wsPingPeriod     = (wsPongWait * 9) / 10
	wsMaxMessageSize = 512 * 1024 // 512 KB
)

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // allow all origins for local dev
	},
}

// newWSHub creates and starts a WebSocket hub.
func newWSHub() *wsHub {
	h := &wsHub{
		connections: make(map[*wsConn]struct{}),
		sessions:    make(map[string]map[*wsConn]struct{}),
		register:    make(chan *wsConn),
		unregister:  make(chan *wsConn),
		events:      make(chan wsEventEnvelope, 256),
	}
	go h.run()
	return h
}

// run is the hub's main loop, handling registration/unregistration and
// event broadcast routing.
func (h *wsHub) run() {
	for {
		select {
		case conn := <-h.register:
			h.mu.Lock()
			h.connections[conn] = struct{}{}
			h.mu.Unlock()

		case conn := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.connections[conn]; ok {
				delete(h.connections, conn)
				close(conn.send)
				// Remove conn from all session subscriptions.
				for sid := range conn.subscribed {
					if subs, ok := h.sessions[sid]; ok {
						delete(subs, conn)
						if len(subs) == 0 {
							delete(h.sessions, sid)
						}
					}
				}
			}
			h.mu.Unlock()

		case env := <-h.events:
			b, err := json.Marshal(env.Event)
			if err != nil {
				log.Printf("[ws] marshal event: %v", err)
				continue
			}
			h.mu.Lock()
			if env.SessionID != "" {
				// Targeted broadcast to session subscribers.
				if subs, ok := h.sessions[env.SessionID]; ok {
					for conn := range subs {
						select {
						case conn.send <- b:
						default:
							// Slow consumer — drop.
						}
					}
				}
			} else {
				// Global broadcast to all connections.
				for conn := range h.connections {
					select {
					case conn.send <- b:
					default:
					}
				}
			}
			h.mu.Unlock()
		}
	}
}

// subscribe adds a connection to a session's subscriber set.
func (h *wsHub) subscribe(conn *wsConn, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn.subscribed[sessionID] = struct{}{}
	if h.sessions[sessionID] == nil {
		h.sessions[sessionID] = make(map[*wsConn]struct{})
	}
	h.sessions[sessionID][conn] = struct{}{}
}

// unsubscribe removes a connection from a session's subscriber set.
func (h *wsHub) unsubscribe(conn *wsConn, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(conn.subscribed, sessionID)
	if subs, ok := h.sessions[sessionID]; ok {
		delete(subs, conn)
		if len(subs) == 0 {
			delete(h.sessions, sessionID)
		}
	}
}

// broadcast sends an event to all subscribers of a session, or globally if
// sessionID is empty.
func (h *wsHub) broadcast(sessionID string, event any) {
	h.events <- wsEventEnvelope{SessionID: sessionID, Event: event}
}

// handleWS upgrades an HTTP connection to WebSocket and registers it with
// the hub. Auth is already checked by requireAuth middleware; this handler
// only upgrades the connection.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws] upgrade: %v", err)
		return
	}

	c := &wsConn{
		hub:        s.wsHub,
		conn:       conn,
		send:       make(chan []byte, 256),
		subscribed: make(map[string]struct{}),
	}

	s.wsHub.register <- c

	// Start write pump in a goroutine.
	go c.writePump()
	// Run read pump in the current goroutine (blocks until disconnect).
	c.readPump()
}

// writePump pumps messages from the send channel to the WebSocket connection.
func (c *wsConn) writePump() {
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

// readPump reads messages from the WebSocket connection and dispatches them.
func (c *wsConn) readPump() {
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
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("[ws] read: %v", err)
			}
			break
		}

		var msg wsInMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			log.Printf("[ws] unmarshal inbound: %v", err)
			continue
		}

		c.dispatch(msg.Type, raw)
	}
}

// dispatch routes an inbound WS message to the appropriate handler.
func (c *wsConn) dispatch(msgType string, raw []byte) {
	switch msgType {
	case "list_sessions":
		sessions := c.hub.s.listSessionsBrief()
		b, _ := json.Marshal(wsEventSessionList{
			Type:     "session_list",
			Sessions: sessions,
		})
		c.send <- b

	case "subscribe":
		var msg wsMsgSubscribe
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.subscribe(c, msg.SessionID)
		c.hub.s.SetSubscribed(c, msg.SessionID)
		// Confirm subscription so the frontend can enable input.
		b, _ := json.Marshal(map[string]any{"type": "subscribed", "session_id": msg.SessionID})
		c.send <- b
		// Replay any in-progress live state for this session.
		c.hub.s.replayLiveState(msg.SessionID, c)

	case "unsubscribe":
		var msg wsMsgUnsubscribe
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.unsubscribe(c, msg.SessionID)

	case "user_message", "message":
		var msg wsMsgUserMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.s.handleWSUserMessage(c, &msg)

	case "interrupt":
		var msg wsMsgInterrupt
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.s.handleWSInterrupt(msg.SessionID)

	case "retry":
		var msg wsMsgRetry
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.s.handleWSRetry(c, msg.SessionID)

	case "run_task":
		var msg wsMsgRunTask
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.s.handleWSRunTask(c, msg.SessionID)

	case "confirmation":
		var msg wsMsgConfirmation
		if err := json.Unmarshal(raw, &msg); err != nil {
			return
		}
		c.hub.s.handleWSConfirmation(msg.ConfID, msg.Result)

	default:
		log.Printf("[ws] unknown message type: %q", msgType)
	}
}

// ─── Hub field helper ──────────────────────────────────────────────────────
// wsHub needs a pointer to Server to call session methods. Break import cycle
// by embedding a hub field via interface.

type wsHubOwner interface {
	listSessionsBrief() []wsSessionInfo
	handleWSUserMessage(conn *wsConn, msg *wsMsgUserMessage)
	handleWSInterrupt(sessionID string)
	handleWSRetry(conn *wsConn, sessionID string)
	handleWSRunTask(conn *wsConn, sessionID string)
	handleWSConfirmation(confID, result string)
	replayLiveState(sessionID string, conn *wsConn)
	SetSubscribed(conn *wsConn, sessionID string)
}

func (h *wsHub) init(s wsHubOwner) {
	h.s = s
}
