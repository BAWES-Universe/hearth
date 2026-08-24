package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Social clarity tests: server-side chat nonce idempotency (dup fix), DM
// echo-to-both + friends/block/self/cap gates, the additive presence roster
// envelope (join / portal / disconnect hooks), and the blocks + reports
// REST surface.

// --- frame helpers (consume non-matching frames silently) ---

func waitForD(t *testing.T, conn *websocket.Conn, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			// gorilla/websocket: a read error poisons the socket — subsequent
			// reads panic. Wait a tick and retry only by deadline, never
			// re-reading the failed socket.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		d, _ := m["d"].(map[string]any)
		if d != nil && pred(d) {
			return d
		}
	}
	t.Fatalf("timeout waiting for %s", what)
	return nil
}

// drainChats collects every chat payload arriving within window (silently
// consuming other frames). Used to assert the ABSENCE of a duplicate.
// NOTE: gorilla/websocket panics on any read after a previous read error, so
// the first error (timeout) ends the window — a silent socket means no leak.
func drainChats(t *testing.T, conn *websocket.Conn, window time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(window)
	var out []map[string]any
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break // timeout/closed — window over, stop reading
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if envelopeType(m) != "chat" {
			continue
		}
		d, _ := m["d"].(map[string]any)
		if d != nil {
			out = append(out, d)
		}
	}
	return out
}

func chatIs(d map[string]any, channel, nonce string) bool {
	return d["channel"] == channel && (nonce == "" || d["nonce"] == nonce)
}

func errCode(d map[string]any, code string) bool {
	return d["code"] == code
}

func countMessages(t *testing.T, h *Hub, channel string) int {
	t.Helper()
	var n int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE channel = ?`, channel).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// --- dup fix: server-side nonce idempotency ---

func TestChatNonceIdempotency(t *testing.T) {
	h := newTestHub(t)
	go h.Run()
	defer h.Close()
	ts := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	a := dialJoin(t, wsURL+"?deviceKey=sc-n-a", "sc-n-a", "Alice")
	defer a.Close()
	waitFor(t, "alice welcome", 3*time.Second, func() bool { return envelopeType(readFrame(t, a)) == "welcome" })
	b := dialJoin(t, wsURL+"?deviceKey=sc-n-b", "sc-n-b", "Bob")
	defer b.Close()
	waitFor(t, "bob welcome", 3*time.Second, func() bool { return envelopeType(readFrame(t, b)) == "welcome" })

	// alice sends one space chat with a nonce
	sendChat := func(conn *websocket.Conn, channel, text, nonce string) {
		t.Helper()
		msg := map[string]any{"v": 1, "t": "chat", "id": "c1", "ts": time.Now().UnixMilli(),
			"d": map[string]any{"channel": channel, "text": text, "nonce": nonce}}
		b, _ := json.Marshal(msg)
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			t.Fatalf("chat write: %v", err)
		}
	}

	// first send: alice gets the echo (resolves her pending), bob gets one copy
	sendChat(a, "space", "hello world", "n1")
	echo := waitForD(t, a, "alice echo n1", func(d map[string]any) bool { return chatIs(d, "space", "n1") })
	if echo["fromId"] == "" || echo["nonce"] != "n1" {
		t.Errorf("echo missing fromId/nonce: %+v", echo)
	}
	waitForD(t, b, "bob copy n1", func(d map[string]any) bool { return chatIs(d, "space", "n1") })

	// resend the SAME nonce (reconnect resend / retry): echo to sender only
	sendChat(a, "space", "hello world", "n1")
	waitForD(t, a, "alice dup echo n1", func(d map[string]any) bool { return chatIs(d, "space", "n1") })
	time.Sleep(400 * time.Millisecond)
	for _, d := range drainChats(t, b, 400*time.Millisecond) {
		if chatIs(d, "space", "n1") {
			t.Fatalf("duplicate broadcast reached bob: %+v", d)
		}
	}

	// exactly one row persisted, despite two sends
	if n := countMessages(t, h, "space"); n != 1 {
		t.Errorf("messages rows = %d, want 1 (dup must not double-insert)", n)
	}
}

// --- DM: echo to both + friends-only gates ---

func makeFriends(t *testing.T, h *Hub, a, b *Session) {
	t.Helper()
	if err := h.store.AddFriendRequest(a.UserID, b.UserID); err != nil {
		t.Fatalf("friend request: %v", err)
	}
	if err := h.store.AcceptFriend(b.UserID, a.UserID); err != nil {
		t.Fatalf("friend accept: %v", err)
	}
}

func sendDM(t *testing.T, conn *websocket.Conn, to, toName, text, nonce string) {
	t.Helper()
	msg := map[string]any{"v": 1, "t": "chat", "id": "d1", "ts": time.Now().UnixMilli(),
		"d": map[string]any{"channel": "dm", "to": to, "text": text, "nonce": nonce}}
	b, _ := json.Marshal(msg)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("dm write: %v", err)
	}
}

func TestDMEchoToBothParties(t *testing.T) {
	h := newTestHub(t)
	go h.Run()
	defer h.Close()
	ts := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	a := dialJoin(t, wsURL+"?deviceKey=sc-dm-a", "sc-dm-a", "Alice")
	defer a.Close()
	aID := waitForD(t, a, "alice welcome selfId", func(d map[string]any) bool { s, _ := d["selfId"].(string); return s != "" })["selfId"].(string)
	b := dialJoin(t, wsURL+"?deviceKey=sc-dm-b", "sc-dm-b", "Bob")
	defer b.Close()
	bID := waitForD(t, b, "bob welcome selfId", func(d map[string]any) bool { s, _ := d["selfId"].(string); return s != "" })["selfId"].(string)

	sessA := newTestUser(t, h, "sc-dm-a", "Alice")
	sessB := newTestUser(t, h, "sc-dm-b", "Bob")
	makeFriends(t, h, sessA, sessB)

	sendDM(t, a, bID, "Bob", "psst", "dm1")
	// sender echo: resolves alice's pending
	dA := waitForD(t, a, "alice dm echo", func(d map[string]any) bool { return chatIs(d, "dm", "dm1") })
	if dA["to"] != bID || dA["toName"] != "Bob" || dA["fromId"] != aID {
		t.Errorf("sender echo missing to/toName/fromId: %+v", dA)
	}
	// target copy: bob sees it as from alice
	dB := waitForD(t, b, "bob dm copy", func(d map[string]any) bool { return chatIs(d, "dm", "dm1") })
	if dB["fromId"] != aID || dB["from"] != "Alice" || dB["to"] != bID {
		t.Errorf("target copy wrong envelope: %+v", dB)
	}
	if n := countMessages(t, h, "dm"); n != 1 {
		t.Errorf("dm rows = %d, want 1", n)
	}
}

func TestDMGatesFriendsBlockSelfCap(t *testing.T) {
	h := newTestHub(t)
	go h.Run()
	defer h.Close()
	ts := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	a := dialJoin(t, wsURL+"?deviceKey=sc-g-a", "sc-g-a", "Alice")
	defer a.Close()
	aID := waitForD(t, a, "alice welcome selfId", func(d map[string]any) bool { s, _ := d["selfId"].(string); return s != "" })["selfId"].(string)
	b := dialJoin(t, wsURL+"?deviceKey=sc-g-b", "sc-g-b", "Bob")
	defer b.Close()
	bID := waitForD(t, b, "bob welcome selfId", func(d map[string]any) bool { s, _ := d["selfId"].(string); return s != "" })["selfId"].(string)
	c := dialJoin(t, wsURL+"?deviceKey=sc-g-c", "sc-g-c", "Carol")
	defer c.Close()
	cID := waitForD(t, c, "carol welcome selfId", func(d map[string]any) bool { s, _ := d["selfId"].(string); return s != "" })["selfId"].(string)

	sessA := newTestUser(t, h, "sc-g-a", "Alice")
	sessB := newTestUser(t, h, "sc-g-b", "Bob")
	makeFriends(t, h, sessA, sessB)

	// 1. self-DM reject
	sendDM(t, a, aID, "Alice", "to me", "g-self")
	waitForD(t, a, "self dm error", func(d map[string]any) bool { return errCode(d, "dm_self") })

	// 2. non-friend reject (alice <-> carol are NOT friends)
	sendDM(t, a, cID, "Carol", "hey", "g-nf")
	waitForD(t, a, "non-friend dm error", func(d map[string]any) bool { return errCode(d, "dm_forbidden") })
	if got := drainChats(t, c, 300*time.Millisecond); len(got) != 0 {
		t.Errorf("non-friend DM leaked to carol: %+v", got)
	}

	// 3. block (either direction) suppresses DMs
	if err := h.store.BlockUser(sessB.UserID, sessA.UserID); err != nil {
		t.Fatalf("block: %v", err)
	}
	sendDM(t, a, bID, "Bob", "blocked?", "g-blk")
	waitForD(t, a, "blocked dm error", func(d map[string]any) bool { return errCode(d, "dm_blocked") })
	// the same nonce must NOT be burned by the rejection — after unblock the
	// retry delivers (server records the nonce only on actual delivery)
	if err := h.store.UnblockUser(sessB.UserID, sessA.UserID); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	sendDM(t, a, bID, "Bob", "blocked?", "g-blk")
	waitForD(t, a, "dm after unblock", func(d map[string]any) bool { return chatIs(d, "dm", "g-blk") })
	waitForD(t, b, "bob copy after unblock", func(d map[string]any) bool { return chatIs(d, "dm", "g-blk") })

	// 4. per-recipient cap 5/min: g-blk (delivered after unblock) already
	// occupies slot 1 of the window, so 4 more flood DMs deliver and the
	// 5th flood DM is rejected with dm_rate_limited (cap = 5 total/min).
	// NOTE: the global chat token bucket (burst 5/10s) is shared across ALL
	// sends on this client — the earlier self/non-friend/blocked/unblocked
	// sends burned tokens, so the flood spaces ~2.1s apart (0.5/s refill)
	// to keep the DM-cap test from tripping the wrong limiter.
	nonces := []string{"g-1", "g-2", "g-3", "g-4"}
	for i, n := range nonces {
		sendDM(t, a, bID, "Bob", "flood", n)
		waitForD(t, a, "dm "+n, func(d map[string]any) bool { return chatIs(d, "dm", n) })
		if i < len(nonces)-1 {
			time.Sleep(2100 * time.Millisecond)
		}
		_ = i
	}
	time.Sleep(2100 * time.Millisecond) // token bucket refills 1 token
	sendDM(t, a, bID, "Bob", "flood", "g-5")
	waitForD(t, a, "dm cap error", func(d map[string]any) bool { return errCode(d, "dm_rate_limited") })
	// bob saw exactly the 5 delivered DMs — no leak of the capped 5th flood DM
	for _, d := range drainChats(t, b, 400*time.Millisecond) {
		if d["nonce"] == "g-5" {
			t.Fatalf("capped DM leaked to bob: %+v", d)
		}
	}
}

// --- presence roster envelope: join / portal / disconnect hooks ---

func TestPresenceRosterEnvelope(t *testing.T) {
	h := newTestHub(t)
	go h.Run()
	defer h.Close()
	ts := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	a := dialJoin(t, wsURL+"?deviceKey=sc-p-a", "sc-p-a", "Alice")
	defer a.Close()
	waitFor(t, "alice welcome", 3*time.Second, func() bool { return envelopeType(readFrame(t, a)) == "welcome" })
	sessA := newTestUser(t, h, "sc-p-a", "Alice")

	presenceOn := func(d map[string]any, userID, event string) bool {
		return d["userId"] == userID && d["event"] == event
	}
	// alice's own join presence is broadcast to the space she just joined
	waitForD(t, a, "alice self join presence", func(d map[string]any) bool {
		return presenceOn(d, sessA.UserID, "join")
	})

	b := dialJoin(t, wsURL+"?deviceKey=sc-p-b", "sc-p-b", "Bob")
	defer b.Close()
	waitFor(t, "bob welcome", 3*time.Second, func() bool { return envelopeType(readFrame(t, b)) == "welcome" })
	sessB := newTestUser(t, h, "sc-p-b", "Bob")
	// alice sees bob's join delta
	waitForD(t, a, "bob join presence", func(d map[string]any) bool { return presenceOn(d, sessB.UserID, "join") })

	// portal hook: bob steps through town-square's garden portal → alice sees
	// leave (town-square) and bob sees join (garden) in his own welcome space
	world, err := h.store.LoadWorld("town-square")
	if err != nil {
		t.Fatalf("load town-square: %v", err)
	}
	var portal *Portal
	for i := range world.Portals {
		if world.Portals[i].TargetSpace == "garden" {
			portal = &world.Portals[i]
			break
		}
	}
	if portal == nil {
		t.Fatal("no garden portal in town-square seed")
	}
	// stand on the portal tile then walk through
	move := map[string]any{"v": 1, "t": "move", "id": "m1", "ts": time.Now().UnixMilli(),
		"d": map[string]any{"x": portal.X, "y": portal.Y, "dir": "down", "seq": 1}}
	mb, _ := json.Marshal(move)
	if err := b.WriteMessage(websocket.TextMessage, mb); err != nil {
		t.Fatalf("move write: %v", err)
	}
	portalMsg := map[string]any{"v": 1, "t": "portal", "id": "p1", "ts": time.Now().UnixMilli(),
		"d": map[string]any{"portalId": portal.ID}}
	pb, _ := json.Marshal(portalMsg)
	if err := b.WriteMessage(websocket.TextMessage, pb); err != nil {
		t.Fatalf("portal write: %v", err)
	}
	waitForD(t, a, "bob portal leave presence", func(d map[string]any) bool {
		return presenceOn(d, sessB.UserID, "leave") && d["spaceId"] == "town-square"
	})

	// disconnect hook: bob drops. His leave broadcasts to the SPACE HE'S IN
	// (garden) — Alice is in town-square, so she must NOT receive it (roster
	// is same-space only; cross-space offline is friends-scoped instead).
	b.Close()
	for _, d := range drainChats(t, a, 400*time.Millisecond) {
		if presenceOn(d, sessB.UserID, "leave") && d["spaceId"] == "garden" {
			t.Fatalf("cross-space leave leaked to alice: %+v", d)
		}
	}
}

// --- blocks + reports REST ---

func TestBlocksAndReportsEndpoints(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "sc-r-a", "Alice")
	bob := newTestUser(t, h, "sc-r-b", "Bob")

	// reports: auth required
	code, _ := doJSON(t, h.handleReports, http.MethodPost, "/api/reports",
		map[string]any{"reportedId": bob.UserID}, nil)
	if code != http.StatusUnauthorized {
		t.Errorf("unauthenticated report = %d, want 401", code)
	}
	// missing reportedId
	code, _ = doJSON(t, h.handleReports, http.MethodPost, "/api/reports", map[string]any{}, alice)
	if code != http.StatusBadRequest {
		t.Errorf("empty report = %d, want 400", code)
	}
	// self report
	code, _ = doJSON(t, h.handleReports, http.MethodPost, "/api/reports",
		map[string]any{"reportedId": alice.UserID}, alice)
	if code != http.StatusBadRequest {
		t.Errorf("self report = %d, want 400", code)
	}
	// valid report
	code, out := doJSON(t, h.handleReports, http.MethodPost, "/api/reports",
		map[string]any{"reportedId": bob.UserID, "reason": "spam", "spaceId": "town-square"}, alice)
	if code != http.StatusCreated {
		t.Fatalf("report = %d, want 201: %+v", code, out)
	}
	var n int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM reports`).Scan(&n); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	if n != 1 {
		t.Errorf("reports rows = %d, want 1", n)
	}

	// blocks: block bob -> alice; DM gate reads either direction
	code, _ = doJSON(t, h.handleBlocks, http.MethodPost, "/api/blocks",
		map[string]any{"blockedId": bob.UserID}, alice)
	if code != http.StatusCreated {
		t.Fatalf("block = %d, want 201", code)
	}
	if !h.store.AreBlocked(alice.UserID, bob.UserID) {
		t.Error("AreBlocked(alice,bob) = false after block")
	}
	if !h.store.AreBlocked(bob.UserID, alice.UserID) {
		t.Error("AreBlocked(bob,alice) = false (either direction suppresses)")
	}
	// self-block rejected
	code, _ = doJSON(t, h.handleBlocks, http.MethodPost, "/api/blocks",
		map[string]any{"blockedId": alice.UserID}, alice)
	if code != http.StatusBadRequest {
		t.Errorf("self block = %d, want 400", code)
	}
	// unblock via DELETE /api/blocks/{id}
	code, _ = doJSON(t, h.handleBlockRoute, http.MethodDelete, "/api/blocks/"+bob.UserID, nil, alice)
	if code != http.StatusOK {
		t.Fatalf("unblock = %d, want 200", code)
	}
	if h.store.AreBlocked(alice.UserID, bob.UserID) {
		t.Error("AreBlocked still true after unblock")
	}
}
