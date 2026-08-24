package main

// Live-test regression tests (khalid 2026-08-24):
//  1. town-square movement — spawn snaps to a passable tile when walls were
//     painted on/near the spawn point (A* refuses a blocked start).
//  2. remote player visibility — the state frame is the FROZEN entity ARRAY
//     (PROTOCOL.md), so the client's Array.isArray(d) dispatch renders peers.
//  3. proximity chat — short WorkAdventure-style bubble radius, sender echoed.
//  4. per-space + global chat — space stays inside one world; global crosses
//     every world; dm unchanged.

import (
	"testing"
	"time"
)

// dialTestWSAt is dialTestWS with an explicit spawn (join accepts x/y).
func dialTestWSAt(t *testing.T, wsURL, deviceKey, name, space string, x, y int) *testWSClient {
	t.Helper()
	c := dialTestWS(t, wsURL, deviceKey, name, space)
	// re-join with coordinates (first join used the world spawn)
	c.send("join", map[string]any{
		"deviceKey": deviceKey, "name": name, "spaceId": space, "x": x, "y": y,
	})
	for {
		m := c.next(5 * time.Second)
		if m == nil {
			t.Fatal("no welcome after reposition join")
		}
		if m["t"] == "welcome" {
			d, _ := m["d"].(map[string]any)
			c.selfID, _ = d["selfId"].(string)
			return c
		}
	}
}

func waitMsg(t *testing.T, c *testWSClient, typ string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		left := time.Until(deadline)
		if left < 0 {
			left = time.Millisecond
		}
		m := c.next(left)
		if m == nil {
			continue
		}
		if m["t"] == typ {
			return m
		}
	}
	return nil
}

// TestSpawnSnapsToPassable reproduces the town-square live-test bug: walls
// painted on the spawn tile freeze tap-to-move. A joining player must land on
// the nearest passable tile instead of the wall.
func TestSpawnSnapsToPassable(t *testing.T) {
	h, wsURL := testWSServer(t)
	ts := h.space("town-square")
	if ts == nil {
		t.Fatal("town-square space missing")
	}
	// Pollute the spawn like the live DB: wall on the spawn tile + neighbors.
	for _, p := range [][2]int{{16, 16}, {15, 16}, {17, 16}, {16, 15}, {16, 17}} {
		ts.World.SetTile(p[0], p[1], "wall")
	}
	if ts.World.Passable(16, 16) {
		t.Fatal("test setup: spawn tile should be impassable")
	}

	c := dialTestWS(t, wsURL, "spawn-test-"+t.Name(), "SpawnTest", "town-square")
	defer c.close()

	// dialTestWS consumes the welcome internally, so read the entity's landed
	// position straight from the space (the state stream excludes self).
	ts.mu.RLock()
	e := ts.entities[c.selfID]
	ts.mu.RUnlock()
	if e == nil {
		t.Fatal("entity missing after join")
	}
	x, y := e.X, e.Y
	if int(x) == 16 && int(y) == 16 {
		t.Fatalf("entity spawned ON the wall tile (16,16)")
	}
	if !ts.World.Passable(int(x), int(y)) {
		t.Fatalf("entity spawned on impassable tile %d,%d", int(x), int(y))
	}

	// movement must work: move to a passable tile and confirm the echo state
	c.send("move", map[string]any{"x": int(x) + 1, "y": int(y), "dir": "right", "seq": 1})
	st := waitMsg(t, c, "state", 3*time.Second)
	if st == nil {
		t.Fatal("no state frame after move")
	}
}

// TestStateFrameIsFrozenArray: the state message d must be the entity ARRAY
// (PROTOCOL.md line 20). The old {spaceId,entities,t} object wrapper made the
// client's Array.isArray(d) dispatch pass [] and remotes never rendered.
func TestStateFrameIsFrozenArray(t *testing.T) {
	_, wsURL := testWSServer(t)
	a := dialTestWS(t, wsURL, "vis-a-"+t.Name(), "VisA", "garden")
	defer a.close()
	b := dialTestWS(t, wsURL, "vis-b-"+t.Name(), "VisB", "garden")
	defer b.close()

	// B moves; A must receive a state frame whose d is an ARRAY containing B.
	b.send("move", map[string]any{"x": 26, "y": 16, "dir": "right", "seq": 1})
	st := waitMsg(t, a, "state", 5*time.Second)
	if st == nil {
		t.Fatal("A never received a state frame")
	}
	arr, ok := st["d"].([]any)
	if !ok {
		t.Fatalf("state d is not an array: %T %v", st["d"], st["d"])
	}
	if len(arr) == 0 {
		t.Fatal("state array is empty — remotes would never render")
	}
	found := false
	for _, raw := range arr {
		e, _ := raw.(map[string]any)
		if e["id"] == b.selfID {
			found = true
			if _, ok := e["x"].(float64); !ok {
				t.Fatalf("state entity missing numeric x: %v", e)
			}
		}
	}
	if !found {
		t.Fatalf("state array does not contain peer %s (entities: %v)", b.selfID, arr)
	}
}

// TestProximityChatBubbleRange: proximity reaches players within the bubble
// radius (incl. the SENDER — self echo for the pending message + own bubble)
// and stops beyond it.
func TestProximityChatBubbleRange(t *testing.T) {
	_, wsURL := testWSServer(t)
	a := dialTestWSAt(t, wsURL, "prox-a-"+t.Name(), "ProxA", "garden", 10, 10)
	defer a.close()
	b := dialTestWSAt(t, wsURL, "prox-b-"+t.Name(), "ProxB", "garden", 10, 11) // 1 tile
	defer b.close()
	c := dialTestWSAt(t, wsURL, "prox-c-"+t.Name(), "ProxC", "garden", 10, 30) // 19 tiles
	defer c.close()

	a.send("chat", map[string]any{"channel": "proximity", "text": "bubble hi", "nonce": "n1"})

	// self echo: A receives its own proximity chat
	self := waitMsg(t, a, "chat", 3*time.Second)
	if self == nil {
		t.Fatal("sender did not receive own proximity echo")
	}
	if d, _ := self["d"].(map[string]any); d["text"] != "bubble hi" {
		t.Fatalf("self echo text wrong: %v", self["d"])
	}
	// in range: B receives it
	bMsg := waitMsg(t, b, "chat", 3*time.Second)
	if bMsg == nil {
		t.Fatal("in-range peer B did not receive proximity chat")
	}
	// out of range: C must NOT receive it within 1.5s
	if m := waitMsg(t, c, "chat", 1500*time.Millisecond); m != nil {
		t.Fatalf("out-of-range peer C received proximity chat: %v", m["d"])
	}
}

// TestSpaceAndGlobalChatRouting: space chat stays inside one world; global
// chat crosses every world.
func TestSpaceAndGlobalChatRouting(t *testing.T) {
	_, wsURL := testWSServer(t)
	gardenA := dialTestWS(t, wsURL, "sp-a-"+t.Name(), "GardenA", "garden")
	defer gardenA.close()
	gardenB := dialTestWS(t, wsURL, "sp-b-"+t.Name(), "GardenB", "garden")
	defer gardenB.close()
	labC := dialTestWS(t, wsURL, "sp-c-"+t.Name(), "LabC", "lab")
	defer labC.close()

	// space chat: garden only — LabC must NOT receive it
	gardenA.send("chat", map[string]any{"channel": "space", "text": "garden only", "nonce": "n1"})
	if m := waitMsg(t, labC, "chat", 1200*time.Millisecond); m != nil {
		t.Fatalf("space chat leaked across worlds: %v", m["d"])
	}
	if m := waitMsg(t, gardenB, "chat", 3*time.Second); m == nil {
		t.Fatal("space chat did not reach peer in the same space")
	}

	// global chat: EVERYONE receives it (all spaces)
	labC.send("chat", map[string]any{"channel": "global", "text": "hello all", "nonce": "n2"})
	if m := waitMsg(t, gardenA, "chat", 3*time.Second); m == nil {
		t.Fatal("global chat did not reach garden peer A")
	}
	if m := waitMsg(t, gardenB, "chat", 3*time.Second); m == nil {
		t.Fatal("global chat did not reach garden peer B")
	}
	if m := waitMsg(t, labC, "chat", 3*time.Second); m == nil {
		t.Fatal("global chat did not echo to sender in lab")
	}
}
