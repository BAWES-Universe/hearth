package main

// T2 — MCP backend adapter. Implements hearth/mcp.Backend against the Hearth
// monolith so the /mcp endpoint can offer world read/edit/chat/bot tools to
// AI agents.
//
// Mutating tools (edit/chat/bot.run) authenticate with a bot deviceKey and
// run through the SAME op-log path bots use (server/bot.go): the adapter
// spawns an in-process BotClient that dials this server's own /ws endpoint,
// joins the world, and emits frozen edit ops with idem keys — every mutation
// is append-only, replay-safe (deduped acks), and audited as role=bot
// (docs/BOT-PROTOCOL.md). Read-only tools use the store/hub APIs directly.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"hearth/hmf"
	"hearth/mcp"
)

// mcpBackend adapts the Hub/Store to the MCP tool backend.
type mcpBackend struct {
	hub    *Hub
	wsBase string // ws://127.0.0.1:PORT/ws — where in-process bots dial
}

// newMCPBackend builds the backend for the given listen addr (HEARTH_ADDR).
func newMCPBackend(hub *Hub, addr string) *mcpBackend {
	return &mcpBackend{hub: hub, wsBase: botWSBase(addr)}
}

// botWSBase derives the loopback WS URL for a listen addr. Bots dial
// locally (same box), so external reachability is irrelevant.
func botWSBase(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "ws://127.0.0.1:8090/ws"
	}
	return "ws://127.0.0.1:" + port + "/ws"
}

// botConfig returns a BotConfig pointed at this server's own /ws endpoint.
func (b *mcpBackend) botConfig(deviceKey, name, worldID, runID string, script *BotScript) BotConfig {
	return BotConfig{
		Name: name, DeviceKey: deviceKey, World: worldID,
		URL: b.wsBase, RunID: runID, Script: script,
		Interval: 5 * time.Millisecond, Timeout: 30 * time.Second,
	}
}

// Worlds lists the published-worlds directory (shared ranking with REST).
func (b *mcpBackend) Worlds(q string) ([]map[string]any, error) {
	return b.hub.directory(q)
}

// World returns a published world's full HMF v1 doc. Drafts are invisible.
func (b *mcpBackend) World(id string) (map[string]any, error) {
	meta, err := b.hub.store.worldMeta(id)
	if err != nil {
		return nil, fmt.Errorf("world not found: %s", id)
	}
	if !meta.IsPublished {
		return nil, fmt.Errorf("world not found or not published: %s (drafts are invisible to MCP)", id)
	}
	w, err := b.hub.store.LoadWorld(id)
	if err != nil {
		return nil, err
	}
	return w.GeoJSON(), nil
}

// Edit applies ONE frozen edit op through the bot op-log (BotClient).
func (b *mcpBackend) Edit(deviceKey, worldID, runID string, op mcp.EditOp) (mcp.EditResult, error) {
	if runID == "" {
		runID = "mcp-" + randHex(4)
	}
	if op.Tile != "" {
		// strict palette check: an unknown name must be rejected, never
		// silently mapped to floor (that would turn a typo into an erase)
		if _, ok := hmf.TileIDOK(op.Tile); !ok {
			return mcp.EditResult{}, fmt.Errorf("unknown tile name %q (palette: %s)", op.Tile, hmf.PaletteNames())
		}
	}
	if op.Op == "erase" {
		op.Tile = ""
		op.TileID = 0
	}
	name := "MCP"
	script := &BotScript{V: 1, Name: name, World: worldID, Ops: []BotScriptOp{
		{Op: op.Op, X: op.X, Y: op.Y, Tile: op.Tile, TileID: op.TileID},
	}}
	res := NewBotClient(b.botConfig(deviceKey, name, worldID, runID, script)).Run()
	if res.FirstErr != "" {
		return mcp.EditResult{}, errors.New(res.FirstErr)
	}
	out := mcp.EditResult{
		RunID: res.RunID, World: worldID,
		Applied: res.Applied > 0, Deduped: res.Deduped > 0,
		BotID: res.BotID, Actor: res.UserID,
		Audit: "role=bot",
	}
	if len(res.Seqs) > 0 {
		out.Seq = res.Seqs[0]
	}
	return out, nil
}

// BotRun executes a multi-op BotScript through the bot op-log.
func (b *mcpBackend) BotRun(deviceKey, name, worldID, runID string, ops []mcp.ScriptOp) (mcp.BotRunResult, error) {
	if name == "" {
		name = "MCP"
	}
	script := &BotScript{V: 1, Name: name, World: worldID}
	for _, o := range ops {
		if o.Tile != "" {
			if _, ok := hmf.TileIDOK(o.Tile); !ok {
				return mcp.BotRunResult{}, fmt.Errorf("unknown tile name %q (palette: %s)", o.Tile, hmf.PaletteNames())
			}
		}
		script.Ops = append(script.Ops, BotScriptOp{Op: o.Op, X: o.X, Y: o.Y, W: o.W, H: o.H, Tile: o.Tile, TileID: o.TileID})
	}
	if runID == "" {
		runID = "bot-" + slug(name) + "-" + randHex(4)
	}
	res := NewBotClient(b.botConfig(deviceKey, name, worldID, runID, script)).Run()
	return mcp.BotRunResult{
		RunID: res.RunID, World: res.World, BotID: res.BotID, Actor: res.UserID,
		Ops: res.Ops, Applied: res.Applied, Deduped: res.Deduped,
		Seqs: res.Seqs, FirstErr: res.FirstErr, Done: res.Done,
	}, nil
}

// Chat sends a chat message as the bot account into a space. The bot dials
// /ws like any client, joins, sends the chat envelope, and confirms delivery
// via its own broadcast echo (fromId == selfId).
func (b *mcpBackend) Chat(deviceKey, name, worldID, channel, text string) (mcp.ChatResult, error) {
	if name == "" {
		name = "MCP"
	}
	if channel == "" {
		channel = "space"
	}
	u, err := url.Parse(b.wsBase)
	if err != nil {
		return mcp.ChatResult{}, fmt.Errorf("chat: bad ws url: %w", err)
	}
	q := u.Query()
	q.Set("deviceKey", deviceKey)
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		return mcp.ChatResult{}, fmt.Errorf("chat dial: %w", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(map[string]any{"v": 1, "t": "join", "d": map[string]any{
		"deviceKey": deviceKey, "name": name, "spaceId": worldID,
	}}); err != nil {
		return mcp.ChatResult{}, fmt.Errorf("chat join: %w", err)
	}
	selfID := ""
	deadline := time.Now().Add(15 * time.Second)
	for selfID == "" {
		if time.Now().After(deadline) {
			return mcp.ChatResult{}, errors.New("chat join timeout (no welcome; world may not exist)")
		}
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var env struct {
			T string         `json:"t"`
			D map[string]any `json:"d"`
		}
		if err := conn.ReadJSON(&env); err != nil {
			return mcp.ChatResult{}, fmt.Errorf("chat read: %w", err)
		}
		switch env.T {
		case "welcome":
			selfID, _ = env.D["selfId"].(string)
		case "error":
			code, _ := env.D["code"].(string)
			msg, _ := env.D["message"].(string)
			return mcp.ChatResult{}, fmt.Errorf("chat join rejected: %s: %s", code, msg)
		}
	}

	if err := conn.WriteJSON(map[string]any{"v": 1, "t": "chat", "d": map[string]any{
		"channel": channel, "text": text,
	}}); err != nil {
		return mcp.ChatResult{}, fmt.Errorf("chat send: %w", err)
	}
	ts := ""
	for ts == "" {
		if time.Now().After(deadline) {
			return mcp.ChatResult{}, errors.New("chat echo timeout")
		}
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var env struct {
			T string         `json:"t"`
			D map[string]any `json:"d"`
		}
		if err := conn.ReadJSON(&env); err != nil {
			return mcp.ChatResult{}, fmt.Errorf("chat read: %w", err)
		}
		if env.T != "chat" {
			continue // state/bot_msg/others — ignore
		}
		if from, _ := env.D["fromId"].(string); from != selfID {
			continue // another speaker's echo
		}
		ts, _ = env.D["ts"].(string)
	}
	return mcp.ChatResult{
		Delivered: true, FromID: selfID, Name: name,
		Channel: channel, Text: text, TS: ts, Actor: hashDeviceKey(deviceKey),
	}, nil
}

// Presence lists the live occupants of a space.
func (b *mcpBackend) Presence(worldID string) ([]map[string]any, error) {
	sp := b.hub.space(worldID)
	if sp == nil {
		return nil, fmt.Errorf("world not found: %s", worldID)
	}
	snaps := sp.EntitySnaps()
	out := make([]map[string]any, 0, len(snaps))
	for _, e := range snaps {
		p := e.PublicJSON()
		uid := ""
		// Entity is set once at join and never mutated — safe to read
		// without the client mutex (see hearth-development skill).
		if e.Client != nil && e.Client.Entity != nil {
			uid = e.Client.Entity.UserID
		}
		p["userId"] = uid
		out = append(out, p)
	}
	return out, nil
}

// Activity returns the recent append-only audit/activity feed of a world.
func (b *mcpBackend) Activity(worldID string, limit int) ([]map[string]any, error) {
	if _, err := b.hub.store.worldMeta(worldID); err != nil {
		return nil, fmt.Errorf("world not found: %s", worldID)
	}
	events, err := b.hub.store.RecentActivity(worldID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		var diff any
		if e.Diff != "" {
			_ = json.Unmarshal([]byte(e.Diff), &diff)
		}
		out = append(out, map[string]any{
			"id": e.ID, "worldId": e.WorldID, "actor": e.Actor, "role": e.Role,
			"kind": e.Kind, "action": e.Action, "target": e.Target,
			"diff": diff, "ip": e.IP, "ts": e.TS,
		})
	}
	return out, nil
}
