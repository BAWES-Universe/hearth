package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// T2 avatar platform tests: custom asset upload/list/image/safe-archive,
// sets + versioning, entitlements (deny + allow at join/update), grant kinds
// (direct/tag/email-domain/time-limited), generative determinism, spec
// round-trip persistence, image sniffing and the audit trail.

// tinyPNG is a 1x1 transparent PNG (valid magic + IHDR dims).
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func uploadAsset(t *testing.T, h *Hub, sess *Session, layer, name string) string {
	t.Helper()
	code, out := doJSON(t, h.handleAvatars, http.MethodPost, "/api/avatars/assets",
		map[string]any{"layer": layer, "name": name, "kind": "image/png", "data": tinyPNG}, sess)
	if code != http.StatusCreated {
		t.Fatalf("upload = %d: %+v", code, out)
	}
	id, _ := out["asset"].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("upload missing asset id: %+v", out)
	}
	return id
}

// rawDo is like doJSON but returns the raw body (for the image endpoint).
func rawDo(t *testing.T, h http.HandlerFunc, method, path string, sess *Session) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if sess != nil {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: sess.ID})
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes()
}

// regSync forces the governance snapshot to rebuild after direct store writes
// (the handlers invalidate on their own; tests write via the store, so the
// snapshot must be invalidated explicitly or a <30s-old snapshot no-ops).
func regSync(h *Hub) {
	h.avatarReg.invalidate()
	h.avatarReg.refresh(h.store)
}

func TestT2UploadListImageSafeArchive(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a-key", "T2Alice")

	id := uploadAsset(t, h, alice, "body", "mybody")
	want := "asset:" + id

	// list shows the active asset (no bytes)
	code, out := doJSON(t, h.handleAvatars, http.MethodGet, "/api/avatars/assets", nil, alice)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	rows := out["assets"].([]any)
	if len(rows) != 1 {
		t.Fatalf("assets = %d, want 1", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["layer"] != "body" || row["status"] != "active" || row["width"].(float64) != 1 {
		t.Fatalf("asset row = %+v", row)
	}

	// image endpoint serves the raw bytes
	code, raw := rawDo(t, h.handleAvatars, http.MethodGet, "/api/avatars/assets/"+id+"/image", alice)
	if code != http.StatusOK || len(raw) == 0 || raw[0] != 0x89 {
		t.Fatalf("image = %d, %d bytes", code, len(raw))
	}

	// safe-archive: worn -> 409
	sp := h.space("town-square")
	if sp == nil {
		t.Fatalf("town-square space missing")
	}
	sp.AddEntity(&Entity{ID: "wearer-1", Name: "wearer", Avatar: Avatar{Spec: &AvatarSpec{V: 1, Body: want}}})
	if !h.avatarOptionWorn(want) {
		t.Fatal("asset should be reported worn")
	}
	code, out = doJSON(t, h.handleAvatars, http.MethodDelete, "/api/avatars/assets/"+id, nil, alice)
	if code != http.StatusConflict {
		t.Fatalf("archive-worn = %d, want 409: %+v", code, out)
	}
	if msg := out["error"].(string); !strings.Contains(msg, "worn") {
		t.Fatalf("worn error = %q", msg)
	}

	// not worn anymore -> archive ok
	sp.RemoveEntity(&Entity{ID: "wearer-1"})
	code, out = doJSON(t, h.handleAvatars, http.MethodDelete, "/api/avatars/assets/"+id, nil, alice)
	if code != http.StatusOK {
		t.Fatalf("archive = %d: %+v", code, out)
	}
	code, out = doJSON(t, h.handleAvatars, http.MethodGet, "/api/avatars/assets", nil, alice)
	if n := len(out["assets"].([]any)); n != 0 {
		t.Fatalf("after archive assets = %d, want 0", n)
	}

	// audit rows exist (append-only, kind=avatar)
	var n int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM activity_events WHERE kind='avatar' AND target=?`, id).Scan(&n); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if n < 2 { // asset.upload + asset.archive
		t.Fatalf("audit rows = %d, want >= 2", n)
	}
}

func TestT2EntitlementDenyThenAllow(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a2-key", "T2Alice")
	bob := newTestUser(t, h, "t2-b2-key", "T2Bob")

	id := uploadAsset(t, h, alice, "outfit", "aliceCoat")
	want := "asset:" + id

	// B wears A's asset with no set -> denied, normalized to fallback
	bSpec := AvatarSpec{V: 1, Body: "round", Skin: "warm", Hair: "bob", Outfit: want, Accessory: "none"}
	got, denied := h.entitledSpec(bob.UserID, bSpec, false)
	if got.Outfit == want {
		t.Fatalf("deny case kept foreign asset: %+v", got)
	}
	if !containsStr(denied, "outfit:"+want) {
		t.Fatalf("denied list missing outfit:want: %v", denied)
	}
	if !optionAllowed("outfit", got.Outfit, false) {
		t.Fatalf("fallback not a valid human option: %+v", got)
	}

	// A adds the asset to a public set -> anyone may wear it
	set := &avatarSetRow{ID: newUUID(), Name: "Alice Public", Scope: setScopePublic, Version: 1,
		CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := h.store.CreateAvatarSet(set, []setItem{{Layer: "outfit", OptionID: want}}); err != nil {
		t.Fatalf("create set: %v", err)
	}
	_ = h.store.MoveAvatarAssetToSet(id, set.ID)
	regSync(h)

	got, denied = h.entitledSpec(bob.UserID, bSpec, false)
	if got.Outfit != want {
		t.Fatalf("allow case still denied after public set: %+v denied=%v", got, denied)
	}

	// archived set revokes the option for everyone but the owner
	if err := h.store.ArchiveAvatarSet(set.ID, "test"); err != nil {
		t.Fatalf("archive set: %v", err)
	}
	regSync(h)
	got, _ = h.entitledSpec(bob.UserID, bSpec, false)
	if got.Outfit == want {
		t.Fatalf("archived set still grants: %+v", got)
	}
	// owner still entitled
	gotA, _ := h.entitledSpec(alice.UserID, bSpec, false)
	if gotA.Outfit != want {
		t.Fatalf("owner lost own asset: %+v", gotA)
	}
}

func TestT2UserGrantedScopeRequiresGrant(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a3-key", "T2Alice")
	bob := newTestUser(t, h, "t2-b3-key", "T2Bob")
	cara := newTestUser(t, h, "t2-c3-key", "T2Cara")

	id := uploadAsset(t, h, alice, "accessory", "aliceHalo")
	want := "asset:" + id
	set := &avatarSetRow{ID: newUUID(), Name: "Alice VIP", Scope: setScopeUserGranted, Version: 1,
		CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := h.store.CreateAvatarSet(set, []setItem{{Layer: "accessory", OptionID: want}}); err != nil {
		t.Fatalf("create set: %v", err)
	}
	_ = h.store.MoveAvatarAssetToSet(id, set.ID)
	regSync(h)

	// no grant -> denied for both B and C
	spec := AvatarSpec{V: 1, Body: "round", Skin: "warm", Hair: "bob", Outfit: "tee", Accessory: want}
	if got, _ := h.entitledSpec(bob.UserID, spec, false); got.Accessory == want {
		t.Fatalf("B wore asset without grant: %+v", got)
	}
	// direct grant to B -> allowed; C still denied
	if err := h.store.AddAvatarGrant(&avatarGrantRow{ID: newUUID(), UserID: bob.UserID, SetID: set.ID,
		Kind: grantDirect, CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	regSync(h)
	if got, _ := h.entitledSpec(bob.UserID, spec, false); got.Accessory != want {
		t.Fatalf("B denied despite direct grant: %+v", got)
	}
	if got, _ := h.entitledSpec(cara.UserID, spec, false); got.Accessory == want {
		t.Fatalf("C wore asset without grant: %+v", got)
	}
	// time-limited grant that already expired -> denied
	expired := &avatarGrantRow{ID: newUUID(), UserID: cara.UserID, SetID: set.ID, Kind: grantTimeLimited,
		ExpiresAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := h.store.AddAvatarGrant(expired); err != nil {
		t.Fatalf("expired grant: %v", err)
	}
	regSync(h)
	if got, _ := h.entitledSpec(cara.UserID, spec, false); got.Accessory == want {
		t.Fatalf("C wore asset with expired grant: %+v", got)
	}
}

func TestT2ClaimCheckedGrants(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a4-key", "T2Alice")
	vip := newTestUser(t, h, "t2-vip-key", "T2Vip")
	regular := newTestUser(t, h, "t2-reg-key", "T2Regular")

	id := uploadAsset(t, h, alice, "hair", "vipHair")
	want := "asset:" + id
	set := &avatarSetRow{ID: newUUID(), Name: "VIP Look", Scope: setScopeMembership, Version: 1,
		CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := h.store.CreateAvatarSet(set, []setItem{{Layer: "hair", OptionID: want}}); err != nil {
		t.Fatalf("create set: %v", err)
	}
	_ = h.store.MoveAvatarAssetToSet(id, set.ID)
	// tag grant for "vip"; only the vip member carries that claim
	if err := h.store.AddAvatarGrant(&avatarGrantRow{ID: newUUID(), UserID: vip.UserID, SetID: set.ID,
		Kind: grantTag, Match: "vip", CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("tag grant: %v", err)
	}
	_ = h.store.AddUserClaim(vip.UserID, "tag", "vip")
	// email-domain grant for "@bawes.com"; regular has no claims
	if err := h.store.AddAvatarGrant(&avatarGrantRow{ID: newUUID(), UserID: regular.UserID, SetID: set.ID,
		Kind: grantEmailDomain, Match: "bawes.com", CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatalf("domain grant: %v", err)
	}
	regSync(h)

	spec := AvatarSpec{V: 1, Body: "round", Skin: "warm", Hair: want, Outfit: "tee", Accessory: "none"}
	if got, _ := h.entitledSpec(vip.UserID, spec, false); got.Hair != want {
		t.Fatalf("vip denied despite tag claim: %+v", got)
	}
	if got, _ := h.entitledSpec(regular.UserID, spec, false); got.Hair == want {
		t.Fatalf("regular wore vip asset (no claims): %+v", got)
	}
	// claim arrives -> now entitled
	_ = h.store.AddUserClaim(regular.UserID, "email_domain", "BAWES.COM") // lowercased by store
	regSync(h)
	if got, _ := h.entitledSpec(regular.UserID, spec, false); got.Hair != want {
		t.Fatalf("regular denied despite domain claim: %+v", got)
	}
}

func TestT2SetVersioningAndGovernance(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a5-key", "T2Alice")

	set := &avatarSetRow{ID: newUUID(), Name: "V1", Scope: setScopePublic, Version: 1,
		CreatedBy: alice.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err := h.store.CreateAvatarSet(set, []setItem{{Layer: "body", OptionID: "bean"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	v, err := h.store.AvatarSetRow(set.ID)
	if err != nil || v.Version != 1 {
		t.Fatalf("initial version = %+v err=%v", v, err)
	}
	v2, err := h.store.AddAvatarSetItem(set.ID, "skin", "deep", alice.UserID)
	if err != nil || v2 != 2 {
		t.Fatalf("add item version = %d err=%v, want 2", v2, err)
	}
	v3, err := h.store.RemoveAvatarSetItem(set.ID, "body", "bean")
	if err != nil || v3 != 3 {
		t.Fatalf("remove item version = %d err=%v, want 3", v3, err)
	}
	items, err := h.store.AvatarSetItems(set.ID)
	if err != nil || len(items) != 1 || items[0] != "skin:deep" {
		t.Fatalf("items = %v err=%v", items, err)
	}
	// world scope requires the owner; a non-owner is rejected by the handler
	world := h.store.ListWorlds()
	if len(world) == 0 {
		t.Fatalf("worlds = 0")
	}
	bob := newTestUser(t, h, "t2-b5-key", "T2Bob")
	code, out := doJSON(t, h.handleAvatars, http.MethodPost, "/api/avatars/sets",
		map[string]any{"name": "TheirWorld", "scope": setScopeWorld, "worldId": world[0].ID}, bob)
	if code != http.StatusForbidden {
		t.Fatalf("non-owner world set = %d: %+v", code, out)
	}
	// set archive via handler (empty set, nothing worn)
	code, out = doJSON(t, h.handleAvatars, http.MethodPost, "/api/avatars/sets/"+set.ID+"/archive", nil, alice)
	if code != http.StatusOK {
		t.Fatalf("archive set = %d: %+v", code, out)
	}
	row, _ := h.store.AvatarSetRow(set.ID)
	if !row.Archived {
		t.Fatal("set not archived")
	}
}

func TestT2GenerateDeterministicAndValid(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a6-key", "T2Alice")

	a := h.generateAvatarSpec(alice.UserID, "a friendly pirate in a cape")
	b := h.generateAvatarSpec(alice.UserID, "a friendly pirate in a cape")
	if a != b {
		t.Fatalf("same prompt+member must be deterministic: %+v vs %+v", a, b)
	}
	if a.V != avatarSpecV {
		t.Fatalf("generated spec version = %d", a.V)
	}
	for _, layer := range avatarLayers {
		id := layerValue(a, layer)
		if !strings.HasPrefix(id, assetOptionPrefix) && !optionAllowed(layer, id, false) {
			t.Fatalf("generated layer %s invalid: %q", layer, id)
		}
	}
	// different prompt changes the outcome (not asserted for equality — the
	// sampler may collide on a layer; only determinism is a hard contract)
	if generativeModel == "" || !strings.Contains(generativeModel, "v1") {
		t.Fatalf("generative model doc missing: %q", generativeModel)
	}
}

func TestT2RoundTripPersistReload(t *testing.T) {
	resetAvatarStore(t)
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a7-key", "T2Alice")

	id := uploadAsset(t, h, alice, "body", "suit")
	want := "asset:" + id
	in := AvatarSpec{V: 1, Body: want, Skin: "warm", Hair: "bob", Outfit: "tee", Accessory: "none"}

	got := resolveAvatarSpecT2(h, alice.UserID, &in, "127.0.0.1")
	if got != in {
		t.Fatalf("incoming asset spec not honored: %+v", got)
	}
	// reload with no incoming spec -> identical stored spec
	got2 := resolveAvatarSpecT2(h, alice.UserID, nil, "127.0.0.1")
	if got2 != in {
		t.Fatalf("round-trip mismatch: %+v want %+v", got2, in)
	}
}

func TestT2SniffImage(t *testing.T) {
	raw, _ := base64.StdEncoding.DecodeString(tinyPNG)
	kind, w, h, err := sniffImage(raw)
	if err != nil || kind != "image/png" || w != 1 || h != 1 {
		t.Fatalf("png sniff = %s %dx%d err=%v", kind, w, h, err)
	}
	if _, _, _, err := sniffImage([]byte("not an image at all")); err == nil {
		t.Fatal("garbage should be rejected")
	}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01}
	kind, _, _, err = sniffImage(jpeg)
	if err != nil || kind != "image/jpeg" {
		t.Fatalf("jpeg sniff = %s err=%v", kind, err)
	}
	// oversized upload is rejected by the handler; sniffImage returns the
	// header dims which the handler validates (1x1 above passes the limit)
}

func TestT2AvatarUpdateWSEnvelope(t *testing.T) {
	h := newTestHub(t)
	alice := newTestUser(t, h, "t2-a8-key", "T2Alice")
	id := uploadAsset(t, h, alice, "accessory", "ring")
	want := "asset:" + id

	// parseAvatarSpec accepts the asset option id (42 chars — longer than
	// catalog ids) so the WS join + avatar_update path is not blocked
	a := map[string]any{"spec": map[string]any{"v": 1.0, "body": "round", "skin": "warm",
		"hair": "bob", "outfit": "tee", "accessory": want}}
	spec := parseAvatarSpec(a)
	if spec == nil || spec.Accessory != want {
		t.Fatalf("parseAvatarSpec rejected asset option: %+v", spec)
	}
	got := resolveAvatarSpecT2(h, alice.UserID, spec, "10.0.0.1")
	if got.Accessory != want {
		t.Fatalf("owner asset denied on avatar_update path: %+v", got)
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
