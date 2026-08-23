package main

// S7 bot spawn API — POST/GET /api/bots.
//
// POST /api/bots {"name":"Bricky","world":"garden","action":"build",
//                 "runId":"demo-1","script":{...}}  -> 202 {runId,status:"spawned",...}
//
// Spawns a headless builder bot that joins the world over the server's own
// /ws endpoint and writes its op sequence through the same edit envelope
// humans use. With no script the built-in demo runs (house + heart in
// garden). Reusing runId replays idempotently (deduped acks).
//
// GET /api/bots            -> status list (newest first)
// GET /api/bots/{runId}    -> one run status

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (h *Hub) handleBots(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.spawnBot(w, r)
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bots": h.bots.List()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Hub) handleBotStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := strings.TrimPrefix(r.URL.Path, "/api/bots/")
	runID = strings.TrimSuffix(runID, "/")
	if runID == "" {
		http.NotFound(w, r)
		return
	}
	st := h.bots.Get(runID)
	if st == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no such bot run"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bot": st})
}

// spawnBot starts a headless builder bot against this server's own /ws path.
func (h *Hub) spawnBot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string     `json:"name"`
		World      string     `json:"world"`
		Action     string     `json:"action"` // 'build'
		RunID      string     `json:"runId"`
		Script     *BotScript `json:"script"`
		IntervalMS int        `json:"intervalMs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	if body.Action != "" && body.Action != "build" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "action must be 'build'"})
		return
	}
	if body.World == "" {
		body.World = "garden"
	}
	script := body.Script
	if script == nil {
		script = DemoBuildScript()
	}
	if script.World == "" {
		script.World = body.World
	}
	if script.Name == "" {
		script.Name = body.Name
	}
	if script.Name == "" {
		script.Name = "Bricky"
	}
	deviceKey := "bot-" + slug(script.Name)
	runID := body.RunID
	if runID == "" {
		runID = deviceKey + "-" + randHex(4)
	}
	cfg := BotConfig{
		Name: script.Name, DeviceKey: deviceKey, World: script.World,
		URL: "ws://" + r.Host + "/ws", RunID: runID, Script: script,
		Interval: time.Duration(body.IntervalMS) * time.Millisecond,
	}
	st := h.bots.Spawn(cfg)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "status": "spawned",
		"bot": map[string]any{
			"runId": st.RunID, "name": st.Name, "deviceKey": st.DeviceKey,
			"world": st.World, "userId": st.UserID, "ops": st.Ops,
		},
	})
}
