package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// T2 directory gravity tests: live-event wiring (chat + human edit emit),
// member gravity (determinism + formula), and the server-rendered HMF v1
// preview thumbnails.

// TestChatAndEditEmitActivityRows drives the REAL WS path: a human join +
// chat + edit must each append an activity_events row (kind chat/edit) so
// gravity Love reflects live engagement, not just bot ops.
func TestChatAndEditEmitActivityRows(t *testing.T) {
	h := newTestHub(t)
	go h.Run()
	defer h.Close()

	ts := httptest.NewServer(http.HandlerFunc(h.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	conn := dialJoin(t, wsURL+"?deviceKey=t2-grav-chat", "t2-grav-chat", "Chatty")
	defer conn.Close()
	waitFor(t, "welcome", 3*time.Second, func() bool {
		return envelopeType(readFrame(t, conn)) == "welcome"
	})

	// chat on the space channel (world-local → Love contribution)
	send := func(typ string, d map[string]any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"v": 1, "t": typ, "id": "m1", "ts": time.Now().UnixMilli(), "d": d})
		if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
			t.Fatalf("send %s: %v", typ, err)
		}
	}
	send("chat", map[string]any{"channel": "space", "text": "gravity hello", "nonce": "n1"})
	// edit a tile in town-square (showcase → any authenticated session)
	send("edit", map[string]any{"op": "paint", "x": 2, "y": 2, "tileId": 1})

	// both ops must land in activity_events (chat/edit kinds, member role)
	waitFor(t, "activity rows", 3*time.Second, func() bool {
		ev, err := h.store.RecentActivity("town-square", 20)
		if err != nil {
			return false
		}
		var chatRow, editRow bool
		for _, e := range ev {
			if e.Kind == "chat" && e.Action == "space" {
				chatRow = true
			}
			if e.Kind == "edit" && e.Action == "paint" {
				editRow = true
			}
		}
		return chatRow && editRow
	})

	ev, err := h.store.RecentActivity("town-square", 20)
	if err != nil {
		t.Fatal(err)
	}
	var chatRow, editRow bool
	for _, e := range ev {
		if e.Kind == "chat" && e.Action == "space" && e.Role == "member" && e.Actor != "" {
			chatRow = true
		}
		if e.Kind == "edit" && e.Action == "paint" && e.Role == "member" && e.Actor != "" {
			editRow = true
		}
	}
	if !chatRow {
		t.Errorf("no member chat activity row: %+v", ev)
	}
	if !editRow {
		t.Errorf("no member edit activity row: %+v", ev)
	}
}

// TestMemberGravityDeterminismAndFormula: identical activity → identical
// member scores; recompute twice → same persisted rows; and the formula
// factors move the way the docs say (chat+edits → love, distinct worlds →
// reach, recency → momentum).
func TestMemberGravityDeterminismAndFormula(t *testing.T) {
	s := newTestStore(t)
	// two members with IDENTICAL event sets (chat x3 + one join each)
	for _, m := range []string{"m-alpha", "m-beta"} {
		for i := 0; i < 3; i++ {
			if err := s.Emit("w-1", m, "member", "chat", "space", "w-1", "", ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Emit("w-1", m, "member", "presence", "join", "w-1", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecomputeGravity(); err != nil {
		t.Fatal(err)
	}
	ga := s.MemberGravityFor("m-alpha")
	gb := s.MemberGravityFor("m-beta")
	if ga.Love != gb.Love || ga.Reach != gb.Reach || ga.Momentum != gb.Momentum || ga.Gravity != gb.Gravity {
		t.Errorf("identical member inputs diverged: alpha=%+v beta=%+v", ga, gb)
	}
	// determinism: a second recompute must persist near-identical values —
	// exact floats drift by nanoseconds of decay between runs; determinism
	// is (a) identical inputs → identical scores in one snapshot, and
	// (b) stable ranking across snapshots (verified in the S1 test).
	if err := s.RecomputeGravity(); err != nil {
		t.Fatal(err)
	}
	ga2 := s.MemberGravityFor("m-alpha")
	eps := 1e-6
	if math.Abs(ga2.Love-ga.Love) > eps || math.Abs(ga2.Reach-ga.Reach) > eps ||
		math.Abs(ga2.Momentum-ga.Momentum) > eps || math.Abs(ga2.Gravity-ga.Gravity) > eps {
		t.Errorf("member scores drifted across recompute: %+v vs %+v", ga, ga2)
	}
	// formula sanity: 3 chat + 1 join (love, under per-day cap) and the join
	// also sets reach=1 (one distinct world visited) → gravity = product.
	if ga.Love != 4 {
		t.Errorf("love = %f, want 4 (3 chats + 1 join, under per-day cap)", ga.Love)
	}
	if ga.Reach != 1 {
		t.Errorf("reach = %f, want 1 (one distinct world joined)", ga.Reach)
	}
	if math.Abs(ga.Gravity-(1+ga.Love)*(1+ga.Reach)*(1+ga.Momentum)) > eps {
		t.Errorf("gravity = %f, want (1+love)*(1+reach)*(1+momentum)", ga.Gravity)
	}
	// member with no activity: zeros, not an error
	zero := s.MemberGravityFor("nobody-here")
	if zero.Love != 0 || zero.Reach != 0 || zero.Momentum != 0 || zero.Gravity != 0 {
		t.Errorf("empty member = %+v, want zeros", zero)
	}
	// member reach = distinct worlds visited (footprint): m-alpha joins w-2
	if err := s.Emit("w-2", "m-alpha", "member", "presence", "join", "w-2", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RecomputeGravity(); err != nil {
		t.Fatal(err)
	}
	ga3 := s.MemberGravityFor("m-alpha")
	if ga3.Reach != 2 {
		t.Errorf("reach after second world = %f, want 2", ga3.Reach)
	}
	if ga3.Gravity <= ga.Gravity {
		t.Errorf("gravity did not rise with reach: %f -> %f", ga.Gravity, ga3.Gravity)
	}
}

// TestThumbnailEndpointRendersPNG: the directory thumbnail route returns a
// real PNG with the right content type, and rendering is deterministic
// (same world → identical bytes).
func TestThumbnailEndpointRendersPNG(t *testing.T) {
	h := newTestHub(t)
	// town-square is a seeded, published, showcase world with walls — a real
	// map to render.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/worlds/town-square/thumbnail", nil)
	h.handleWorldThumbnail(rr, req, "town-square")
	if rr.Code != http.StatusOK {
		t.Fatalf("thumbnail status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %s, want image/png", ct)
	}
	body := rr.Body.Bytes()
	if len(body) < 8 || body[0] != 0x89 || body[1] != 'P' || body[2] != 'N' || body[3] != 'G' {
		t.Fatalf("body is not a PNG: %d bytes, magic=%v", len(body), body[:8])
	}
	// determinism: same world → byte-identical PNG
	rr2 := httptest.NewRecorder()
	h.handleWorldThumbnail(rr2, httptest.NewRequest(http.MethodGet, "/api/worlds/town-square/thumbnail", nil), "town-square")
	if rr2.Body.String() != rr.Body.String() {
		t.Errorf("thumbnail not deterministic across renders")
	}
	// unknown world → 404
	rr3 := httptest.NewRecorder()
	h.handleWorldThumbnail(rr3, httptest.NewRequest(http.MethodGet, "/api/worlds/does-not-exist/thumbnail", nil), "does-not-exist")
	if rr3.Code != http.StatusNotFound {
		t.Errorf("unknown world thumbnail = %d, want 404", rr3.Code)
	}
}

// TestDirectoryEntryCarriesThumbnailURL: GET /api/worlds returns a thumbnail
// URL string per published world (client <img> renders it lazily).
func TestDirectoryEntryCarriesThumbnailURL(t *testing.T) {
	h := newTestHub(t)
	code, out := doJSON(t, h.listWorlds, http.MethodGet, "/api/worlds", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	worlds, _ := out["worlds"].([]any)
	if len(worlds) == 0 {
		t.Fatal("no worlds in directory")
	}
	first := worlds[0].(map[string]any)
	thumb, _ := first["thumbnail"].(string)
	if thumb == "" || !strings.HasSuffix(thumb, "/thumbnail") {
		t.Errorf("thumbnail = %v, want a /thumbnail URL", first["thumbnail"])
	}
}
