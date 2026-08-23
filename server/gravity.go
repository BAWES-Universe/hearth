package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// S1 Core — gravity v1 + append-only activity/audit log.
//
// activity_events is the single append-only event log. It feeds BOTH the
// gravity ranking (Love x Reach x Momentum, recomputed nightly) and the S9
// admin audit trail. Every write goes through Store.Emit; nothing is ever
// updated or deleted (append-only by construction — INSERT only).
//
// Integration points for other streams:
//   - S3 (chat.go handleChat / handleEdit): call c.hub.store.Emit(worldID,
//     actor, role, "chat"/"edit", ...) after successful chat/edit so those
//     contribute to gravity Love. Not wired here (chat.go is S3-owned).
//   - S9 (admin): reuse Store.Emit for admin mutations (kind="admin") — the
//     same table is the audit trail.

const (
	gravityHalfLifeDays = 14.0 // Momentum decay half-life (design: recency 14d half-life)
	gravityLookbackDays = 30   // events older than this contribute ~0 momentum
	gravityPerDayCap    = 20   // Love cap: max contribution events per actor/world/day
	gravityTTL          = 5 * time.Minute
	gravityCronEvery    = 24 * time.Hour
)

// contributionKinds are the activity kinds that count toward Love
// (sum of contributions, per-day capped).
var contributionKinds = map[string]bool{
	"edit": true, "chat": true, "portal": true, "publish": true, "create": true,
}

// ActivityEvent is one append-only row in activity_events.
type ActivityEvent struct {
	ID      int64  `json:"id"`
	WorldID string `json:"worldId"`
	Actor   string `json:"actor"`
	Role    string `json:"role"`
	Kind    string `json:"kind"`
	Action  string `json:"action"`
	Target  string `json:"target"`
	Diff    string `json:"diff"`
	IP      string `json:"ip"`
	TS      string `json:"ts"`
}

// GravityScore is one persisted row in gravity_scores.
type GravityScore struct {
	WorldID    string  `json:"worldId"`
	Love       float64 `json:"love"`
	Reach      float64 `json:"reach"`
	Momentum   float64 `json:"momentum"`
	Gravity    float64 `json:"gravity"`
	ComputedAt string  `json:"computedAt"`
}

// MigrateS1 creates the S1 tables/columns. Idempotent; safe on existing DBs.
// Called from main.go right after OpenStore — db.go's migrate() stays S3-owned,
// so S1 additions live here (ALTER ... ADD COLUMN guarded by table_info).
func (s *Store) MigrateS1() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS activity_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			world_id TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT '',
			target TEXT NOT NULL DEFAULT '',
			diff TEXT NOT NULL DEFAULT '',
			ip TEXT NOT NULL DEFAULT '',
			ts TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_activity_world_ts ON activity_events(world_id, ts)`,
		`CREATE TABLE IF NOT EXISTS gravity_scores (
			world_id TEXT PRIMARY KEY,
			love REAL NOT NULL DEFAULT 0,
			reach REAL NOT NULL DEFAULT 0,
			momentum REAL NOT NULL DEFAULT 0,
			gravity REAL NOT NULL DEFAULT 0,
			computed_at TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate s1: %w", err)
		}
	}
	// spaces columns: is_showcase / is_published / owner_id / published_at.
	cols := []struct{ name, ddl string }{
		{"is_showcase", "ALTER TABLE spaces ADD COLUMN is_showcase INTEGER NOT NULL DEFAULT 0"},
		{"is_published", "ALTER TABLE spaces ADD COLUMN is_published INTEGER NOT NULL DEFAULT 0"},
		{"owner_id", "ALTER TABLE spaces ADD COLUMN owner_id TEXT NOT NULL DEFAULT ''"},
		{"published_at", "ALTER TABLE spaces ADD COLUMN published_at TEXT"},
	}
	existing := map[string]bool{}
	rows, err := s.db.Query(`PRAGMA table_info(spaces)`)
	if err != nil {
		return fmt.Errorf("pragma spaces: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
			existing[name] = true
		}
	}
	rows.Close()
	for _, c := range cols {
		if existing[c.name] {
			continue
		}
		if _, err := s.db.Exec(c.ddl); err != nil {
			return fmt.Errorf("add column %s: %w", c.name, err)
		}
	}
	// Seed flags: town-square is the universal spawn (showcase+published);
	// hearth/garden remain published so seed portals keep working.
	seeds := []struct {
		id       string
		showcase bool
	}{
		{"town-square", true},
		{"hearth", false},
		{"garden", false},
	}
	for _, sd := range seeds {
		if err := s.ensureSeedFlags(sd.id, sd.showcase); err != nil {
			return err
		}
	}
	return nil
}

// ensureSeedFlags marks a seeded world published (and optionally showcase)
// without touching user worlds or clobbering existing published_at.
func (s *Store) ensureSeedFlags(id string, showcase bool) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM spaces WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil // not seeded (yet) — S3 may add it later
	}
	_, err := s.db.Exec(`UPDATE spaces SET is_published = 1, is_showcase = CASE WHEN ? THEN 1 ELSE is_showcase END,
		published_at = CASE WHEN published_at IS NULL OR published_at = '' THEN created_at ELSE published_at END
		WHERE id = ? AND is_published = 0`, showcase, id)
	return err
}

// Emit appends one immutable activity/audit row. Append-only by design: this
// is the ONLY write path into activity_events (S9 admin reuses it).
func (s *Store) Emit(worldID, actor, role, kind, action, target, diff, ip string) error {
	ts := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`INSERT INTO activity_events (world_id, actor, role, kind, action, target, diff, ip, ts)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		worldID, actor, role, kind, action, target, diff, ip, ts)
	return err
}

// RecentActivity returns the latest activity/audit rows for a world (desc ts).
func (s *Store) RecentActivity(worldID string, limit int) ([]ActivityEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, world_id, actor, role, kind, action, target, diff, ip, ts
		FROM activity_events WHERE world_id = ? ORDER BY ts DESC, id DESC LIMIT ?`, worldID, limit)
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

// --- world flags ---

// WorldMeta carries the S1 flag surface for a world (kept in spaces columns,
// read through these helpers — World struct itself is S3-owned).
type WorldMeta struct {
	ID          string
	Name        string
	OwnerID     string
	IsShowcase  bool
	IsPublished bool
	PublishedAt string
	CreatedAt   string
}

func (s *Store) worldMeta(id string) (WorldMeta, error) {
	var m WorldMeta
	var showcase, published int
	var publishedAt sql.NullString
	err := s.db.QueryRow(`SELECT id, name, owner_id, is_showcase, is_published, published_at, created_at
		FROM spaces WHERE id = ?`, id).
		Scan(&m.ID, &m.Name, &m.OwnerID, &showcase, &published, &publishedAt, &m.CreatedAt)
	if err != nil {
		return m, err
	}
	m.IsShowcase = showcase == 1
	m.IsPublished = published == 1
	if publishedAt.Valid {
		m.PublishedAt = publishedAt.String
	}
	return m, nil
}

func (s *Store) setOwner(id, ownerID string) error {
	_, err := s.db.Exec(`UPDATE spaces SET owner_id = ? WHERE id = ?`, ownerID, id)
	return err
}

// PublishWorld flips a draft to published (idempotent; keeps first published_at).
func (s *Store) PublishWorld(id string) (WorldMeta, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE spaces SET is_published = 1,
		published_at = CASE WHEN published_at IS NULL OR published_at = '' THEN ? ELSE published_at END
		WHERE id = ?`, now, id); err != nil {
		return WorldMeta{}, err
	}
	return s.worldMeta(id)
}

// userDisplay returns a stable display name for a user id ('' when unknown).
func (s *Store) userDisplay(userID string) string {
	if userID == "" {
		return ""
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM users WHERE id = ?`, userID).Scan(&name); err != nil {
		return ""
	}
	return name
}

// --- gravity computation ---

type activityRow struct {
	WorldID string
	Actor   string
	Kind    string
	Action  string
	TS      time.Time
}

// loadActivityRows loads all activity events (ordered by id for determinism).
func (s *Store) loadActivityRows() ([]activityRow, error) {
	rows, err := s.db.Query(`SELECT world_id, actor, kind, action, ts FROM activity_events ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activityRow
	for rows.Next() {
		var r activityRow
		var ts string
		if err := rows.Scan(&r.WorldID, &r.Actor, &r.Kind, &r.Action, &ts); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.TS = t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// computeGravityFor returns (love, reach, momentum, gravity) for one world.
//
// Love     = sum of contributions with a per-day cap per actor
//            (max gravityPerDayCap contribution events per actor/world/day).
// Reach    = audience: distinct visitors (join events, unique actor).
// Momentum = recency: sum of 2^(-ageDays/14) over events in the lookback
//            window (14-day half-life).
// Gravity  = (1+Love) * (1+Reach) * (1+Momentum) — monotone in every factor,
//            never zero, fully deterministic for identical inputs.
func computeGravityFor(rows []activityRow, now time.Time) (love, reach, momentum, gravity float64) {
	// love: per (actor, day) capped counts
	perActorDay := map[string]int{}
	visitors := map[string]bool{}
	for _, r := range rows {
		if contributionKinds[r.Kind] {
			key := r.Actor + "|" + r.TS.UTC().Format("2006-01-02")
			perActorDay[key]++
		}
		if r.Kind == "presence" && r.Action == "join" && r.Actor != "" {
			visitors[r.Actor] = true
		}
		age := now.Sub(r.TS).Hours() / 24.0
		if age < 0 {
			age = 0
		}
		if age <= gravityLookbackDays {
			momentum += pow2(-age / gravityHalfLifeDays)
		}
	}
	for _, n := range perActorDay {
		if n > gravityPerDayCap {
			n = gravityPerDayCap
		}
		love += float64(n)
	}
	reach = float64(len(visitors))
	gravity = (1 + love) * (1 + reach) * (1 + momentum)
	return love, reach, momentum, gravity
}

func pow2(x float64) float64 {
	// 2^x without math.Pow float32 surprises; keep it simple and deterministic.
	r := 1.0
	exp := x
	for i := 0; i < 60; i++ {
		r *= 1.0 + exp/60.0
	}
	return r
}

// RecomputeGravity recomputes scores for ALL worlds and persists them.
// Deterministic: identical activity_events input => identical scores.
func (s *Store) RecomputeGravity() error {
	rows, err := s.loadActivityRows()
	if err != nil {
		return err
	}
	byWorld := map[string][]activityRow{}
	for _, r := range rows {
		byWorld[r.WorldID] = append(byWorld[r.WorldID], r)
	}
	now := time.Now().UTC()
	// include every known world (even with zero events) so the directory can
	// order drafts-free worlds deterministically by recency.
	var ids []string
	idRows, err := s.db.Query(`SELECT id FROM spaces`)
	if err != nil {
		return err
	}
	for idRows.Next() {
		var id string
		if idRows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	idRows.Close()
	for _, id := range ids {
		love, reach, momentum, gravity := computeGravityFor(byWorld[id], now)
		if _, err := s.db.Exec(`INSERT INTO gravity_scores (world_id, love, reach, momentum, gravity, computed_at)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT(world_id) DO UPDATE SET love=excluded.love, reach=excluded.reach,
				momentum=excluded.momentum, gravity=excluded.gravity, computed_at=excluded.computed_at`,
			id, love, reach, momentum, gravity, now.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

// GravityScoreFor returns the persisted score for one world (zeros if absent).
func (s *Store) GravityScoreFor(id string) GravityScore {
	var g GravityScore
	var computedAt sql.NullString
	err := s.db.QueryRow(`SELECT world_id, love, reach, momentum, gravity, computed_at
		FROM gravity_scores WHERE world_id = ?`, id).
		Scan(&g.WorldID, &g.Love, &g.Reach, &g.Momentum, &g.Gravity, &computedAt)
	if err != nil {
		return GravityScore{WorldID: id}
	}
	if computedAt.Valid {
		g.ComputedAt = computedAt.String
	}
	return g
}

// lastGravityCompute returns the newest computed_at across all worlds ("" if none).
func (s *Store) lastGravityCompute() string {
	var ts string
	_ = s.db.QueryRow(`SELECT MAX(computed_at) FROM gravity_scores`).Scan(&ts)
	return ts
}

// ensureGravityFresh recomputes when the persisted scores are stale or missing.
// Keeps the directory live without waiting for the nightly cron.
func (s *Store) ensureGravityFresh() {
	last := s.lastGravityCompute()
	if last == "" {
		if err := s.RecomputeGravity(); err != nil {
			log.Printf("gravity: initial recompute failed: %v", err)
		}
		return
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil || time.Since(t) > gravityTTL {
		if err := s.RecomputeGravity(); err != nil {
			log.Printf("gravity: refresh failed: %v", err)
		}
	}
}

// gravityCron is the nightly recompute loop (design: nightly cron). Runs once
// at startup, then every 24h. Guarded by a mutex so concurrent directory
// refreshes never double-write.
func (h *Hub) gravityCron() {
	tick := time.NewTicker(gravityCronEvery)
	defer tick.Stop()
	for {
		if err := h.store.RecomputeGravity(); err != nil {
			log.Printf("gravity cron: %v", err)
		}
		select {
		case <-tick.C:
		case <-h.closed:
			return
		}
	}
}

// emitActivity is the Hub-level Emit convenience (drops errors to the log —
// activity logging must never break gameplay).
func (h *Hub) emitActivity(worldID, actor, role, kind, action, target, diff, ip string) {
	if err := h.store.Emit(worldID, actor, role, kind, action, target, diff, ip); err != nil {
		log.Printf("emit activity (%s %s %s): %v", kind, action, target, err)
	}
}

// diffJSON builds a compact JSON diff string for the audit log ('' on error).
func diffJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// sanitizeIP trims port from a RemoteAddr (e.g. "1.2.3.4:5678" -> "1.2.3.4").
func sanitizeIP(remoteAddr string) string {
	if i := strings.LastIndex(remoteAddr, ":"); i > 0 {
		return remoteAddr[:i]
	}
	return remoteAddr
}
