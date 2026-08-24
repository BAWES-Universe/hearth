package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// S1 Core — world CRUD REST + universal hub routing support.
//
// API surface (all JSON, PROTOCOL.md untouched):
//
//	POST /api/worlds           create a blank-canvas draft world (owner = session)
//	POST /api/worlds/{id}/publish   draft -> published (idempotent)
//	GET  /api/worlds           directory: published worlds, gravity desc then
//	                           recency desc; ?q= name search; headcount + owner
//	GET  /api/worlds/{id}      world doc (flags + gravity + live headcount)
//
// Spawn routing: the WS join handler defaults to town-square (universal spawn)
// when no spaceId is given — see ws.go handleJoin. Portal destination
// resolution by world id happens in ws.go handlePortal (target world must be
// published). Everything here is read-only or write-once (create/publish)
// and appends audit rows via Store.Emit for S9.

// sessionFromRequest resolves the session cookie only (mirrors resolveAuth).
// A deviceKey is NOT accepted from the URL query: query strings leak through
// proxy logs, Referer headers and browser history, and the old fallback also
// wrote a session row on every read. Guests authenticate via POST
// /api/auth/guest (sets the httpOnly cookie) or the WS join payload.
// Returns nil when unauthenticated.
func (h *Hub) sessionFromRequest(r *http.Request) *Session {
	if ck, err := r.Cookie(cookieName); err == nil && ck.Value != "" {
		if s, err := h.store.GetSession(ck.Value); err == nil && s != nil {
			return s
		}
	}
	return nil
}

// handleWorlds: POST create draft; GET directory.
func (h *Hub) handleWorlds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.createWorld(w, r)
	case http.MethodGet:
		h.listWorlds(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleWorldRoute: /api/worlds/{id}, /api/worlds/{id}/publish,
// /api/worlds/{id}/activity (read-only audit/activity feed for a world).
func (h *Hub) handleWorldRoute(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/worlds/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(id, "/publish") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.publishWorld(w, r, strings.TrimSuffix(id, "/publish"))
		return
	}
	if strings.HasSuffix(id, "/activity") {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.worldActivity(w, r, strings.TrimSuffix(id, "/activity"))
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.getWorld(w, r, id)
}

// worldActivity: GET /api/worlds/{id}/activity?limit=N — recent append-only
// activity/audit rows for a world (desc ts). Read-only; S9 admin builds the
// full audit console on Store.Emit + this feed.
func (h *Hub) worldActivity(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.store.worldMeta(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	events, err := h.store.RecentActivity(id, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "worldId": id, "events": events})
}

// parseInt was a hand-rolled digit parser, replaced by strconv.Atoi (which
// also catches overflow). Deleted — all call sites use strconv directly.

// createWorld: POST /api/worlds — blank canvas draft owned by the session user.
// <60s "My world" flow: create -> (S3 editor paints) -> publish.
func (h *Hub) createWorld(w http.ResponseWriter, r *http.Request) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	var body struct {
		Name   string `json:"name"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10) // 16KB: name + dimensions only
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	world, err := h.store.CreateSpace(body.Name, body.Width, body.Height)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	if err := h.store.setOwner(world.ID, sess.UserID); err != nil {
		log.Printf("world create: set owner: %v", err)
	}
	// register in the live hub so the draft is immediately joinable/editable
	h.mu.Lock()
	h.spaces[world.ID] = NewSpaceState(world)
	h.mu.Unlock()

	h.emitActivity(world.ID, sess.UserID, "member", "world", "create", world.ID,
		diffJSON(map[string]any{"name": world.Name, "width": world.Width, "height": world.Height}),
		sanitizeIP(r.RemoteAddr))

	meta, _ := h.store.worldMeta(world.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "id": world.ID, "name": world.Name,
		"width": world.Width, "height": world.Height,
		"is_published": meta.IsPublished, "is_showcase": meta.IsShowcase,
		"owner": map[string]any{"id": sess.UserID, "name": h.store.userDisplay(sess.UserID)},
	})
}

// publishWorld: POST /api/worlds/{id}/publish — draft -> published.
// Owner-only. Ownerless worlds may only be published by the system (seeds):
// an ownerless, non-showcase world is not publishable by any client, so a
// stolen deviceKey or a stale row can never surface a world into the public
// directory. Emits an audit row for S9.
func (h *Hub) publishWorld(w http.ResponseWriter, r *http.Request, id string) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	meta, err := h.store.worldMeta(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	if meta.OwnerID != "" && meta.OwnerID != sess.UserID {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "only the owner can publish this world"})
		return
	}
	if meta.OwnerID == "" && !meta.IsShowcase {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "only the owner can publish this world"})
		return
	}
	updated, err := h.store.PublishWorld(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.emitActivity(id, sess.UserID, "member", "world", "publish", id,
		diffJSON(map[string]any{"is_published": true, "published_at": updated.PublishedAt}),
		sanitizeIP(r.RemoteAddr))
	// a publish is a contribution — refresh gravity so the directory moves
	_ = h.store.RecomputeGravity()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "name": updated.Name,
		"is_published": true, "is_showcase": updated.IsShowcase,
		"published_at": updated.PublishedAt,
		"owner": map[string]any{"id": updated.OwnerID, "name": h.store.userDisplay(updated.OwnerID)},
	})
}

// getWorld: GET /api/worlds/{id} — world meta + flags + gravity + headcount.
// Drafts are visible only to their owner (directory only shows published).
func (h *Hub) getWorld(w http.ResponseWriter, r *http.Request, id string) {
	meta, err := h.store.worldMeta(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	if !meta.IsPublished {
		sess := h.sessionFromRequest(r)
		if sess == nil || meta.OwnerID != sess.UserID {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
			return
		}
	}
	score := h.store.GravityScoreFor(id)
	headcount := 0
	if sp := h.space(id); sp != nil {
		for _, e := range sp.EntitySnaps() {
			if !e.Bot {
				headcount++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "world": map[string]any{
			"id": meta.ID, "name": meta.Name,
			"is_showcase": meta.IsShowcase, "is_published": meta.IsPublished,
			"published_at": meta.PublishedAt, "created_at": meta.CreatedAt,
			"owner":  map[string]any{"id": meta.OwnerID, "name": h.store.userDisplay(meta.OwnerID)},
			"gravity": map[string]any{
				"love": score.Love, "reach": score.Reach,
				"momentum": score.Momentum, "gravity": score.Gravity,
			},
			"headcount": headcount,
		},
	})
}

// listWorlds: GET /api/worlds — published worlds only, gravity desc then
// recency desc (then id asc for full determinism). ?q= filters by name.
func (h *Hub) listWorlds(w http.ResponseWriter, r *http.Request) {
	// keep directory ranking fresh (cron also runs nightly)
	h.store.ensureGravityFresh()

	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out, err := h.directory(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "worlds": out})
}

// directory returns the published-worlds directory (gravity desc, recency
// desc, id asc; optional q name filter). Shared by the REST endpoint
// (listWorlds) and the MCP worlds.list tool (server/mcp_adapter.go).
func (h *Hub) directory(q string) ([]map[string]any, error) {
	rows, err := h.store.db.Query(`SELECT id FROM spaces WHERE is_published = 1`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	type dirEntry struct {
		id      string
		name    string
		meta    WorldMeta
		score   GravityScore
		recency string // published_at (fallback created_at) for tie-break
	}
	entries := make([]dirEntry, 0, len(ids))
	for _, id := range ids {
		meta, err := h.store.worldMeta(id)
		if err != nil {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(meta.Name), q) {
			continue
		}
		rec := meta.PublishedAt
		if rec == "" {
			rec = meta.CreatedAt
		}
		// fetch meta + score exactly once per world and reuse for both the
		// sort and the output loop (was 4 queries per world).
		entries = append(entries, dirEntry{id: id, name: meta.Name, meta: meta, score: h.store.GravityScoreFor(id), recency: rec})
	}
	// gravity desc, then recency desc, then id asc (deterministic)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score.Gravity != entries[j].score.Gravity {
			return entries[i].score.Gravity > entries[j].score.Gravity
		}
		if entries[i].recency != entries[j].recency {
			return entries[i].recency > entries[j].recency
		}
		return entries[i].id < entries[j].id
	})

	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		meta, score := e.meta, e.score
		headcount := 0
		if sp := h.space(e.id); sp != nil {
			for _, ent := range sp.EntitySnaps() {
				if !ent.Bot {
					headcount++
				}
			}
		}
		out = append(out, map[string]any{
			"id": e.id, "name": e.name,
			"is_showcase":   meta.IsShowcase,
			"is_published":  true,
			"published_at":  meta.PublishedAt,
			"created_at":    meta.CreatedAt,
			"owner":         map[string]any{"id": meta.OwnerID, "name": h.store.userDisplay(meta.OwnerID)},
			"headcount":     headcount,
			"gravity":       map[string]any{"love": score.Love, "reach": score.Reach, "momentum": score.Momentum, "gravity": score.Gravity},
			"thumbnail":     nil, // S2 renders from HMF v1; placeholder for now
		})
	}
	return out, nil
}
