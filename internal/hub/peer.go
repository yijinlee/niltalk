package hub

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
)

// Peer represents an individual peer / connection into a room.
type Peer struct {
	// Peer's chat handle.
	ID     string
	Handle string

	ws *websocket.Conn

	// Channel for outbound messages.
	dataQ chan []byte

	// Peer's room.
	room *Room

	// Rate limiting.
	numMessages int
	lastMessage time.Time
}

type peerInfo struct {
	ID     string `json:"id"`
	Handle string `json:"handle"`
}

// newPeer returns a new instance of Peer.
func newPeer(id, handle string, ws *websocket.Conn, room *Room) *Peer {
	return &Peer{
		ID:     id,
		Handle: handle,
		ws:     ws,
		dataQ:  make(chan []byte, 100),
		room:   room,
	}
}

// RunListener is a blocking function that reads incoming messages from a peer's
// WS connection until its dropped or there's an error. This should be invoked
// as a goroutine.
func (p *Peer) RunListener() {
	p.ws.SetReadLimit(int64(p.room.hub.cfg.MaxMessageLen))
	for {
		_, m, err := p.ws.ReadMessage()
		if err != nil {
			break
		}
		p.processMessage(m)
	}

	// WS connection is closed.
	p.ws.Close()
	p.room.queuePeerReq(TypePeerLeave, p)
}

// RunWriter is a blocking function that writes messages in a peer's queue to the
// peer's WS connection. This should be invoked as a goroutine.
func (p *Peer) RunWriter() {
	defer p.ws.Close()
	for {
		select {
		// Wait for outgoing message to appear in the channel.
		case message, ok := <-p.dataQ:
			if !ok {
				p.writeWSData(websocket.CloseMessage, []byte{})
				return
			}
			if err := p.writeWSData(websocket.TextMessage, message); err != nil {
				return
			}
		}
	}
}

// SendData queues a message to be written to the peer's WS.
func (p *Peer) SendData(b []byte) {
	p.dataQ <- b
}

// writeWSData writes the given payload to the peer's WS connection.
func (p *Peer) writeWSData(msgType int, payload []byte) error {
	p.ws.SetWriteDeadline(time.Now().Add(p.room.hub.cfg.WSTimeout))
	return p.ws.WriteMessage(msgType, payload)
}

// writeWSControl writes the given control payload to the peer's WS connection.
func (p *Peer) writeWSControl(control int, payload []byte) error {
	return p.ws.WriteControl(websocket.CloseMessage, payload, time.Time{})
}

// processMessage processes incoming messages from peers.
func (p *Peer) processMessage(b []byte) {
	var m payloadMsgWrap

	if err := json.Unmarshal(b, &m); err != nil {
		// TODO: Respond
		return
	}

	switch m.Type {
	// Message to the room.
	case TypeMessage:
		// Check rate limits and update counters.
		now := time.Now()
		if p.numMessages > 0 {
			if (p.numMessages%p.room.hub.cfg.RateLimitMessages+1) >= p.room.hub.cfg.RateLimitMessages &&
				time.Since(p.lastMessage) < p.room.hub.cfg.RateLimitInterval {
				p.room.hub.Store.RemoveSession(p.ID, p.room.ID)
				p.writeWSControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, TypePeerRateLimited))
				p.ws.Close()
				return
			}
		}
		p.lastMessage = now
		p.numMessages++

		msg, ok := m.Data.(string)
		if !ok {
			// TODO: Respond
			return
		}
		p.room.Broadcast(p.room.makeMessagePayload(msg, p, m.Type), true)

	case TypeUploading:
		data, ok := m.Data.(map[string]interface{})
		if !ok {
			// TODO: Respond
			return
		}
		p.room.Broadcast(p.room.makeUploadPayload(data, p, m.Type), false)

	case TypeUpload:
		// Check rate limits and update counters.
		now := time.Now()
		if p.numMessages > 0 {
			if (p.numMessages%p.room.hub.cfg.RateLimitMessages+1) >= p.room.hub.cfg.RateLimitMessages &&
				time.Since(p.lastMessage) < p.room.hub.cfg.RateLimitInterval {
				p.room.hub.Store.RemoveSession(p.ID, p.room.ID)
				p.writeWSControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, TypePeerRateLimited))
				p.ws.Close()
				return
			}
		}
		p.lastMessage = now
		p.numMessages++

		msg, ok := m.Data.(map[string]interface{})
		if !ok {
			// TODO: Respond
			return
		}
		p.room.Broadcast(p.room.makeUploadPayload(msg, p, m.Type), true)

	// "Typing" status.
	case TypeTyping:
		p.room.Broadcast(p.room.makePeerUpdatePayload(p, TypeTyping), false)

	// Request for peers list
	case TypePeerList:
		p.room.sendPeerList(p)

	// Request growl notification
	case TypeGrowl:
		data, ok := m.Data.(map[string]interface{})
		if !ok {
			// TODO: Respond
			return
		}
		var to string
		{
			x, ok := data["to"]
			if ok {
				to, _ = x.(string)
			}
		}
		var from string
		{
			x, ok := data["from"]
			if ok {
				from, _ = x.(string)
			}
		}
		var msg string
		{
			x, ok := data["msg"]
			if ok {
				msg, _ = x.(string)
			}
		}

		p.room.HandleGrowlNotifications(from, to, msg)

	case TypePing:
		data, ok := m.Data.(map[string]interface{})
		if !ok {
			// TODO: Respond
			return
		}
		var to string
		{
			x, ok := data["to"]
			if ok {
				to, _ = x.(string)
			}
		}
		p.room.forwardTo(m.Type, to, m.Data)

	// Dipose of a room.
	case TypeRoomDispose:
		p.room.Dispose()
	default:
	}
}
