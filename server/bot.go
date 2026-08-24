package main

// S7 — headless bot builder: the agent-facing op-log client.
//
// A BotClient is a headless Go bot that authenticates with a deviceKey,
// joins a live world over the SAME /ws path humans use, receives the welcome
// (world state), and writes paint/place ops through the same frozen edit
// envelope humans use ({t:'edit', d:{op,x,y,tileId,idem}}). Every op flows
// through handleEdit → ApplyOp → AppendOp, so bot builds are persisted in the
// same append-only op_log, attributed to the bot's account (op.actor = user
// id of the bot deviceKey), replay-safe via idempotency keys, and undoable by
// humans through the same compensating-inverse-op stream.
//
// The agent-facing contract is documented in docs/BOT-PROTOCOL.md. Spawn
// paths: CLI ("hearth-server bot build ..."), REST (POST /api/bots), and
// programmatic (NewBotClient(cfg).Run() — used by the Go tests).
//
// Bot op-sequence scripts are small JSON documents:
//
//	{"v":1,"name":"bricky","world":"garden","ops":[
//	  {"op":"paint","x":5,"y":5,"tile":"wall"},
//	  {"op":"rect","x":5,"y":6,"w":5,"h":3,"tile":"wall"},
//	  {"op":"paint","x":7,"y":7,"tile":"door"}
//	]}
//
// op is a frozen HMF v1 kind (paint|erase|place) or batch sugar
// (rect|ring|line) that the client expands into frozen paint ops with cells
// before sending. tile (palette name) and tileId (numeric) are both accepted.
// Each emitted op carries idem = "<runId>:<index>" — replaying the same
// runId is a no-op (server acks deduped:true).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"hearth/hmf"
)

// BotScript is the agent-facing op-sequence format (docs/BOT-PROTOCOL.md).
type BotScript struct {
	V     int           `json:"v"`     // format version (1)
	Name  string        `json:"name"`  // bot display name
	World string        `json:"world"` // target world id (e.g. "garden")
	Ops   []BotScriptOp `json:"ops"`   // ordered op sequence
}

// BotScriptOp is one op of a script. The wire only ever carries frozen HMF v1
// ops; rect/ring/line are client-side sugar expanded into paint ops.
type BotScriptOp struct {
	Op     string `json:"op"`               // paint|erase|place|rect|ring|line
	X      int    `json:"x,omitempty"`      // anchor x (or rect/ring origin)
	Y      int    `json:"y,omitempty"`      // anchor y (or rect/ring origin)
	W      int    `json:"w,omitempty"`      // rect/ring width
	H      int    `json:"h,omitempty"`      // rect/ring height
	Tile   string `json:"tile,omitempty"`   // palette name (floor|wall|...)
	TileID int    `json:"tileId,omitempty"` // numeric palette id (either works)
}

// ParseBotScript validates a bot op-sequence JSON document.
func ParseBotScript(b []byte) (*BotScript, error) {
	var s BotScript
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse bot script: %w", err)
	}
	if s.V != 1 {
		return nil, fmt.Errorf("bot script: unsupported version %d (want 1)", s.V)
	}
	if len(s.Ops) == 0 {
		return nil, errors.New("bot script: ops required")
	}
	for i, op := range s.Ops {
		switch op.Op {
		case "paint", "erase", "place", "rect", "ring", "line":
		default:
			return nil, fmt.Errorf("bot script op %d: unknown op %q", i, op.Op)
		}
		if op.Op != "erase" && op.Tile == "" && op.TileID == 0 {
			return nil, fmt.Errorf("bot script op %d: tile or tileId required", i)
		}
	}
	return &s, nil
}

// DemoBuildScript is the built-in demo: a recognizable 5x5 house (roof, wall,
// flower windows, door) plus a heart of flowers, built in garden's SE meadow
// (seed-clear grass at x16..31, y20..25 — the same area the live demo builds).
func DemoBuildScript() *BotScript {
	return &BotScript{
		V: 1, Name: "Bricky", World: "garden",
		Ops: []BotScriptOp{
			// house: roof (5x2), walls (5x3), flower windows, door
			{Op: "rect", X: 16, Y: 20, W: 5, H: 2, Tile: "roof"},
			{Op: "rect", X: 16, Y: 22, W: 5, H: 3, Tile: "wall"},
			{Op: "paint", X: 17, Y: 23, Tile: "flower"},
			{Op: "paint", X: 19, Y: 23, Tile: "flower"},
			{Op: "paint", X: 18, Y: 24, Tile: "door"},
			// heart of flowers (7x6 pixel heart at x25..31, y20..25)
			{Op: "paint", X: 26, Y: 20, Tile: "flower"},
			{Op: "paint", X: 27, Y: 20, Tile: "flower"},
			{Op: "paint", X: 29, Y: 20, Tile: "flower"},
			{Op: "paint", X: 30, Y: 20, Tile: "flower"},
			{Op: "rect", X: 25, Y: 21, W: 7, H: 2, Tile: "flower"},
			{Op: "rect", X: 26, Y: 23, W: 5, H: 1, Tile: "flower"},
			{Op: "rect", X: 27, Y: 24, W: 3, H: 1, Tile: "flower"},
			{Op: "paint", X: 28, Y: 25, Tile: "flower"},
		},
	}
}

// BotConfig configures one bot run.
type BotConfig struct {
	Name      string        // display name (also upserted on the user row)
	DeviceKey string        // bot account key; audit attributes to sha256(this)
	World     string        // target world id
	URL       string        // ws://host[:port]/ws (default ws://127.0.0.1:8090/ws)
	RunID     string        // stable idempotency namespace; replay-safe
	Script    *BotScript    // op sequence to emit
	Interval  time.Duration // delay between ops (default 120ms)
	Timeout   time.Duration // overall run timeout (default 90s)
}

// BotResult is the outcome of one bot run (JSON-friendly for the CLI/API).
type BotResult struct {
	RunID    string  `json:"runId"`
	Name     string  `json:"name"`
	World    string  `json:"world"`
	BotID    string  `json:"botId"`   // entity/session id from welcome
	UserID   string  `json:"userId"`  // bot account id (sha256(deviceKey))
	Ops      int     `json:"ops"`     // ops in the script
	Applied  int     `json:"applied"` // ops applied by the server
	Deduped  int     `json:"deduped"` // ops skipped as already applied (replay)
	Seqs     []int64 `json:"seqs"`    // server op seqs, in order
	FirstErr string  `json:"firstErr,omitempty"`
	Done     bool    `json:"done"`
}

// BotClient is a headless Go bot: authenticates with a deviceKey, joins a
// live world over WS, and emits frozen editor ops via the edit envelope.
type BotClient struct {
	cfg    BotConfig
	conn   *websocket.Conn
	selfID string
	mu     sync.Mutex
	res    BotResult
}

// NewBotClient builds a bot client for cfg (defaults filled at Run).
func NewBotClient(cfg BotConfig) *BotClient {
	return &BotClient{cfg: cfg}
}

// Run connects, joins the world, emits the script's ops through the edit
// envelope (one op in flight at a time, correlated by ack), and returns the
// result. Replay-safe: re-running with the same RunID acks every op as
// deduped instead of re-applying.
func (b *BotClient) Run() *BotResult {
	cfg := b.cfg
	if cfg.World == "" {
		cfg.World = "garden"
	}
	if cfg.URL == "" {
		cfg.URL = "ws://127.0.0.1:8090/ws"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 120 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.DeviceKey == "" {
		cfg.DeviceKey = "bot-" + slug(cfg.Name)
	}
	if cfg.Name == "" {
		cfg.Name = "Bot"
	}
	if cfg.RunID == "" {
		cfg.RunID = "bot-" + slug(cfg.Name) + "-" + randHex(4)
	}
	b.cfg = cfg

	res := &b.res
	res.RunID = cfg.RunID
	res.Name = cfg.Name
	res.World = cfg.World
	res.UserID = hashDeviceKey(cfg.DeviceKey)
	res.Ops = 0
	if cfg.Script != nil {
		res.Ops = len(cfg.Script.Ops)
	}
	defer func() { res.Done = true }()

	u, err := url.Parse(cfg.URL)
	if err != nil {
		res.FirstErr = "bad url: " + err.Error()
		return res
	}
	q := u.Query()
	q.Set("deviceKey", cfg.DeviceKey)
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		res.FirstErr = "dial: " + err.Error()
		return res
	}
	defer conn.Close()
	b.conn = conn

	if err := conn.WriteJSON(map[string]any{
		"v": 1, "t": "join",
		"d": map[string]any{
			"deviceKey": cfg.DeviceKey,
			"name":      cfg.Name,
			"spaceId":   cfg.World,
		},
	}); err != nil {
		res.FirstErr = "join send: " + err.Error()
		return res
	}

	deadline := time.Now().Add(cfg.Timeout)
	acked := 0
	sent := 0
	ops := cfg.Script.Ops
	for acked < len(ops) {
		if time.Now().After(deadline) {
			if res.FirstErr == "" {
				res.FirstErr = fmt.Sprintf("timeout: %d/%d ops acked", acked, len(ops))
			}
			break
		}
		// send the next op once the previous one is acked (strict ordering;
		// acks from other actors are filtered by by == selfID)
		if b.selfID != "" && sent < len(ops) && sent == acked {
			m, err := b.editMessage(sent, ops[sent])
			if err != nil {
				res.FirstErr = err.Error()
				break
			}
			if err := conn.WriteJSON(m); err != nil {
				res.FirstErr = "edit send: " + err.Error()
				break
			}
			sent++
			time.Sleep(cfg.Interval)
		}
		if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
			res.FirstErr = err.Error()
			break
		}
		var env struct {
			T string          `json:"t"`
			D json.RawMessage `json:"d"`
		}
		if err := conn.ReadJSON(&env); err != nil {
			if res.FirstErr == "" {
				res.FirstErr = "read: " + err.Error()
			}
			break
		}
		if env.T != "edit" && env.T != "welcome" && env.T != "error" {
			continue // state/chat/pong/etc — ignore
		}
		// d may be ANY JSON (frozen contract: state's d is an entity ARRAY).
		// Only the handled types below are object payloads; anything that
		// fails to decode as an object is skipped, never fatal.
		var d map[string]any
		if len(env.D) > 0 {
			if err := json.Unmarshal(env.D, &d); err != nil {
				continue
			}
		}
		switch env.T {
		case "welcome":
			b.selfID, _ = d["selfId"].(string)
			res.BotID = b.selfID
		case "edit":
			by, _ := d["by"].(string)
			if by != b.selfID {
				continue // another actor's edit — not ours
			}
			applied, _ := d["applied"].(bool)
			deduped, _ := d["deduped"].(bool)
			if seq := int64FromAny(d["seq"]); seq > 0 {
				res.Seqs = append(res.Seqs, seq)
			}
			if applied && !deduped {
				res.Applied++
			}
			if deduped {
				res.Deduped++
			}
			acked++
		case "error":
			code, _ := d["code"].(string)
			msg, _ := d["message"].(string)
			if res.FirstErr == "" {
				res.FirstErr = code + ": " + msg
			}
			acked++ // do not hang on a rejected op
		}
	}
	return res
}

// editMessage builds one frozen edit envelope for script op idx, expanding
// rect/ring/line sugar into a batch paint op with cells, and tagging the op
// with its idempotency key "<runId>:<idx>".
func (b *BotClient) editMessage(idx int, sop BotScriptOp) (map[string]any, error) {
	kind := sop.Op
	var cells []hmf.Cell
	switch kind {
	case "rect":
		if sop.W <= 0 || sop.H <= 0 {
			return nil, fmt.Errorf("bot op %d: rect needs w,h > 0", idx)
		}
		for y := sop.Y; y < sop.Y+sop.H; y++ {
			for x := sop.X; x < sop.X+sop.W; x++ {
				cells = append(cells, hmf.Cell{X: x, Y: y})
			}
		}
		kind = "paint"
	case "ring":
		if sop.W <= 0 || sop.H <= 0 {
			return nil, fmt.Errorf("bot op %d: ring needs w,h > 0", idx)
		}
		x1, y1 := sop.X, sop.Y
		x2, y2 := sop.X+sop.W-1, sop.Y+sop.H-1
		for x := x1; x <= x2; x++ {
			cells = append(cells, hmf.Cell{X: x, Y: y1}, hmf.Cell{X: x, Y: y2})
		}
		for y := y1 + 1; y < y2; y++ {
			cells = append(cells, hmf.Cell{X: x1, Y: y}, hmf.Cell{X: x2, Y: y})
		}
		kind = "paint"
	case "line":
		cells = botBresenham(sop.X, sop.Y, sop.W, sop.H)
		kind = "paint"
	}
	tid := sop.TileID
	if tid == 0 && sop.Tile != "" {
		tid = hmf.TileID(sop.Tile)
	}
	if kind == "erase" {
		tid = hmf.FloorTile
	}
	d := map[string]any{
		"op":     kind,
		"x":      sop.X,
		"y":      sop.Y,
		"tileId": tid,
		"idem":   fmt.Sprintf("%s:%d", b.cfg.RunID, idx),
	}
	if len(cells) > 0 {
		d["cells"] = cells
	}
	return map[string]any{"v": 1, "t": "edit", "d": d}, nil
}

// botBresenham returns the cells of a line from (x0,y0) to (x1,y1).
func botBresenham(x0, y0, x1, y1 int) []hmf.Cell {
	var out []hmf.Cell
	dx := x1 - x0
	if dx < 0 {
		dx = -dx
	}
	dy := y1 - y0
	if dy < 0 {
		dy = -dy
	}
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx - dy
	for {
		out = append(out, hmf.Cell{X: x0, Y: y0})
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
	return out
}

func int64FromAny(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// --- BotManager: in-process spawn registry (REST /api/bots, tests) ---

// BotRunStatus is the live status of one spawned bot run.
type BotRunStatus struct {
	RunID      string  `json:"runId"`
	Name       string  `json:"name"`
	World      string  `json:"world"`
	DeviceKey  string  `json:"deviceKey"`
	UserID     string  `json:"userId"`
	Status     string  `json:"status"` // running|done|error
	Ops        int     `json:"ops"`
	Applied    int     `json:"applied"`
	Deduped    int     `json:"deduped"`
	Seqs       []int64 `json:"seqs"`
	Err        string  `json:"err,omitempty"`
	StartedAt  string  `json:"startedAt"`
	FinishedAt string  `json:"finishedAt,omitempty"`
}

// BotManager tracks spawned bot runs (one goroutine per run).
type BotManager struct {
	mu   sync.Mutex
	runs map[string]*BotRunStatus
}

func NewBotManager() *BotManager {
	return &BotManager{runs: map[string]*BotRunStatus{}}
}

// Spawn starts cfg.Script in a goroutine and returns its status handle.
func (bm *BotManager) Spawn(cfg BotConfig) *BotRunStatus {
	now := time.Now().UTC().Format(time.RFC3339)
	run := &BotRunStatus{
		RunID: cfg.RunID, Name: cfg.Name, World: cfg.World,
		DeviceKey: cfg.DeviceKey, UserID: hashDeviceKey(cfg.DeviceKey),
		Status: "running", StartedAt: now,
	}
	if cfg.Script != nil {
		run.Ops = len(cfg.Script.Ops)
	}
	bm.mu.Lock()
	bm.runs[cfg.RunID] = run
	bm.mu.Unlock()
	go func() {
		res := NewBotClient(cfg).Run()
		bm.mu.Lock()
		defer bm.mu.Unlock()
		run.Applied = res.Applied
		run.Deduped = res.Deduped
		run.Seqs = res.Seqs
		run.Err = res.FirstErr
		run.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		if res.FirstErr != "" {
			run.Status = "error"
		} else {
			run.Status = "done"
		}
	}()
	return run
}

// Get returns the status of one run (nil when unknown).
func (bm *BotManager) Get(runID string) *BotRunStatus {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return bm.runs[runID]
}

// List returns run statuses, newest first (cap 50).
func (bm *BotManager) List() []*BotRunStatus {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	out := make([]*BotRunStatus, 0, len(bm.runs))
	for _, r := range bm.runs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}
