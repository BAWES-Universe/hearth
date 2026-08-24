package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// World ownership + edit ACL (world-ownership stream).
//
//   - spaces.owner_id      = the creator's account (column already migrated,
//     see gravity.go) — set at create time from the session user.
//   - world_editors        = invited collaborators (edit role).
//   - world_invites        = single-use tokens that grant the editor role.
//
// Guests (no session) are view-only; owners and editors may edit. The WS
// edit gate (chat.go canEditWorld) and the REST world routes share these
// store helpers.

const worldEditorsDDL = `CREATE TABLE IF NOT EXISTS world_editors (
	world_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	granted_at TEXT NOT NULL,
	PRIMARY KEY (world_id, user_id)
)`

const worldInvitesDDL = `CREATE TABLE IF NOT EXISTS world_invites (
	token TEXT PRIMARY KEY,
	world_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	uses_left INTEGER NOT NULL DEFAULT 1
)`

// GrantEditor adds a user to a world's editor ACL (idempotent).
func (s *Store) GrantEditor(worldID, userID string) error {
	_, err := s.db.Exec(`INSERT INTO world_editors (world_id, user_id, granted_at) VALUES (?,?,?)
		ON CONFLICT(world_id, user_id) DO NOTHING`,
		worldID, userID, time.Now().UTC().Format(time.RFC3339))
	return err
}

// IsEditor reports whether userID holds the editor role on worldID.
func (s *Store) IsEditor(worldID, userID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM world_editors WHERE world_id = ? AND user_id = ?`,
		worldID, userID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListEditorIDs returns the editor ACL user ids of a world (oldest grant
// first — stable for the editors roster shown to the owner).
func (s *Store) ListEditorIDs(worldID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT user_id FROM world_editors WHERE world_id = ? ORDER BY granted_at`, worldID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CreateInvite mints a single-use invite token for a world.
func (s *Store) CreateInvite(worldID, createdBy string) (string, error) {
	token := randHex(16)
	_, err := s.db.Exec(`INSERT INTO world_invites (token, world_id, created_by, created_at, uses_left) VALUES (?,?,?,?,1)`,
		token, worldID, createdBy, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeInvite redeems one invite token and returns the target world id.
// Single-use tokens are deleted on redemption (a second redemption with the
// same token fails). Unknown/used tokens return an error.
func (s *Store) ConsumeInvite(token string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var worldID string
	var uses int
	err = tx.QueryRow(`SELECT world_id, uses_left FROM world_invites WHERE token = ?`, token).
		Scan(&worldID, &uses)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("invite not found or already used")
	}
	if err != nil {
		return "", err
	}
	if uses <= 1 {
		if _, err := tx.Exec(`DELETE FROM world_invites WHERE token = ?`, token); err != nil {
			return "", err
		}
	} else {
		if _, err := tx.Exec(`UPDATE world_invites SET uses_left = uses_left - 1 WHERE token = ?`, token); err != nil {
			return "", err
		}
	}
	return worldID, tx.Commit()
}

// ListMyWorlds returns the worlds a user owns or edits, newest first, each
// with its role ("owner" | "editor"). Feeds /api/me and /api/worlds/mine.
func (s *Store) ListMyWorlds(userID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT id FROM spaces WHERE owner_id = ? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	var owned []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		owned = append(owned, id)
	}
	rows.Close()

	type pair struct{ id, role string }
	out := make([]pair, 0, len(owned)+2)
	seen := map[string]bool{}
	for _, id := range owned {
		seen[id] = true
		out = append(out, pair{id, "owner"})
	}
	erows, err := s.db.Query(`SELECT world_id FROM world_editors WHERE user_id = ? ORDER BY granted_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	for erows.Next() {
		var id string
		if err := erows.Scan(&id); err != nil {
			erows.Close()
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, pair{id, "editor"})
	}
	if err := erows.Err(); err != nil {
		erows.Close()
		return nil, err
	}
	erows.Close()

	res := make([]map[string]any, 0, len(out))
	for _, p := range out {
		meta, err := s.worldMeta(p.id)
		if err != nil {
			continue
		}
		res = append(res, map[string]any{
			"id": meta.ID, "name": meta.Name,
			"role": p.role, "is_showcase": meta.IsShowcase,
			"is_published": meta.IsPublished, "created_at": meta.CreatedAt,
		})
	}
	return res, nil
}

// --- functional objects (door/npc/sign/light) persistence ---

// UpsertObject persists one placed functional object as a world_entities row
// (kind + position + human fields in the data JSON).
func (s *Store) UpsertObject(spaceID string, o WorldObject) error {
	data, err := json.Marshal(map[string]any{"name": o.Name, "text": o.Text, "data": o.Data})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO world_entities (space_id, id, kind, x, y, data) VALUES (?,?,?,?,?,?)
		ON CONFLICT(space_id, id) DO UPDATE SET kind=excluded.kind, x=excluded.x, y=excluded.y, data=excluded.data`,
		spaceID, o.ID, o.Kind, o.X, o.Y, string(data))
	return err
}

// DeleteObject removes a placed object row (object removal op).
func (s *Store) DeleteObject(spaceID, id string) error {
	_, err := s.db.Exec(`DELETE FROM world_entities WHERE space_id = ? AND id = ?`, spaceID, id)
	return err
}

// loadObjects hydrates a world's Objects from world_entities rows.
func (s *Store) loadObjects(spaceID string, w *World) error {
	rows, err := s.db.Query(`SELECT id, kind, x, y, data FROM world_entities WHERE space_id = ? ORDER BY id`, spaceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	w.Objects = nil
	for rows.Next() {
		var o WorldObject
		var dataB []byte
		if err := rows.Scan(&o.ID, &o.Kind, &o.X, &o.Y, &dataB); err != nil {
			return err
		}
		var extra struct {
			Name string         `json:"name"`
			Text string         `json:"text"`
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(dataB, &extra)
		o.Name, o.Text, o.Data = extra.Name, extra.Text, extra.Data
		w.Objects = append(w.Objects, o)
	}
	return rows.Err()
}

// saveObjects rewrites a world's object rows (full-save path: seeds,
// backfill, template re-save).
func (s *Store) saveObjects(spaceID string, objs []WorldObject) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM world_entities WHERE space_id = ?`, spaceID); err != nil {
		return err
	}
	for _, o := range objs {
		data, err := json.Marshal(map[string]any{"name": o.Name, "text": o.Text, "data": o.Data})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO world_entities (space_id, id, kind, x, y, data) VALUES (?,?,?,?,?,?)`,
			spaceID, o.ID, o.Kind, o.X, o.Y, string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
