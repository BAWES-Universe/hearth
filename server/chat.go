package main

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"hearth/hmf"
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

// chatProximityRadius is the WorkAdventure-style bubble range: proximity
// chat reaches players within this many tiles. Presence AOI stays at
// aoiRadius (20) — being near is not the same as being able to whisper.
const chatProximityRadius = 6

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
	case "proximity", "space", "dm", "global":
	default:
		c.sendError("invalid_channel", "channel must be proximity|space|dm|global")
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
	nonce := getString(msg, "nonce")

	payload := map[string]any{
		"channel": channel,
		"from":    c.Entity.Name, "fromId": c.Entity.ID,
		"text": text, "ts": time.Now().UTC().Format(time.RFC3339),
		"nonce": nonce,
	}

	// Server-side nonce idempotency (social clarity dup fix): a resend of a
	// nonce this session already delivered (reconnect resend, manual retry,
	// double-tap) is echoed to the SENDER ONLY — no re-broadcast, no second
	// DB row. The nonce is recorded (recordChatNonce) only once the message
	// passes every gate below and is actually delivered, so a rejected send
	// never burns its nonce.
	if nonce != "" && c.hub.chatNonceSeen(c.Session.ID, nonce) {
		c.emit("chat", payload)
		log.Printf("chat[%s] %s: nonce %s deduped (echo sender only)", channel, c.Entity.Name, nonce)
		return
	}

	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	switch channel {
	case "dm":
		to := getString(msg, "to")
		if to == "" {
			c.sendError("dm_requires_peer", "dm requires a recipient id")
			return
		}
		if to == c.Entity.ID {
			c.sendError("dm_self", "you cannot message yourself")
			return
		}
		target := c.hub.findEntity(to)
		if target == nil || target.Client == nil {
			c.sendError("peer_not_found", "dm target offline: "+to)
			return
		}
		// Friends-only v1 (critic guardrail): the server enforces the
		// friendship graph, blocks (either direction), a per-recipient
		// 5/min cap, and rejects self-DM — the client UI is not the gate.
		if c.Session == nil || target.UserID == "" || !c.hub.store.AreFriends(c.Session.UserID, target.UserID) {
			c.sendError("dm_forbidden", "dm requires being friends")
			return
		}
		if c.hub.store.AreBlocked(c.Session.UserID, target.UserID) {
			c.sendError("dm_blocked", "this conversation is blocked")
			return
		}
		if !c.allowDMRate(to) {
			c.sendError("dm_rate_limited", "slow down — 5 DMs per minute to one person")
			return
		}
		c.hub.recordChatNonce(c.Session.ID, nonce)
		payload["to"] = target.ID
		payload["toName"] = target.Name
		// DM echoes to BOTH target and sender (with nonce): the sender's
		// pending message resolves on the echo; the target gets the copy.
		target.Client.emit("chat", payload)
		c.emit("chat", payload)
	case "space":
		c.hub.recordChatNonce(c.Session.ID, nonce)
		sp.BroadcastEnvelope("chat", payload)
	case "global":
		c.hub.recordChatNonce(c.Session.ID, nonce)
		// Across ALL spaces — the chat sheet's "All" tab. Every connected
		// client in every world receives it.
		h := c.hub
		h.mu.RLock()
		spaces := make([]*SpaceState, 0, len(h.spaces))
		for _, s := range h.spaces {
			spaces = append(spaces, s)
		}
		h.mu.RUnlock()
		for _, s := range spaces {
			s.BroadcastEnvelope("chat", payload)
		}
	case "proximity":
		c.hub.recordChatNonce(c.Session.ID, nonce)
		// WorkAdventure-style: short bubble range, and the SENDER is
		// included (skip="") so their own bubble renders and their pending
		// sheet message resolves — without the echo the pending message
		// would sit at "…" forever.
		for _, e := range sp.AOI(c.Entity.X, c.Entity.Y, chatProximityRadius, "") {
			if e.Client != nil {
				e.Client.emit("chat", payload)
			}
		}
	}

	if err := c.hub.store.InsertMessage(c.spaceID, c.Session.ID, c.Session.UserID, c.Entity.Name, channel, text); err != nil {
		log.Printf("insert message: %v", err)
	}
	log.Printf("chat[%s] %s: %.60s", channel, c.Entity.Name, text)
}

// allowDMRate enforces the per-recipient DM cap (5 messages per minute to
// the same person). Independent of the global chat token bucket: the burst
// limiter throttles throughput, this window throttles harassment volume.
func (c *Client) allowDMRate(peerID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	window := c.dmWindow[peerID]
	kept := window[:0]
	for _, ts := range window {
		if now.Sub(ts) < time.Minute {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= 5 {
		c.dmWindow[peerID] = kept
		return false
	}
	c.dmWindow[peerID] = append(kept, now)
	return true
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

// handleEdit applies one frozen HMF v1 editor op (docs/HMF-v1.md).
// Ops: paint | erase | place | zone | portal | publish | chunk_get.
// Every mutation is server-arbitrated (LWW by arrival order), persisted to
// the chunk/zones/portals tables + op_log, and broadcast to everyone in the
// space as an op-stream delta carrying the touched chunk revisions (clients
// use those to detect missed deltas and refetch+replay).
func (c *Client) handleEdit(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	op := parseEditOp(msg)
	if op == nil {
		c.sendError("bad_edit", "edit requires op (paint|erase|place|zone|portal|publish)")
		return
	}
	if op.Op == "chunk_get" {
		c.hub.handleChunkGet(sp, c, op)
		return
	}

	// Ownership gate: only the world owner may mutate a USER world; without
	// this, any authenticated client could paint or publish any world.
	// Showcase worlds are the shared co-authoring hub (v2 plan §1/§7 — the T1
	// exit demo edits a showcase wall live; bots and humans co-build there),
	// so any authenticated session may edit showcase content. Denies on any
	// lookup error (authz gates fail closed).
	if !c.canEditSpace(sp.World) {
		c.sendError("edit_forbidden", "only the owner of this world can edit it")
		return
	}

	// Agent-facing idempotency (docs/BOT-PROTOCOL.md): an op already applied
	// under the same (space, idem) key is acknowledged as deduped instead of
	// being re-applied — replaying a bot_run is safe and free.
	if op.Idem != "" {
		if seq, ok := c.hub.store.OpIdemSeq(sp.World.ID, op.Idem); ok {
			op.Seq = seq
			op.By = c.Entity.ID
			if c.Session != nil {
				op.Actor = c.Session.UserID
			}
			ack := &EditAck{Op: op, Deduped: true}
			c.broadcastEditAck(sp, ack)
			log.Printf("edit[%s] %s: op=%s deduped idem=%s seq=%d", sp.World.ID, c.Entity.Name, op.Op, op.Idem, seq)
			return
		}
	}

	// Allocate the op seq BEFORE applying: an applied op must never carry
	// seq=0 or be missing from the op_log (undo/replay integrity). If
	// sequencing fails, the edit is rejected before anything mutates.
	seq, err := c.hub.store.NextOpSeq(sp.World.ID)
	if err != nil {
		c.sendError("edit_rejected", "op sequencing failed")
		return
	}
	op.Seq = seq

	ack := c.hub.applyEditOp(sp, c, op)
	if ack.Err != "" {
		c.sendError("edit_rejected", ack.Err)
		return
	}
	// Append-only op_log (build history / undo trail). A failed append is
	// logged — the op is already applied with a valid seq, so the ack must
	// not claim failure.
	if err := c.hub.store.AppendOp(sp.World.ID, seq, op); err != nil {
		log.Printf("append op %s: %v", sp.World.ID, err)
	}
	c.broadcastEditAck(sp, ack)
	// Audit attribution: agent/bot ops (those carrying an idempotency key)
	// emit an append-only activity row keyed to the acting account so the
	// audit feed (GET /api/worlds/{id}/activity) attributes the build to the
	// bot user id (docs/BOT-PROTOCOL.md).
	if op.Idem != "" && op.Actor != "" {
		c.hub.emitActivity(sp.World.ID, op.Actor, "bot", "edit", op.Op, sp.World.ID,
			diffJSON(map[string]any{
				"seq": op.Seq, "idem": op.Idem,
				"x": op.X, "y": op.Y, "tileId": op.TileID, "cells": len(op.Cells),
			}),
			sanitizeIP(c.conn.RemoteAddr().String()))
	}
	// log the entity id (op.By), not the display name, on the hot edit path
	log.Printf("edit[%s] %s by=%s: op=%s seq=%d cells=%d", sp.World.ID, c.Entity.Name, op.By, op.Op, op.Seq, len(ack.Cells))
}

// canEditWorld authorizes an account on a world: owners and invited editors
// edit; showcase/seed worlds are the shared co-building hub (plan v2) and are
// editable by any authenticated session — humans and bots co-author them
// (bots additionally audited + idempotent, docs/BOT-PROTOCOL.md); guests
// (no session) are view-only. Denies on any lookup error (authz gates fail
// closed).
func (h *Hub) canEditWorld(sess *Session, w *World) bool {
	if w.IsShowcase {
		return sess != nil
	}
	if sess == nil {
		return false
	}
	meta, err := h.store.worldMeta(w.ID)
	if err != nil {
		return false
	}
	if meta.OwnerID == sess.UserID {
		return true
	}
	ok, err := h.store.IsEditor(w.ID, sess.UserID)
	return err == nil && ok
}

func (c *Client) canEditSpace(w *World) bool {
	return c.hub.canEditWorld(c.Session, w)
}

// parseEditOp builds an hmf.Op from a client edit message. Accepts the frozen
// string form ({tile:{t:"wall"}}) and the numeric palette form ({tileId:1}),
// single-cell (x,y) and batch (cells:[{x,y}...]) payloads.
func parseEditOp(msg map[string]any) *hmf.Op {
	op := &hmf.Op{Op: getString(msg, "op")}
	if op.Op == "" {
		// legacy edit: {x,y,tile|tileId} defaults to paint
		op.Op = "paint"
	}
	op.X, _ = getInt(msg, "x")
	op.Y, _ = getInt(msg, "y")
	op.UndoOf, _ = msgInt64(msg, "undoOf")
	op.CX, _ = getInt(msg, "cx")
	op.CY, _ = getInt(msg, "cy")
	op.Idem = getString(msg, "idem")

	if cells, ok := msg["cells"].([]any); ok && len(cells) > 0 {
		op.Cells = make([]hmf.Cell, 0, len(cells))
		for _, raw := range cells {
			cell, ok := raw.(map[string]any)
			if !ok {
				return nil
			}
			cx, okX := getInt(cell, "x")
			cy, okY := getInt(cell, "y")
			if !okX || !okY {
				return nil
			}
			op.Cells = append(op.Cells, hmf.Cell{X: cx, Y: cy})
		}
	}

	switch op.Op {
	case "paint", "erase", "place":
		// (0,0) is a valid tile: require the KEYS, not non-zero values.
		// The old guard exempted erase, so a bare {"op":"erase"} erased
		// tile (0,0) instead of being rejected.
		if len(op.Cells) == 0 {
			_, hasX := msg["x"]
			_, hasY := msg["y"]
			if !hasX || !hasY {
				return nil
			}
		}
		if tid, ok := getInt(msg, "tileId"); ok {
			op.TileID = tid
		} else if tile, ok := msg["tile"].(map[string]any); ok {
			name := getString(tile, "t")
			// unknown names must be REJECTED, not mapped to floor: mapping
			// them to 0 turned a paint with a typo into an erase.
			id, ok := hmf.TileIDOK(name)
			if !ok {
				return nil
			}
			op.TileID = id
		}
	case "portal":
		if p, ok := msg["portal"].(map[string]any); ok {
			op.Portal = &hmf.Portal{
				ID:          getString(p, "id"),
				X:           mustInt(p, "x"),
				Y:           mustInt(p, "y"),
				TargetSpace: getString(p, "targetSpace"),
				TargetX:     mustInt(p, "targetX"),
				TargetY:     mustInt(p, "targetY"),
			}
		} else {
			op.PortalID = getString(msg, "portalId")
		}
		if op.Portal == nil && op.PortalID == "" {
			return nil
		}
		if op.Portal != nil && !intFieldsPresent(payload(msg, "portal"), "x", "y", "targetX", "targetY") {
			return nil
		}
	case "zone":
		if z, ok := msg["zone"].(map[string]any); ok {
			op.Zone = &hmf.Zone{
				ID:   getString(z, "id"),
				Name: getString(z, "name"),
				X:    mustInt(z, "x"),
				Y:    mustInt(z, "y"),
				W:    mustInt(z, "w"),
				H:    mustInt(z, "h"),
			}
		} else {
			op.ZoneID = getString(msg, "zoneId")
		}
		if op.Zone == nil && op.ZoneID == "" {
			return nil
		}
		if op.Zone != nil && !intFieldsPresent(payload(msg, "zone"), "x", "y", "w", "h") {
			return nil
		}
	case "publish":
		// no payload
	case "object":
		// functional object placement (door|npc|sign|light): upsert with a
		// payload, remove with objectId only.
		if o, ok := msg["object"].(map[string]any); ok {
			if !intFieldsPresent(payload(msg, "object"), "x", "y") {
				return nil
			}
			kind := getString(o, "kind")
			if !hmf.ValidObjectKind(kind) {
				return nil
			}
			op.Object = &hmf.Object{
				ID:   getString(o, "id"),
				Kind: kind,
				X:    mustInt(o, "x"),
				Y:    mustInt(o, "y"),
				Name: getString(o, "name"),
				Text: getString(o, "text"),
			}
			if d, ok := o["data"].(map[string]any); ok {
				op.Object.Data = d
			}
		} else {
			op.ObjectID = getString(msg, "objectId")
		}
		if op.Object == nil && op.ObjectID == "" {
			return nil
		}
	case "chunk_get":
		// cx/cy already read
	default:
		return nil
	}
	return op
}

func payload(msg map[string]any, key string) map[string]any {
	m, _ := msg[key].(map[string]any)
	return m
}

// intFieldsPresent reports whether every named field exists as a JSON number
// in m. The old intOf() silently returned 0 for missing/wrong-typed fields,
// which placed portals/zones at (0,0) — reject instead.
func intFieldsPresent(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := getInt(m, k); !ok {
			return false
		}
	}
	return true
}

// mustInt reads a JSON-number field, defaulting to 0 when absent — callers
// must have validated presence via intFieldsPresent first.
func mustInt(m map[string]any, k string) int {
	v, _ := getInt(m, k)
	return v
}

func msgInt64(m map[string]any, k string) (int64, bool) {
	switch v := m[k].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

// broadcastEditAck pushes the op-stream delta to everyone in the space
// (including the acting client, which uses it for undo bookkeeping).
func (c *Client) broadcastEditAck(sp *SpaceState, ack *EditAck) {
	op := ack.Op
	d := map[string]any{
		"op":      op.Op,
		"by":      op.By,
		"seq":     op.Seq,
		"spaceId": sp.World.ID,
		"undoOf":  op.UndoOf,
		"ts":      chunkTimestamp(),
		"applied": true,
	}
	if ack.Deduped {
		// replay-safe skip: an op with the same idem key was already applied
		d["deduped"] = true
	}
	if op.Idem != "" {
		// echo the idempotency key so agents can correlate acks without
		// relying on arrival order (docs/BOT-PROTOCOL.md)
		d["idem"] = op.Idem
	}
	if len(ack.Chunks) > 0 {
		d["chunks"] = ack.Chunks
	}
	switch op.Op {
	case "paint", "erase", "place":
		if len(ack.Cells) == 1 {
			ch := ack.Cells[0]
			d["x"] = ch.X
			d["y"] = ch.Y
			d["tileId"] = ch.TileID
			d["tile"] = map[string]any{"t": TileName(ch.TileID)}
			d["priorTileId"] = ch.Prior
		} else if len(ack.Cells) > 1 {
			cells := make([]map[string]any, 0, len(ack.Cells))
			for _, ch := range ack.Cells {
				cells = append(cells, map[string]any{
					"x": ch.X, "y": ch.Y, "tileId": ch.TileID, "priorTileId": ch.Prior,
				})
			}
			d["cells"] = cells
		}
	case "portal":
		if op.Portal != nil {
			d["portal"] = op.Portal
			d["x"] = op.Portal.X
			d["y"] = op.Portal.Y
		} else {
			d["portalId"] = op.PortalID
		}
	case "zone":
		if op.Zone != nil {
			d["zone"] = op.Zone
		} else {
			d["zoneId"] = op.ZoneID
		}
	case "object":
		if op.Object != nil {
			d["object"] = op.Object
		} else {
			d["objectId"] = op.ObjectID
		}
	case "publish":
		d["isPublished"] = true
	}
	sp.BroadcastEnvelope("edit", d)
}
