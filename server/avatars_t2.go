package main

// T2 avatar platform — custom asset upload, generative picker, and
// sets/scopes + entitlements with governance (docs/AVATARS.md).
//
// Everything here is ADDITIVE: the frozen PROTOCOL.md v0 envelope is
// untouched. New surface:
//
//	REST (all require the hearth_session cookie; see sessionFromRequest):
//	  POST   /api/avatars/generate          {prompt} -> {ok, model, spec}
//	  POST   /api/avatars/assets            {layer,name,kind,data(base64)}
//	  GET    /api/avatars/assets            my active assets (no image bytes)
//	  GET    /api/avatars/assets/{id}/image raw image bytes (for <img>/canvas)
//	  DELETE /api/avatars/assets/{id}       safe-archive (409 when worn)
//	  POST   /api/avatars/sets              {name,scope,worldId?,items?}
//	  GET    /api/avatars/sets              sets visible to me
//	  POST   /api/avatars/sets/{id}/items   {layer,optionId}  (bumps version)
//	  DELETE /api/avatars/sets/{id}/items/{layer}/{optionId}  (bumps version)
//	  POST   /api/avatars/sets/{id}/archive safe-archive (409 when worn)
//	  POST   /api/avatars/grants            {setId,userId,kind?,match?,expiresAt?}
//
//	WS (additive envelope):
//	  client -> server {t:'avatar_update', d:{spec:{...}}}
//	  server -> client {t:'avatar_update', d:{userId,name,spec,self?}}
//	    broadcast to the AOI of the space; the 12Hz state stream carries the
//	    new look to every viewer as usual.
//
// Governance model:
//   - Custom assets live in avatar_assets. An asset's option id is
//     "asset:<uuid>". A user may always wear their OWN active asset; wearing
//     someone else's requires the asset's set to grant them access.
//   - avatar_sets are versioned collections with a scope:
//     public | universe | world | membership | user-granted | npc-only.
//     Catalog options (T1) stay free for humans (NPC-only aside) — sets gate
//     the custom assets they contain. Every item change bumps the version.
//   - Entitlements are avatar_grants rows: kind = direct | time-limited |
//     tag | sub | email-domain | membership. Claim-checked kinds (tag,
//     email-domain) verify against user_claims. Expiry is RFC3339 ('' = never).
//   - Enforcement runs server-side per join (and on every avatar_update):
//     non-entitled options are normalized to the member's first entitled
//     option per layer. All tables are mirrored in an in-memory snapshot
//     (AvatarRegistry), so the check is pure map lookups — p95 < 50ms target.
//   - Safe-archive: deleting an asset or archiving a set is BLOCKED (409)
//     while any live entity wears one of its options; otherwise it is a
//     soft archive (status/flag) so old specs re-normalize instead of
//     breaking. Every mutation appends an immutable audit row via Store.Emit
//     (kind="avatar").
//   - Generative picker: POST /api/avatars/generate seeds a deterministic
//     sampler from the entitled layer catalog (model = "hearth-catalog-sampler
//     v1": FNV-1a(prompt,userID) -> xorshift64 -> one entitled option per
//     layer). Same prompt + same member => same candidate. No external image
//     model is called in T2 — documented in docs/AVATARS.md.

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// assetOptionPrefix marks a custom uploaded asset in an AvatarSpec layer.
const assetOptionPrefix = "asset:"

// Set scopes (avatar_sets.scope).
const (
	setScopePublic       = "public"
	setScopeUniverse     = "universe"
	setScopeWorld        = "world"
	setScopeMembership   = "membership"
	setScopeUserGranted  = "user-granted"
	setScopeNPCOnly      = "npc-only"
)

// Entitlement grant kinds (avatar_grants.kind).
const (
	grantDirect      = "direct"
	grantTimeLimited = "time-limited"
	grantTag         = "tag"
	grantSub         = "sub"
	grantEmailDomain = "email-domain"
	grantMembership  = "membership"
)

// avatar asset/upload limits.
const (
	avatarMaxAssetBytes = 512 << 10 // 512 KiB raw image
	avatarMaxDim        = 1024      // px per side
	avatarSetNameMax    = 48
)

// validAvatarScopes is the closed set of governance scopes.
var validAvatarScopes = map[string]bool{
	setScopePublic: true, setScopeUniverse: true, setScopeWorld: true,
	setScopeMembership: true, setScopeUserGranted: true, setScopeNPCOnly: true,
}

// validAvatarGrantKinds is the closed set of entitlement kinds.
var validAvatarGrantKinds = map[string]bool{
	grantDirect: true, grantTimeLimited: true, grantTag: true,
	grantSub: true, grantEmailDomain: true, grantMembership: true,
}

// avatarAssetRow is the persisted asset row.
type avatarAssetRow struct {
	ID        string
	OwnerID   string
	Layer     string
	Name      string
	Kind      string
	Data      []byte
	Width     int
	Height    int
	Status    string // active | archived
	SetID     string
	CreatedAt string
}

// AvatarAsset is the public (data-free) asset shape.
type AvatarAsset struct {
	ID        string `json:"id"`
	OwnerID   string `json:"ownerId"`
	Layer     string `json:"layer"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

func (a *avatarAssetRow) public() AvatarAsset {
	return AvatarAsset{
		ID: a.ID, OwnerID: a.OwnerID, Layer: a.Layer, Name: a.Name,
		Kind: a.Kind, Width: a.Width, Height: a.Height,
		Status: a.Status, CreatedAt: a.CreatedAt,
	}
}

// avatarSetRow is the persisted set row.
type avatarSetRow struct {
	ID           string
	Name         string
	Scope        string
	WorldID      string
	Version      int
	CreatedBy    string
	CreatedAt    string
	Archived     bool
	ArchivedAt   string
	ArchiveReason string
}

// AvatarSet is the public set shape.
type AvatarSet struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scope     string   `json:"scope"`
	WorldID   string   `json:"worldId,omitempty"`
	Version   int      `json:"version"`
	CreatedBy string   `json:"createdBy"`
	CreatedAt string   `json:"createdAt"`
	Archived  bool     `json:"archived"`
	Items     []string `json:"items,omitempty"` // "layer:optionId"
}

func (s *avatarSetRow) public(items []string) AvatarSet {
	return AvatarSet{
		ID: s.ID, Name: s.Name, Scope: s.Scope, WorldID: s.WorldID,
		Version: s.Version, CreatedBy: s.CreatedBy, CreatedAt: s.CreatedAt,
		Archived: s.Archived, Items: items,
	}
}

// avatarGrantRow is one entitlement grant.
type avatarGrantRow struct {
	ID        string
	UserID    string
	SetID     string
	Kind      string
	Match     string
	ExpiresAt string // RFC3339; '' = never
	CreatedBy string
	CreatedAt string
}

// --- store layer ---

// AddAvatarAsset inserts an uploaded asset (owner always entitled to it).
func (s *Store) AddAvatarAsset(a *avatarAssetRow) error {
	_, err := s.db.Exec(`INSERT INTO avatar_assets
		(id, owner_id, layer, name, kind, data, width, height, status, set_id, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerID, a.Layer, a.Name, a.Kind, a.Data,
		a.Width, a.Height, a.Status, a.SetID, a.CreatedAt)
	return err
}

// AvatarAssetsOf returns the assets of one owner (optionally active only).
func (s *Store) AvatarAssetsOf(ownerID, status string) ([]*avatarAssetRow, error) {
	q := `SELECT id, owner_id, layer, name, kind, data, width, height, status, set_id, created_at
		FROM avatar_assets WHERE owner_id = ?`
	args := []any{ownerID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*avatarAssetRow
	for rows.Next() {
		var a avatarAssetRow
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Layer, &a.Name, &a.Kind, &a.Data,
			&a.Width, &a.Height, &a.Status, &a.SetID, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// AvatarAsset gets one asset row by id.
func (s *Store) AvatarAsset(id string) (*avatarAssetRow, error) {
	var a avatarAssetRow
	err := s.db.QueryRow(`SELECT id, owner_id, layer, name, kind, data, width, height, status, set_id, created_at
		FROM avatar_assets WHERE id = ?`, id).
		Scan(&a.ID, &a.OwnerID, &a.Layer, &a.Name, &a.Kind, &a.Data,
			&a.Width, &a.Height, &a.Status, &a.SetID, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ArchiveAvatarAsset soft-deletes an asset (status -> archived).
func (s *Store) ArchiveAvatarAsset(id string) error {
	_, err := s.db.Exec(`UPDATE avatar_assets SET status = 'archived' WHERE id = ?`, id)
	return err
}

// MoveAvatarAssetToSet places an asset under a set ('' detaches it).
func (s *Store) MoveAvatarAssetToSet(id, setID string) error {
	_, err := s.db.Exec(`UPDATE avatar_assets SET set_id = ? WHERE id = ?`, setID, id)
	return err
}

// CreateAvatarSet inserts a set at version 1 with its initial items.
func (s *Store) CreateAvatarSet(set *avatarSetRow, items []setItem) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO avatar_sets
		(id, name, scope, world_id, version, created_by, created_at, archived, archived_at, archive_reason)
		VALUES (?,?,?,?,?,?,?,0,'','')`,
		set.ID, set.Name, set.Scope, set.WorldID, set.Version, set.CreatedBy, set.CreatedAt); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.Exec(`INSERT INTO avatar_set_items (set_id, layer, option_id, added_by, created_at) VALUES (?,?,?,?,?)`,
			set.ID, it.Layer, it.OptionID, set.CreatedBy, set.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AvatarSetRow gets one set row.
func (s *Store) AvatarSetRow(id string) (*avatarSetRow, error) {
	var r avatarSetRow
	var archived int
	err := s.db.QueryRow(`SELECT id, name, scope, world_id, version, created_by, created_at, archived, archived_at, archive_reason
		FROM avatar_sets WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Scope, &r.WorldID, &r.Version, &r.CreatedBy,
			&r.CreatedAt, &archived, &r.ArchivedAt, &r.ArchiveReason)
	if err != nil {
		return nil, err
	}
	r.Archived = archived == 1
	return &r, nil
}

// ListAvatarSets returns every set row (the platform is small; filtering is
// done by the registry when computing visibility).
func (s *Store) ListAvatarSets() ([]*avatarSetRow, error) {
	rows, err := s.db.Query(`SELECT id, name, scope, world_id, version, created_by, created_at, archived, archived_at, archive_reason
		FROM avatar_sets ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*avatarSetRow
	for rows.Next() {
		var r avatarSetRow
		var archived int
		if err := rows.Scan(&r.ID, &r.Name, &r.Scope, &r.WorldID, &r.Version, &r.CreatedBy,
			&r.CreatedAt, &archived, &r.ArchivedAt, &r.ArchiveReason); err != nil {
			return nil, err
		}
		r.Archived = archived == 1
		out = append(out, &r)
	}
	return out, rows.Err()
}

// AvatarSetItems returns "layer:optionId" strings of a set, sorted.
func (s *Store) AvatarSetItems(setID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT layer, option_id FROM avatar_set_items WHERE set_id = ?`, setID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var layer, opt string
		if err := rows.Scan(&layer, &opt); err != nil {
			return nil, err
		}
		out = append(out, layer+":"+opt)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// AddAvatarSetItem adds one option and bumps the set version (transactional).
func (s *Store) AddAvatarSetItem(setID, layer, optionID, by string) (int, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO avatar_set_items (set_id, layer, option_id, added_by, created_at) VALUES (?,?,?,?,?)`,
		setID, layer, optionID, by, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE avatar_sets SET version = version + 1 WHERE id = ?`, setID); err != nil {
		return 0, err
	}
	var v int
	if err := tx.QueryRow(`SELECT version FROM avatar_sets WHERE id = ?`, setID).Scan(&v); err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// RemoveAvatarSetItem removes one option and bumps the set version.
func (s *Store) RemoveAvatarSetItem(setID, layer, optionID string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM avatar_set_items WHERE set_id = ? AND layer = ? AND option_id = ?`,
		setID, layer, optionID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE avatar_sets SET version = version + 1 WHERE id = ?`, setID); err != nil {
		return 0, err
	}
	var v int
	if err := tx.QueryRow(`SELECT version FROM avatar_sets WHERE id = ?`, setID).Scan(&v); err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

// ArchiveAvatarSet soft-deletes a set (archived=1).
func (s *Store) ArchiveAvatarSet(id, reason string) error {
	_, err := s.db.Exec(`UPDATE avatar_sets SET archived = 1, archived_at = ?, archive_reason = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339), reason, id)
	return err
}

// AddAvatarGrant inserts an entitlement grant.
func (s *Store) AddAvatarGrant(g *avatarGrantRow) error {
	_, err := s.db.Exec(`INSERT INTO avatar_grants (id, user_id, set_id, kind, match, expires_at, created_by, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		g.ID, g.UserID, g.SetID, g.Kind, g.Match, g.ExpiresAt, g.CreatedBy, g.CreatedAt)
	return err
}

// AvatarGrantsOf returns all grants for a user.
func (s *Store) AvatarGrantsOf(userID string) ([]avatarGrantRow, error) {
	rows, err := s.db.Query(`SELECT id, user_id, set_id, kind, match, expires_at, created_by, created_at
		FROM avatar_grants WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []avatarGrantRow
	for rows.Next() {
		var g avatarGrantRow
		if err := rows.Scan(&g.ID, &g.UserID, &g.SetID, &g.Kind, &g.Match,
			&g.ExpiresAt, &g.CreatedBy, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AddUserClaim records a member claim (tag / email domain).
func (s *Store) AddUserClaim(userID, claimType, claimValue string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO user_claims (user_id, claim_type, claim_value) VALUES (?,?,?)`,
		userID, claimType, strings.ToLower(claimValue))
	return err
}

// UserClaims returns a user's claims grouped by type.
func (s *Store) UserClaims(userID string) (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT claim_type, claim_value FROM user_claims WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var t, v string
		if err := rows.Scan(&t, &v); err != nil {
			return nil, err
		}
		out[t] = append(out[t], v)
	}
	return out, rows.Err()
}

// --- AvatarRegistry: in-memory mirror of the governance tables ---
//
// Every entitlement check reads ONLY this snapshot (map lookups, no I/O),
// which is what makes the per-join p95 < 50ms target hold trivially. The
// snapshot is rebuilt after every governance write (assets, sets, grants,
// claims) and at most once per 30s, so even foreign writes (admin console)
// converge quickly.

// AvatarRegistry is the cached governance snapshot.
type AvatarRegistry struct {
	mu     sync.RWMutex
	assets map[string]*avatarAssetRow // id -> asset (active + archived)
	sets   map[string]*avatarSetRow   // id -> set
	items  map[string][]string        // setID -> "layer:optionId"
	grants map[string][]avatarGrantRow // userID -> grants
	claims map[string]map[string][]string // userID -> type -> values
	worlds map[string]string          // worldID -> ownerID (world-scope check)
	loaded time.Time
	err    error
}

func newAvatarRegistry() *AvatarRegistry {
	return &AvatarRegistry{}
}

// load rebuilds the snapshot from the store. Idempotent; called lazily and
// after writes. A store error is retained and surfaced to the caller (the
// join path then degrades to the catalog-only entitlement, never blocks).
func (r *AvatarRegistry) load(s *Store) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	assets, err := func() ([]*avatarAssetRow, error) {
		rows, err := s.db.Query(`SELECT id, owner_id, layer, name, kind, data, width, height, status, set_id, created_at FROM avatar_assets`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []*avatarAssetRow
		for rows.Next() {
			var a avatarAssetRow
			if err := rows.Scan(&a.ID, &a.OwnerID, &a.Layer, &a.Name, &a.Kind, &a.Data,
				&a.Width, &a.Height, &a.Status, &a.SetID, &a.CreatedAt); err != nil {
				return nil, err
			}
			out = append(out, &a)
		}
		return out, rows.Err()
	}()
	if err != nil {
		r.err = err
		return err
	}
	sets, err := s.ListAvatarSets()
	if err != nil {
		r.err = err
		return err
	}
	r.assets = map[string]*avatarAssetRow{}
	for _, a := range assets {
		r.assets[a.ID] = a
	}
	r.sets = map[string]*avatarSetRow{}
	r.items = map[string][]string{}
	r.worlds = map[string]string{}
	for _, set := range sets {
		r.sets[set.ID] = set
		items, err := s.AvatarSetItems(set.ID)
		if err != nil {
			r.err = err
			return err
		}
		r.items[set.ID] = items
		if set.Scope == setScopeWorld && set.WorldID != "" {
			if meta, err := s.worldMeta(set.WorldID); err == nil {
				r.worlds[set.WorldID] = meta.OwnerID
			}
		}
	}
	rows, err := s.db.Query(`SELECT id, user_id, set_id, kind, match, expires_at, created_by, created_at FROM avatar_grants`)
	if err != nil {
		r.err = err
		return err
	}
	defer rows.Close()
	r.grants = map[string][]avatarGrantRow{}
	for rows.Next() {
		var g avatarGrantRow
		if err := rows.Scan(&g.ID, &g.UserID, &g.SetID, &g.Kind, &g.Match,
			&g.ExpiresAt, &g.CreatedBy, &g.CreatedAt); err != nil {
			r.err = err
			return err
		}
		r.grants[g.UserID] = append(r.grants[g.UserID], g)
	}
	if err := rows.Err(); err != nil {
		r.err = err
		return err
	}
	claimRows, err := s.db.Query(`SELECT user_id, claim_type, claim_value FROM user_claims`)
	if err != nil {
		r.err = err
		return err
	}
	defer claimRows.Close()
	r.claims = map[string]map[string][]string{}
	for claimRows.Next() {
		var uid, ct, cv string
		if err := claimRows.Scan(&uid, &ct, &cv); err != nil {
			r.err = err
			return err
		}
		if r.claims[uid] == nil {
			r.claims[uid] = map[string][]string{}
		}
		r.claims[uid][ct] = append(r.claims[uid][ct], cv)
	}
	if err := claimRows.Err(); err != nil {
		r.err = err
		return err
	}
	r.loaded = time.Now()
	r.err = nil
	return nil
}

// refresh loads the snapshot if it is missing, stale (>30s), or errored.
func (r *AvatarRegistry) refresh(s *Store) {
	r.mu.RLock()
	stale := r.assets == nil || time.Since(r.loaded) > 30*time.Second || r.err != nil
	r.mu.RUnlock()
	if stale {
		_ = r.load(s)
	}
}

// invalidate forces the next check to rebuild (call after every write).
func (r *AvatarRegistry) invalidate() {
	r.mu.Lock()
	r.assets = nil
	r.err = nil
	r.mu.Unlock()
}

func (r *AvatarRegistry) asset(id string) *avatarAssetRow {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.assets[id]
}

func (r *AvatarRegistry) set(id string) *avatarSetRow {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sets[id]
}

func (r *AvatarRegistry) worldOwner(worldID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.worlds[worldID]
}

// grantFor returns true when userID has a live grant on setID of the given
// kind (kind "" matches any). Claim-checked kinds verify against claims;
// expired grants never match.
func (r *AvatarRegistry) grantFor(userID, setID, kind string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, g := range r.grants[userID] {
		if g.SetID != setID {
			continue
		}
		if kind != "" && g.Kind != kind {
			continue
		}
		if g.ExpiresAt != "" && g.ExpiresAt <= now {
			continue
		}
		switch g.Kind {
		case grantTag:
			if !r.hasClaimLocked(userID, "tag", g.Match) {
				continue
			}
		case grantEmailDomain:
			if !r.hasClaimLocked(userID, "email_domain", g.Match) {
				continue
			}
		}
		return true
	}
	return false
}

func (r *AvatarRegistry) hasClaimLocked(userID, claimType, value string) bool {
	for _, v := range r.claims[userID][claimType] {
		if v == value {
			return true
		}
	}
	return false
}

// --- entitlement enforcement ---

// assetEntitled decides whether userID may wear a custom asset (npc = bot).
// Owner always; otherwise the asset's set must be live and grant access.
func (r *AvatarRegistry) assetEntitled(userID, assetID string, npc bool) bool {
	a := r.asset(assetID)
	if a == nil || a.Status != "active" {
		return false
	}
	if npc {
		return true // platform-controlled entities may wear any active asset
	}
	if a.OwnerID == userID {
		return true
	}
	if a.SetID == "" {
		return false // owner-only asset, not shared through any set
	}
	set := r.set(a.SetID)
	if set == nil || set.Archived {
		return false
	}
	switch set.Scope {
	case setScopePublic, setScopeUniverse:
		return true
	case setScopeNPCOnly:
		return false // humans never wear NPC-only looks
	case setScopeWorld:
		return r.worldOwner(set.WorldID) == userID || r.grantFor(userID, set.ID, "")
	case setScopeMembership, setScopeUserGranted:
		return r.grantFor(userID, set.ID, "")
	}
	return false
}

// optionEntitled is the per-option check for one layer value. Catalog
// options keep their T1 rule (NPC-only gate for humans); "asset:" options
// go through assetEntitled. This is the ONLY enforcement point used by the
// join path and avatar_update.
func (r *AvatarRegistry) optionEntitled(userID, layer, optionID string, npc bool) bool {
	if strings.HasPrefix(optionID, assetOptionPrefix) {
		return r.assetEntitled(userID, strings.TrimPrefix(optionID, assetOptionPrefix), npc)
	}
	return optionAllowed(layer, optionID, npc)
}

// firstEntitledOption returns the fallback option for a layer: catalog
// human options first (free by design), then the user's own active assets.
func (r *AvatarRegistry) firstEntitledOption(userID, layer string, npc bool) string {
	if d := defaultOption(layer, npc); d != "" {
		return d
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var own []string
	for _, a := range r.assets {
		if a.Status == "active" && a.OwnerID == userID && a.Layer == layer {
			own = append(own, assetOptionPrefix+a.ID)
		}
	}
	if len(own) == 0 {
		return avatarCatalog[layer][0].ID
	}
	sort.Strings(own)
	return own[0]
}

// entitledSpec validates + entitlement-enforces a spec for userID. Every
// non-entitled layer falls back to the member's first entitled option.
// Returns the normalized spec and the list of denied options (audited).
func (h *Hub) entitledSpec(userID string, spec AvatarSpec, npc bool) (AvatarSpec, []string) {
	r := h.avatarReg
	r.refresh(h.store)
	out := spec
	if out.V == 0 {
		out.V = avatarSpecV
	}
	var denied []string
	for _, layer := range avatarLayers {
		id := layerValue(out, layer)
		if id != "" {
			if r.optionEntitled(userID, layer, id, npc) {
				continue
			}
			denied = append(denied, layer+":"+id)
		}
		setLayerValue(&out, layer, r.firstEntitledOption(userID, layer, npc))
	}
	return out, denied
}

// resolveAvatarSpecT2 is the T2 join policy: incoming validated+entitled
// spec wins and persists; else stored spec; else deterministic default.
// Denied options are audited (kind="avatar", action="entitlement.deny").
func resolveAvatarSpecT2(h *Hub, userID string, incoming *AvatarSpec, ip string) AvatarSpec {
	st, err := avatarStore()
	if incoming != nil {
		spec, denied := h.entitledSpec(userID, *incoming, false)
		for _, d := range denied {
			_ = h.store.Emit("", userID, "member", "avatar", "entitlement.deny", d, diffJSON(map[string]any{"layer": strings.SplitN(d, ":", 2)[0]}), ip)
		}
		if err == nil {
			_ = st.Put(userID, spec)
		}
		return spec
	}
	if err == nil {
		if stored, ok := st.Get(userID); ok {
			spec, denied := h.entitledSpec(userID, stored, false)
			for _, d := range denied {
				_ = h.store.Emit("", userID, "member", "avatar", "entitlement.deny", d, diffJSON(map[string]any{"layer": strings.SplitN(d, ":", 2)[0]}), ip)
			}
			if len(denied) > 0 && st.Put(userID, spec) == nil {
				// normalized copy persisted so the next join is clean
			}
			return spec
		}
	}
	spec, _ := h.entitledSpec(userID, defaultAvatarSpec(userID), false)
	return spec
}

// --- generative picker ---

// generativeModel is the documented model string for the picker.
const generativeModel = "hearth-catalog-sampler v1 (FNV-1a(prompt,userID) -> xorshift64 over the entitled layer catalog; deterministic per member, no external image model in T2)"

// generateAvatarSpec seeds a deterministic sampler from the entitled layer
// catalog: hash(prompt,userID) -> xorshift64 -> one option per layer.
// Same prompt + same member + same catalog => identical candidate.
func (h *Hub) generateAvatarSpec(userID, prompt string) AvatarSpec {
	h.avatarReg.refresh(h.store)
	f := fnv.New64a()
	f.Write([]byte(prompt))
	f.Write([]byte{0})
	f.Write([]byte(userID))
	seed := f.Sum64()
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	x := seed
	next := func() uint64 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		return x
	}
	r := h.avatarReg
	spec := AvatarSpec{V: avatarSpecV}
	for _, layer := range avatarLayers {
		var pool []string
		for _, o := range avatarCatalog[layer] {
			if !o.NPCOnly {
				pool = append(pool, o.ID)
			}
		}
		// the member's own active assets join the pool for that layer
		r.mu.RLock()
		for _, a := range r.assets {
			if a.Status == "active" && a.OwnerID == userID && a.Layer == layer {
				pool = append(pool, assetOptionPrefix+a.ID)
			}
		}
		r.mu.RUnlock()
		if len(pool) == 0 {
			pool = []string{avatarCatalog[layer][0].ID}
		}
		sort.Strings(pool)
		setLayerValue(&spec, layer, pool[int(next()%uint64(len(pool)))])
	}
	return spec
}

// --- safe-archive (worn check) ---

// avatarOptionWorn reports whether any LIVE entity currently wears optionID
// in any space (safe-archive gate: a worn look must never be archived).
func (h *Hub) avatarOptionWorn(optionID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sp := range h.spaces {
		for _, e := range sp.EntitySnaps() {
			if e.Avatar.Spec == nil {
				continue
			}
			for _, layer := range avatarLayers {
				if layerValue(*e.Avatar.Spec, layer) == optionID {
					return true
				}
			}
		}
	}
	return false
}

// avatarSetWorn reports whether any live entity wears an option of the set.
func (h *Hub) avatarSetWorn(setID string) bool {
	h.avatarReg.refresh(h.store)
	for _, it := range h.avatarReg.setItems(setID) {
		if h.avatarOptionWorn(strings.SplitN(it, ":", 2)[1]) {
			return true
		}
	}
	return false
}

func (r *AvatarRegistry) setItems(setID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.items[setID]
}

// --- image validation (magic bytes + dims; no external deps) ---

// sniffImage validates the magic bytes of a PNG/GIF/JPEG/WebP and returns
// kind + pixel dimensions (0 when the container does not carry a quick
// header size — JPEG dimension scan is skipped; the client scales anyway).
func sniffImage(data []byte) (kind string, w, h int, err error) {
	if len(data) < 12 {
		return "", 0, 0, fmt.Errorf("image too small")
	}
	switch {
	case len(data) >= 8 && data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G':
		// PNG: 8-byte signature, then IHDR: length(4) type(4) width(4) height(4)
		if len(data) >= 24 {
			w = int(binary.BigEndian.Uint32(data[16:20]))
			h = int(binary.BigEndian.Uint32(data[20:24]))
		}
		return "image/png", w, h, nil
	case len(data) >= 6 && (data[0] == 'G' && data[1] == 'I' && data[2] == 'F'):
		// GIF: "GIF87a"/"GIF89a", little-endian width/height at 6/8
		w = int(data[6]) | int(data[7])<<8
		h = int(data[8]) | int(data[9])<<8
		return "image/gif", w, h, nil
	case data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "image/jpeg", 0, 0, nil
	case len(data) >= 12 && data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P':
		// WebP: RIFF....WEBP (dims need VP8X/VP8 parsing — left at 0)
		return "image/webp", 0, 0, nil
	}
	return "", 0, 0, fmt.Errorf("unsupported image type (png/gif/jpeg/webp only)")
}

// --- REST handlers ---

// avatarSession resolves the acting user or 401s (mirrors friendSession).
func (h *Hub) avatarSession(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return nil, false
	}
	return sess, true
}

// handleAvatars routes /api/avatars/*.
func (h *Hub) handleAvatars(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/avatars/")
	switch {
	case path == "generate" && r.Method == http.MethodPost:
		h.avatarGenerate(w, r)
	case path == "assets" && r.Method == http.MethodPost:
		h.avatarUpload(w, r)
	case path == "assets" && r.Method == http.MethodGet:
		h.avatarListAssets(w, r)
	case path == "sets" && r.Method == http.MethodPost:
		h.avatarCreateSet(w, r)
	case path == "sets" && r.Method == http.MethodGet:
		h.avatarListSets(w, r)
	case path == "grants" && r.Method == http.MethodPost:
		h.avatarCreateGrant(w, r)
	case strings.HasPrefix(path, "assets/") && strings.HasSuffix(path, "/image") && r.Method == http.MethodGet:
		h.avatarAssetImage(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "assets/"), "/image"))
	case strings.HasPrefix(path, "assets/") && r.Method == http.MethodDelete:
		h.avatarArchiveAsset(w, r, strings.TrimPrefix(path, "assets/"))
	case strings.HasPrefix(path, "sets/") && strings.HasSuffix(path, "/archive") && r.Method == http.MethodPost:
		h.avatarArchiveSet(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "sets/"), "/archive"))
	case strings.HasPrefix(path, "sets/") && strings.HasSuffix(path, "/items") && r.Method == http.MethodPost:
		h.avatarAddSetItem(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "sets/"), "/items"))
	case strings.Contains(path, "/items/") && r.Method == http.MethodDelete:
		// sets/{id}/items/{layer}/{optionId}
		rest := strings.TrimPrefix(path, "sets/")
		parts := strings.SplitN(rest, "/items/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			h.avatarRemoveSetItem(w, r, parts[0], parts[1])
			return
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

// avatarUpload: POST /api/avatars/assets — accept a base64 image + layer,
// validate, persist, audit, invalidate the registry.
func (h *Hub) avatarUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Layer string `json:"layer"`
		Name  string `json:"name"`
		Kind  string `json:"kind"`
		Data  string `json:"data"` // base64
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	if !validLayer(body.Layer) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid layer (body/skin/hair/outfit/accessory)"})
		return
	}
	raw, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil || len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid base64 image data"})
		return
	}
	if len(raw) > avatarMaxAssetBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "image exceeds 512 KiB"})
		return
	}
	kind, iw, ih, err := sniffImage(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if iw > avatarMaxDim || ih > avatarMaxDim {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("image exceeds %dx%d px", avatarMaxDim, avatarMaxDim)})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = kind
	}
	if len(name) > 48 {
		name = name[:48]
	}
	a := &avatarAssetRow{
		ID: newUUID(), OwnerID: sess.UserID, Layer: body.Layer, Name: name,
		Kind: kind, Data: raw, Width: iw, Height: ih,
		Status: "active", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := h.store.AddAvatarAsset(a); err != nil {
		log.Printf("avatar upload: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.avatarReg.invalidate()
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "asset.upload", a.ID,
		diffJSON(map[string]any{"name": a.Name, "layer": a.Layer, "kind": a.Kind, "bytes": len(raw)}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "asset": a.public()})
}

// avatarListAssets: GET /api/avatars/assets — my active assets (no bytes).
func (h *Hub) avatarListAssets(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	rows, err := h.store.AvatarAssetsOf(sess.UserID, "active")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	out := make([]AvatarAsset, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.public())
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "assets": out})
}

// avatarAssetImage: GET /api/avatars/assets/{id}/image — raw bytes for
// <img>/canvas (auth via cookie; same-origin so canvas stays untainted).
func (h *Hub) avatarAssetImage(w http.ResponseWriter, r *http.Request, id string) {
	if _, ok := h.avatarSession(w, r); !ok {
		return
	}
	a, err := h.store.AvatarAsset(id)
	if err != nil || a.Status != "active" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.Kind)
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(a.Data)
}

// avatarArchiveAsset: DELETE /api/avatars/assets/{id} — safe archive.
func (h *Hub) avatarArchiveAsset(w http.ResponseWriter, r *http.Request, id string) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	a, err := h.store.AvatarAsset(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if a.OwnerID != sess.UserID {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "not your asset"})
		return
	}
	if a.Status != "active" {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "asset already archived"})
		return
	}
	if h.avatarOptionWorn(assetOptionPrefix + id) {
		_ = h.store.Emit("", sess.UserID, "member", "avatar", "asset.archive.denied", id,
			diffJSON(map[string]any{"reason": "worn"}), sanitizeIP(r.RemoteAddr))
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "asset is currently worn — archive blocked"})
		return
	}
	if err := h.store.ArchiveAvatarAsset(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.avatarReg.invalidate()
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "asset.archive", id,
		diffJSON(map[string]any{"name": a.Name}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": id})
}

// avatarCreateSet: POST /api/avatars/sets — {name,scope,worldId?,items?}.
// world scope requires owning the world; npc-only sets may only contain
// catalog NPC options or the creator's own assets (bots stay platform-fed).
func (h *Hub) avatarCreateSet(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Name    string     `json:"name"`
		Scope   string     `json:"scope"`
		WorldID string     `json:"worldId"`
		Items   []setItem  `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > avatarSetNameMax {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "set name required (max 48)"})
		return
	}
	if !validAvatarScopes[body.Scope] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid scope (public/universe/world/membership/user-granted/npc-only)"})
		return
	}
	if body.Scope == setScopeWorld {
		if body.WorldID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "world scope requires worldId"})
			return
		}
		meta, err := h.store.worldMeta(body.WorldID)
		if err != nil || meta.OwnerID != sess.UserID {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "world scope requires owning the world"})
			return
		}
	}
	if len(body.Items) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "too many items (max 100)"})
		return
	}
	// custom-asset items must be the creator's own active assets and not
	// already governed by another set (one governing set per asset)
	for _, it := range body.Items {
		if !strings.HasPrefix(it.OptionID, assetOptionPrefix) {
			continue
		}
		if err := h.checkAssetForSet(sess.UserID, it.OptionID); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	set := &avatarSetRow{
		ID: newUUID(), Name: name, Scope: body.Scope, WorldID: body.WorldID,
		Version: 1, CreatedBy: sess.UserID, CreatedAt: now,
	}
	if err := h.store.CreateAvatarSet(set, body.Items); err != nil {
		log.Printf("avatar set create: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	for _, it := range body.Items {
		if strings.HasPrefix(it.OptionID, assetOptionPrefix) {
			_ = h.store.MoveAvatarAssetToSet(strings.TrimPrefix(it.OptionID, assetOptionPrefix), set.ID)
		}
	}
	h.avatarReg.invalidate()
	items, _ := h.store.AvatarSetItems(set.ID)
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "set.create", set.ID,
		diffJSON(map[string]any{"name": set.Name, "scope": set.Scope, "worldId": set.WorldID, "items": items}),
		sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "set": set.public(items)})
}

// avatarListSets: GET /api/avatars/sets — sets visible to me: public,
// universe, world-scope (owner or world-visible), membership/user-granted
// (I have a grant), npc-only (hidden from humans unless I own it), plus any
// set I created.
func (h *Hub) avatarListSets(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	h.avatarReg.refresh(h.store)
	reg := h.avatarReg
	sets, err := h.store.ListAvatarSets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	var out []AvatarSet
	for _, s := range sets {
		if s.Archived {
			continue
		}
		vis := s.CreatedBy == sess.UserID || s.Scope == setScopePublic || s.Scope == setScopeUniverse
		if !vis {
			switch s.Scope {
			case setScopeWorld:
				vis = reg.worldOwner(s.WorldID) == sess.UserID
			case setScopeMembership, setScopeUserGranted:
				vis = reg.grantFor(sess.UserID, s.ID, "")
			case setScopeNPCOnly:
				vis = false // hidden from the human picker
			}
		}
		if !vis {
			continue
		}
		items, _ := h.store.AvatarSetItems(s.ID)
		out = append(out, s.public(items))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sets": out})
}

// avatarAddSetItem: POST /api/avatars/sets/{id}/items — {layer,optionId}.
// Bumps the version; audits; invalidates the registry.
func (h *Hub) avatarAddSetItem(w http.ResponseWriter, r *http.Request, setID string) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	set, err := h.store.AvatarSetRow(setID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if set.CreatedBy != sess.UserID {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "not your set"})
		return
	}
	var body setItem
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	if !validLayer(body.Layer) || strings.TrimSpace(body.OptionID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "layer + optionId required"})
		return
	}
	// NPC-only sets may only hold catalog NPC options (bots stay distinct).
	if set.Scope == setScopeNPCOnly && !strings.HasPrefix(body.OptionID, assetOptionPrefix) &&
		!optionAllowed(body.Layer, body.OptionID, true) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "npc-only sets hold NPC catalog options or assets"})
		return
	}
	// custom-asset items move governance to this set (one governing set each)
	if strings.HasPrefix(body.OptionID, assetOptionPrefix) {
		if err := h.checkAssetForSet(sess.UserID, body.OptionID); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	v, err := h.store.AddAvatarSetItem(set.ID, body.Layer, strings.TrimSpace(body.OptionID), sess.UserID)
	if err != nil {
		log.Printf("avatar set item add: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	if strings.HasPrefix(body.OptionID, assetOptionPrefix) {
		_ = h.store.MoveAvatarAssetToSet(strings.TrimPrefix(body.OptionID, assetOptionPrefix), set.ID)
	}
	h.avatarReg.invalidate()
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "set.item.add", set.ID,
		diffJSON(map[string]any{"layer": body.Layer, "optionId": body.OptionID, "version": v}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": v})
}

// avatarRemoveSetItem: DELETE /api/avatars/sets/{id}/items/{layer}/{optionId}.
func (h *Hub) avatarRemoveSetItem(w http.ResponseWriter, r *http.Request, setID, layerOpt string) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	set, err := h.store.AvatarSetRow(setID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if set.CreatedBy != sess.UserID {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "not your set"})
		return
	}
	parts := strings.SplitN(layerOpt, "/", 2)
	if len(parts) != 2 || !validLayer(parts[0]) || parts[1] == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "expected /items/{layer}/{optionId}"})
		return
	}
	v, err := h.store.RemoveAvatarSetItem(set.ID, parts[0], parts[1])
	if err != nil {
		log.Printf("avatar set item remove: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	// detach governance when the removed option was this set's asset
	if strings.HasPrefix(parts[1], assetOptionPrefix) {
		if a, err := h.store.AvatarAsset(strings.TrimPrefix(parts[1], assetOptionPrefix)); err == nil && a.SetID == set.ID {
			_ = h.store.MoveAvatarAssetToSet(a.ID, "")
		}
	}
	h.avatarReg.invalidate()
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "set.item.remove", set.ID,
		diffJSON(map[string]any{"layer": parts[0], "optionId": parts[1], "version": v}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": v})
}

// avatarArchiveSet: POST /api/avatars/sets/{id}/archive — safe archive.
func (h *Hub) avatarArchiveSet(w http.ResponseWriter, r *http.Request, setID string) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	set, err := h.store.AvatarSetRow(setID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if set.CreatedBy != sess.UserID {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "not your set"})
		return
	}
	if set.Archived {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "set already archived"})
		return
	}
	if h.avatarSetWorn(set.ID) {
		_ = h.store.Emit("", sess.UserID, "member", "avatar", "set.archive.denied", set.ID,
			diffJSON(map[string]any{"reason": "worn"}), sanitizeIP(r.RemoteAddr))
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "a set option is currently worn — archive blocked"})
		return
	}
	if err := h.store.ArchiveAvatarSet(set.ID, "safe-archive"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.avatarReg.invalidate()
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "set.archive", set.ID,
		diffJSON(map[string]any{"name": set.Name, "scope": set.Scope}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": set.ID})
}

// avatarCreateGrant: POST /api/avatars/grants — {setId,userId,kind?,match?,expiresAt?}.
// The set creator (or the asset owner) may grant access to another member.
func (h *Hub) avatarCreateGrant(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		SetID     string `json:"setId"`
		UserID    string `json:"userId"`
		Kind      string `json:"kind"`
		Match     string `json:"match"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	set, err := h.store.AvatarSetRow(body.SetID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if set.CreatedBy != sess.UserID {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "not your set"})
		return
	}
	if body.Kind == "" {
		body.Kind = grantDirect
	}
	if !validAvatarGrantKinds[body.Kind] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid kind (direct/time-limited/tag/sub/email-domain/membership)"})
		return
	}
	if body.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, body.ExpiresAt); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "expiresAt must be RFC3339"})
			return
		}
	}
	if body.Kind == grantTag && strings.TrimSpace(body.Match) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "tag grant requires match=<tag>"})
		return
	}
	if body.Kind == grantEmailDomain && strings.TrimSpace(body.Match) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "email-domain grant requires match=<domain>"})
		return
	}
	g := &avatarGrantRow{
		ID: newUUID(), UserID: body.UserID, SetID: set.ID, Kind: body.Kind,
		Match: strings.ToLower(strings.TrimSpace(body.Match)), ExpiresAt: body.ExpiresAt,
		CreatedBy: sess.UserID, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := h.store.AddAvatarGrant(g); err != nil {
		log.Printf("avatar grant: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.avatarReg.invalidate()
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "grant.create", set.ID,
		diffJSON(map[string]any{"grantee": g.UserID, "kind": g.Kind, "match": g.Match, "expiresAt": g.ExpiresAt}),
		sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "grant": g.ID})
}

// avatarGenerate: POST /api/avatars/generate — {prompt} -> {ok, model, spec}.
func (h *Hub) avatarGenerate(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.avatarSession(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		prompt = "a friendly traveler"
	}
	if len(prompt) > 500 {
		prompt = prompt[:500]
	}
	spec := h.generateAvatarSpec(sess.UserID, prompt)
	_ = h.store.Emit("", sess.UserID, "member", "avatar", "generate", "",
		diffJSON(map[string]any{"prompt": prompt, "spec": spec}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": generativeModel, "spec": spec})
}

// --- WS additive envelope: avatar_update ---

// handleAvatarUpdate: {t:'avatar_update', d:{spec:{...}}} — the member's
// look changes live. Validated + entitlement-enforced + persisted, the live
// entity's avatar is replaced, and the new look is broadcast to the AOI
// ({t:'avatar_update'}) plus the usual 12Hz state stream.
func (c *Client) handleAvatarUpdate(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	spec := parseAvatarSpec(msg)
	if spec == nil {
		c.sendError("bad_avatar", "avatar_update requires a spec")
		return
	}
	userID := c.sessionUserID()
	if userID == "" {
		c.sendError("auth_required", "avatar_update requires a session")
		return
	}
	s := resolveAvatarSpecT2(c.hub, userID, spec, sanitizeIP(c.conn.RemoteAddr().String()))
	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	sp.SetEntityAvatar(c.Entity.ID, s)
	c.Entity.Avatar.Spec = &s
	// broadcast the new look to everyone in AOI (additive envelope)
	cx, cy := c.pos()
	for _, snap := range sp.AOI(cx, cy, aoiRadius, c.Entity.ID) {
		if snap.Client != nil {
			snap.Client.emit("avatar_update", map[string]any{
				"userId": userID, "name": c.Entity.Name, "spec": s,
			})
		}
	}
	c.emit("avatar_update", map[string]any{
		"userId": userID, "name": c.Entity.Name, "spec": s, "self": true,
	})
	log.Printf("avatar_update: %s (%s) -> %s", c.Entity.Name, userID, specKeyOf(s))
}

// SetEntityAvatar atomically replaces a live entity's avatar spec.
func (sp *SpaceState) SetEntityAvatar(id string, spec AvatarSpec) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if e, ok := sp.entities[id]; ok {
		e.Avatar.Spec = &spec
	}
}

func specKeyOf(s AvatarSpec) string {
	return s.Body + "/" + s.Skin + "/" + s.Hair + "/" + s.Outfit + "/" + s.Accessory
}

// validLayer reports whether layer is one of the five picker layers.
func validLayer(layer string) bool {
	switch layer {
	case "body", "skin", "hair", "outfit", "accessory":
		return true
	}
	return false
}

// setItem is a (layer, optionId) pair in a set.
type setItem struct {
	Layer    string `json:"layer"`
	OptionID string `json:"optionId"`
}

// checkAssetForSet validates that an "asset:<id>" option may be governed by
// a set created by userID: it must exist, be active, be the creator's own,
// and not already be governed by another set.
func (h *Hub) checkAssetForSet(userID, optionID string) error {
	a, err := h.store.AvatarAsset(strings.TrimPrefix(optionID, assetOptionPrefix))
	if err != nil {
		return fmt.Errorf("no such asset")
	}
	if a.OwnerID != userID {
		return fmt.Errorf("asset is not yours")
	}
	if a.Status != "active" {
		return fmt.Errorf("asset is archived")
	}
	if a.SetID != "" {
		return fmt.Errorf("asset already belongs to a set")
	}
	return nil
}
