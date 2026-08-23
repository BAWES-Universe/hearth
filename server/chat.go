package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// TokenBucket rate limiter. Hearth chat: capacity 5 (burst 5/10s), refill
// 0.5 tokens/s (sustained 1 msg / 2s).
type TokenBucket struct {
	mu       sync.Mutex
	capacity float64
	tokens   float64
	rate     float64 // tokens per second
	last     time.Time
}

func NewTokenBucket(capacity, rate float64) *TokenBucket {
	return &TokenBucket{capacity: capacity, tokens: capacity, rate: rate, last: time.Now()}
}

func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.last).Seconds()
	tb.last = now
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

func (c *Client) handleChat(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	channel := getString(msg, "channel")
	switch channel {
	case "proximity", "space", "dm":
	default:
		c.sendError("invalid_channel", "channel must be proximity|space|dm")
		return
	}
	text := getString(msg, "text")
	if text == "" {
		c.sendError("empty_chat", "chat text required")
		return
	}
	if len(text) > chatMaxBytes {
		c.sendError("too_large", "chat message exceeds 2048 bytes")
		return
	}
	if !c.rate.Allow() {
		c.sendError("rate_limited", "slow down — burst 5/10s, sustained 1/2s")
		return
	}

		payload := map[string]any{
			"channel": channel,
			"from":    c.Entity.Name, "fromId": c.Entity.ID,
			"text": text, "ts": time.Now().UTC().Format(time.RFC3339),
		}

	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	switch channel {
	case "space":
		sp.BroadcastEnvelope("chat", payload)
	case "proximity":
		for _, e := range sp.AOI(c.Entity.X, c.Entity.Y, aoiRadius, c.Entity.ID) {
			if e.Client != nil {
				e.Client.emit("chat", payload)
			}
		}
	case "dm":
		to := getString(msg, "to")
		target := c.hub.findEntity(to)
		if target == nil || target.Client == nil {
			c.sendError("peer_not_found", "dm target offline: "+to)
			return
		}
		target.Client.emit("chat", payload)
	}

	if err := c.hub.store.InsertMessage(c.spaceID, c.Session.ID, c.Session.UserID, c.Entity.Name, channel, text); err != nil {
		log.Printf("insert message: %v", err)
	}
	log.Printf("chat[%s] %s: %.60s", channel, c.Entity.Name, text)
}

// BroadcastToClients sends v to every connected player in this space.
func (sp *SpaceState) BroadcastToClients(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	for _, e := range sp.EntitySnaps() {
		if e.Client != nil {
			e.Client.enqueue(b)
		}
	}
}

// BroadcastEnvelope wraps d in the frozen envelope ({"v":1,"t":t,"d":d}) and
// broadcasts to every connected player in this space.
func (sp *SpaceState) BroadcastEnvelope(t string, d map[string]any) {
	sp.BroadcastToClients(map[string]any{"v": 1, "t": t, "d": d})
}

func (c *Client) handleEdit(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	x, okX := getInt(msg, "x")
	y, okY := getInt(msg, "y")
	if !okX || !okY {
		c.sendError("bad_edit", "edit requires x and y")
		return
	}
	tile, _ := msg["tile"].(map[string]any)
	t, _ := tile["t"].(string)
	if t == "" {
		t = "floor"
	}
	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	if x < 0 || y < 0 || x >= sp.World.Width || y >= sp.World.Height {
		c.sendError("edit_out_of_bounds", "edit outside map bounds")
		return
	}
	sp.World.SetTile(x, y, t)
	if err := c.hub.store.SaveWorld(sp.World); err != nil {
		log.Printf("save world after edit: %v", err)
	}
	sp.BroadcastEnvelope("edit", map[string]any{
		"spaceId": sp.World.ID, "x": x, "y": y,
		"tile": map[string]any{"t": t}, "by": c.Entity.ID,
	})
	log.Printf("edit: %s set (%d,%d) = %s", c.Entity.Name, x, y, t)
}
