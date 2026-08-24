package main

import (
	"net/http"
	"testing"

	"hearth/hmf"
)

// World-ownership stream tests: template seeding (no random walls), the edit
// ACL (owner / editor / stranger), the invite flow, functional objects, and
// the /api/me identity surface.

func wallCount(w *World) int {
	n := 0
	for _, tl := range w.Tiles {
		if tl.T == "wall" {
			n++
		}
	}
	return n
}

func TestCreateWorldTemplates(t *testing.T) {
	h := newTestHub(t)
	sess := newTestUser(t, h, "tpl-key", "Templater")

	for _, tpl := range []string{"empty_lot", "cozy_room", "plaza"} {
		code, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
			map[string]any{"name": "T-" + tpl, "template": tpl}, sess)
		if code != http.StatusCreated {
			t.Fatalf("create %s = %d, want 201: %+v", tpl, code, out)
		}
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("create %s: no id", tpl)
		}
		w, err := h.store.LoadWorld(id)
		if err != nil {
			t.Fatalf("load %s: %v", tpl, err)
		}
		// spawn must ALWAYS be a passable open tile
		if !w.Passable(w.Spawn.X, w.Spawn.Y) {
			t.Errorf("%s: spawn (%d,%d) is impassable", tpl, w.Spawn.X, w.Spawn.Y)
		}
		if tpl == "empty_lot" {
			if n := wallCount(w); n != 0 {
				t.Errorf("empty_lot has %d walls, want 0 (no random walls)", n)
			}
		}
	}
}

// testEditClient builds a Client that can run edit ops against the hub.
func testEditClient(h *Hub, sess *Session) *Client {
	return &Client{hub: h, Session: sess, Entity: &Entity{ID: "e-" + sess.ID[:6]}}
}

func TestWorldEditACL(t *testing.T) {
	h := newTestHub(t)
	owner := newTestUser(t, h, "acl-owner", "Owner")
	stranger := newTestUser(t, h, "acl-stranger", "Stranger")
	viewer := newTestUser(t, h, "acl-viewer", "Viewer")

	_, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "ACL World"}, owner)
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatal("no world id")
	}
	w, err := h.store.LoadWorld(id)
	if err != nil {
		t.Fatal(err)
	}

	// owner edits, stranger cannot (403-shaped gate)
	if !h.canEditWorld(owner, w) {
		t.Error("owner cannot edit own world")
	}
	if h.canEditWorld(stranger, w) {
		t.Error("stranger can edit — ACL broken")
	}
	// viewers cannot publish (REST)
	code, _ := doJSON(t, func(wr http.ResponseWriter, r *http.Request) { h.publishWorld(wr, r, id) },
		http.MethodPost, "/api/worlds/"+id+"/publish", nil, viewer)
	if code != http.StatusForbidden {
		t.Errorf("viewer publish = %d, want 403", code)
	}

	// owner mints an invite; the stranger redeems it and becomes an editor
	code, inv := doJSON(t, func(wr http.ResponseWriter, r *http.Request) { h.inviteWorld(wr, r, id) },
		http.MethodPost, "/api/worlds/"+id+"/invite", nil, owner)
	if code != http.StatusOK {
		t.Fatalf("invite = %d: %+v", code, inv)
	}
	token, _ := inv["token"].(string)
	if token == "" {
		t.Fatal("no invite token")
	}
	code, join := doJSON(t, h.joinWorld, http.MethodGet, "/api/worlds/join?invite="+token, nil, stranger)
	if code != http.StatusOK {
		t.Fatalf("join invite = %d: %+v", code, join)
	}
	if join["worldId"] != id {
		t.Errorf("join worldId = %v, want %s", join["worldId"], id)
	}
	if !h.canEditWorld(stranger, w) {
		t.Error("invite did not grant editor role")
	}
	// editor can publish (REST publish is owner-or-editor)
	code, _ = doJSON(t, func(wr http.ResponseWriter, r *http.Request) { h.publishWorld(wr, r, id) },
		http.MethodPost, "/api/worlds/"+id+"/publish", nil, stranger)
	if code != http.StatusOK {
		t.Errorf("editor publish = %d, want 200", code)
	}

	// single-use: a second redemption of the same token fails
	code, _ = doJSON(t, h.joinWorld, http.MethodGet, "/api/worlds/join?invite="+token, nil, viewer)
	if code != http.StatusNotFound {
		t.Errorf("second invite redemption = %d, want 404 (single-use)", code)
	}
}

func TestObjectPlaceOpPersists(t *testing.T) {
	h := newTestHub(t)
	owner := newTestUser(t, h, "obj-key", "Obj")
	_, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "ObjWorld"}, owner)
	id, _ := out["id"].(string)
	sp := h.space(id)
	if sp == nil {
		t.Fatal("world not registered in hub")
	}

	c := testEditClient(h, owner)
	op := &hmf.Op{Op: "object", Object: &hmf.Object{Kind: "sign", X: 4, Y: 5, Name: "Welcome", Text: "hi there"}}
	ack := h.applyEditOp(sp, c, op)
	if ack.Err != "" {
		t.Fatalf("apply object op: %s", ack.Err)
	}
	if len(sp.World.Objects) != 1 {
		t.Fatalf("world has %d objects, want 1", len(sp.World.Objects))
	}
	got := sp.World.Objects[0]
	if got.Kind != "sign" || got.Text != "hi there" {
		t.Errorf("object = %+v, want sign/hi there", got)
	}
	if got.ID == "" {
		t.Error("object id not generated")
	}

	// persisted: a fresh load returns the object
	loaded, err := h.store.LoadWorld(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Objects) != 1 || loaded.Objects[0].Kind != "sign" {
		t.Errorf("persisted objects = %+v, want 1 sign", loaded.Objects)
	}

	// removal op
	rop := &hmf.Op{Op: "object", ObjectID: got.ID}
	if ack := h.applyEditOp(sp, c, rop); ack.Err != "" {
		t.Fatalf("remove object op: %s", ack.Err)
	}
	if len(sp.World.Objects) != 0 {
		t.Errorf("world has %d objects after removal, want 0", len(sp.World.Objects))
	}
}

func TestMeListsMyWorlds(t *testing.T) {
	h := newTestHub(t)
	sess := newTestUser(t, h, "me-key", "Me")
	_, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "Mine"}, sess)
	id, _ := out["id"].(string)

	code, me := doJSON(t, h.handleMe, http.MethodGet, "/api/me", nil, sess)
	if code != http.StatusOK {
		t.Fatalf("me = %d: %+v", code, me)
	}
	worlds, _ := me["worlds"].([]any)
	if len(worlds) != 1 {
		t.Fatalf("me worlds = %d, want 1: %+v", len(worlds), worlds)
	}
	first := worlds[0].(map[string]any)
	if first["id"] != id || first["role"] != "owner" {
		t.Errorf("me world = %+v, want owner of %s", first, id)
	}

	// /api/worlds/mine mirrors it
	code, mine := doJSON(t, h.myWorlds, http.MethodGet, "/api/worlds/mine", nil, sess)
	if code != http.StatusOK {
		t.Fatalf("mine = %d", code)
	}
	if mw, _ := mine["worlds"].([]any); len(mw) != 1 {
		t.Errorf("mine worlds = %d, want 1", len(mw))
	}
}

func TestInviteRejectsStranger(t *testing.T) {
	h := newTestHub(t)
	owner := newTestUser(t, h, "inv-owner", "Owner")
	stranger := newTestUser(t, h, "inv-stranger", "Stranger")
	_, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "Inv"}, owner)
	id, _ := out["id"].(string)

	// a viewer cannot mint invites
	code, _ := doJSON(t, func(wr http.ResponseWriter, r *http.Request) { h.inviteWorld(wr, r, id) },
		http.MethodPost, "/api/worlds/"+id+"/invite", nil, stranger)
	if code != http.StatusForbidden {
		t.Errorf("stranger invite = %d, want 403", code)
	}
}
