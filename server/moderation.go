package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Social clarity moderation surface (additive — PROTOCOL.md untouched):
//
//	POST   /api/reports        {reportedId, reason?, spaceId?} -> report row
//	POST   /api/blocks         {blockedId} -> block (one direction)
//	DELETE /api/blocks/{id}    unblock
//
// Both require the hearth_session cookie (sessionFromRequest), same as
// /api/friends. DMs are suppressed when EITHER direction is blocked
// (store.AreBlocked) — the block is enforced at the chat gate, not here.

// handleReports: POST /api/reports — record an abuse report. The reported id
// may be a userId (preferred, from the roster/presence) or an entity/session
// id; we store it verbatim so the moderation dashboard can join either way.
func (h *Hub) handleReports(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	var body struct {
		ReportedID string `json:"reportedId"`
		Reason     string `json:"reason"`
		SpaceID    string `json:"spaceId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	reported := strings.TrimSpace(body.ReportedID)
	if reported == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "reportedId required"})
		return
	}
	if reported == sess.UserID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "you cannot report yourself"})
		return
	}
	if err := h.store.InsertReport(sess.UserID, reported, strings.TrimSpace(body.SpaceID), strings.TrimSpace(body.Reason)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// handleBlocks: POST /api/blocks — block a user (one direction).
func (h *Hub) handleBlocks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	var body struct {
		BlockedID string `json:"blockedId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	blocked := strings.TrimSpace(body.BlockedID)
	if blocked == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "blockedId required"})
		return
	}
	if blocked == sess.UserID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "you cannot block yourself"})
		return
	}
	if h.store.userDisplay(blocked) == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no such user"})
		return
	}
	if err := h.store.BlockUser(sess.UserID, blocked); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "blockedId": blocked})
}

// handleBlockRoute: DELETE /api/blocks/{id} — unblock.
func (h *Hub) handleBlockRoute(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/blocks/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	if err := h.store.UnblockUser(sess.UserID, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
