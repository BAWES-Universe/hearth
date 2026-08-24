package main

// T2 editor v2 — custom asset upload (additive envelope).
//
// Surface:
//
//	POST /api/worlds/{id}/assets   multipart upload (field "file"): image
//	                              png/jpeg/gif/webp, <= 512KB. Auth: session;
//	                              entitlement: same gate as edit ops (owner or
//	                              showcase co-editor). Stores bytes in SQLite
//	                              (world_assets) and returns the asset record.
//	GET  /api/assets/{id}         serves the stored image bytes (worlds are
//	                              public-readable; the id is an unguessable
//	                              uuid, uploads are world-scoped).
//
// Placement rides the frozen edit op stream as an additive op kind:
//
//	{t:'edit', d:{op:'asset', asset:{assetId, x, y}}}            → place
//	{t:'edit', d:{op:'asset', asset:{assetId, x, y, remove}}}    → remove
//
// It is server-arbitrated, persisted (world_asset_placements), appended to
// the op_log like every other edit, and broadcast to everyone in the space.
// PROTOCOL.md is untouched; old clients ignore unknown op kinds server-side
// and simply don't render assets client-side.

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"hearth/hmf"
)

const maxAssetBytes = 512 << 10 // 512KB

// Asset is one uploaded image in a world's asset registry.
type Asset struct {
	ID        string `json:"id"`
	SpaceID   string `json:"spaceId"`
	Name      string `json:"name"`
	Mime      string `json:"mime"`
	W         int    `json:"w"`
	H         int    `json:"h"`
	OwnerID   string `json:"ownerId"`
	CreatedAt string `json:"createdAt"`
}

// assetMime detects image mime from magic bytes (stdlib decodable formats).
func assetMime(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:4]) == "GIF8"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// decodeAssetDims reads pixel dimensions when the format is stdlib-decodable
// (png/jpeg/gif); webp returns 0,0 (the client renders at tile size anyway).
func decodeAssetDims(mime string, b []byte) (int, int) {
	if mime == "image/webp" {
		return 0, 0
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return 0, 0
	}
	return cfg.Width, cfg.Height
}

// CreateAsset stores an uploaded image in the world registry.
func (s *Store) CreateAsset(spaceID, name, mime string, w, h int, data []byte, owner string) (*Asset, error) {
	a := &Asset{
		ID: uuid.NewString(), SpaceID: spaceID, Name: name, Mime: mime,
		W: w, H: h, OwnerID: owner, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := s.db.Exec(`INSERT INTO world_assets (id, space_id, name, mime, w, h, owner_id, created_at, data)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		a.ID, a.SpaceID, a.Name, a.Mime, a.W, a.H, a.OwnerID, a.CreatedAt, data); err != nil {
		return nil, err
	}
	return a, nil
}

// AssetByID loads one asset row (without the image bytes).
func (s *Store) AssetByID(id string) (*Asset, error) {
	row := s.db.QueryRow(`SELECT id, space_id, name, mime, w, h, owner_id, created_at FROM world_assets WHERE id = ?`, id)
	a := &Asset{}
	if err := row.Scan(&a.ID, &a.SpaceID, &a.Name, &a.Mime, &a.W, &a.H, &a.OwnerID, &a.CreatedAt); err != nil {
		return nil, err
	}
	return a, nil
}

// AssetBytes loads the stored image bytes for an asset id.
func (s *Store) AssetBytes(id string) ([]byte, string, error) {
	var b []byte
	var mime string
	if err := s.db.QueryRow(`SELECT data, mime FROM world_assets WHERE id = ?`, id).Scan(&b, &mime); err != nil {
		return nil, "", err
	}
	return b, mime, nil
}

// ListAssets lists the registry of a world (newest first).
func (s *Store) ListAssets(spaceID string) ([]Asset, error) {
	rows, err := s.db.Query(`SELECT id, space_id, name, mime, w, h, owner_id, created_at
		FROM world_assets WHERE space_id = ? ORDER BY created_at DESC, id`, spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Asset
	for rows.Next() {
		a := Asset{}
		if err := rows.Scan(&a.ID, &a.SpaceID, &a.Name, &a.Mime, &a.W, &a.H, &a.OwnerID, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpsertAssetPlacement records one placed asset cell (idempotent).
func (s *Store) UpsertAssetPlacement(spaceID, assetID string, x, y int) error {
	_, err := s.db.Exec(`INSERT INTO world_asset_placements (space_id, asset_id, x, y) VALUES (?,?,?,?)
		ON CONFLICT(space_id, asset_id, x, y) DO UPDATE SET asset_id=excluded.asset_id`,
		spaceID, assetID, x, y)
	return err
}

// DeleteAssetPlacement removes one placed asset cell.
func (s *Store) DeleteAssetPlacement(spaceID, assetID string, x, y int) error {
	_, err := s.db.Exec(`DELETE FROM world_asset_placements WHERE space_id = ? AND asset_id = ? AND x = ? AND y = ?`,
		spaceID, assetID, x, y)
	return err
}

// saveAssetPlacements rewrites the placement rows of a world (delete-all +
// insert — same pattern as zones/portals).
func (s *Store) saveAssetPlacements(spaceID string, assets []WorldAssetPlacement) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM world_asset_placements WHERE space_id = ?`, spaceID); err != nil {
		return err
	}
	for _, a := range assets {
		if _, err := tx.Exec(`INSERT INTO world_asset_placements (space_id, asset_id, x, y) VALUES (?,?,?,?)`,
			spaceID, a.AssetID, a.X, a.Y); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loadAssetPlacements loads a world's placed assets, denormalizing name/url
// from the registry (placements store only asset_id + cell).
func (s *Store) loadAssetPlacements(spaceID string) ([]WorldAssetPlacement, error) {
	rows, err := s.db.Query(`SELECT p.asset_id, p.x, p.y, a.name
		FROM world_asset_placements p JOIN world_assets a ON a.id = p.asset_id
		WHERE p.space_id = ? ORDER BY p.y, p.x`, spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []WorldAssetPlacement{}
	for rows.Next() {
		p := WorldAssetPlacement{}
		if err := rows.Scan(&p.AssetID, &p.X, &p.Y, &p.Name); err != nil {
			return nil, err
		}
		p.URL = assetURL(p.AssetID)
		out = append(out, p)
	}
	return out, rows.Err()
}

func assetURL(id string) string { return "/api/assets/" + id }

// handleWorldAssets: POST /api/worlds/{id}/assets — multipart upload;
// GET — the world's asset registry (for the editor palette).
func (h *Hub) handleWorldAssets(w http.ResponseWriter, r *http.Request, id string) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	sp := h.space(id)
	if sp == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	// Same entitlement gate as edit ops: owner, or any authenticated
	// session on a showcase world. Ownerless non-showcase worlds reject.
	if !h.canEditSession(sess, sp.World) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "only the owner of this world can add assets"})
		return
	}
	if r.Method == http.MethodGet {
		list, err := h.store.ListAssets(sp.World.ID)
		if err != nil {
			log.Printf("asset list %s: %v", sp.World.ID, err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, a := range list {
			out = append(out, map[string]any{
				"id": a.ID, "name": a.Name, "mime": a.Mime,
				"w": a.W, "h": a.H, "url": assetURL(a.ID),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "assets": out})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAssetBytes+64<<10)
	if err := r.ParseMultipartForm(maxAssetBytes + 64<<10); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "expected multipart form"})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "missing file field"})
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxAssetBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "read failed"})
		return
	}
	if len(data) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "empty file"})
		return
	}
	if len(data) > maxAssetBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "image too large (max 512KB)"})
		return
	}
	mime := assetMime(data)
	if mime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported image type (png/jpeg/gif/webp)"})
		return
	}
	wid, hgt := decodeAssetDims(mime, data)
	name := r.FormValue("name")
	if name == "" {
		name = "asset"
	}
	asset, err := h.store.CreateAsset(sp.World.ID, name, mime, wid, hgt, data, sess.UserID)
	if err != nil {
		log.Printf("asset create %s: %v", sp.World.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.emitActivity(sp.World.ID, sess.UserID, "member", "asset", "upload", asset.ID,
		diffJSON(map[string]any{"name": asset.Name, "mime": asset.Mime, "w": asset.W, "h": asset.H}),
		sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "asset": map[string]any{
			"id": asset.ID, "name": asset.Name, "mime": asset.Mime,
			"w": asset.W, "h": asset.H, "url": assetURL(asset.ID),
		},
	})
}

// handleAssetGet: GET /api/assets/{id} — serves stored image bytes.
func (h *Hub) handleAssetGet(w http.ResponseWriter, r *http.Request) {
	id := stringsTrimPrefixPath(r.URL.Path, "/api/assets/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	b, mime, err := h.store.AssetBytes(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}

func stringsTrimPrefixPath(s, prefix string) string {
	out := s
	for len(out) >= len(prefix) && out[:len(prefix)] == prefix {
		out = out[len(prefix):]
	}
	return out
}

// canEditSession is the session-level half of canEditSpace (shareable with
// REST handlers that have a Session but no Client entity).
func (h *Hub) canEditSession(sess *Session, w *World) bool {
	if w.IsShowcase {
		return sess != nil
	}
	meta, err := h.store.worldMeta(w.ID)
	if err != nil {
		return false
	}
	if meta.OwnerID == "" {
		return false
	}
	return meta.OwnerID == sess.UserID
}

// applyAssetOp places or removes a user asset (RAM + placement table). The
// asset must exist in THIS world's registry (never cross-world).
func (h *Hub) applyAssetOp(w *World, op *hmf.Op) error {
	a := op.Asset
	if a == nil || a.AssetID == "" {
		return errors.New("edit_rejected: asset op requires assetId")
	}
	if a.X < 0 || a.Y < 0 || a.X >= w.Width || a.Y >= w.Height {
		return errors.New("edit_rejected: asset outside map bounds")
	}
	reg, err := h.store.AssetByID(a.AssetID)
	if err != nil || reg == nil || reg.SpaceID != w.ID {
		return errors.New("edit_rejected: unknown asset")
	}
	if a.Remove {
		w.mu.Lock()
		for i := range w.Assets {
			if w.Assets[i].AssetID == a.AssetID && w.Assets[i].X == a.X && w.Assets[i].Y == a.Y {
				w.Assets = append(w.Assets[:i], w.Assets[i+1:]...)
				break
			}
		}
		w.mu.Unlock()
		return h.store.DeleteAssetPlacement(w.ID, a.AssetID, a.X, a.Y)
	}
	w.mu.Lock()
	found := false
	for i := range w.Assets {
		if w.Assets[i].AssetID == a.AssetID && w.Assets[i].X == a.X && w.Assets[i].Y == a.Y {
			found = true
			break
		}
	}
	if !found {
		w.Assets = append(w.Assets, WorldAssetPlacement{
			AssetID: a.AssetID, Name: reg.Name, URL: assetURL(a.AssetID), X: a.X, Y: a.Y,
		})
	}
	w.mu.Unlock()
	return h.store.UpsertAssetPlacement(w.ID, a.AssetID, a.X, a.Y)
}
