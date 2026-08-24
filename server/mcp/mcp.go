// Package mcp implements a minimal Model Context Protocol server for Hearth
// over the streamable HTTP transport (MCP spec revision 2025-06-18).
//
// It exposes Hearth's world + bot op-log capabilities as MCP tools so AI
// agents (Claude Desktop, custom MCP clients, ...) can inspect published
// worlds and build in them through the SAME append-only, idempotent op-log
// that bots use (docs/BOT-PROTOCOL.md). Every mutation authenticates with a
// bot deviceKey, carries an idempotency key (replay-safe), and is audited as
// role=bot.
//
// Wire: JSON-RPC 2.0 over POST /mcp on the existing :8090 listener. The
// streamable-HTTP session contract is honored: initialize issues a session id
// (Mcp-Session-Id response header), subsequent requests carry it back,
// unknown session ids get 404, DELETE terminates a session, and notifications
// (no id / notifications/*) are answered with 202 and an empty body. Requests
// without a session id are also processed (stateless-tolerant superset of the
// spec) so curl and simple clients work without session plumbing. GET returns
// 405 (no server->client SSE stream is offered).
//
// No SDK dependency — stdlib only. The Backend interface is implemented by
// the Hearth monolith (server/mcp_adapter.go) which reuses server/bot.go's
// BotClient rather than reimplementing op semantics.
package mcp

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ServerVersion is advertised to clients via initialize.serverInfo.
const ServerVersion = "0.1.0"

// supportedProtocols are the MCP protocol revisions this server speaks,
// newest first. initialize echoes the client's version when recognized,
// otherwise negotiates the newest.
var supportedProtocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const latestProtocol = "2025-06-18"

// maxSessionAge evicts idle sessions after 24h (lazy, on access).
const maxSessionAge = 24 * time.Hour

// maxSessions caps the session registry (evicts oldest beyond the cap).
const maxSessions = 256

// --- Backend: implemented by the Hearth monolith (package main) ---

// Backend is the Hearth surface the MCP tools operate on. Read-only tools
// (worlds/world/presence/activity) go through store/hub APIs; mutating tools
// (edit/chat/bot.run) authenticate with a bot deviceKey and run through the
// same op-log path bots use (server/bot.go), so every mutation is
// append-only, replay-safe and audited as role=bot.
type Backend interface {
	// Worlds lists the published-worlds directory (gravity-ranked cards).
	Worlds(q string) ([]map[string]any, error)
	// World returns a published world's full doc (HMF v1 GeoJSON: tiles,
	// chunks, zones, portals, spawn, palette). Drafts are invisible.
	World(id string) (map[string]any, error)
	// Edit applies ONE frozen edit op (paint|erase|place) through the bot
	// op-log. runID is the idempotency namespace: replaying the same runID
	// is a no-op (deduped ack with the original seq).
	Edit(deviceKey, worldID, runID string, op EditOp) (EditResult, error)
	// BotRun executes a multi-op BotScript against a world (bot op-log).
	BotRun(deviceKey, name, worldID, runID string, ops []ScriptOp) (BotRunResult, error)
	// Chat sends a chat message as the bot account into a space.
	Chat(deviceKey, name, worldID, channel, text string) (ChatResult, error)
	// Presence lists the live occupants of a space.
	Presence(worldID string) ([]map[string]any, error)
	// Activity returns the recent append-only audit/activity feed of a world
	// (bot ops appear as kind=edit, role=bot rows).
	Activity(worldID string, limit int) ([]map[string]any, error)
}

// EditOp is one frozen HMF v1 edit op (single cell). Tile (palette name) and
// TileID (numeric) are interchangeable; erase ignores both.
type EditOp struct {
	Op     string `json:"op"`               // paint | erase | place
	X      int    `json:"x"`                //
	Y      int    `json:"y"`                //
	Tile   string `json:"tile,omitempty"`   // palette name (wall|roof|flower|...)
	TileID int    `json:"tileId,omitempty"` // numeric palette id
}

// EditResult is the outcome of one op through the bot op-log.
type EditResult struct {
	RunID   string `json:"runId"`            // idempotency namespace used
	World   string `json:"world"`            // target world id
	Applied bool   `json:"applied"`          // op applied to the world
	Deduped bool   `json:"deduped"`          // skipped as already applied (replay)
	Seq     int64  `json:"seq,omitempty"`    // op_log seq (original seq on dedupe)
	BotID   string `json:"botId,omitempty"`  // entity/session id of the bot
	Actor   string `json:"actor"`            // account id = sha256(deviceKey)
	Audit   string `json:"audit"`            // "role=bot" attribution marker
}

// ScriptOp is one op of a BotScript (docs/BOT-PROTOCOL.md §7). rect/ring/line
// are client-side sugar expanded into frozen paint ops with cells.
type ScriptOp struct {
	Op     string `json:"op"`               // paint|erase|place|rect|ring|line
	X      int    `json:"x,omitempty"`      //
	Y      int    `json:"y,omitempty"`      //
	W      int    `json:"w,omitempty"`      // rect/ring width
	H      int    `json:"h,omitempty"`      // rect/ring height
	Tile   string `json:"tile,omitempty"`   // palette name
	TileID int    `json:"tileId,omitempty"` // numeric palette id
}

// BotRunResult is the outcome of a full bot run.
type BotRunResult struct {
	RunID    string  `json:"runId"`
	World    string  `json:"world"`
	BotID    string  `json:"botId,omitempty"`
	Actor    string  `json:"actor"`
	Ops      int     `json:"ops"`
	Applied  int     `json:"applied"`
	Deduped  int     `json:"deduped"`
	Seqs     []int64 `json:"seqs"`
	FirstErr string  `json:"firstErr,omitempty"`
	Done     bool    `json:"done"`
}

// ChatResult is the outcome of sending a chat message as the bot account.
type ChatResult struct {
	Delivered bool   `json:"delivered"`
	FromID    string `json:"fromId"`
	Name      string `json:"name"`
	Channel   string `json:"channel"`
	Text      string `json:"text"`
	TS        string `json:"ts"`
	Actor     string `json:"actor"`
}

// --- Server ---

// Server is the streamable-HTTP MCP endpoint (an http.Handler; mount on
// POST /mcp of the existing listener).
type Server struct {
	backend Backend
	logf    func(format string, args ...any)

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id        string
	createdAt time.Time
	lastSeen  time.Time
}

// NewServer builds an MCP server over the given Hearth backend.
func NewServer(b Backend) *Server {
	return &Server{
		backend:  b,
		logf:     log.Printf,
		sessions: map[string]*session{},
	}
}

// ServeHTTP implements the streamable HTTP transport:
//
//	POST   /mcp   JSON-RPC 2.0 request -> JSON or SSE response
//	DELETE /mcp   terminate a session (Mcp-Session-Id)
//	OPTIONS       CORS preflight (browser-based MCP clients)
//	GET    /mcp   405 (no server->client stream offered)
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodDelete:
		s.handleDelete(w, r)
	case http.MethodOptions:
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Mcp-Session-Id")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "POST, DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// rpcRequest is a JSON-RPC 2.0 request (id may be nil for notifications).
type rpcRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		s.writeError(w, r, nil, -32600, "Content-Type must be application/json", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.writeError(w, r, nil, -32603, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, r, nil, -32700, "parse error: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Method == "" {
		s.writeError(w, r, req.ID, -32600, "invalid request: method required", http.StatusBadRequest)
		return
	}

	// JSON-RPC notification (no id) or MCP notification method: the transport
	// answers 202 Accepted with an empty body and no JSON-RPC response.
	if req.ID == nil || strings.HasPrefix(req.Method, "notifications/") {
		if req.Method == "notifications/initialized" {
			s.logf("mcp: client initialized (session=%s)", r.Header.Get("Mcp-Session-Id"))
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Session handling (streamable HTTP). A presented-but-unknown session id
	// is a 404 per the spec; a missing session id is tolerated (stateless
	// processing) so simple clients and curl work without session plumbing.
	if req.Method != "initialize" {
		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" && !s.validSession(sid) {
			http.Error(w, "unknown session id", http.StatusNotFound)
			return
		}
	}

	result, rpcErr := s.dispatch(req)
	if rpcErr != nil {
		s.writeError(w, r, req.ID, rpcErr.Code, rpcErr.Message, http.StatusOK)
		return
	}
	if req.Method == "initialize" {
		sess := s.newSession()
		w.Header().Set("Mcp-Session-Id", sess.id)
	}
	s.writeResponse(w, r, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		http.Error(w, "Mcp-Session-Id required", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	delete(s.sessions, sid)
	s.mu.Unlock()
	s.logf("mcp: session terminated %s", sid)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) validSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return false
	}
	if time.Since(sess.lastSeen) > maxSessionAge {
		delete(s.sessions, id)
		return false
	}
	sess.lastSeen = time.Now()
	return true
}

func (s *Server) newSession() *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	// evict expired sessions first
	now := time.Now()
	for id, sess := range s.sessions {
		if now.Sub(sess.lastSeen) > maxSessionAge {
			delete(s.sessions, id)
		}
	}
	// cap: drop the oldest when over
	for len(s.sessions) >= maxSessions {
		var oldestID string
		var oldest time.Time
		for id, sess := range s.sessions {
			if oldestID == "" || sess.lastSeen.Before(oldest) {
				oldestID, oldest = id, sess.lastSeen
			}
		}
		delete(s.sessions, oldestID)
	}
	sess := &session{id: randID(16), createdAt: now, lastSeen: now}
	s.sessions[sess.id] = sess
	return sess
}

// randID returns a hex session id (crypto/rand; falls back to time on failure).
func randID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("mcp-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b)
}

func (s *Server) dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.Params)
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		return s.handleToolCall(req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func (s *Server) handleInitialize(params map[string]any) (any, *rpcError) {
	pv := str(params, "protocolVersion")
	if pv == "" {
		pv = latestProtocol
	}
	negotiated := pv
	known := false
	for _, v := range supportedProtocols {
		if v == pv {
			known = true
			break
		}
	}
	if !known {
		negotiated = latestProtocol
	}
	return map[string]any{
		"protocolVersion": negotiated,
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{"name": "hearth-mcp", "version": ServerVersion},
	}, nil
}

func (s *Server) handleToolCall(params map[string]any) (any, *rpcError) {
	name := str(params, "name")
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	var out any
	var err error
	switch name {
	case "worlds.list":
		out, err = s.toolWorldsList(args)
	case "world.read":
		out, err = s.toolWorldRead(args)
	case "world.edit":
		out, err = s.toolWorldEdit(args)
	case "world.chat":
		out, err = s.toolWorldChat(args)
	case "presence.list":
		out, err = s.toolPresenceList(args)
	case "bot.run":
		out, err = s.toolBotRun(args)
	case "world.activity":
		out, err = s.toolWorldActivity(args)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + name}
	}
	if err != nil {
		return toolErrorResult(err), nil
	}
	return toolResult(out), nil
}

// --- tools ---

const paletteDoc = "floor|wall|water|grass|stone|sand|path|wood|lava|ice|flower|bush|rock|tree|roof|door|fence|bridge|crystal|dirt"

func toolList() []map[string]any {
	return []map[string]any{
		{
			"name": "worlds.list",
			"description": "List published worlds (the public directory, gravity-ranked). " +
				"Returns id, name, owner, headcount and gravity for every published world. Read-only.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q": map[string]any{"type": "string", "description": "Optional name substring filter"},
				},
			},
		},
		{
			"name": "world.read",
			"description": "Read a published world's full state: HMF v1 document with tiles " +
				"(x, y, t name + tileId), chunks (rev summary), zones, portals, spawn and the palette. " +
				"Drafts are invisible (only published worlds are readable). Read-only.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worldId": map[string]any{"type": "string", "description": "World id (e.g. garden, town-square)"},
				},
				"required": []string{"worldId"},
			},
		},
		{
			"name": "world.edit",
			"description": "Paint/erase/place ONE tile through the bot op-log (docs/BOT-PROTOCOL.md). " +
				"Authenticates with a bot deviceKey, sends a frozen edit op with an idempotency key, " +
				"and returns applied/deduped + the op_log seq. Every op is append-only and audited as " +
				"role=bot. Replaying the same runId is a safe no-op (deduped). Showcase worlds are " +
				"co-editable by any bot; user-owned worlds only by the owning bot account.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worldId":   map[string]any{"type": "string", "description": "Target world id"},
					"op":        map[string]any{"type": "string", "enum": []string{"paint", "erase", "place"}, "description": "Frozen HMF v1 op"},
					"x":         map[string]any{"type": "integer", "description": "Tile x"},
					"y":         map[string]any{"type": "integer", "description": "Tile y"},
					"tile":      map[string]any{"type": "string", "description": "Palette name: " + paletteDoc},
					"tileId":    map[string]any{"type": "integer", "description": "Numeric palette id (0=floor); either tile or tileId"},
					"deviceKey": map[string]any{"type": "string", "description": "Bot account key (bot-<name>); audit attributes to sha256(deviceKey)"},
					"runId":     map[string]any{"type": "string", "description": "Idempotency namespace; replaying the same runId dedupes"},
				},
				"required": []string{"worldId", "op", "x", "y", "deviceKey"},
			},
		},
		{
			"name": "world.chat",
			"description": "Send a chat message as the bot account into a space. The bot joins the " +
				"world, sends the message, and confirms delivery via its own echo. Audited as role=bot.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worldId":   map[string]any{"type": "string", "description": "Target world id"},
					"text":      map[string]any{"type": "string", "description": "Message text (max 2048 bytes)"},
					"channel":   map[string]any{"type": "string", "enum": []string{"space", "proximity"}, "description": "Defaults to space (whole room)"},
					"deviceKey": map[string]any{"type": "string", "description": "Bot account key"},
					"name":      map[string]any{"type": "string", "description": "Display name (defaults to MCP)"},
				},
				"required": []string{"worldId", "text", "deviceKey"},
			},
		},
		{
			"name": "presence.list",
			"description": "Who is in a space right now: live occupants (players and ambient bots) " +
				"with id, name, position, direction, bot flag and avatar. Read-only.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worldId": map[string]any{"type": "string", "description": "World id"},
				},
				"required": []string{"worldId"},
			},
		},
		{
			"name": "bot.run",
			"description": "Execute a full BotScript (ordered multi-op build) against a world " +
				"through the bot op-log (docs/BOT-PROTOCOL.md §7). Supports paint|erase|place plus " +
				"rect|ring|line sugar (expanded into paint ops). Every op carries an idem key; " +
				"replaying the same runId dedupes (safe retries). Audited as role=bot.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"world":     map[string]any{"type": "string", "description": "Target world id"},
					"ops": map[string]any{
						"type": "array",
						"description": "Ordered op sequence",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"op":     map[string]any{"type": "string", "enum": []string{"paint", "erase", "place", "rect", "ring", "line"}},
								"x":      map[string]any{"type": "integer"},
								"y":      map[string]any{"type": "integer"},
								"w":      map[string]any{"type": "integer", "description": "rect/ring width"},
								"h":      map[string]any{"type": "integer", "description": "rect/ring height"},
								"tile":   map[string]any{"type": "string", "description": "Palette name: " + paletteDoc},
								"tileId": map[string]any{"type": "integer"},
							},
							"required": []string{"op", "x", "y"},
						},
					},
					"deviceKey": map[string]any{"type": "string", "description": "Bot account key"},
					"name":      map[string]any{"type": "string", "description": "Bot display name (defaults to MCP)"},
					"runId":     map[string]any{"type": "string", "description": "Idempotency namespace; replay dedupes"},
				},
				"required": []string{"world", "ops", "deviceKey"},
			},
		},
		{
			"name": "world.activity",
			"description": "Recent append-only activity/audit feed for a world. Bot and MCP ops " +
				"appear as kind=edit, role=bot rows with the acting account and idem key. Read-only.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"worldId": map[string]any{"type": "string", "description": "World id"},
					"limit":   map[string]any{"type": "integer", "description": "Max rows (1..100, default 20)"},
				},
				"required": []string{"worldId"},
			},
		},
	}
}

func toolResult(v any) map[string]any {
	b, _ := json.MarshalIndent(v, "", "  ")
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(b)}},
		"structuredContent": v,
		"isError":           false,
	}
}

func toolErrorResult(err error) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "error: " + err.Error()}},
		"isError": true,
	}
}

// --- tool handlers ---

func (s *Server) toolWorldsList(args map[string]any) (any, error) {
	worlds, err := s.backend.Worlds(str(args, "q"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"worlds": worlds}, nil
}

func (s *Server) toolWorldRead(args map[string]any) (any, error) {
	id := str(args, "worldId")
	if id == "" {
		return nil, errors.New("worldId is required")
	}
	return s.backend.World(id)
}

func (s *Server) toolWorldEdit(args map[string]any) (any, error) {
	id := str(args, "worldId")
	op := str(args, "op")
	key := str(args, "deviceKey")
	if id == "" {
		return nil, errors.New("worldId is required")
	}
	if key == "" {
		return nil, errors.New("deviceKey is required (bot account, e.g. bot-<name>)")
	}
	switch op {
	case "paint", "erase", "place":
	default:
		return nil, errors.New("op must be paint|erase|place")
	}
	x, okX := intField(args, "x")
	y, okY := intField(args, "y")
	if !okX || !okY {
		return nil, errors.New("x and y are required")
	}
	tile := str(args, "tile")
	tileID, hasTileID := intField(args, "tileId")
	if op != "erase" && tile == "" && !hasTileID {
		return nil, errors.New("tile or tileId is required (palette: " + paletteDoc + ")")
	}
	res, err := s.backend.Edit(key, id, str(args, "runId"), EditOp{Op: op, X: x, Y: y, Tile: tile, TileID: tileID})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Server) toolWorldChat(args map[string]any) (any, error) {
	id := str(args, "worldId")
	text := str(args, "text")
	key := str(args, "deviceKey")
	if id == "" {
		return nil, errors.New("worldId is required")
	}
	if key == "" {
		return nil, errors.New("deviceKey is required (bot account)")
	}
	if text == "" {
		return nil, errors.New("text is required")
	}
	if len(text) > 2048 {
		return nil, errors.New("text exceeds 2048 bytes")
	}
	channel := str(args, "channel")
	switch channel {
	case "", "space", "proximity":
	default:
		return nil, errors.New("channel must be space|proximity")
	}
	if channel == "" {
		channel = "space"
	}
	res, err := s.backend.Chat(key, str(args, "name"), id, channel, text)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Server) toolPresenceList(args map[string]any) (any, error) {
	id := str(args, "worldId")
	if id == "" {
		return nil, errors.New("worldId is required")
	}
	occ, err := s.backend.Presence(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"worldId": id, "occupants": occ}, nil
}

func (s *Server) toolBotRun(args map[string]any) (any, error) {
	world := str(args, "world")
	key := str(args, "deviceKey")
	if world == "" {
		return nil, errors.New("world is required")
	}
	if key == "" {
		return nil, errors.New("deviceKey is required (bot account)")
	}
	rawOps, ok := args["ops"].([]any)
	if !ok || len(rawOps) == 0 {
		return nil, errors.New("ops must be a non-empty array")
	}
	ops := make([]ScriptOp, 0, len(rawOps))
	for i, raw := range rawOps {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("ops[%d]: expected object", i)
		}
		op := str(m, "op")
		switch op {
		case "paint", "erase", "place", "rect", "ring", "line":
		default:
			return nil, fmt.Errorf("ops[%d]: unknown op %q", i, op)
		}
		x, okX := intField(m, "x")
		y, okY := intField(m, "y")
		if !okX || !okY {
			return nil, fmt.Errorf("ops[%d]: x and y are required", i)
		}
		tile := str(m, "tile")
		tileID, hasTileID := intField(m, "tileId")
		if op != "erase" && tile == "" && !hasTileID {
			return nil, fmt.Errorf("ops[%d]: tile or tileId required", i)
		}
		w, _ := intField(m, "w")
		h, _ := intField(m, "h")
		ops = append(ops, ScriptOp{Op: op, X: x, Y: y, W: w, H: h, Tile: tile, TileID: tileID})
	}
	return s.backend.BotRun(key, str(args, "name"), world, str(args, "runId"), ops)
}

func (s *Server) toolWorldActivity(args map[string]any) (any, error) {
	id := str(args, "worldId")
	if id == "" {
		return nil, errors.New("worldId is required")
	}
	limit := 20
	if v, ok := intField(args, "limit"); ok {
		limit = v
	}
	events, err := s.backend.Activity(id, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"worldId": id, "events": events}, nil
}

// --- response writers ---

func (s *Server) writeResponse(w http.ResponseWriter, r *http.Request, resp map[string]any) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.logf("mcp: marshal response: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		// streamable HTTP: honor the client's Accept with an SSE frame
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, id any, code int, msg string, status int) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// --- param helpers ---

func str(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func intField(m map[string]any, k string) (int, bool) {
	switch v := m[k].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}
