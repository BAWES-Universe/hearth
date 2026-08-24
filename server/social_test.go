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

// T2 social layer tests: friend add/accept/remove/list over REST + the
// presence event fanout over a real WebSocket (join/offline) + the social
// activity wiring.

func TestFriendRequestAcceptRemoveList(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-alice-key", "Alice")
	bob := newTestUser(t, h, "t2-bob-key", "Bob")

	// both lists start empty
	for _, sess := range []*Session{alice, bob} {
		code, out := doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, sess)
		if code != http.StatusOK {
			t.Fatalf("list = %d", code)
		}
		if n := len(out["friends"].([]any)); n != 0 {
			t.Fatalf("initial friends = %d, want 0", n)
		}
	}

	// alice requests bob
	code, out := doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{"friendId": bob.UserID}, alice)
	if code != http.StatusCreated {
		t.Fatalf("request = %d, want 201: %+v", code, out)
	}

	// alice sees pending (outgoing); bob sees requested (incoming)
	_, outA := doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, alice)
	rowA := outA["friends"].([]any)[0].(map[string]any)
	if rowA["status"] != friendPending || rowA["friendId"] != bob.UserID {
		t.Errorf("alice row = %+v, want status=pending friend=%s", rowA, bob.UserID)
	}
	_, outB := doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, bob)
	rowB := outB["friends"].([]any)[0].(map[string]any)
	if rowB["status"] != friendRequested || rowB["friendId"] != alice.UserID {
		t.Errorf("bob row = %+v, want status=requested friend=%s", rowB, alice.UserID)
	}
	if rowB["name"] != "Alice" {
		t.Errorf("friend name = %v, want Alice", rowB["name"])
	}

	// bob accepts
	code, out = doJSON(t, func(w http.ResponseWriter, r *http.Request) {
		h.respondFriend(w, r, alice.UserID, "accept")
	}, http.MethodPost, "/api/friends/"+alice.UserID+"/accept", nil, bob)
	if code != http.StatusOK {
		t.Fatalf("accept = %d, want 200: %+v", code, out)
	}

	// both see accepted
	_, outA = doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, alice)
	if outA["friends"].([]any)[0].(map[string]any)["status"] != friendAccepted {
		t.Errorf("alice after accept = %+v", outA)
	}
	_, outB = doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, bob)
	if outB["friends"].([]any)[0].(map[string]any)["status"] != friendAccepted {
		t.Errorf("bob after accept = %+v", outB)
	}

	// accepting twice is rejected (no incoming request anymore)
	code, _ = doJSON(t, func(w http.ResponseWriter, r *http.Request) {
		h.respondFriend(w, r, alice.UserID, "accept")
	}, http.MethodPost, "/api/friends/"+alice.UserID+"/accept", nil, bob)
	if code != http.StatusConflict {
		t.Errorf("double accept = %d, want 409", code)
	}

	// store-level invariants
	if !h.store.AreFriends(alice.UserID, bob.UserID) {
		t.Error("AreFriends(alice,bob) = false after accept")
	}
	ids, _ := h.store.FriendIDs(alice.UserID)
	if len(ids) != 1 || ids[0] != bob.UserID {
		t.Errorf("FriendIDs(alice) = %v, want [bob]", ids)
	}

	// bob removes alice — both sides empty
	code, _ = doJSON(t, func(w http.ResponseWriter, r *http.Request) {
		h.removeFriend(w, r, alice.UserID)
	}, http.MethodDelete, "/api/friends/"+alice.UserID, nil, bob)
	if code != http.StatusOK {
		t.Fatalf("remove = %d", code)
	}
	_, outA = doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, alice)
	_, outB = doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, bob)
	if len(outA["friends"].([]any)) != 0 || len(outB["friends"].([]any)) != 0 {
		t.Errorf("lists not empty after remove: A=%+v B=%+v", outA, outB)
	}
}

func TestFriendRequestValidation(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-alice-key-2", "Alice")

	// unauthenticated
	code, _ := doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, nil)
	if code != http.StatusUnauthorized {
		t.Errorf("no session list = %d, want 401", code)
	}
	// self-friend
	code, out := doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{"friendId": alice.UserID}, alice)
	if code != http.StatusBadRequest {
		t.Errorf("self friend = %d, want 400: %+v", code, out)
	}
	// unknown user
	code, out = doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{"friendId": "nobody-here"}, alice)
	if code != http.StatusNotFound {
		t.Errorf("unknown friend = %d, want 404: %+v", code, out)
	}
	// empty body
	code, out = doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{}, alice)
	if code != http.StatusBadRequest {
		t.Errorf("empty request = %d, want 400: %+v", code, out)
	}
	// friend by raw deviceKey (hashed server-side like UpsertUser)
	bob := newTestUser(t, h, "t2-bob-key-2", "Bob")
	code, out = doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{"deviceKey": "t2-bob-key-2"}, alice)
	if code != http.StatusCreated {
		t.Fatalf("friend by deviceKey = %d, want 201: %+v", code, out)
	}
	if out["friendId"] != bob.UserID {
		t.Errorf("deviceKey lookup resolved to %v, want %s", out["friendId"], bob.UserID)
	}
	// duplicate request
	code, _ = doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{"friendId": bob.UserID}, alice)
	if code != http.StatusConflict {
		t.Errorf("duplicate request = %d, want 409", code)
	}
}

// TestFriendPresenceEventOnJoin drives the REAL join path over a WebSocket:
// alice and bob are friends; bob joins town-square → alice receives a
// {t:'friend_presence'} event; alice's REST list shows bob online; bob
// disconnects → alice receives the offline event; and the accept had already
// appended a 'friend' activity row to alice's space feed (social wiring).
func TestFriendPresenceEventOnJoin(t *testing.T) {
	h := newTestHub(t)
	go h.Run()
	defer h.Close()

	ts := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	// alice connects and joins town-square
	aliceWS := dialJoin(t, wsURL+"?deviceKey=t2-pres-alice", "t2-pres-alice", "Alice")
	defer aliceWS.Close()
	waitFor(t, "alice welcome", 3*time.Second, func() bool {
		return envelopeType(readFrame(t, aliceWS)) == "welcome"
	})

	// bob connects, joins town-square (the act under test)
	bobWS := dialJoin(t, wsURL+"?deviceKey=t2-pres-bob", "t2-pres-bob", "Bob")
	defer bobWS.Close()
	waitFor(t, "bob welcome", 3*time.Second, func() bool {
		return envelopeType(readFrame(t, bobWS)) == "welcome"
	})

	// make them friends through the REST path — bob accepts alice's request.
	// (The REST accept is what wires the {t:'friend'} notify + the social
	// activity row; store-level calls would skip both.)
	sessA := newTestUser(t, h, "t2-pres-alice", "Alice")
	sessB := newTestUser(t, h, "t2-pres-bob", "Bob")
	code, out := doJSON(t, h.handleFriends, http.MethodPost, "/api/friends",
		map[string]any{"friendId": sessB.UserID}, sessA)
	if code != http.StatusCreated {
		t.Fatalf("request = %d: %+v", code, out)
	}
	code, out = doJSON(t, func(w http.ResponseWriter, r *http.Request) {
		h.respondFriend(w, r, sessA.UserID, "accept")
	}, http.MethodPost, "/api/friends/"+sessA.UserID+"/accept", nil, sessB)
	if code != http.StatusOK {
		t.Fatalf("accept = %d: %+v", code, out)
	}

	// now bob (re)joins — alice must see the presence event
	// (join again to trigger the hook after the friendship exists)
	bobWS.Close()
	waitFor(t, "bob offline event on alice", 3*time.Second, func() bool {
		return presenceEventOn(readFrame(t, aliceWS), sessB.UserID, "offline")
	})
	bobWS = dialJoin(t, wsURL+"?deviceKey=t2-pres-bob", "t2-pres-bob", "Bob")
	defer bobWS.Close()
	waitFor(t, "bob welcome 2", 3*time.Second, func() bool {
		return envelopeType(readFrame(t, bobWS)) == "welcome"
	})
	waitFor(t, "bob join presence event on alice", 3*time.Second, func() bool {
		return presenceEventOn(readFrame(t, aliceWS), sessB.UserID, "join")
	})

	// alice's friend list shows bob ONLINE with his space
	_, out = doJSON(t, h.handleFriends, http.MethodGet, "/api/friends", nil, sessA)
	row := out["friends"].([]any)[0].(map[string]any)
	if row["online"] != true {
		t.Errorf("bob online = %v, want true (WS connected)", row["online"])
	}
	if row["space"] != "town-square" {
		t.Errorf("bob space = %v, want town-square", row["space"])
	}

	// social activity wiring: the accept appended a 'friend' row to the
	// accepter's space feed (bob accepted while online in town-square)
	events, err := h.store.RecentActivity("town-square", 20)
	if err != nil {
		t.Fatalf("recent activity: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "friend" && e.Action == "accept" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no friend/accept activity row in town-square feed: %+v", events)
	}

	// bob disconnects → offline event on alice
	bobWS.Close()
	waitFor(t, "bob final offline event on alice", 3*time.Second, func() bool {
		return presenceEventOn(readFrame(t, aliceWS), sessB.UserID, "offline")
	})
}

// --- helpers ---

// dialJoin dials a WS URL, sends the join envelope, returns the conn.
func dialJoin(t *testing.T, url, deviceKey, name string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	join := map[string]any{"v": 1, "t": "join", "id": "j1", "ts": time.Now().UnixMilli(), "d": map[string]any{
		"deviceKey": deviceKey, "name": name, "lang": "en", "space": "town-square", "guest": true,
	}}
	b, _ := json.Marshal(join)
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		t.Fatalf("join write: %v", err)
	}
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return nil // timeout/closed — treat as no frame
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("bad frame: %v", err)
	}
	return m
}

func envelopeType(m map[string]any) string {
	if m == nil {
		return ""
	}
	t, _ := m["t"].(string)
	return t
}

func presenceEventOn(m map[string]any, userID, event string) bool {
	if m == nil || envelopeType(m) != "friend_presence" {
		return false
	}
	d, _ := m["d"].(map[string]any)
	return d != nil && d["userId"] == userID && d["event"] == event
}

// waitFor polls cond until true or the timeout elapses (frames are consumed
// by readFrame inside cond, so callers must not read concurrently).
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
