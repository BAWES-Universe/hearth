package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// AvatarSpec is the layered avatar identity (T1): one curated option per
// layer (body/skin/hair/outfit/accessory), rendered identically for every
// viewer. It is persisted server-side per member (user id = sha256(deviceKey))
// so a look survives devices and sessions. T2 adds upload + sets/entitlements.
type AvatarSpec struct {
	V         int    `json:"v"`
	Body      string `json:"body"`
	Skin      string `json:"skin"`
	Hair      string `json:"hair"`
	Outfit    string `json:"outfit"`
	Accessory string `json:"accessory"`
}

const avatarSpecV = 1

// avatarOption is one curated catalog entry. NPCOnly options exist for
// bots/NPCs only: they are hidden from the human picker and rejected for
// human members, keeping NPC/bot avatars visually distinct.
type avatarOption struct {
	ID      string
	Label   string
	NPCOnly bool
}

// avatarCatalog is the server-side mirror of client/src/avatar/catalog.ts.
// Every layer has >= 4 options; T1 is curated only (no uploads).
var avatarCatalog = map[string][]avatarOption{
	"body": {
		{ID: "round", Label: "Round"},
		{ID: "bean", Label: "Bean"},
		{ID: "slim", Label: "Slim"},
		{ID: "square", Label: "Square"},
		{ID: "bot", Label: "Robot", NPCOnly: true},
	},
	"skin": {
		{ID: "warm", Label: "Warm"},
		{ID: "fair", Label: "Fair"},
		{ID: "olive", Label: "Olive"},
		{ID: "deep", Label: "Deep"},
		{ID: "cool", Label: "Cool"},
	},
	"hair": {
		{ID: "bob", Label: "Bob"},
		{ID: "curls", Label: "Curls"},
		{ID: "mohawk", Label: "Mohawk"},
		{ID: "cap", Label: "Cap"},
		{ID: "bald", Label: "Bald"},
	},
	"outfit": {
		{ID: "hoodie", Label: "Hoodie"},
		{ID: "tee", Label: "Tee"},
		{ID: "robe", Label: "Robe"},
		{ID: "vest", Label: "Vest"},
		{ID: "dress", Label: "Dress"},
	},
	"accessory": {
		{ID: "none", Label: "None"},
		{ID: "glasses", Label: "Glasses"},
		{ID: "crown", Label: "Crown"},
		{ID: "scarf", Label: "Scarf"},
		{ID: "halo", Label: "Halo"},
	},
}

// avatarLayers is the fixed draw order (also the picker tab order).
var avatarLayers = []string{"body", "skin", "hair", "outfit", "accessory"}

func layerValue(s AvatarSpec, layer string) string {
	switch layer {
	case "body":
		return s.Body
	case "skin":
		return s.Skin
	case "hair":
		return s.Hair
	case "outfit":
		return s.Outfit
	case "accessory":
		return s.Accessory
	}
	return ""
}

func setLayerValue(s *AvatarSpec, layer, id string) {
	switch layer {
	case "body":
		s.Body = id
	case "skin":
		s.Skin = id
	case "hair":
		s.Hair = id
	case "outfit":
		s.Outfit = id
	case "accessory":
		s.Accessory = id
	}
}

// optionAllowed reports whether id is a catalog option usable by the member
// class (npc=false = human). NPC-only options are the only gate in T1.
func optionAllowed(layer, id string, npc bool) bool {
	for _, o := range avatarCatalog[layer] {
		if o.ID == id {
			return npc || !o.NPCOnly
		}
	}
	return false
}

// defaultOption returns the first catalog option usable by the member class.
func defaultOption(layer string, npc bool) string {
	for _, o := range avatarCatalog[layer] {
		if npc || !o.NPCOnly {
			return o.ID
		}
	}
	return avatarCatalog[layer][0].ID
}

// validateAvatarSpec normalizes a spec: unknown options fall back per layer
// and NPC-only options are rejected for human members (bots may use the full
// catalog). The result is always fully valid.
func validateAvatarSpec(spec AvatarSpec, npc bool) AvatarSpec {
	out := spec
	if out.V == 0 {
		out.V = avatarSpecV
	}
	for _, layer := range avatarLayers {
		if !optionAllowed(layer, layerValue(out, layer), npc) {
			setLayerValue(&out, layer, defaultOption(layer, npc))
		}
	}
	return out
}

// defaultAvatarSpec derives a stable default look from a member id hash, so a
// fresh device gets a coherent look that survives reloads.
func defaultAvatarSpec(userID string) AvatarSpec {
	h := fnv.New32a()
	h.Write([]byte(userID))
	r := h.Sum32()
	spec := AvatarSpec{V: avatarSpecV}
	for _, layer := range avatarLayers {
		opts := avatarCatalog[layer]
		// pick from human options only; skip NPC-only
		var pick []avatarOption
		for _, o := range opts {
			if !o.NPCOnly {
				pick = append(pick, o)
			}
		}
		if len(pick) == 0 {
			pick = opts
		}
		setLayerValue(&spec, layer, pick[int(r)%len(pick)].ID)
		r = r*31 + 7
	}
	return spec
}

// robotAvatarSpec is the ambient-bot look (NPC-only body, hidden from picker).
func robotAvatarSpec(i int) AvatarSpec {
	spec := AvatarSpec{
		V: avatarSpecV, Body: "bot", Skin: "cool", Hair: "bald",
		Outfit: "vest", Accessory: "none",
	}
	if i%2 == 1 {
		spec.Outfit = "hoodie"
	}
	return spec
}

// --- entitlement stubs (T1) ---
// T1: every catalog option is free for every member; NPC-only is the only
// gate. T2/T3 add sets/scopes (public/universe/world/membership/user-granted),
// entitlements (tag/sub/email-domain/direct/time-limited), safe-archive and
// audit — all documented in the avatar-platform governance contract.

// canUseAvatarOption is the per-option entitlement check (T1 stub).
func canUseAvatarOption(userID, layer, optionID string) bool {
	_ = userID // T2: membership/tag/sub gates land here
	return optionAllowed(layer, optionID, false)
}

// avatarEntitlementCheck is the per-join server-side check (T1 stub; the
// <50ms cached target is trivially met — nothing to gate yet).
func avatarEntitlementCheck(userID string, spec AvatarSpec) (AvatarSpec, bool) {
	valid := validateAvatarSpec(spec, false)
	return valid, valid == spec
}

// parseAvatarSpec extracts the layered spec from a join message's avatar
// object ({color?, icon?, spec?}). Returns nil when absent or empty.
func parseAvatarSpec(a map[string]any) *AvatarSpec {
	raw, ok := a["spec"].(map[string]any)
	if !ok {
		return nil
	}
	spec := AvatarSpec{V: avatarSpecV}
	for _, layer := range avatarLayers {
		if s, ok := raw[layer].(string); ok {
			setLayerValue(&spec, layer, s)
		}
	}
	if spec.Body == "" && spec.Skin == "" && spec.Hair == "" && spec.Outfit == "" && spec.Accessory == "" {
		return nil
	}
	return &spec
}

// resolveAvatarSpec applies the join policy: an incoming validated spec wins
// and is persisted per member; otherwise the member's stored spec; otherwise
// a device-key default. Storage failures degrade to in-memory, never to a
// failed join.
func resolveAvatarSpec(userID string, incoming *AvatarSpec) AvatarSpec {
	st, err := avatarStore()
	if incoming != nil {
		spec := validateAvatarSpec(*incoming, false)
		if err == nil {
			_ = st.Put(userID, spec) // best-effort persist
		}
		return spec
	}
	if err == nil {
		if spec, ok := st.Get(userID); ok {
			return validateAvatarSpec(spec, false)
		}
	}
	return validateAvatarSpec(defaultAvatarSpec(userID), false)
}

// --- AvatarStore: per-member avatar_spec persistence ---

// AvatarStore persists one avatar_spec per member. It opens the same WAL-mode
// DB file the main store uses (separate pool; low traffic). Falls back to an
// in-memory map on storage errors so joins never fail.
type AvatarStore struct {
	db  *sql.DB
	mu  sync.RWMutex
	mem map[string]AvatarSpec
}

var (
	avatarStoreOnce sync.Once
	avatarStoreInst *AvatarStore
	avatarStoreErr  error
)

func avatarStore() (*AvatarStore, error) {
	avatarStoreOnce.Do(func() {
		path := os.Getenv("HEARTH_DB")
		if path == "" {
			path = filepath.Join("data", "hearth.db")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			avatarStoreErr = fmt.Errorf("avatar store mkdir: %w", err)
			return
		}
		dsn := "file:" + path +
			"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			avatarStoreErr = fmt.Errorf("avatar store: %w", err)
			return
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS avatar_specs (
			user_id TEXT PRIMARY KEY,
			spec TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`); err != nil {
			if cerr := db.Close(); cerr != nil {
				log.Printf("avatar store close: %v", cerr)
			}
			avatarStoreErr = fmt.Errorf("avatar store migrate: %w", err)
			return
		}
		avatarStoreInst = &AvatarStore{db: db, mem: map[string]AvatarSpec{}}
	})
	return avatarStoreInst, avatarStoreErr
}

// Get returns the stored spec for a member (user id = sha256(deviceKey)).
// Context-bounded so a stalled SQLite write cannot block the join path
// indefinitely (resolveAvatarSpec runs synchronously on the WS read loop).
func (s *AvatarStore) Get(userID string) (AvatarSpec, bool) {
	if s == nil || s.db == nil {
		return AvatarSpec{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM avatar_specs WHERE user_id = ?`, userID).Scan(&raw)
	if err == nil {
		var spec AvatarSpec
		if json.Unmarshal([]byte(raw), &spec) == nil {
			return spec, true
		}
	}
	s.mu.RLock()
	spec, ok := s.mem[userID]
	s.mu.RUnlock()
	return spec, ok
}

// Put upserts the member's spec; on DB failure it degrades to the in-memory
// map (process-lifetime persistence) and returns the error.
func (s *AvatarStore) Put(userID string, spec AvatarSpec) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("avatar store unavailable")
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO avatar_specs (user_id, spec, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET spec = excluded.spec, updated_at = excluded.updated_at`,
		userID, string(b), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		s.mu.Lock()
		s.mem[userID] = spec
		s.mu.Unlock()
	}
	return err
}
