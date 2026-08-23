package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// S9 Admin v1 — admin REST skeleton + embedded /admin Preact console +
// append-only audit + API-key auth.
//
// API surface (all JSON, API-key protected via X-API-Key):
//
//	GET    /api/admin/overview          counts for the console Overview cards
//	GET    /api/admin/worlds            all worlds (incl. drafts) w/ flags+gravity
//	GET    /api/admin/worlds/{id}       one world (admin sees drafts)
//	POST   /api/admin/worlds            create a world (operator) — audited
//	POST   /api/admin/worlds/{id}/publish   publish a world — audited
//	DELETE /api/admin/worlds/{id}       delete a world — audited
//	GET    /api/admin/members           users + operator role + online flag
//	GET    /api/admin/audit             append-only audit log (all kinds)
//	GET    /api/admin/tokens            list service tokens (no secrets)
//	POST   /api/admin/tokens            create a scoped service token — audited
//	DELETE /api/admin/tokens/{id}       revoke a service token — audited
//
// Auth model (design §S9):
//   - The operator API key (env ADMIN_API_KEY) grants the full operator role;
//     compared in constant time (crypto/subtle) against X-API-Key.
//   - Service tokens (service_tokens table, sha256-hashed at rest) carry
//     scoped capabilities (worlds.read/worlds.write/members.read/audit.read/
//     tokens.manage; bot.spawn + mcp.manage_own reserved for future bot/MCP
//     endpoints — stub vocabulary) and are validated by the same middleware.
//   - CORS_ALLOWED_ORIGINS (comma-separated) is respected for cross-origin
//     callers (recursive bots, external tooling); same-origin is unaffected.
//   - HEARTH_BOOTSTRAP_OPERATOR_KEY seeds the first operator on first boot
//     (no operators exist yet) so the console has an audit identity.
//
// Every admin mutation appends an immutable row via Store.Emit (kind="admin")
// — the same append-only activity_events table that feeds gravity (S1).

// --- env configuration ---

func adminAPIKey() string { return os.Getenv("ADMIN_API_KEY") }

func bootstrapOperatorKey() string { return os.Getenv("HEARTH_BOOTSTRAP_OPERATOR_KEY") }

// adminAllowedOrigins parses CORS_ALLOWED_ORIGINS (comma-separated origins;
// "*" allows any). nil => same-origin only.
func adminAllowedOrigins() []string {
	v := os.Getenv("CORS_ALLOWED_ORIGINS")
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// knownServiceCaps is the scoped-capability vocabulary. The admin REST
// endpoints gate on the worlds/members/audit/tokens caps; bot.spawn and
// mcp.manage_own are reserved for future recursive-bot / MCP endpoints
// (stub per T1 scope).
var knownServiceCaps = map[string]bool{
	"worlds.read": true, "worlds.write": true,
	"members.read": true, "audit.read": true, "tokens.manage": true,
	"bot.spawn": true, "mcp.manage_own": true,
}

// --- schema (S9 additions; idempotent, runs after MigrateS1) ---

// MigrateS9 creates the admin tables. Called from main.go right after
// MigrateS1 (activity_events must exist for bootstrap audit rows).
func (s *Store) MigrateS9() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS admin_operators (
			user_id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT 'operator',
			created_at TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS service_tokens (
			id TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			capabilities TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			last_used TEXT
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// BootstrapOperator creates the first operator from HEARTH_BOOTSTRAP_OPERATOR_KEY
// when the operators table is empty. No-op when the env key is unset or an
// operator already exists. Audited (append-only).
func (s *Store) BootstrapOperator() error {
	key := bootstrapOperatorKey()
	if key == "" {
		return nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_operators`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	id := "op-" + hashDeviceKey(key)
	name := "bootstrap-operator"
	now := time.Now().UTC().Format(time.RFC3339)
	// The operator is also a member (users row) so the Members list and
	// online tracking see it; the admin_operators row marks the role.
	if _, err := s.db.Exec(`INSERT INTO users (id, device_key, name, created_at, last_seen)
		VALUES (?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, id, id, name, now, now); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO admin_operators (user_id, name, role, created_at, created_by)
		VALUES (?,?,?,?,?)`, id, name, "operator", now, "system"); err != nil {
		return err
	}
	if err := s.Emit("", id, "operator", "admin", "admin.operator.bootstrap", id,
		diffJSON(map[string]any{"name": name, "via": "HEARTH_BOOTSTRAP_OPERATOR_KEY"}), ""); err != nil {
		return err
	}
	log.Printf("admin: bootstrapped operator %q (id=%s)", name, id)
	return nil
}

func (s *Store) firstOperatorName() string {
	var name string
	_ = s.db.QueryRow(`SELECT name FROM admin_operators ORDER BY created_at LIMIT 1`).Scan(&name)
	return name
}

func (s *Store) countSpaces() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM spaces`).Scan(&n)
	return n, err
}

func (s *Store) countPublished() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM spaces WHERE is_published = 1`).Scan(&n)
	return n, err
}

func (s *Store) countUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) countOperators() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_operators`).Scan(&n)
	return n, err
}

func (s *Store) countAuditEvents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM activity_events`).Scan(&n)
	return n, err
}

// --- auth principal ---

// adminPrincipal is the authenticated identity of an /api/admin/* request.
// Operators (API key) bypass capability checks; service tokens are scoped to
// their capabilities list.
type adminPrincipal struct {
	Role string // "operator" | "service"
	Name string
	ID   string
	Caps map[string]bool
}

func (p *adminPrincipal) hasCap(c string) bool {
	if p == nil {
		return false
	}
	if p.Role == "operator" {
		return true
	}
	return p.Caps[c]
}

type adminPrincipalKey struct{}

// authorizeAdmin resolves X-API-Key to a principal: the operator API key
// (constant-time compare) or a scoped service token. nil => unauthorized.
func (h *Hub) authorizeAdmin(r *http.Request) *adminPrincipal {
	key := r.Header.Get("X-API-Key")
	if key == "" {
		return nil
	}
	want := adminAPIKey()
	if want != "" && subtle.ConstantTimeCompare([]byte(key), []byte(want)) == 1 {
		return &adminPrincipal{Role: "operator", Name: h.store.firstOperatorName(), ID: "api-key"}
	}
	tok, err := h.store.serviceTokenByHash(hashToken(key))
	if err == nil && tok != nil {
		caps := map[string]bool{}
		for _, c := range tok.Capabilities {
			caps[c] = true
		}
		return &adminPrincipal{Role: "service", Name: tok.Name, ID: tok.ID, Caps: caps}
	}
	return nil
}

func adminPrincipalOf(r *http.Request) *adminPrincipal {
	p, _ := r.Context().Value(adminPrincipalKey{}).(*adminPrincipal)
	return p
}

// requireAdminCap gates a handler on a capability (used for method-specific
// caps inside collection/item routes). Writes a 403 JSON error when missing.
func (h *Hub) requireAdminCap(w http.ResponseWriter, r *http.Request, cap string) bool {
	p := adminPrincipalOf(r)
	if p == nil || !p.hasCap(cap) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "missing capability: " + cap})
		return false
	}
	return true
}

// adminAuth wraps a handler with CORS handling + API-key auth + capability
// gate. caps are required in addition to authentication (all checked after
// the principal is resolved).
func (h *Hub) adminAuth(caps ...string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !applyAdminCORS(w, r) {
				return
			}
			p := h.authorizeAdmin(r)
			if p == nil {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid or missing API key"})
				return
			}
			for _, c := range caps {
				if !p.hasCap(c) {
					writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "missing capability: " + c})
					return
				}
			}
			next(w, r.WithContext(context.WithValue(r.Context(), adminPrincipalKey{}, p)))
		}
	}
}

// applyAdminCORS honors CORS_ALLOWED_ORIGINS. Same-origin / non-browser
// requests pass through untouched. Preflight for a disallowed origin is
// rejected; simple requests for a disallowed origin proceed without the
// ACAO header (the browser blocks response reads). Returns false when the
// request has been fully handled (preflight) and must not continue.
func applyAdminCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	allowed := false
	for _, a := range adminAllowedOrigins() {
		if a == "*" || a == origin {
			allowed = true
			break
		}
	}
	if !allowed {
		if r.Method == http.MethodOptions {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "origin not allowed"})
			return false
		}
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	return true
}

// adminEmit appends one audit row for an admin mutation (kind="admin"),
// attributed to the authenticated principal.
func (h *Hub) adminEmit(r *http.Request, action, target string, diff any) {
	p := adminPrincipalOf(r)
	actor, role := "admin", "operator"
	if p != nil {
		actor, role = p.Name, p.Role
		if actor == "" {
			actor = p.ID
		}
	}
	h.emitActivity(target, actor, role, "admin", action, target, diffJSON(diff), sanitizeIP(r.RemoteAddr))
}

// --- route registration ---

// RegisterAdminRoutes wires /api/admin/* and the embedded /admin console.
// Called from main.go after the mux is created (S9 stream owns this call).
func (h *Hub) RegisterAdminRoutes(mux *http.ServeMux) {
	auth := h.adminAuth
	mux.HandleFunc("/api/admin/overview", auth()(h.adminOverview))
	mux.HandleFunc("/api/admin/worlds", auth()(h.adminWorldsCollection))
	mux.HandleFunc("/api/admin/worlds/", auth()(h.adminWorldItem))
	mux.HandleFunc("/api/admin/members", auth("members.read")(h.adminMembers))
	mux.HandleFunc("/api/admin/audit", auth("audit.read")(h.adminAudit))
	mux.HandleFunc("/api/admin/tokens", auth()(h.adminTokensCollection))
	mux.HandleFunc("/api/admin/tokens/", auth()(h.adminTokenItem))
	// embedded /admin Preact console (SPA shell; assets via the static handler)
	mux.HandleFunc("/admin", h.serveAdminConsole)
	mux.HandleFunc("/admin/", h.serveAdminConsole)
}

// serveAdminConsole serves the admin console shell (client/dist/admin.html,
// falling back to the embedded copy). Asset requests (/assets/*) are handled
// by the regular static handler.
func (h *Hub) serveAdminConsole(w http.ResponseWriter, r *http.Request) {
	clientDir := filepath.Join("..", "client", "dist")
	if b, err := os.ReadFile(filepath.Join(clientDir, "admin.html")); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
		return
	}
	if b, err := embeddedDist.ReadFile("dist/admin.html"); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
		return
	}
	http.NotFound(w, r)
}

// --- overview ---

func (h *Hub) adminOverview(w http.ResponseWriter, r *http.Request) {
	worlds, err1 := h.store.countSpaces()
	published, err2 := h.store.countPublished()
	members, err3 := h.store.countUsers()
	operators, err4 := h.store.countOperators()
	auditEvents, err5 := h.store.countAuditEvents()
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	clients, bots := h.liveCounts()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "overview": map[string]any{
		"worlds": worlds, "worldsPublished": published,
		"members": members, "operators": operators,
		"auditEvents": auditEvents, "clientsLive": clients, "botsLive": bots,
	}})
}

// liveCounts counts live player entities and ambient bots across all spaces.
func (h *Hub) liveCounts() (clients, bots int) {
	h.mu.RLock()
	spaces := make([]*SpaceState, 0, len(h.spaces))
	for _, sp := range h.spaces {
		spaces = append(spaces, sp)
	}
	h.mu.RUnlock()
	for _, sp := range spaces {
		for _, e := range sp.EntitySnaps() {
			if e.Bot {
				bots++
			} else {
				clients++
			}
		}
	}
	return clients, bots
}

// --- worlds ---

// adminWorldsCollection: GET list (worlds.read) / POST create (worlds.write).
func (h *Hub) adminWorldsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !h.requireAdminCap(w, r, "worlds.read") {
			return
		}
		h.adminWorldsList(w, r)
	case http.MethodPost:
		if !h.requireAdminCap(w, r, "worlds.write") {
			return
		}
		h.adminWorldsCreate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminWorldItem: GET {id} (worlds.read) / DELETE {id} (worlds.write) /
// POST {id}/publish (worlds.write).
func (h *Hub) adminWorldItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/worlds/")
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
		if !h.requireAdminCap(w, r, "worlds.write") {
			return
		}
		h.adminWorldPublish(w, r, strings.TrimSuffix(id, "/publish"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !h.requireAdminCap(w, r, "worlds.read") {
			return
		}
		h.adminWorldGet(w, r, id)
	case http.MethodDelete:
		if !h.requireAdminCap(w, r, "worlds.write") {
			return
		}
		h.adminWorldDelete(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminWorldsList: every world (drafts included — the console is the
// operator's view), desc by created_at, with flags + gravity + headcount.
func (h *Hub) adminWorldsList(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.db.Query(`SELECT id FROM spaces ORDER BY created_at DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		meta, err := h.store.worldMeta(id)
		if err != nil {
			continue
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
		out = append(out, map[string]any{
			"id": meta.ID, "name": meta.Name,
			"is_showcase": meta.IsShowcase, "is_published": meta.IsPublished,
			"published_at": meta.PublishedAt, "created_at": meta.CreatedAt,
			"owner":     map[string]any{"id": meta.OwnerID, "name": h.store.userDisplay(meta.OwnerID)},
			"headcount": headcount,
			"gravity": map[string]any{
				"love": score.Love, "reach": score.Reach,
				"momentum": score.Momentum, "gravity": score.Gravity,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "worlds": out})
}

func (h *Hub) adminWorldGet(w http.ResponseWriter, r *http.Request, id string) {
	meta, err := h.store.worldMeta(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "world": map[string]any{
		"id": meta.ID, "name": meta.Name,
		"is_showcase": meta.IsShowcase, "is_published": meta.IsPublished,
		"published_at": meta.PublishedAt, "created_at": meta.CreatedAt,
		"owner":     map[string]any{"id": meta.OwnerID, "name": h.store.userDisplay(meta.OwnerID)},
		"headcount": headcount,
		"gravity": map[string]any{
			"love": score.Love, "reach": score.Reach,
			"momentum": score.Momentum, "gravity": score.Gravity,
		},
	}})
}

// adminWorldsCreate: operator world creation (optionally published in one
// step). Audited.
func (h *Hub) adminWorldsCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
		Publish bool  `json:"publish"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	world, err := h.store.CreateSpace(body.Name, body.Width, body.Height)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	p := adminPrincipalOf(r)
	owner := "admin"
	if p != nil && p.ID != "" {
		owner = p.ID
	}
	if err := h.store.setOwner(world.ID, owner); err != nil {
		log.Printf("admin world create: set owner: %v", err)
	}
	h.mu.Lock()
	h.spaces[world.ID] = NewSpaceState(world)
	h.mu.Unlock()

	if body.Publish {
		if _, err := h.store.PublishWorld(world.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		_ = h.store.RecomputeGravity()
	}
	h.adminEmit(r, "admin.world.create", world.ID, map[string]any{
		"name": world.Name, "width": world.Width, "height": world.Height, "publish": body.Publish,
	})
	meta, _ := h.store.worldMeta(world.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "id": world.ID, "name": world.Name,
		"width": world.Width, "height": world.Height,
		"is_published": meta.IsPublished, "is_showcase": meta.IsShowcase,
		"owner": map[string]any{"id": meta.OwnerID, "name": h.store.userDisplay(meta.OwnerID)},
	})
}

// adminWorldPublish: operator publish of any world. Audited.
func (h *Hub) adminWorldPublish(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.store.worldMeta(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	updated, err := h.store.PublishWorld(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.adminEmit(r, "admin.world.publish", id, map[string]any{"published_at": updated.PublishedAt})
	_ = h.store.RecomputeGravity()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "name": updated.Name,
		"is_published": true, "published_at": updated.PublishedAt,
		"owner": map[string]any{"id": updated.OwnerID, "name": h.store.userDisplay(updated.OwnerID)},
	})
}

// adminWorldDelete: removes the world row (+ cascade), drops the live space,
// disconnects players inside it. Audited.
func (h *Hub) adminWorldDelete(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.store.worldMeta(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	var doomed []*Client
	if sp := h.space(id); sp != nil {
		for _, e := range sp.EntitySnaps() {
			if e.Client != nil {
				doomed = append(doomed, e.Client)
			}
		}
	}
	h.mu.Lock()
	delete(h.spaces, id)
	h.mu.Unlock()
	if err := h.store.DeleteWorld(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	for _, c := range doomed {
		h.removeClient(c)
	}
	h.adminEmit(r, "admin.world.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "deleted": true})
}

// DeleteWorld removes a world and every dependent row (append-only tables
// included — the audit trail keeps its history; world-scoped events are
// dropped with the world).
func (s *Store) DeleteWorld(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM op_log WHERE space_id = ?`,
		`DELETE FROM snapshots WHERE space_id = ?`,
		`DELETE FROM messages WHERE space_id = ?`,
		`DELETE FROM activity_events WHERE world_id = ?`,
		`DELETE FROM spaces WHERE id = ?`, // cascades maps/map_chunks/world_zones/world_portals/world_entities
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- members ---

// adminMembers: users with operator role + online flag (live hub entities).
func (h *Hub) adminMembers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.store.db.Query(`SELECT u.id, u.name, u.created_at, u.last_seen,
		CASE WHEN op.user_id IS NULL THEN 0 ELSE 1 END
		FROM users u LEFT JOIN admin_operators op ON op.user_id = u.id
		ORDER BY u.created_at`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	type memberRow struct {
		id, name, createdAt string
		lastSeen            sql.NullString
		isOperator          bool
	}
	var mems []memberRow
	for rows.Next() {
		var m memberRow
		var op int
		if err := rows.Scan(&m.id, &m.name, &m.createdAt, &m.lastSeen, &op); err != nil {
			continue
		}
		m.isOperator = op == 1
		mems = append(mems, m)
	}
	rows.Close()

	online := map[string]bool{}
	h.mu.RLock()
	spaces := make([]*SpaceState, 0, len(h.spaces))
	for _, sp := range h.spaces {
		spaces = append(spaces, sp)
	}
	h.mu.RUnlock()
	for _, sp := range spaces {
		for _, e := range sp.EntitySnaps() {
			// UserID rides on the Client's Entity (set once at join, never
			// mutated afterwards); EntitySnap itself doesn't carry it.
			if !e.Bot && e.Client != nil && e.Client.Entity != nil && e.Client.Entity.UserID != "" {
				online[e.Client.Entity.UserID] = true
			}
		}
	}

	out := make([]map[string]any, 0, len(mems))
	for _, m := range mems {
		role := "member"
		if m.isOperator {
			role = "operator"
		}
		out = append(out, map[string]any{
			"id": m.id, "name": m.name, "role": role,
			"createdAt": m.createdAt, "lastSeen": m.lastSeen.String, "online": online[m.id],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "members": out})
}

// --- audit ---

// adminAudit: GET /api/admin/audit?limit=&kind=&worldId= — the append-only
// log, newest first.
func (h *Hub) adminAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	events, err := h.store.ListAudit(r.URL.Query().Get("kind"), r.URL.Query().Get("worldId"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "events": events})
}

// ListAudit returns the newest audit/activity rows, optionally filtered by
// kind and/or world id (desc ts, then desc id for determinism).
func (s *Store) ListAudit(kind, worldID string, limit int) ([]ActivityEvent, error) {
	q := `SELECT id, world_id, actor, role, kind, action, target, diff, ip, ts FROM activity_events`
	var args []any
	var conds []string
	if kind != "" {
		conds = append(conds, `kind = ?`)
		args = append(args, kind)
	}
	if worldID != "" {
		conds = append(conds, `world_id = ?`)
		args = append(args, worldID)
	}
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityEvent
	for rows.Next() {
		var e ActivityEvent
		if err := rows.Scan(&e.ID, &e.WorldID, &e.Actor, &e.Role, &e.Kind, &e.Action, &e.Target, &e.Diff, &e.IP, &e.TS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- service tokens (scoped capabilities stub) ---

// ServiceTokenRow is the non-secret view of a service token. TokenHash is
// never serialized.
type ServiceTokenRow struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	CreatedAt    string   `json:"createdAt"`
	CreatedBy    string   `json:"createdBy"`
	LastUsed     string   `json:"lastUsed"`
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (s *Store) CreateServiceToken(name string, caps []string, createdBy string) (id, raw string, err error) {
	capsJSON, _ := json.Marshal(caps)
	id = "st-" + randHex(8)
	raw = "st_" + randHex(24)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.Exec(`INSERT INTO service_tokens (id, token_hash, name, capabilities, created_at, created_by)
		VALUES (?,?,?,?,?,?)`, id, hashToken(raw), name, string(capsJSON), now, createdBy)
	return id, raw, err
}

func (s *Store) serviceTokenByHash(h string) (*ServiceTokenRow, error) {
	var row ServiceTokenRow
	var capsB string
	var lastUsed sql.NullString
	err := s.db.QueryRow(`SELECT id, name, capabilities, created_at, created_by, last_used
		FROM service_tokens WHERE token_hash = ?`, h).
		Scan(&row.ID, &row.Name, &capsB, &row.CreatedAt, &row.CreatedBy, &lastUsed)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(capsB), &row.Capabilities); err != nil {
		row.Capabilities = nil
	}
	if lastUsed.Valid {
		row.LastUsed = lastUsed.String
	}
	return &row, nil
}

func (s *Store) ListServiceTokens() ([]ServiceTokenRow, error) {
	rows, err := s.db.Query(`SELECT id, name, capabilities, created_at, created_by, last_used
		FROM service_tokens ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceTokenRow
	for rows.Next() {
		var row ServiceTokenRow
		var capsB string
		var lastUsed sql.NullString
		if err := rows.Scan(&row.ID, &row.Name, &capsB, &row.CreatedAt, &row.CreatedBy, &lastUsed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(capsB), &row.Capabilities); err != nil {
			row.Capabilities = nil
		}
		if lastUsed.Valid {
			row.LastUsed = lastUsed.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) RevokeServiceToken(id string) error {
	_, err := s.db.Exec(`DELETE FROM service_tokens WHERE id = ?`, id)
	return err
}

func (s *Store) TouchServiceToken(id string) {
	_, _ = s.db.Exec(`UPDATE service_tokens SET last_used = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), id)
}

// adminTokensCollection: GET list / POST create (both tokens.manage).
func (h *Hub) adminTokensCollection(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminCap(w, r, "tokens.manage") {
		return
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := h.store.ListServiceTokens()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tokens": tokens})
	case http.MethodPost:
		var body struct {
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name required"})
			return
		}
		if len(body.Capabilities) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "at least one capability required"})
			return
		}
		for _, c := range body.Capabilities {
			if !knownServiceCaps[c] {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown capability: " + c})
				return
			}
		}
		p := adminPrincipalOf(r)
		createdBy := "admin"
		if p != nil && p.ID != "" {
			createdBy = p.ID
		}
		id, raw, err := h.store.CreateServiceToken(strings.TrimSpace(body.Name), body.Capabilities, createdBy)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		h.adminEmit(r, "admin.service_token.create", id, map[string]any{"name": body.Name, "capabilities": body.Capabilities})
		writeJSON(w, http.StatusCreated, map[string]any{
			"ok": true, "id": id, "name": body.Name,
			"capabilities": body.Capabilities, "token": raw, // shown once
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminTokenItem: DELETE /api/admin/tokens/{id} (tokens.manage) — revoke.
func (h *Hub) adminTokenItem(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminCap(w, r, "tokens.manage") {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/tokens/")
	id = strings.TrimSuffix(id, "/")
	if id == "" || r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.store.RevokeServiceToken(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.adminEmit(r, "admin.service_token.revoke", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "revoked": true})
}
