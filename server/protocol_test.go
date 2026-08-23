package main

import (
	"encoding/json"
	"testing"
)

// Protocol contract tests (PROTOCOL.md v0). Wire format is map-based JSON;
// messages carry {"type":"move","d":{...}} (note: "type", not "t" — the v0
// implementation uses "type"; PROTOCOL.md says "t" — see serverHandlesType).
// These tests lock what the server actually accepts; amending the protocol
// requires amending these tests in the same commit (round+sign rule).

func TestWireEnvelopeParses(t *testing.T) {
	raw := `{"type":"move","id":"abc","ts":1234,"d":{"x":12.5,"y":8.2,"dir":"right","seq":1}}`
	var msg map[string]any
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg["type"] != "move" {
		t.Errorf("type = %v, want move", msg["type"])
	}
	if msg["id"] == "" || msg["ts"] == nil {
		t.Errorf("envelope missing id/ts")
	}
	d, ok := msg["d"].(map[string]any)
	if !ok {
		t.Fatalf("d missing or not an object")
	}
	if d["x"] != 12.5 || d["y"] != 8.2 || d["dir"] != "right" || d["seq"] != float64(1) {
		t.Errorf("move data wrong: %+v", d)
	}
}

func TestServerDispatchCoversProtocolTypes(t *testing.T) {
	// The dispatch switch in ws.go handleMessage must cover every client->server
	// type from PROTOCOL.md. "ping" is the health type (server replies pong).
	want := []string{"join", "move", "chat", "edit", "portal", "signal", "media", "bot_msg", "ping"}
	for _, typ := range want {
		if !serverHandlesType(typ) {
			t.Errorf("protocol type %q not handled by dispatch", typ)
		}
	}
}

func TestChatPayloadLimit(t *testing.T) {
	// Protocol: chat text <= 2KB.
	if chatMaxBytes != 2048 {
		t.Errorf("chatMaxBytes = %d, want 2048", chatMaxBytes)
	}
}
