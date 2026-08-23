package main

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

// catalog invariants: >= 4 options per layer, all ids unique, every layer has
// at least one human-usable option and at least one NPC-only body option.
func TestAvatarCatalogInvariants(t *testing.T) {
	for _, layer := range avatarLayers {
		opts := avatarCatalog[layer]
		if len(opts) < 4 {
			t.Errorf("layer %q has %d options, want >= 4", layer, len(opts))
		}
		seen := map[string]bool{}
		human := 0
		for _, o := range opts {
			if seen[o.ID] {
				t.Errorf("layer %q duplicate option id %q", layer, o.ID)
			}
			seen[o.ID] = true
			if o.Label == "" {
				t.Errorf("layer %q option %q missing label", layer, o.ID)
			}
			if !o.NPCOnly {
				human++
			}
		}
		if human == 0 {
			t.Errorf("layer %q has no human-usable options", layer)
		}
	}
	if !optionAllowed("body", "bot", true) {
		t.Error("npc body 'bot' should be allowed for bots")
	}
	if optionAllowed("body", "bot", false) {
		t.Error("npc body 'bot' must be rejected for humans")
	}
}

func TestValidateAvatarSpec(t *testing.T) {
	spec := AvatarSpec{V: 1, Body: "round", Skin: "warm", Hair: "bob", Outfit: "hoodie", Accessory: "glasses"}
	got := validateAvatarSpec(spec, false)
	if got != spec {
		t.Errorf("valid spec mutated: %+v", got)
	}
	// unknown + NPC-only options fall back to valid defaults for humans
	bad := AvatarSpec{V: 1, Body: "bot", Skin: "warp", Hair: "bob", Outfit: "hoodie", Accessory: "none"}
	got = validateAvatarSpec(bad, false)
	if got.Body != "round" {
		t.Errorf("npc body not rejected for human: %+v", got)
	}
	if got.Skin == "warp" {
		t.Errorf("unknown skin not normalized: %+v", got)
	}
	for _, layer := range avatarLayers {
		if !optionAllowed(layer, layerValue(got, layer), false) {
			t.Errorf("validated spec still has bad %s: %+v", layer, got)
		}
	}
	// bots may keep the NPC-only body
	botSpec := validateAvatarSpec(bad, true)
	if botSpec.Body != "bot" {
		t.Errorf("npc body rejected for bot: %+v", botSpec)
	}
}

func TestDefaultAvatarSpecDeterministic(t *testing.T) {
	a := defaultAvatarSpec("user-1")
	b := defaultAvatarSpec("user-1")
	c := defaultAvatarSpec("user-2")
	if a != b {
		t.Errorf("default not deterministic for same user: %+v vs %+v", a, b)
	}
	if a == c {
		t.Errorf("defaults for different users should differ: %+v", a)
	}
	for _, layer := range avatarLayers {
		if !optionAllowed(layer, layerValue(a, layer), false) {
			t.Errorf("default has non-human option for %s: %+v", layer, a)
		}
	}
	// robot spec uses the NPC-only body
	r := robotAvatarSpec(0)
	if r.Body != "bot" || !optionAllowed("body", r.Body, true) {
		t.Errorf("robot spec wrong: %+v", r)
	}
}

func TestParseAvatarSpec(t *testing.T) {
	a := map[string]any{
		"color": "#ff0000",
		"spec":  map[string]any{"v": 1.0, "body": "bean", "skin": "deep", "hair": "curls", "outfit": "robe", "accessory": "crown"},
	}
	spec := parseAvatarSpec(a)
	if spec == nil || spec.Body != "bean" || spec.Accessory != "crown" || spec.V != 1 {
		t.Fatalf("parse failed: %+v", spec)
	}
	if parseAvatarSpec(map[string]any{"color": "#fff"}) != nil {
		t.Error("no spec map should parse to nil")
	}
	if parseAvatarSpec(map[string]any{"spec": map[string]any{}}) != nil {
		t.Error("empty spec should parse to nil")
	}
	// mixed-type layer values are ignored safely
	if parseAvatarSpec(map[string]any{"spec": map[string]any{"body": 42}}) != nil {
		t.Error("non-string layer should yield nil (empty spec)")
	}
}

// resetAvatarStore points the store singleton at a fresh temp DB so tests are
// hermetic regardless of run order.
func resetAvatarStore(t *testing.T) {
	t.Helper()
	t.Setenv("HEARTH_DB", filepath.Join(t.TempDir(), "test.db"))
	avatarStoreOnce = sync.Once{}
	avatarStoreInst = nil
	avatarStoreErr = nil
}

func TestResolveAvatarSpecPolicy(t *testing.T) {
	resetAvatarStore(t)
	userID := "resolve-test-user"
	in := AvatarSpec{V: 1, Body: "slim", Skin: "olive", Hair: "mohawk", Outfit: "vest", Accessory: "scarf"}
	got := resolveAvatarSpec(userID, &in)
	if got != in {
		t.Errorf("incoming spec not honored: %+v", got)
	}
	// reload with no incoming spec -> stored spec persists
	got2 := resolveAvatarSpec(userID, nil)
	if got2 != in {
		t.Errorf("stored spec not returned on reload: %+v", got2)
	}
	// a different user with no stored spec gets a valid deterministic default
	got3 := resolveAvatarSpec("other-user", nil)
	for _, layer := range avatarLayers {
		if !optionAllowed(layer, layerValue(got3, layer), false) {
			t.Errorf("default not human-valid for %s: %+v", layer, got3)
		}
	}
}

func TestAvatarEntitlementStubs(t *testing.T) {
	if !canUseAvatarOption("u1", "hair", "bob") {
		t.Error("free option should be allowed")
	}
	if canUseAvatarOption("u1", "body", "bot") {
		t.Error("NPC-only option must be blocked for humans")
	}
	spec := AvatarSpec{V: 1, Body: "bot", Skin: "warm", Hair: "bob", Outfit: "tee", Accessory: "none"}
	valid, ok := avatarEntitlementCheck("u1", spec)
	if ok {
		t.Error("NPC-only body should fail the human entitlement check")
	}
	if valid.Body != "round" {
		t.Errorf("entitlement check should normalize NPC body: %+v", valid)
	}
}

func TestAvatarStoreRoundTrip(t *testing.T) {
	resetAvatarStore(t)

	st, err := avatarStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	spec := AvatarSpec{V: 1, Body: "round", Skin: "fair", Hair: "cap", Outfit: "dress", Accessory: "halo"}
	if err := st.Put("member-1", spec); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok := st.Get("member-1")
	if !ok || got != spec {
		t.Fatalf("round-trip mismatch: %+v ok=%v", got, ok)
	}
	// update overwrites
	spec2 := spec
	spec2.Hair = "curls"
	if err := st.Put("member-1", spec2); err != nil {
		t.Fatalf("put2: %v", err)
	}
	got, _ = st.Get("member-1")
	if got.Hair != "curls" {
		t.Fatalf("update did not persist: %+v", got)
	}
	if _, ok := st.Get("member-nope"); ok {
		t.Fatal("missing member should not be found")
	}
	// JSON round-trip through the wire shape
	b, _ := json.Marshal(spec2)
	var back AvatarSpec
	if err := json.Unmarshal(b, &back); err != nil || back != spec2 {
		t.Fatalf("json round-trip: %+v err=%v", back, err)
	}
}
