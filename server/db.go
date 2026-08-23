package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database (pure-Go modernc.org/sqlite, WAL mode).
// Holds spaces/maps/users/sessions/messages; entity positions are RAM-only.
type Store struct {
	db *sql.DB
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
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// SeedDefaults creates the hearth + garden worlds on first run.
func (s *Store) SeedDefaults() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM spaces`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	for _, w := range []*World{defaultWorld("hearth", "Hearth"), defaultWorld("garden", "Garden")} {
		if err := s.SaveWorld(w); err != nil {
			return err
		}
	}
	return nil
}

// --- spaces / maps ---

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
	if _, err := s.db.Exec(`INSERT INTO spaces (id, name, width, height, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, width=excluded.width, height=excluded.height`,
		w.ID, w.Name, w.Width, w.Height, now); err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO maps (space_id, tiles, zones, portals, spawn_x, spawn_y) VALUES (?,?,?,?,?,?)
		ON CONFLICT(space_id) DO UPDATE SET tiles=excluded.tiles, zones=excluded.zones, portals=excluded.portals, spawn_x=excluded.spawn_x, spawn_y=excluded.spawn_y`,
		w.ID, string(tiles), string(zones), string(portals), w.Spawn.X, w.Spawn.Y)
	return err
}

func (s *Store) LoadWorld(id string) (*World, error) {
	var w World
	var tilesB, zonesB, portalsB []byte
	err := s.db.QueryRow(`SELECT sp.id, sp.name, sp.width, sp.height, m.tiles, m.zones, m.portals, m.spawn_x, m.spawn_y
		FROM spaces sp JOIN maps m ON m.space_id = sp.id WHERE sp.id = ?`, id).
		Scan(&w.ID, &w.Name, &w.Width, &w.Height, &tilesB, &zonesB, &portalsB, &w.Spawn.X, &w.Spawn.Y)
	if err != nil {
		return nil, err
	}
	w.Tiles, err = unmarshalTiles(tilesB)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(zonesB, &w.Zones); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(portalsB, &w.Portals); err != nil {
		return nil, err
	}
	return &w, nil
}

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
		Tiles:   map[string]*Tile{},
		Spawn:   Spawn{X: width / 2, Y: height / 2},
		Zones:   []Zone{{ID: "main", Name: name, X: 0, Y: 0, W: width, H: height}},
		Portals: []Portal{},
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
