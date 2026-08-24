package main

// T2 editor v2 tests (docs/EDITOR.md):
//  1. freeform stroke round-trip: a batch stroke op persists; a compensating
//     per-cell inverse op (multi-prior restore) returns each cell to its own
//     prior tile — the freeform-undo contract.
//  2. animation: the animated-tile table is deterministic and rides the world
//     doc ("anims"); animated palette entries persist like any tile.
//  3. custom asset upload: multipart upload → stored bytes served back →
//     placement op broadcast to a second client → world doc carries the
//     placement → removal op cleans it up.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hearth/hmf"
)

// tinyPNG is a valid 1x1 transparent PNG (stdlib-decodable).
const tinyPNG = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0P\x0f\x00\x04\x85\x01\x80\x84\xa9\x8c!\x00\x00\x00\x00IEND\xaeB`\x82"

// uploadAsset posts a multipart image to /api/worlds/{id}/assets and returns
// the parsed asset record.
func uploadAsset(t *testing.T, h *Hub, worldID string, sess *Session, name string, data []byte) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "asset.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("name", name); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/worlds/"+worldID+"/assets", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if sess != nil {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: sess.ID})
	}
	rr := httptest.NewRecorder()
	h.handleWorldAssets(rr, req, worldID)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload: status %d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("upload decode: %v", err)
	}
	asset, _ := out["asset"].(map[string]any)
	if asset == nil {
		t.Fatalf("upload: no asset in %v", out)
	}
	return asset
}

// waitEditAck waits for an edit ack matching the predicate.
func waitEditAck(t *testing.T, c *testWSClient, pred func(d map[string]any) bool, what string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := c.next(time.Until(deadline))
		if m == nil {
			break
		}
		if m["t"] != "edit" {
			continue
		}
		d, _ := m["d"].(map[string]any)
		if d == nil {
			continue
		}
		if pred(d) {
			return d
		}
	}
	t.Fatalf("timeout waiting for edit ack: %s", what)
	return nil
}

// TestFreeformStrokeRoundTripAndUndo: a multi-cell stroke persists, and the
// compensating per-cell inverse op restores each cell to its OWN prior tile.
func TestFreeformStrokeRoundTripAndUndo(t *testing.T) {
	h, wsURL := testWSServer(t)
	key := "t2-editor-" + t.Name()
	sess := newTestUser(t, h, key, "T2Owner")

	code, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "Stroke World"}, sess)
	if code != http.StatusCreated {
		t.Fatalf("create world: %d %v", code, out)
	}
	worldID, _ := out["id"].(string)

	c := dialTestWS(t, wsURL, key, "T2Owner", worldID)

	// build two different prior tiles the stroke will overwrite
	c.send("edit", map[string]any{"op": "paint", "x": 5, "y": 5, "tileId": 1})
	waitEditAck(t, c, func(d map[string]any) bool { return d["x"] == float64(5) && d["y"] == float64(5) }, "paint wall 5,5")
	c.send("edit", map[string]any{"op": "paint", "x": 6, "y": 5, "tileId": 3})
	waitEditAck(t, c, func(d map[string]any) bool { return d["x"] == float64(6) && d["y"] == float64(5) }, "paint grass 6,5")

	// freeform stroke: one batch op over both cells
	c.send("edit", map[string]any{
		"op": "paint", "tileId": 2,
		"cells": []map[string]any{{"x": 5, "y": 5}, {"x": 6, "y": 5}, {"x": 7, "y": 5}},
	})
	ack := waitEditAck(t, c, func(d map[string]any) bool {
		_, ok := d["cells"].([]any)
		return ok
	}, "stroke batch ack")
	cells, _ := ack["cells"].([]any)
	if len(cells) != 3 {
		t.Fatalf("stroke ack cells = %d, want 3 (%v)", len(cells), ack)
	}
	// prior tiles captured for the inverse op
	prior := map[int]int{}
	for _, raw := range cells {
		cell := raw.(map[string]any)
		prior[int(cell["x"].(float64))] = int(cell["priorTileId"].(float64))
	}
	if prior[5] != 1 || prior[6] != 3 || prior[7] != 0 {
		t.Fatalf("prior tiles = %v, want 5->1 6->3 7->0", prior)
	}

	// reload the world doc: stroke persisted
	_, doc := doJSON(t, h.handleSpaceGet, http.MethodGet, "/api/spaces/"+worldID, nil, nil)
	tiles, _ := doc["tiles"].([]any)
	got := map[string]int{}
	for _, raw := range tiles {
		tl := raw.(map[string]any)
		got[fmt.Sprintf("%v,%v", tl["x"], tl["y"])] = int(tl["tileId"].(float64))
	}
	if got["5,5"] != 2 || got["6,5"] != 2 || got["7,5"] != 2 {
		t.Fatalf("world tiles after stroke = %v, want water at 5,5 6,5 7,5", got)
	}

	// undo: single batch inverse op with PER-CELL prior tiles (freeform undo)
	c.send("edit", map[string]any{
		"op": "paint",
		"cells": []map[string]any{
			{"x": 5, "y": 5, "tileId": prior[5]},
			{"x": 6, "y": 5, "tileId": prior[6]},
			{"x": 7, "y": 5, "tileId": prior[7]},
		},
	})
	waitEditAck(t, c, func(d map[string]any) bool {
		cs, ok := d["cells"].([]any)
		if !ok || len(cs) != 3 {
			return false
		}
		last := cs[2].(map[string]any)
		return last["tileId"] == float64(0)
	}, "undo batch ack")

	_, doc = doJSON(t, h.handleSpaceGet, http.MethodGet, "/api/spaces/"+worldID, nil, nil)
	tiles, _ = doc["tiles"].([]any)
	got = map[string]int{}
	for _, raw := range tiles {
		tl := raw.(map[string]any)
		got[fmt.Sprintf("%v,%v", tl["x"], tl["y"])] = int(tl["tileId"].(float64))
	}
	if got["5,5"] != 1 || got["6,5"] != 3 {
		t.Fatalf("undo restore failed: %v (want 5,5->wall 6,5->grass)", got)
	}
	if _, ok := got["7,5"]; ok {
		t.Fatalf("undo should have erased 7,5 (painted over floor): %v", got)
	}
}

// TestAnimsTable: the animated-tile table is deterministic, sorted, and rides
// the world doc; animated palette entries persist like any tile.
func TestAnimsTable(t *testing.T) {
	anims := hmf.Anims()
	if len(anims) != 4 {
		t.Fatalf("anims = %d entries, want 4 (water/lava/torch/glow)", len(anims))
	}
	want := []int{2, 8, 20, 21}
	for i, a := range anims {
		if a.TileID != want[i] {
			t.Fatalf("anims[%d].tileId = %d, want %d (sorted)", i, a.TileID, want[i])
		}
		if a.Frames <= 0 || a.FPS <= 0 {
			t.Fatalf("anim %d: frames=%d fps=%v must be positive", a.TileID, a.Frames, a.FPS)
		}
	}
	if !hmf.IsAnimated(20) || !hmf.IsAnimated(2) || hmf.IsAnimated(1) {
		t.Fatal("IsAnimated wrong for torch/water/wall")
	}

	h, wsURL := testWSServer(t)
	key := "t2-anim-" + t.Name()
	sess := newTestUser(t, h, key, "T2Anim")
	code, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "Anim World"}, sess)
	if code != http.StatusCreated {
		t.Fatalf("create world: %d", code)
	}
	worldID, _ := out["id"].(string)
	c := dialTestWS(t, wsURL, key, "T2Anim", worldID)

	// paint an animated torch tile — must persist like any other tile
	c.send("edit", map[string]any{"op": "paint", "x": 4, "y": 4, "tileId": 20})
	waitEditAck(t, c, func(d map[string]any) bool { return d["tileId"] == float64(20) }, "torch paint")

	_, doc := doJSON(t, h.handleSpaceGet, http.MethodGet, "/api/spaces/"+worldID, nil, nil)
	animsDoc, _ := doc["anims"].([]any)
	if len(animsDoc) != 4 {
		t.Fatalf("world doc anims = %d, want 4", len(animsDoc))
	}
	// torch persisted in the tile map
	found := false
	for _, raw := range doc["tiles"].([]any) {
		tl := raw.(map[string]any)
		if tl["x"] == float64(4) && tl["y"] == float64(4) && tl["tileId"] == float64(20) {
			found = true
		}
	}
	if !found {
		t.Fatal("torch tile not persisted in world doc")
	}
}

// TestAssetUploadPlaceBroadcastRemove: upload → stored bytes → placement op
// broadcast to a second client → world doc carries it → remove cleans up.
func TestAssetUploadPlaceBroadcastRemove(t *testing.T) {
	h, wsURL := testWSServer(t)
	key := "t2-asset-" + t.Name()
	sess := newTestUser(t, h, key, "T2Asset")
	code, out := doJSON(t, h.createWorld, http.MethodPost, "/api/worlds",
		map[string]any{"name": "Asset World"}, sess)
	if code != http.StatusCreated {
		t.Fatalf("create world: %d", code)
	}
	worldID, _ := out["id"].(string)

	// upload (owner session)
	asset := uploadAsset(t, h, worldID, sess, "logo", []byte(tinyPNG))
	assetID, _ := asset["id"].(string)
	url, _ := asset["url"].(string)
	if assetID == "" || url == "" {
		t.Fatalf("asset record missing id/url: %v", asset)
	}

	// stored bytes served back with the right mime
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	h.handleAssetGet(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("asset get: %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("asset content-type = %q, want image/png", ct)
	}
	if !bytes.Equal(rr.Body.Bytes(), []byte(tinyPNG)) {
		t.Fatal("asset bytes mismatch")
	}

	// owner + observer join the world
	owner := dialTestWS(t, wsURL, key, "T2Asset", worldID)
	observer := dialTestWS(t, wsURL, "t2-asset-obs-"+t.Name(), "T2Observer", worldID)

	// place the asset → observer must receive the broadcast
	observerAckP := waitEditAckAsync(observer, func(d map[string]any) bool {
		a, ok := d["asset"].(map[string]any)
		return ok && a["assetId"] == assetID
	})
	owner.send("edit", map[string]any{"op": "asset", "asset": map[string]any{"assetId": assetID, "x": 5, "y": 6}})
	oad := waitEditAck(t, owner, func(d map[string]any) bool {
		a, ok := d["asset"].(map[string]any)
		return ok && a["assetId"] == assetID
	}, "owner asset ack")
	oaPl, ok := oad["asset"].(map[string]any)
	if !ok {
		t.Fatalf("owner asset ack missing asset payload: %v", oad)
	}
	if oaPl["remove"] == true {
		t.Fatal("place ack should not be remove")
	}
	obsAck := <-observerAckP
	if obsAck == nil {
		t.Fatal("timeout waiting for observer asset broadcast")
	}
	oa, ok := obsAck["asset"].(map[string]any)
	if !ok {
		t.Fatalf("observer asset ack missing asset payload: %v", obsAck)
	}
	if oa["url"] == "" || oa["remove"] == true {
		t.Fatalf("observer asset ack missing url/remove flag: %v", oa)
	}

	// world doc carries the placement (with denormalized url)
	_, doc := doJSON(t, h.handleSpaceGet, http.MethodGet, "/api/spaces/"+worldID, nil, nil)
	assetsDoc, _ := doc["assets"].([]any)
	if len(assetsDoc) != 1 {
		t.Fatalf("world doc assets = %d, want 1", len(assetsDoc))
	}
	pl := assetsDoc[0].(map[string]any)
	if pl["assetId"] != assetID || pl["x"] != float64(5) || pl["y"] != float64(6) || pl["url"] != url {
		t.Fatalf("placement = %v", pl)
	}

	// remove op cleans up
	owner.send("edit", map[string]any{"op": "asset", "asset": map[string]any{"assetId": assetID, "x": 5, "y": 6, "remove": true}})
	waitEditAck(t, owner, func(d map[string]any) bool {
		a, ok := d["asset"].(map[string]any)
		return ok && a["remove"] == true
	}, "asset remove ack")
	_, doc = doJSON(t, h.handleSpaceGet, http.MethodGet, "/api/spaces/"+worldID, nil, nil)
	if n := len(doc["assets"].([]any)); n != 0 {
		t.Fatalf("world doc assets after remove = %d, want 0", n)
	}
}

// waitEditAckAsync is the non-fatal variant (predicate checked on a goroutine
// so the caller can trigger the op AFTER attaching the waiter).
func waitEditAckAsync(c *testWSClient, pred func(d map[string]any) bool) chan map[string]any {
	ch := make(chan map[string]any, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			m := c.next(time.Until(deadline))
			if m == nil {
				ch <- nil
				return
			}
			if m["t"] != "edit" {
				continue
			}
			d, _ := m["d"].(map[string]any)
			if d != nil && pred(d) {
				ch <- d
				return
			}
		}
		ch <- nil
	}()
	return ch
}
