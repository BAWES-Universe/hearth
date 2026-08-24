package main

// media_bridge.go wires the media/ SFU package (Pion, pre-negotiated 12+6+2
// subscriber transceivers, Top-K audio) into the WS hub.
//
// Protocol: ADDITIVE only — PROTOCOL.md v0 (frozen) is untouched. The new
// envelope types live in docs/MEDIA.md:
//
//	client -> server:
//	  {t:"media_join",   d:{space?}}  join the voice bubble of a space
//	  {t:"media_leave",  d:{}}        leave the voice bubble
//	  {t:"media_signal", d:{pc,type,sdp?,candidate?}}  SFU signaling
//	server -> client:
//	  {t:"media_state",  d:{joined,space,peers:[{id,name}]}}  bubble membership
//	  {t:"media_signal", d:{peerId,pc,type,sdp?,candidate?,slots?}}  SFU signals
//
// Signaling SDP/ICE rides the hub as JSON text frames; audio/video payloads
// flow peer-to-peer through the SFU and NEVER through the hub.

import (
	"encoding/json"
	"log"

	"github.com/pion/webrtc/v4"
	"hearth/media"
)

// defaultICEServers give clients a STUN path for NAT'd peers (host candidates
// cover same-LAN phones, the day-0 gate).
func defaultICEServers() []webrtc.ICEServer {
	return []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
}

// newMediaSFU builds the shared SFU instance with server-side logging.
func newMediaSFU() *media.Media {
	return media.New(media.Config{
		ICEServers: defaultICEServers(),
		Logf:       func(format string, args ...any) { log.Printf("[sfu] "+format, args...) },
	})
}

// mediaRelay drains the SFU's single events stream and routes each signal to
// the owning client's WebSocket (envelope {t:"media_signal", d:{...}}). Runs
// for the process lifetime; the channel never closes.
func (h *Hub) mediaRelay() {
	for msg := range h.sfu.Events() {
		c := h.clientByPeerID(msg.PeerID)
		if c == nil {
			continue
		}
		c.emit("media_signal", signalToMap(msg))
	}
}

// clientByPeerID resolves a media peer (entity/session id) to its WS client.
func (h *Hub) clientByPeerID(peerID string) *Client {
	if e := h.findEntity(peerID); e != nil {
		return e.Client
	}
	return nil
}

// signalToMap converts a media.SignalMsg to the envelope payload map. The
// SignalMsg json tags already match the wire shape (type, pc, peerId, sdp,
// candidate, slots, ...).
func signalToMap(m media.SignalMsg) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		log.Printf("marshal media signal: %v", err)
		return map[string]any{"peerId": m.PeerID, "type": string(m.Type), "pc": string(m.PC)}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{"peerId": m.PeerID, "type": string(m.Type), "pc": string(m.PC)}
	}
	return out
}

// bubblePeers lists the bubble members of a space (id + name) — used for the
// media_state peers payload so clients can map voices to avatars.
func (h *Hub) bubblePeers(spaceID string) []map[string]any {
	h.mu.RLock()
	ids := make([]string, 0, 4)
	for pid, sp := range h.bubbles {
		if sp == spaceID {
			ids = append(ids, pid)
		}
	}
	h.mu.RUnlock()
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		e := h.findEntity(id)
		if e == nil {
			continue
		}
		out = append(out, map[string]any{"id": id, "name": e.Name})
	}
	return out
}

// broadcastBubble sends a media_state payload to every client currently in a
// voice bubble (joined, not merely present in the space).
func (h *Hub) broadcastBubble(spaceID string, payload map[string]any) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.mu.Lock()
		pid := ""
		if c.Entity != nil {
			pid = c.Entity.ID
		}
		c.mu.Unlock()
		if pid == "" {
			continue
		}
		h.mu.RLock()
		_, member := h.bubbles[pid]
		h.mu.RUnlock()
		if member {
			c.emit("media_state", payload)
		}
	}
}

// handleMediaJoin joins the client's voice bubble for a space. Idempotent for
// the same space; joining a different space leaves the old bubble first. The
// SFU immediately emits the pre-negotiated subscriber offer (12+6+2 m-lines)
// on the events stream, which mediaRelay forwards as media_signal.
func (c *Client) handleMediaJoin(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	spaceID := getString(msg, "space")
	if spaceID == "" {
		spaceID = c.spaceID
	}
	if spaceID == "" {
		c.sendError("bad_media", "media_join requires a space")
		return
	}
	if c.hub.space(spaceID) == nil {
		c.sendError("space_not_found", "no such space: "+spaceID)
		return
	}
	h := c.hub
	peerID := c.Entity.ID

	h.mu.Lock()
	cur := h.bubbles[peerID]
	h.mu.Unlock()
	if cur == spaceID {
		// already in this bubble — refresh state (idempotent re-join)
		c.emit("media_state", map[string]any{
			"joined": true, "space": spaceID, "peers": h.bubblePeers(spaceID),
		})
		return
	}
	if cur != "" {
		if err := h.sfu.Leave(peerID); err != nil && err != media.ErrNotFound {
			log.Printf("media leave (re-join): %v", err)
		}
		// remaining members of the old bubble see the updated peer list
		h.broadcastBubble(cur, map[string]any{
			"space": cur, "peers": h.bubblePeers(cur),
		})
	}
	if err := h.sfu.Join(peerID, spaceID); err != nil {
		c.sendError("media_error", "media join failed: "+err.Error())
		return
	}
	h.mu.Lock()
	h.bubbles[peerID] = spaceID
	h.mu.Unlock()
	log.Printf("media join: %s (%s) bubble=%s", c.Entity.Name, peerID[:8], spaceID)
	h.broadcastBubble(spaceID, map[string]any{
		"joined": true, "space": spaceID, "peers": h.bubblePeers(spaceID),
	})
}

// handleMediaLeave leaves the voice bubble (tears down both PeerConnections).
func (c *Client) handleMediaLeave(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	h := c.hub
	peerID := c.Entity.ID
	h.mu.Lock()
	spaceID, ok := h.bubbles[peerID]
	delete(h.bubbles, peerID)
	h.mu.Unlock()
	if !ok {
		return // not in a bubble — no-op
	}
	if err := h.sfu.Leave(peerID); err != nil && err != media.ErrNotFound {
		log.Printf("media leave: %v", err)
	}
	c.emit("media_state", map[string]any{"joined": false})
	h.broadcastBubble(spaceID, map[string]any{
		"space": spaceID, "peers": h.bubblePeers(spaceID),
	})
	log.Printf("media leave: %s (%s) bubble=%s", c.Entity.Name, peerID[:8], spaceID)
}

// handleMediaSignal forwards one signaling message from the client to the SFU
// (publisher offer/answer/ICE, subscriber answer/ICE). The SFU's synchronous
// answers (publisher answers) are emitted on its events stream and relayed
// back by mediaRelay — no direct reply here.
func (c *Client) handleMediaSignal(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	h := c.hub
	peerID := c.Entity.ID
	h.mu.RLock()
	_, member := h.bubbles[peerID]
	h.mu.RUnlock()
	if !member {
		c.sendError("media_error", "join a voice bubble first (media_join)")
		return
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		c.sendError("bad_media", "bad media_signal payload")
		return
	}
	sig, err := media.ParseSignal(raw)
	if err != nil {
		c.sendError("bad_media", "bad media_signal payload: "+err.Error())
		return
	}
	sig.PeerID = peerID
	if _, err := h.sfu.HandleSignal(peerID, sig); err != nil {
		c.sendError("media_error", "media signal rejected: "+err.Error())
		return
	}
	log.Printf("media signal: %s (%s) pc=%s type=%s", c.Entity.Name, peerID[:8], sig.PC, sig.Type)
}

// dropBubble removes a disconnecting client from its bubble and notifies the
// remaining members. Called from removeClient.
func (h *Hub) dropBubble(peerID string) {
	h.mu.Lock()
	spaceID, ok := h.bubbles[peerID]
	delete(h.bubbles, peerID)
	h.mu.Unlock()
	if !ok {
		return
	}
	if err := h.sfu.Leave(peerID); err != nil && err != media.ErrNotFound {
		log.Printf("media leave (disconnect): %v", err)
	}
	h.broadcastBubble(spaceID, map[string]any{
		"space": spaceID, "peers": h.bubblePeers(spaceID),
	})
}
