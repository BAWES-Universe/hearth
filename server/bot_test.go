package main

// S7 bot builder acceptance tests (docs/BOT-PROTOCOL.md):
//  1. a headless bot joins garden over a real WS connection and builds the
//     demo structure — tiles persist in GET /api/spaces/{id}, the op_log and
//     the activity/audit feed attribute every op to the bot account.
//  2. replaying the same runId is idempotent (deduped acks, no double apply).
//  3. a human undo works on bot ops (compensating inverse op from the bot's
//     ack priorTileId restores the prior tile; undoOf recorded in op_log).
//  4. script parsing/validation and rect→cells expansion.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"hearth/hmf"
)

// testWSServer boots a full hub + WS server on a random port (the same /ws
// path the live server uses) and returns the ws:// base URL.
func testWSServer(t *testing.T) (*Hub, string) {
	t.Helper()
	h := newTestHub(t)
	go h.Run()
	t.Cleanup(h.Close)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			h.handleWS(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	return h, "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

// testWSClient is a minimal human WS client for tests (observer / undo actor).
type testWSClient struct {
	t      *testing.T
	conn   *websocket.Conn
	selfID string
	msgs   chan map[string]any
}

func dialTestWS(t *testing.T, wsURL, deviceKey, name, space string) *testWSClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL+"?deviceKey="+deviceKey, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	c := &testWSClient{t: t, conn: conn, msgs: make(chan map[string]any, 512)}
	go func() {
		for {
			var env map[string]any
			if err := conn.ReadJSON(&env); err != nil {
				return
			}
			c.msgs <- env
		}
	}()
	c.send("join", map[string]any{"deviceKey": deviceKey, "name": name, "spaceId": space})
	for {
		m := c.next(5 * time.Second)
		if m == nil {
			t.Fatal("no welcome received")
		}
		if m["t"] == "welcome" {
			d, _ := m["d"].(map[string]any)
			c.selfID, _ = d["selfId"].(string)
			return c
		}
	}
}

func (c *testWSClient) send(t string, d map[string]any) {
	c.t.Helper()
	if err := c.conn.WriteJSON(map[string]any{"v": 1, "t": t, "d": d}); err != nil {
		c.t.Fatalf("send %s: %v", t, err)
	}
}

func (c *testWSClient) next(timeout time.Duration) map[string]any {
	select {
	case m := <-c.msgs:
		return m
	case <-time.After(timeout):
		return nil
	}
}

func (c *testWSClient) close() { c.conn.Close() }

// botCfg returns a BotConfig pointed at the test server.
func botCfg(wsURL, name, deviceKey, runID string, script *BotScript) BotConfig {
	return BotConfig{
		Name: name, DeviceKey: deviceKey, World: "garden",
		URL: wsURL, RunID: runID, Script: script,
		Interval: 5 * time.Millisecond, Timeout: 30 * time.Second,
	}
}

// TestBotBuildsHouseInGarden is the acceptance test: a headless bot joins a
// live world over WS and builds a recognizable structure unattended; tiles
// persist; op_log + audit attribute the bot account.
func TestBotBuildsHouseInGarden(t *testing.T) {
	h, wsURL := testWSServer(t)
	script := DemoBuildScript()
	botKey := "bot-bricky-accept"

	res := NewBotClient(botCfg(wsURL, "Bricky", botKey, "accept-run-1", script)).Run()
	if res.FirstErr != "" {
		t.Fatalf("bot run failed: %s", res.FirstErr)
	}
	if res.Applied != len(script.Ops) {
		t.Fatalf("applied = %d, want %d", res.Applied, len(script.Ops))
	}
	if res.Deduped != 0 {
		t.Fatalf("deduped = %d, want 0 on first run", res.Deduped)
	}
	if res.UserID != hashDeviceKey(botKey) {
		t.Fatalf("userId = %q, want bot account %q", res.UserID, hashDeviceKey(botKey))
	}

	// 1) tiles persisted (GET /api/spaces/garden path: LoadWorld + TileAt)
	w, err := h.store.LoadWorld("garden")
	if err != nil {
		t.Fatalf("load garden: %v", err)
	}
	checks := []struct {
		x, y int
		want string
	}{
		{16, 20, "roof"}, {20, 21, "roof"}, // house roof
		{16, 22, "wall"}, {20, 24, "wall"}, // house walls
		{17, 23, "flower"}, {19, 23, "flower"}, // windows
		{18, 24, "door"},                       // door
		{26, 20, "flower"}, {30, 20, "flower"}, // heart lobes
		{25, 21, "flower"}, {31, 21, "flower"}, // heart row
		{28, 25, "flower"},                     // heart tip
	}
	for _, c := range checks {
		if got := w.TileAt(c.x, c.y); got != c.want {
			t.Errorf("tile (%d,%d) = %q, want %q", c.x, c.y, got, c.want)
		}
	}

	// 2) op_log attributes the bot: by = bot entity, actor = bot account,
	//    idem keys = <runId>:<idx>, server seqs ascending.
	ops, err := h.store.LoadOpLog("garden")
	if err != nil {
		t.Fatalf("load op_log: %v", err)
	}
	if len(ops) != len(script.Ops) {
		t.Fatalf("op_log rows = %d, want %d", len(ops), len(script.Ops))
	}
	lastSeq := int64(0)
	for i, op := range ops {
		if op.Actor != hashDeviceKey(botKey) {
			t.Errorf("op %d actor = %q, want bot account %q", i, op.Actor, hashDeviceKey(botKey))
		}
		if op.By != res.BotID {
			t.Errorf("op %d by = %q, want bot entity %q", i, op.By, res.BotID)
		}
		if want := fmt.Sprintf("accept-run-1:%d", i); op.Idem != want {
			t.Errorf("op %d idem = %q, want %q", i, op.Idem, want)
		}
		if op.Seq <= lastSeq {
			t.Errorf("op %d seq = %d not monotonic (last %d)", i, op.Seq, lastSeq)
		}
		lastSeq = op.Seq
	}

	// 3) activity/audit feed attributes the build to the bot account
	events, err := h.store.RecentActivity("garden", 200)
	if err != nil {
		t.Fatalf("recent activity: %v", err)
	}
	botEdits := 0
	for _, e := range events {
		if e.Kind == "edit" && e.Actor == hashDeviceKey(botKey) && e.Role == "bot" {
			botEdits++
		}
	}
	if botEdits != len(script.Ops) {
		t.Errorf("audit edit rows = %d, want %d (bot account)", botEdits, len(script.Ops))
	}

	// 4) the bot account exists as a user with its display name
	if name := h.store.userDisplay(hashDeviceKey(botKey)); name != "Bricky" {
		t.Errorf("bot user name = %q, want Bricky", name)
	}
}

// TestBotReplayIsIdempotent: re-running the same runId is replay-safe — every
// op acks deduped, nothing is re-applied, op_log keeps one row per key.
func TestBotReplayIsIdempotent(t *testing.T) {
	h, wsURL := testWSServer(t)
	script := DemoBuildScript()
	cfg := botCfg(wsURL, "Bricky", "bot-bricky-replay", "replay-run-1", script)

	r1 := NewBotClient(cfg).Run()
	if r1.FirstErr != "" || r1.Applied != len(script.Ops) {
		t.Fatalf("first run: err=%q applied=%d", r1.FirstErr, r1.Applied)
	}

	r2 := NewBotClient(cfg).Run()
	if r2.FirstErr != "" {
		t.Fatalf("replay failed: %s", r2.FirstErr)
	}
	if r2.Applied != 0 {
		t.Errorf("replay applied = %d, want 0 (nothing re-applied)", r2.Applied)
	}
	if r2.Deduped != len(script.Ops) {
		t.Errorf("replay deduped = %d, want %d", r2.Deduped, len(script.Ops))
	}
	if len(r2.Seqs) != len(script.Ops) {
		t.Fatalf("replay seqs = %d, want %d", len(r2.Seqs), len(script.Ops))
	}
	for i := range r1.Seqs {
		if r1.Seqs[i] != r2.Seqs[i] {
			t.Errorf("seq[%d] = %d (replay) vs %d (first) — replays must resolve to the original seqs", i, r2.Seqs[i], r1.Seqs[i])
		}
	}

	ops, err := h.store.LoadOpLog("garden")
	if err != nil {
		t.Fatalf("load op_log: %v", err)
	}
	if len(ops) != len(script.Ops) {
		t.Errorf("op_log rows = %d after replay, want %d (no duplicates)", len(ops), len(script.Ops))
	}
}

// TestHumanUndoOfBotOp: bot ops are compensating-inverse compatible — a human
// observer receives the bot's ack (priorTileId), sends the inverse op, and
// the tile returns to its prior state; the undo op records undoOf.
func TestHumanUndoOfBotOp(t *testing.T) {
	h, wsURL := testWSServer(t)

	// human observer joins BEFORE the bot so it receives the bot's acks
	human := dialTestWS(t, wsURL, "human-undo-1", "Human", "garden")
	defer human.close()

	script := DemoBuildScript()
	res := NewBotClient(botCfg(wsURL, "Bricky", "bot-bricky-undo", "undo-run-1", script)).Run()
	if res.FirstErr != "" || res.Applied != len(script.Ops) {
		t.Fatalf("bot run: err=%q applied=%d", res.FirstErr, res.Applied)
	}

	// find the bot's ack for the door paint (18,24) — single-cell ack with
	// priorTileId (the wall the door was painted over)
	var doorSeq int64
	var doorPrior int
	found := false
	for i := 0; i < 50; i++ {
		m := human.next(3 * time.Second)
		if m == nil {
			t.Fatal("timed out waiting for bot door ack")
		}
		if m["t"] != "edit" {
			continue
		}
		d, _ := m["d"].(map[string]any)
		if d["by"] == human.selfID {
			continue
		}
		x, _ := getInt(d, "x")
		y, _ := getInt(d, "y")
		if x == 18 && y == 24 {
			doorSeq = int64FromAny(d["seq"])
			doorPrior = int(int64FromAny(d["priorTileId"]))
			found = true
			break
		}
	}
	if !found {
		t.Fatal("bot door ack not received by human observer")
	}
	if doorPrior != TileID("wall") {
		t.Fatalf("door priorTileId = %d, want %d (wall)", doorPrior, TileID("wall"))
	}

	// human undoes the bot op: compensating inverse paint + undoOf
	human.send("edit", map[string]any{
		"op": "paint", "x": 18, "y": 24, "tileId": doorPrior, "undoOf": doorSeq,
	})

	// wait for the human's own ack
	acked := false
	for i := 0; i < 50; i++ {
		m := human.next(3 * time.Second)
		if m == nil {
			t.Fatal("timed out waiting for undo ack")
		}
		if m["t"] != "edit" {
			continue
		}
		d, _ := m["d"].(map[string]any)
		if d["by"] == human.selfID {
			acked = true
			break
		}
	}
	if !acked {
		t.Fatal("undo ack not received")
	}

	// tile restored to its prior state (wall — the door is gone)
	w, err := h.store.LoadWorld("garden")
	if err != nil {
		t.Fatalf("load garden: %v", err)
	}
	if got := w.TileAt(18, 24); got != "wall" {
		t.Errorf("after human undo, tile (18,24) = %q, want %q (prior)", got, "wall")
	}
	// the undo op is in the same op stream with undoOf = the bot op's seq
	ops, err := h.store.LoadOpLog("garden")
	if err != nil {
		t.Fatalf("load op_log: %v", err)
	}
	last := ops[len(ops)-1]
	if last.UndoOf != doorSeq {
		t.Errorf("undo op undoOf = %d, want %d (bot door seq)", last.UndoOf, doorSeq)
	}
	if last.By != human.selfID {
		t.Errorf("undo op by = %q, want human entity %q", last.By, human.selfID)
	}
}

// TestBotScriptParse covers the agent-facing script format.
func TestBotScriptParse(t *testing.T) {
	raw := `{"v":1,"name":"bricky","world":"garden","ops":[
		{"op":"paint","x":5,"y":5,"tile":"wall"},
		{"op":"paint","x":6,"y":5,"tileId":13},
		{"op":"rect","x":4,"y":6,"w":5,"h":3,"tile":"wall"},
		{"op":"erase","x":5,"y":5},
		{"op":"line","x":0,"y":0,"w":4,"h":4,"tile":"path"}
	]}`
	s, err := ParseBotScript([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if s.Name != "bricky" || s.World != "garden" || len(s.Ops) != 5 {
		t.Fatalf("script fields wrong: %+v", s)
	}

	// rect expands to w*h cells via editMessage
	bot := NewBotClient(BotConfig{RunID: "parse-run"})
	m, err := bot.editMessage(2, s.Ops[2])
	if err != nil {
		t.Fatalf("editMessage rect: %v", err)
	}
	d := m["d"].(map[string]any)
	cells, _ := d["cells"].([]hmf.Cell)
	if len(cells) != 15 {
		t.Errorf("rect cells = %d, want 15 (5x3)", len(cells))
	}
	if d["idem"] != "parse-run:2" {
		t.Errorf("idem = %v, want parse-run:2", d["idem"])
	}
	if got, _ := d["tileId"].(int); got != TileID("wall") {
		t.Errorf("rect tileId = %v, want %d", d["tileId"], TileID("wall"))
	}

	// line expands to the bresenham cells
	lm, err := bot.editMessage(4, s.Ops[4])
	if err != nil {
		t.Fatalf("editMessage line: %v", err)
	}
	ld := lm["d"].(map[string]any)
	lc, _ := ld["cells"].([]hmf.Cell)
	if len(lc) != 5 {
		t.Errorf("line cells = %d, want 5 (diagonal)", len(lc))
	}

	// rejections: unknown op, missing tile, bad version
	for _, bad := range []string{
		`{"v":1,"ops":[{"op":"explode","x":0,"y":0,"tile":"wall"}]}`,
		`{"v":1,"ops":[{"op":"paint","x":0,"y":0}]}`,
		`{"v":2,"ops":[{"op":"paint","x":0,"y":0,"tile":"wall"}]}`,
		`{"v":1,"ops":[]}`,
	} {
		if _, err := ParseBotScript([]byte(bad)); err == nil {
			t.Errorf("ParseBotScript(%s) should have failed", bad)
		}
	}
}

// TestBotSpawnAPI exercises POST /api/bots through the HTTP handler.
func TestBotSpawnAPI(t *testing.T) {
	h, wsURL := testWSServer(t)
	_ = wsURL

	// POST /api/bots against a fake request whose Host points at the test
	// server — the spawned bot dials ws://<host>/ws.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			h.handleWS(w, r)
			return
		}
		if r.URL.Path == "/api/bots" {
			h.handleBots(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	body := fmt.Sprintf(`{"name":"ApiBot","world":"garden","action":"build","runId":"api-run-1","intervalMs":2}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/bots", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	rr := httptest.NewRecorder()
	h.handleBots(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /api/bots = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bot, _ := out["bot"].(map[string]any)
	runID, _ := bot["runId"].(string)
	if runID != "api-run-1" {
		t.Fatalf("runId = %q, want api-run-1", runID)
	}

	// the run completes asynchronously — poll the status endpoint
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := h.bots.Get(runID)
		if st != nil && st.Status == "done" {
			if st.Applied != 13 {
				t.Fatalf("api bot applied = %d, want 13 (demo script)", st.Applied)
			}
			return
		}
		if st != nil && st.Status == "error" {
			t.Fatalf("api bot error: %s", st.Err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("api bot did not finish in time")
}
