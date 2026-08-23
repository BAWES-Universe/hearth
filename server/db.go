package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"hearth/hmf"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database (pure-Go modernc.org/sqlite, WAL mode).
// Holds spaces/maps/users/sessions/messages; entity positions are RAM-only.
// HMF v1: tile data lives in 16x16 chunks (map_chunks) + zones/portals/
// entities tables + op_log (the frozen editor op stream) + snapshots.
type Store struct {
	db *sql.DB
	// gravityMu serializes RecomputeGravity so the directory refresh and the
	// nightly cron never run two full recomputes concurrently.
	gravityMu sync.Mutex
}

type User struct {
	ID        string // sha256(deviceKey) hex, first 32 chars — raw keys never stored
	DeviceKey string // hashed
	Name      string
}

type Session struct {
	ID     string // uuid
	UserID string
	User   *User
}

func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS spaces (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			width INTEGER NOT NULL DEFAULT 32,
			height INTEGER NOT NULL DEFAULT 32,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS maps (
			space_id TEXT PRIMARY KEY REFERENCES spaces(id) ON DELETE CASCADE,
			tiles TEXT NOT NULL DEFAULT '[]',
			zones TEXT NOT NULL DEFAULT '[]',
			portals TEXT NOT NULL DEFAULT '[]',
			spawn_x INTEGER NOT NULL DEFAULT 16,
			spawn_y INTEGER NOT NULL DEFAULT 16
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			device_key TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			last_seen TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			space_id TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			channel TEXT NOT NULL,
			text TEXT NOT NULL,
			ts TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_space ON messages(space_id, ts)`,
		// --- HMF v1 (docs/HMF-v1.md) ---
		// Chunked tile storage: one row per 16x16 chunk, RLE-encoded, with a
		// per-chunk revision counter for AOI fetch + refetch/replay.
		`CREATE TABLE IF NOT EXISTS map_chunks (
			space_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
			layer TEXT NOT NULL DEFAULT 'main',
			cx INTEGER NOT NULL,
			cy INTEGER NOT NULL,
			rev INTEGER NOT NULL DEFAULT 0,
			rle TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (space_id, layer, cx, cy)
		)`,
		`CREATE TABLE IF NOT EXISTS world_zones (
			space_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			x INTEGER NOT NULL DEFAULT 0,
			y INTEGER NOT NULL DEFAULT 0,
			w INTEGER NOT NULL DEFAULT 0,
			h INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (space_id, id)
		)`,
		`CREATE TABLE IF NOT EXISTS world_portals (
			space_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			x INTEGER NOT NULL DEFAULT 0,
			y INTEGER NOT NULL DEFAULT 0,
			target_space TEXT NOT NULL DEFAULT '',
			target_x INTEGER NOT NULL DEFAULT 0,
			target_y INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (space_id, id)
		)`,
		// Placed entities (decor bots etc.). Ambient presence sim bots are
		// RAM-only (hub.go); this table persists explicit 'place' ops.
		`CREATE TABLE IF NOT EXISTS world_entities (
			space_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
			id TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'bot',
			x INTEGER NOT NULL DEFAULT 0,
			y INTEGER NOT NULL DEFAULT 0,
			data TEXT NOT NULL DEFAULT '{}',
			PRIMARY KEY (space_id, id)
		)`,
		// op_log is the append-only HMF editor op stream (build history).
		`CREATE TABLE IF NOT EXISTS op_log (
			space_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			op TEXT NOT NULL,
			payload TEXT NOT NULL,
			ts TEXT NOT NULL,
			idem TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (space_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_op_log_space ON op_log(space_id)`,
		// Agent-facing idempotency (docs/BOT-PROTOCOL.md): at most one op per
		// (space, idempotency key). Partial index — humans send idem = ''.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_op_log_idem ON op_log(space_id, idem) WHERE idem <> ''`,
		// snapshots: full-world HMF dumps (written on publish; future
		// templates/clones read from here).
		`CREATE TABLE IF NOT EXISTS snapshots (
			id TEXT PRIMARY KEY,
			space_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			world_json TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_space ON snapshots(space_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// HMF v1 header columns on spaces (idempotent ADD COLUMN).
	for _, col := range []struct{ name, ddl string }{
		{"hmf_version", `ALTER TABLE spaces ADD COLUMN hmf_version TEXT NOT NULL DEFAULT 'v1'`},
		{"is_published", `ALTER TABLE spaces ADD COLUMN is_published INTEGER NOT NULL DEFAULT 0`},
		{"is_showcase", `ALTER TABLE spaces ADD COLUMN is_showcase INTEGER NOT NULL DEFAULT 0`},
		{"op_seq", `ALTER TABLE spaces ADD COLUMN op_seq INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := s.ensureColumn("spaces", col.name, col.ddl); err != nil {
			return fmt.Errorf("migrate %s: %w", col.name, err)
		}
	}
	// op_log idempotency column (existing DBs pre-date the idem column).
	if err := s.ensureColumn("op_log", "idem", `ALTER TABLE op_log ADD COLUMN idem TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("migrate op_log idem: %w", err)
	}
	if _, err := s.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_op_log_idem ON op_log(space_id, idem) WHERE idem <> ''`); err != nil {
		return fmt.Errorf("migrate op_log idem index: %w", err)
	}
	return nil
}

// ensureColumn adds a column when it does not exist yet (SQLite ADD COLUMN).
func (s *Store) ensureColumn(table, column, ddl string) error {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(ddl)
	return err
}

// SeedDefaults seeds the town-square hub + the hand-built showcase worlds
// (garden/lab/hall, HMF v1 fixtures built by APPLYING editor op streams).
// Every seed is insert-if-missing; legacy pre-HMF rows with the same id are
// replaced ONCE by their HMF v1 version (never user-edited HMF rows).
func (s *Store) SeedDefaults() error {
	if err := s.ensureWorld(defaultWorld("town-square", "Town Square"), true); err != nil {
		return err
	}
	showcases, err := loadShowcaseWorlds()
	if err != nil {
		return fmt.Errorf("load showcase worlds: %w", err)
	}
	for _, w := range showcases {
		if err := s.ensureWorld(w, true); err != nil {
			return err
		}
	}
	// town-square is the hub: patch in portals to the showcase worlds
	// (append-if-missing — never removes or moves existing portals). A load
	// failure here must surface: the hub would silently ship without its
	// showcase portals.
	ts, err := s.LoadWorld("town-square")
	if err != nil {
		return fmt.Errorf("load hub world for portal patching: %w", err)
	}
	added := false
	for _, p := range showcaseHubPortals() {
		if ts.FindPortal(p.ID) == nil {
			ts.Portals = append(ts.Portals, p)
			added = true
		}
	}
	if added {
		if err := s.SaveWorld(ts); err != nil {
			return err
		}
	}
	return nil
}

// showcaseHubPortals are the town-square -> showcase portals (mirrored by the
// showcase worlds' back-portals in their fixtures).
func showcaseHubPortals() []Portal {
	return []Portal{
		{ID: "hub-garden", X: 4, Y: 16, TargetSpace: "garden", TargetX: 24, TargetY: 16},
		{ID: "hub-lab", X: 27, Y: 8, TargetSpace: "lab", TargetX: 8, TargetY: 24},
		{ID: "hub-hall", X: 27, Y: 24, TargetSpace: "hall", TargetX: 8, TargetY: 8},
	}
}

// ensureWorld inserts a seeded world only when the id is not already present.
// When replaceLegacy is set, a pre-HMF row (no map_chunks) is replaced by the
// seed — HMF v1 rows (editor-produced) are NEVER overwritten by seeding.
func (s *Store) ensureWorld(w *World, replaceLegacy bool) error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM spaces WHERE id = ?`, w.ID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return s.SaveWorld(w)
	}
	if !replaceLegacy {
		return nil
	}
	var chunks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM map_chunks WHERE space_id = ?`, w.ID).Scan(&chunks); err != nil {
		return err
	}
	if chunks > 0 {
		return nil // editor-produced HMF world — never clobber
	}
	if _, err := s.db.Exec(`DELETE FROM world_portals WHERE space_id = ?`, w.ID); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM world_zones WHERE space_id = ?`, w.ID); err != nil {
		return err
	}
	return s.SaveWorld(w)
}

// --- spaces / maps ---

// SaveWorld persists a world: header (spaces), legacy maps JSON (kept for
// backward compat), and the HMF v1 chunk/zones/portals tables.
func (s *Store) SaveWorld(w *World) error {
	tiles, err := json.Marshal(w.TileList())
	if err != nil {
		return err
	}
	zones, err := json.Marshal(w.Zones)
	if err != nil {
		return err
	}
	portals, err := json.Marshal(w.Portals)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO spaces (id, name, width, height, created_at, hmf_version, is_published, is_showcase) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, width=excluded.width, height=excluded.height,
			hmf_version=excluded.hmf_version, is_published=excluded.is_published, is_showcase=excluded.is_showcase`,
		w.ID, w.Name, w.Width, w.Height, now, hmfVersion, b2i(w.IsPublished), b2i(w.IsShowcase)); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO maps (space_id, tiles, zones, portals, spawn_x, spawn_y) VALUES (?,?,?,?,?,?)
		ON CONFLICT(space_id) DO UPDATE SET tiles=excluded.tiles, zones=excluded.zones, portals=excluded.portals, spawn_x=excluded.spawn_x, spawn_y=excluded.spawn_y`,
		w.ID, string(tiles), string(zones), string(portals), w.Spawn.X, w.Spawn.Y)
	if err != nil {
		return err
	}
	if err := s.saveChunks(w); err != nil {
		return err
	}
	if err := s.saveZones(w.ID, w.Zones); err != nil {
		return err
	}
	if err := s.savePortals(w.ID, w.Portals); err != nil {
		return err
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// saveChunks writes every non-empty chunk of the world (16x16 RLE), keeping
// existing revision counters.
func (s *Store) saveChunks(w *World) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	w.mu.RLock()
	grid := hmf.Grid{}
	for _, t := range w.Tiles {
		grid[hmf.Key(t.X, t.Y)] = TileID(t.T)
	}
	w.mu.RUnlock()
	seen := map[string]bool{}
	for _, t := range w.TileList() {
		cx, cy := hmf.ChunkOf(t.X, t.Y)
		k := chunkKey(cx, cy)
		if seen[k] {
			continue
		}
		seen[k] = true
		gridChunk := hmf.EncodeChunk(grid, w.Width, w.Height, cx, cy)
		rle := hmf.EncodeRLE(gridChunk)
		rev := w.ChunkRev(cx, cy)
		if rev == 0 {
			rev = 1
		}
		if _, err := tx.Exec(`INSERT INTO map_chunks (space_id, layer, cx, cy, rev, rle) VALUES (?,?,?,?,?,?)
			ON CONFLICT(space_id, layer, cx, cy) DO UPDATE SET rev=excluded.rev, rle=excluded.rle`,
			w.ID, "main", cx, cy, rev, rle); err != nil {
			return err
		}
		w.mu.Lock()
		w.initChunkRevs()
		if w.ChunkRevs[k] < rev {
			w.ChunkRevs[k] = rev
		}
		w.mu.Unlock()
	}
	return tx.Commit()
}

func (s *Store) saveZones(spaceID string, zones []Zone) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM world_zones WHERE space_id = ?`, spaceID); err != nil {
		return err
	}
	for _, z := range zones {
		if _, err := tx.Exec(`INSERT INTO world_zones (space_id, id, name, x, y, w, h) VALUES (?,?,?,?,?,?,?)`,
			spaceID, z.ID, z.Name, z.X, z.Y, z.W, z.H); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) savePortals(spaceID string, portals []Portal) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM world_portals WHERE space_id = ?`, spaceID); err != nil {
		return err
	}
	for _, p := range portals {
		if _, err := tx.Exec(`INSERT INTO world_portals (space_id, id, x, y, target_space, target_x, target_y) VALUES (?,?,?,?,?,?,?)`,
			spaceID, p.ID, p.X, p.Y, p.TargetSpace, p.TargetX, p.TargetY); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadWorld loads a world: header + spawn, then HMF v1 chunk/zones/portals
// tables. Legacy rows (no chunk data) fall back to the maps JSON and are
// backfilled into the HMF tables.
func (s *Store) LoadWorld(id string) (*World, error) {
	var w World
	var tilesB, zonesB, portalsB []byte
	var published, showcase int
	err := s.db.QueryRow(`SELECT sp.id, sp.name, sp.width, sp.height, sp.hmf_version, sp.is_published, sp.is_showcase,
			m.tiles, m.zones, m.portals, m.spawn_x, m.spawn_y
		FROM spaces sp JOIN maps m ON m.space_id = sp.id WHERE sp.id = ?`, id).
		Scan(&w.ID, &w.Name, &w.Width, &w.Height, &w.HMFVersion, &published, &showcase,
			&tilesB, &zonesB, &portalsB, &w.Spawn.X, &w.Spawn.Y)
	if err != nil {
		return nil, err
	}
	w.IsPublished = published != 0
	w.IsShowcase = showcase != 0
	w.Tiles = map[string]*Tile{}
	w.ChunkRevs = map[string]int{}
	// no w.mu = sync.RWMutex{} needed: a zero RWMutex is ready to use.

	chunks, err := s.loadChunks(id)
	if err != nil {
		return nil, err
	}
	// legacy = a pre-HMF row: no chunk rows AND tile data in the maps JSON.
	// An empty HMF world (CreateSpace blank canvas) has no chunks but also an
	// empty tiles JSON — it must NOT be classified legacy, or every load
	// would run backfillHMF's three write transactions for nothing.
	legacy := len(chunks) == 0 && string(tilesB) != "[]"
	if legacy {
		// pre-HMF row: fall back to the maps JSON, then backfill chunks.
		if w.Tiles, err = unmarshalTiles(tilesB); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(zonesB, &w.Zones); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(portalsB, &w.Portals); err != nil {
			return nil, err
		}
		if w.HMFVersion == "" {
			w.HMFVersion = hmfVersion
		}
		if err := s.backfillHMF(&w); err != nil {
			return nil, err
		}
	} else {
		for _, ch := range chunks {
			grid, err := hmf.DecodeRLE(ch.rle)
			if err != nil {
				return nil, fmt.Errorf("load world %s chunk %d,%d: %w", id, ch.cx, ch.cy, err)
			}
			x0, y0 := hmf.ChunkOrigin(ch.cx, ch.cy)
			for i := 0; i < hmf.ChunkCells; i++ {
				v := grid[i]
				if v == hmf.FloorTile {
					continue
				}
				x, y := x0+i%hmf.ChunkSize, y0+i/hmf.ChunkSize
				if x < 0 || y < 0 || x >= w.Width || y >= w.Height {
					continue
				}
				name := TileName(v)
				if name == "" {
					continue
				}
				w.Tiles[fmt.Sprintf("%d,%d", x, y)] = &Tile{X: x, Y: y, T: name}
			}
			w.ChunkRevs[chunkKey(ch.cx, ch.cy)] = ch.rev
		}
		if err := s.loadZones(id, &w); err != nil {
			return nil, err
		}
		if err := s.loadPortals(id, &w); err != nil {
			return nil, err
		}
	}
	return &w, nil
}

type chunkRow struct {
	cx, cy, rev int
	rle         string
}

func (s *Store) loadChunks(spaceID string) ([]chunkRow, error) {
	rows, err := s.db.Query(`SELECT cx, cy, rev, rle FROM map_chunks WHERE space_id = ? AND layer = 'main' ORDER BY cy, cx`, spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []chunkRow
	for rows.Next() {
		var c chunkRow
		if err := rows.Scan(&c.cx, &c.cy, &c.rev, &c.rle); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// backfillHMF writes chunk/zones/portals rows for a legacy world so it is
// fully HMF v1 from the next load on.
func (s *Store) backfillHMF(w *World) error {
	if err := s.saveChunks(w); err != nil {
		return err
	}
	if err := s.saveZones(w.ID, w.Zones); err != nil {
		return err
	}
	return s.savePortals(w.ID, w.Portals)
}

func (s *Store) loadZones(spaceID string, w *World) error {
	rows, err := s.db.Query(`SELECT id, name, x, y, w, h FROM world_zones WHERE space_id = ? ORDER BY id`, spaceID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	w.Zones = nil
	for rows.Next() {
		var z Zone
		if err := rows.Scan(&z.ID, &z.Name, &z.X, &z.Y, &z.W, &z.H); err != nil {
			return err
		}
		w.Zones = append(w.Zones, z)
	}
	return rows.Err()
}

func (s *Store) loadPortals(spaceID string, w *World) error {
	rows, err := s.db.Query(`SELECT id, x, y, target_space, target_x, target_y FROM world_portals WHERE space_id = ? ORDER BY id`, spaceID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	w.Portals = nil
	for rows.Next() {
		var p Portal
		if err := rows.Scan(&p.ID, &p.X, &p.Y, &p.TargetSpace, &p.TargetX, &p.TargetY); err != nil {
			return err
		}
		w.Portals = append(w.Portals, p)
	}
	return rows.Err()
}

// --- HMF v1 op-stream persistence ---

// SaveChunk persists one chunk row with its new revision. The WHERE guard on
// the upsert keeps a stale lower-rev write (e.g. a slower concurrent edit on
// the same chunk) from ever clobbering a fresher row.
func (s *Store) SaveChunk(spaceID string, cx, cy, rev int, rle string) error {
	_, err := s.db.Exec(`INSERT INTO map_chunks (space_id, layer, cx, cy, rev, rle) VALUES (?,?,?,?,?,?)
		ON CONFLICT(space_id, layer, cx, cy) DO UPDATE SET rev=excluded.rev, rle=excluded.rle
		WHERE excluded.rev > map_chunks.rev`,
		spaceID, "main", cx, cy, rev, rle)
	return err
}

// NextOpSeq allocates the next monotonic per-space op sequence number.
// A single UPDATE ... RETURNING is atomic — the old SELECT-then-UPDATE inside
// a transaction could interleave two allocations on concurrent edits.
func (s *Store) NextOpSeq(spaceID string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(`UPDATE spaces SET op_seq = op_seq + 1 WHERE id = ? RETURNING op_seq`, spaceID).Scan(&seq)
	return seq, err
}

// AppendOp records one applied editor op in the append-only op_log.
// The op's idempotency key (op.Idem) is stored in its own column so replays
// can be deduped cheaply (unique partial index, docs/BOT-PROTOCOL.md).
func (s *Store) AppendOp(spaceID string, seq int64, op *hmf.Op) error {
	payload, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO op_log (space_id, seq, op, payload, ts, idem) VALUES (?,?,?,?,?,?)`,
		spaceID, seq, op.Op, string(payload), time.Now().UTC().Format(time.RFC3339), op.Idem)
	return err
}

// OpIdemSeq resolves a previously applied op's seq by idempotency key.
// Returns (seq, true) when an op with the same (space, idem) already exists —
// the replay is safe to skip and ack as deduped. (_, false) when new.
func (s *Store) OpIdemSeq(spaceID, idem string) (int64, bool) {
	if idem == "" {
		return 0, false
	}
	var seq int64
	err := s.db.QueryRow(`SELECT seq FROM op_log WHERE space_id = ? AND idem = ? ORDER BY seq LIMIT 1`,
		spaceID, idem).Scan(&seq)
	if err != nil {
		return 0, false
	}
	return seq, true
}

// LoadOpLog returns the op stream of a space, oldest first (build history /
// undo trail evidence for the dogfood showcase worlds).
func (s *Store) LoadOpLog(spaceID string) ([]hmf.Op, error) {
	rows, err := s.db.Query(`SELECT payload FROM op_log WHERE space_id = ? ORDER BY seq`, spaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []hmf.Op
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var op hmf.Op
		if err := json.Unmarshal(b, &op); err == nil {
			out = append(out, op)
		}
	}
	return out, rows.Err()
}

// UpsertPortal persists one portal row (used by the portal edit op).
func (s *Store) UpsertPortal(spaceID string, p Portal) error {
	_, err := s.db.Exec(`INSERT INTO world_portals (space_id, id, x, y, target_space, target_x, target_y) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(space_id, id) DO UPDATE SET x=excluded.x, y=excluded.y,
			target_space=excluded.target_space, target_x=excluded.target_x, target_y=excluded.target_y`,
		spaceID, p.ID, p.X, p.Y, p.TargetSpace, p.TargetX, p.TargetY)
	return err
}

// DeletePortal removes one portal row (portal removal op).
func (s *Store) DeletePortal(spaceID, id string) error {
	_, err := s.db.Exec(`DELETE FROM world_portals WHERE space_id = ? AND id = ?`, spaceID, id)
	return err
}

// UpsertZone persists one zone row (zone edit op).
func (s *Store) UpsertZone(spaceID string, z Zone) error {
	_, err := s.db.Exec(`INSERT INTO world_zones (space_id, id, name, x, y, w, h) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(space_id, id) DO UPDATE SET name=excluded.name, x=excluded.x, y=excluded.y,
			w=excluded.w, h=excluded.h`,
		spaceID, z.ID, z.Name, z.X, z.Y, z.W, z.H)
	return err
}

// DeleteZone removes one zone row (zone removal op).
func (s *Store) DeleteZone(spaceID, id string) error {
	_, err := s.db.Exec(`DELETE FROM world_zones WHERE space_id = ? AND id = ?`, spaceID, id)
	return err
}

// SetPublished flips the is_published flag and writes a snapshot (the world
// doc at publish time — the "save" of the publish flow).
func (s *Store) SetPublished(spaceID string, published bool, doc map[string]any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE spaces SET is_published = ? WHERE id = ?`, b2i(published), spaceID); err != nil {
		return err
	}
	if published {
		b, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339)
		// Unique snapshot id: now is second-granularity, so two publishes of
		// the same space within one second would collide on the primary key
		// and roll back the whole publish.
		if _, err := tx.Exec(`INSERT INTO snapshots (id, space_id, name, created_at, world_json) VALUES (?,?,?,?,?)`,
			"snap-"+spaceID+"-"+now+"-"+randHex(4), spaceID, "publish", now, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// --- spaces / maps (legacy) ---

func (s *Store) ListWorlds() []*World {
	rows, err := s.db.Query(`SELECT id FROM spaces ORDER BY created_at`)
	if err != nil {
		return nil
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close() // close before nested loads (MaxOpenConns=1)
	var out []*World
	for _, id := range ids {
		if w, err := s.LoadWorld(id); err == nil {
			out = append(out, w)
		}
	}
	return out
}

// CreateSpace makes a new empty 32x32 (or given) world with a center spawn.
func (s *Store) CreateSpace(name string, width, height int) (*World, error) {
	if name == "" {
		name = "Room"
	}
	if width <= 0 || height <= 0 || width > 512 || height > 512 {
		width, height = 32, 32
	}
	id := slug(name) + "-" + randHex(4)
	w := &World{
		ID: id, Name: name, Width: width, Height: height,
		Tiles:      map[string]*Tile{},
		Spawn:      Spawn{X: width / 2, Y: height / 2},
		Zones:      []Zone{{ID: "main", Name: name, X: 0, Y: 0, W: width, H: height}},
		Portals:    []Portal{},
		HMFVersion: hmfVersion,
		ChunkRevs:  map[string]int{},
	}
	if err := s.SaveWorld(w); err != nil {
		return nil, err
	}
	return w, nil
}

// --- users / sessions ---

func hashDeviceKey(k string) string {
	h := sha256.Sum256([]byte(k))
	return hex.EncodeToString(h[:])[:32]
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (s *Store) UpsertUser(deviceKey, name string) (*User, error) {
	id := hashDeviceKey(deviceKey)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(`INSERT INTO users (id, device_key, name, created_at, last_seen) VALUES (?,?,?,?,?)
		ON CONFLICT(device_key) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE users.name END,
			last_seen = excluded.last_seen`,
		id, id, name, now, now)
	if err != nil {
		return nil, err
	}
	return &User{ID: id, DeviceKey: id, Name: name}, nil
}

func (s *Store) SetUserName(userID, name string) error {
	if name == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, userID)
	return err
}

func (s *Store) CreateSession(u *User) (*Session, error) {
	id := newUUID()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO sessions (id, user_id, created_at) VALUES (?,?,?)`, id, u.ID, now); err != nil {
		return nil, err
	}
	return &Session{ID: id, UserID: u.ID, User: u}, nil
}

func (s *Store) GetSession(id string) (*Session, error) {
	var sess Session
	var u User
	err := s.db.QueryRow(`SELECT sess.id, sess.user_id, u.id, u.device_key, u.name
		FROM sessions sess JOIN users u ON u.id = sess.user_id WHERE sess.id = ?`, id).
		Scan(&sess.ID, &sess.UserID, &u.ID, &u.DeviceKey, &u.Name)
	if err != nil {
		return nil, err
	}
	sess.User = &u
	return &sess, nil
}

func (s *Store) CountSessions() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	return n, err
}

// --- messages ---

func (s *Store) InsertMessage(spaceID, sessionID, userID, name, channel, text string) error {
	_, err := s.db.Exec(`INSERT INTO messages (space_id, session_id, user_id, name, channel, text, ts) VALUES (?,?,?,?,?,?,?)`,
		spaceID, sessionID, userID, name, channel, text, time.Now().UTC().Format(time.RFC3339))
	return err
}

// --- helpers ---

func slug(s string) string {
	out := make([]byte, 0, len(s))
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, byte(c))
		case c >= 'A' && c <= 'Z':
			out = append(out, byte(c-'A'+'a'))
		case c == ' ' || c == '-' || c == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "room"
	}
	return string(out)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	}
	return hex.EncodeToString(b)[:n]
}
