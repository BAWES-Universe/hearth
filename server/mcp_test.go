package main

// T2 — MCP v1 acceptance test: a REAL in-process round-trip over the
// streamable HTTP transport. Boots a hub + WS server + /mcp endpoint,
// then walks the exact client sequence: initialize (session issued) →
// notifications/initialized → tools/list → tools/call. The edit tool
// mutates a live world through the bot op-log and the audit feed shows a
// role=bot row; replaying the same runId dedupes.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hearth/mcp"
)

// mcpTestServer boots hub + WS + /mcp on one httptest server.
func mcpTestServer(t *testing.T) (string, *Hub, *mcpBackend) {
	t.Helper()
	h := newTestHub(t)
	go h.Run()
	t.Cleanup(h.Close)
	be := &mcpBackend{hub: h}
	srv := mcp.NewServer(be)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws":
			h.handleWS(w, r)
		case "/mcp":
			srv.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	be.wsBase = "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return ts.URL, h, be
}

// mcpPost performs one POST /mcp JSON-RPC request; returns status, headers,
// decoded response map and raw body.
func mcpPost(t *testing.T, base, sessID string, body map[string]any, accept string) (int, http.Header, map[string]any, string) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal rpc: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if sessID != "" {
		req.Header.Set("Mcp-Session-Id", sessID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp post: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, resp.Header, m, string(raw)
}

func rpcReq(id int, method string, params map[string]any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

// toolText extracts result.content[0].text from a tools/call response.
func toolText(t *testing.T, res map[string]any) string {
	t.Helper()
	result, ok := res["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", res)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("tool returned isError=true: %v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in result: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("bad content entry: %v", content[0])
	}
	text, _ := first["text"].(string)
	return text
}

func TestMCPRoundTrip(t *testing.T) {
	base, h, _ := mcpTestServer(t)

	// 1) initialize → JSON-RPC result + Mcp-Session-Id issued
	code, hdr, res, _ := mcpPost(t, base, "", rpcReq(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-accept-test", "version": "0.0.1"},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("initialize: status %d", code)
	}
	sessID := hdr.Get("Mcp-Session-Id")
	if sessID == "" {
		t.Fatal("initialize: no Mcp-Session-Id header issued")
	}
	result, _ := res["result"].(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want 2025-06-18", result["protocolVersion"])
	}
	si, _ := result["serverInfo"].(map[string]any)
	if si["name"] != "hearth-mcp" || si["version"] == "" {
		t.Errorf("serverInfo = %v, want hearth-mcp with a version", si)
	}
	caps, _ := result["capabilities"].(map[string]any)
	if caps["tools"] == nil {
		t.Errorf("capabilities = %v, want tools capability", caps)
	}

	// 2) notifications/initialized → 202, empty body, no JSON-RPC response
	code, _, _, raw := mcpPost(t, base, sessID, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, "")
	if code != http.StatusAccepted || raw != "" {
		t.Fatalf("notifications/initialized: status=%d body=%q, want 202 empty", code, raw)
	}

	// 3) tools/list → the seven tools
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(2, "tools/list", nil), "")
	if code != http.StatusOK {
		t.Fatalf("tools/list: status %d", code)
	}
	tools := res["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"worlds.list", "world.read", "world.edit", "world.chat", "presence.list", "bot.run", "world.activity"} {
		if !names[want] {
			t.Errorf("tools/list: missing tool %q (have %v)", want, names)
		}
	}

	// 4) tools/call worlds.list → garden is in the published directory
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(3, "tools/call", map[string]any{
		"name": "worlds.list", "arguments": map[string]any{},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("worlds.list: status %d", code)
	}
	var dir struct {
		Worlds []map[string]any `json:"worlds"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &dir); err != nil {
		t.Fatalf("worlds.list text: %v", err)
	}
	garden := false
	for _, w := range dir.Worlds {
		if w["id"] == "garden" {
			garden = true
			if w["is_published"] != true {
				t.Errorf("garden is_published = %v, want true", w["is_published"])
			}
		}
	}
	if !garden {
		t.Fatalf("worlds.list: garden missing from %v", dir.Worlds)
	}

	// 5) tools/call world.read garden → HMF v1 doc with tiles + chunks
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(4, "tools/call", map[string]any{
		"name": "world.read", "arguments": map[string]any{"worldId": "garden"},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("world.read: status %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(toolText(t, res)), &doc); err != nil {
		t.Fatalf("world.read text: %v", err)
	}
	if doc["hmf"] != "v1" {
		t.Errorf("world.read hmf = %v, want v1", doc["hmf"])
	}
	tiles, _ := doc["tiles"].([]any)
	if len(tiles) == 0 {
		t.Errorf("world.read: garden has no tiles?")
	}
	if _, ok := doc["chunks"]; !ok {
		t.Errorf("world.read: no chunks rev summary")
	}
	if _, ok := doc["palette"]; !ok {
		t.Errorf("world.read: no palette")
	}

	// 6) tools/call world.edit → paint flower at (2,2) via bot op-log
	editKey := "bot-mcp-accept"
	editRun := "mcp-accept-run-1"
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(5, "tools/call", map[string]any{
		"name": "world.edit", "arguments": map[string]any{
			"worldId": "garden", "op": "paint", "x": 2, "y": 2,
			"tile": "flower", "deviceKey": editKey, "runId": editRun,
		},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("world.edit: status %d", code)
	}
	var er struct {
		Applied bool   `json:"applied"`
		Deduped bool   `json:"deduped"`
		Seq     int64  `json:"seq"`
		Actor   string `json:"actor"`
		Audit   string `json:"audit"`
		RunID   string `json:"runId"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &er); err != nil {
		t.Fatalf("world.edit text: %v", err)
	}
	if !er.Applied || er.Deduped {
		t.Fatalf("world.edit: applied=%v deduped=%v, want applied fresh", er.Applied, er.Deduped)
	}
	if er.Seq <= 0 {
		t.Errorf("world.edit: seq = %d, want > 0", er.Seq)
	}
	if want := hashDeviceKey(editKey); er.Actor != want {
		t.Errorf("world.edit actor = %s, want %s", er.Actor, want)
	}
	if er.Audit != "role=bot" {
		t.Errorf("world.edit audit = %q, want role=bot", er.Audit)
	}
	if er.RunID != editRun {
		t.Errorf("world.edit runId = %q, want %q", er.RunID, editRun)
	}

	// 7) the tile actually persisted through the op-log
	w, err := h.store.LoadWorld("garden")
	if err != nil {
		t.Fatalf("load garden: %v", err)
	}
	if got := w.TileAt(2, 2); got != "flower" {
		t.Errorf("garden tile (2,2) = %q, want flower", got)
	}

	// 8) audit feed shows the role=bot row
	events, err := h.store.RecentActivity("garden", 50)
	if err != nil {
		t.Fatalf("recent activity: %v", err)
	}
	foundBotAudit := false
	for _, e := range events {
		if e.Role == "bot" && e.Kind == "edit" && e.Action == "paint" && e.Actor == hashDeviceKey(editKey) {
			foundBotAudit = true
			if !strings.Contains(e.Diff, editRun) {
				t.Errorf("bot audit row diff missing idem runId: %s", e.Diff)
			}
		}
	}
	if !foundBotAudit {
		t.Fatalf("audit feed: no role=bot edit row for %s (events=%v)", editKey, events)
	}

	// 9) replay same runId → deduped, ORIGINAL seq (idempotent)
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(6, "tools/call", map[string]any{
		"name": "world.edit", "arguments": map[string]any{
			"worldId": "garden", "op": "paint", "x": 2, "y": 2,
			"tile": "flower", "deviceKey": editKey, "runId": editRun,
		},
	}), "")
	var er2 struct {
		Applied bool  `json:"applied"`
		Deduped bool  `json:"deduped"`
		Seq     int64 `json:"seq"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &er2); err != nil {
		t.Fatalf("world.edit replay text: %v", err)
	}
	if !er2.Deduped || er2.Applied {
		t.Fatalf("replay: applied=%v deduped=%v, want deduped", er2.Applied, er2.Deduped)
	}
	if er2.Seq != er.Seq {
		t.Errorf("replay seq = %d, want original %d", er2.Seq, er.Seq)
	}

	// 10) validation: edit without deviceKey → isError
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(7, "tools/call", map[string]any{
		"name": "world.edit", "arguments": map[string]any{
			"worldId": "garden", "op": "paint", "x": 3, "y": 3, "tile": "wall",
		},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("bad edit: status %d", code)
	}
	if isErr, _ := res["result"].(map[string]any)["isError"].(bool); !isErr {
		t.Errorf("edit without deviceKey: want isError=true, got %v", res)
	}

	// 11) world.chat → delivered as the bot account
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(8, "tools/call", map[string]any{
		"name": "world.chat", "arguments": map[string]any{
			"worldId": "garden", "text": "hello from mcp accept", "deviceKey": editKey,
		},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("world.chat: status %d", code)
	}
	var cr struct {
		Delivered bool   `json:"delivered"`
		FromID    string `json:"fromId"`
		Actor     string `json:"actor"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &cr); err != nil {
		t.Fatalf("world.chat text: %v", err)
	}
	if !cr.Delivered || cr.FromID == "" {
		t.Fatalf("world.chat: delivered=%v fromId=%q", cr.Delivered, cr.FromID)
	}
	if cr.Actor != hashDeviceKey(editKey) {
		t.Errorf("world.chat actor = %s, want %s", cr.Actor, hashDeviceKey(editKey))
	}

	// 12) presence.list → live occupants of garden
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(9, "tools/call", map[string]any{
		"name": "presence.list", "arguments": map[string]any{"worldId": "garden"},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("presence.list: status %d", code)
	}
	var pr struct {
		WorldID   string           `json:"worldId"`
		Occupants []map[string]any `json:"occupants"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &pr); err != nil {
		t.Fatalf("presence.list text: %v", err)
	}
	if pr.WorldID != "garden" {
		t.Errorf("presence.list worldId = %s", pr.WorldID)
	}
	for _, o := range pr.Occupants {
		if o["id"] == "" || o["name"] == "" {
			t.Errorf("presence.list occupant missing id/name: %v", o)
		}
	}

	// 13) bot.run → multi-op script through the op-log
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(10, "tools/call", map[string]any{
		"name": "bot.run", "arguments": map[string]any{
			"world": "garden", "deviceKey": editKey, "runId": "mcp-accept-botrun-1",
			"ops": []map[string]any{
				{"op": "paint", "x": 4, "y": 4, "tile": "wall"},
				{"op": "paint", "x": 5, "y": 4, "tile": "wall"},
			},
		},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("bot.run: status %d", code)
	}
	var br struct {
		Applied  int    `json:"applied"`
		Ops      int    `json:"ops"`
		FirstErr string `json:"firstErr"`
		Actor    string `json:"actor"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &br); err != nil {
		t.Fatalf("bot.run text: %v", err)
	}
	if br.FirstErr != "" {
		t.Fatalf("bot.run firstErr: %s", br.FirstErr)
	}
	if br.Applied != 2 || br.Ops != 2 {
		t.Errorf("bot.run applied=%d ops=%d, want 2/2", br.Applied, br.Ops)
	}
	if br.Actor != hashDeviceKey(editKey) {
		t.Errorf("bot.run actor = %s", br.Actor)
	}

	// 14) world.activity → bot rows visible through the tool
	code, _, res, _ = mcpPost(t, base, sessID, rpcReq(11, "tools/call", map[string]any{
		"name": "world.activity", "arguments": map[string]any{"worldId": "garden", "limit": 10},
	}), "")
	if code != http.StatusOK {
		t.Fatalf("world.activity: status %d", code)
	}
	var ar struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(toolText(t, res)), &ar); err != nil {
		t.Fatalf("world.activity text: %v", err)
	}
	botRows := 0
	for _, e := range ar.Events {
		if e["role"] == "bot" {
			botRows++
		}
	}
	if botRows == 0 {
		t.Errorf("world.activity: no role=bot rows in %v", ar.Events)
	}

	// 15) unknown session id → 404 (streamable-HTTP session contract)
	code, _, _, _ = mcpPost(t, base, "bogus-session", rpcReq(12, "tools/list", nil), "")
	if code != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", code)
	}

	// 16) DELETE terminates the session → 200
	req, _ := http.NewRequest(http.MethodDelete, base+"/mcp", nil)
	req.Header.Set("Mcp-Session-Id", sessID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("delete session: status %d, want 200", resp.StatusCode)
	}

	// 17) SSE accept → text/event-stream frame (streamable transport)
	code, hdr, _, raw = mcpPost(t, base, "", rpcReq(13, "tools/list", nil), "application/json, text/event-stream")
	if code != http.StatusOK {
		t.Fatalf("sse tools/list: status %d", code)
	}
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("sse content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(raw, "event: message") || !strings.Contains(raw, "worlds.list") {
		t.Errorf("sse body malformed: %.200s", raw)
	}

	// 18) GET /mcp → 405 (no server->client stream offered)
	resp, err = http.Get(base + "/mcp")
	if err != nil {
		t.Fatalf("get /mcp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp: status %d, want 405", resp.StatusCode)
	}
}
