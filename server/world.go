package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"hearth/hmf"
)

// hmfVersion is the HMF format version this server speaks (docs/HMF-v1.md).
const hmfVersion = "v1"

// Tile is a sparse map tile. Only non-floor tiles are stored/serialized.
// T is the canonical string name (frozen palette); TileID is the numeric id
// derived at serialization time for the client palette.
type Tile struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	T      string `json:"t"`
	TileID int    `json:"tileId,omitempty"`
}

type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`
}

type Portal struct {
	ID          string `json:"id"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	TargetSpace string `json:"targetSpace"`
	TargetX     int    `json:"targetX"`
	TargetY     int    `json:"targetY"`
}

type Spawn struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// WorldObject is a functional object (door|npc|sign|light) placed in a world.
// Persisted in world_entities; the client renders each kind as a marker with
// tap interactions (npc/sign talk, door/light are visual).
type WorldObject struct {
	ID   string         `json:"id"`
	Kind string         `json:"kind"`
	X    int            `json:"x"`
	Y    int            `json:"y"`
	Name string         `json:"name,omitempty"`
	Text string         `json:"text,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

// ChunkInfo is one chunk's revision summary (HMF v1). Rev increments on every
// op that touches the chunk; clients use it to detect missed deltas and
// trigger refetch+replay (see docs/HMF-v1.md).
type ChunkInfo struct {
	CX  int `json:"cx"`
	CY  int `json:"cy"`
	Rev int `json:"rev"`
}

// World is a persistent space: tile map + zones + portals + spawn point.
// Live entity positions are RAM-only (see SpaceState in hub.go).
// HMF v1: tiles live in 16x16 chunks (map_chunks table); ChunkRevs mirrors
// the persisted per-chunk revision counters in RAM for cheap reads.
type World struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Width   int              `json:"width"`
	Height  int              `json:"height"`
	Tiles   map[string]*Tile `json:"-"`
	Zones   []Zone           `json:"zones"`
	Portals []Portal         `json:"portals"`
	Objects []WorldObject    `json:"objects,omitempty"`
	Spawn   Spawn            `json:"spawn"`

	// HMF v1 metadata.
	HMFVersion  string         `json:"hmf,omitempty"`
	IsPublished bool           `json:"isPublished"`
	IsShowcase  bool           `json:"isShowcase"`
	ChunkRevs   map[string]int `json:"-"` // "cx,cy" -> rev

	mu sync.RWMutex
}

// --- tile palette (frozen mapping shared with the client editor) ---
// The canonical palette lives in the hmf package (hearth/hmf) so the server,
// the showcase fixture builder, and tests share one definition.

// tileIDs maps canonical tile names to numeric ids (frozen — hmf.Palette).
var tileIDs = hmf.Palette

// tileNames is the reverse id -> name map.
var tileNames = hmf.TileNames()

// passableTiles marks which tiles allow walking (movement grid + A*).
// Collision flags in HMF v1 are derived from this at build time — the
// palette is the single source of truth for passability.
var passableTiles = hmf.PassableSet

// TileType is one palette entry, embedded in the world GeoJSON so clients can
// build their editor palette from the server's source of truth.
type TileType struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Passable bool   `json:"passable"`
}

func tileTypeList() []TileType {
	ids := make([]int, 0, len(tileNames))
	for id := range tileNames {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]TileType, 0, len(ids))
	for _, id := range ids {
		name := tileNames[id]
		out = append(out, TileType{ID: id, Name: name, Passable: passableTiles[name]})
	}
	return out
}

// TileID returns the numeric palette id for a tile name (0 for unknown).
func TileID(name string) int { return hmf.TileID(name) }

// TileName returns the palette name for a numeric id ("" for unknown).
func TileName(id int) string { return hmf.TileName(id) }

// IsPassableTileName reports whether a tile name allows walking.
func IsPassableTileName(name string) bool { return hmf.Passable(name) }

func (w *World) tileKey(x, y int) string { return fmt.Sprintf("%d,%d", x, y) }

func (w *World) SetTile(x, y int, t string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.setTileLocked(x, y, t)
}

func (w *World) setTileLocked(x, y int, t string) {
	if w.Tiles == nil {
		w.Tiles = map[string]*Tile{}
	}
	if t == "" || t == "floor" {
		delete(w.Tiles, w.tileKey(x, y))
		return
	}
	w.Tiles[w.tileKey(x, y)] = &Tile{X: x, Y: y, T: t}
}

func (w *World) TileAt(x, y int) string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if t, ok := w.Tiles[w.tileKey(x, y)]; ok {
		return t.T
	}
	return "floor"
}

// Passable reports whether the tile at (x,y) allows walking. Floor (implicit
// empty tiles) is always passable; impassable palette entries (wall, water,
// ...) block. Out-of-bounds is blocked.
func (w *World) Passable(x, y int) bool {
	if x < 0 || y < 0 || x >= w.Width || y >= w.Height {
		return false
	}
	return IsPassableTileName(w.TileAt(x, y))
}

// NearestPassable BFS-spirals outward from (x,y) to the closest passable
// tile (Manhattan rings, ties broken deterministically). Rescues spawns that
// land on impassable tiles — polluted maps with walls painted on/near the
// spawn point (town-square live-test bug) would otherwise freeze movement
// (A* refuses a blocked start). Returns ok=false only when the whole world
// is impassable.
func (w *World) NearestPassable(x, y int) (int, int, bool) {
	if w.Passable(x, y) {
		return x, y, true
	}
	limit := w.Width
	if w.Height > limit {
		limit = w.Height
	}
	for r := 1; r <= limit; r++ {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				if abs(dx) != r && abs(dy) != r {
					continue // walk the ring only, not the filled square
				}
				nx, ny := x+dx, y+dy
				if w.Passable(nx, ny) {
					return nx, ny, true
				}
			}
		}
	}
	return x, y, false
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TileList returns tiles sorted by y then x (deterministic output) with the
// numeric tileId derived from the frozen palette.
func (w *World) TileList() []Tile {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.TileListLocked()
}

// TileListLocked returns the tile list; caller must hold the read lock.
func (w *World) TileListLocked() []Tile {
	out := make([]Tile, 0, len(w.Tiles))
	for _, t := range w.Tiles {
		c := *t
		c.TileID = tileIDs[c.T]
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}

func (w *World) FindPortal(id string) *Portal {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i := range w.Portals {
		if w.Portals[i].ID == id {
			return &w.Portals[i]
		}
	}
	return nil
}

// FindObject returns the object with the given id (nil when absent).
func (w *World) FindObject(id string) *WorldObject {
	w.mu.RLock()
	defer w.mu.RUnlock()
	for i := range w.Objects {
		if w.Objects[i].ID == id {
			o := w.Objects[i]
			return &o
		}
	}
	return nil
}

// UpsertObject adds or replaces an object in RAM (persisted by the store).
func (w *World) UpsertObject(o WorldObject) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.Objects {
		if w.Objects[i].ID == o.ID {
			w.Objects[i] = o
			return
		}
	}
	w.Objects = append(w.Objects, o)
}

// DeleteObject removes an object from RAM by id (no-op when absent).
func (w *World) DeleteObject(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := range w.Objects {
		if w.Objects[i].ID == id {
			w.Objects = append(w.Objects[:i], w.Objects[i+1:]...)
			return
		}
	}
}

// --- HMF v1 chunk revisions ---

func chunkKey(cx, cy int) string { return fmt.Sprintf("%d,%d", cx, cy) }

func (w *World) initChunkRevs() {
	if w.ChunkRevs == nil {
		w.ChunkRevs = map[string]int{}
	}
}

// bumpChunkRev increments the RAM revision counter of a chunk (persisted by
// the store on the next SaveChunk). Callers hold no World lock.
func (w *World) bumpChunkRev(cx, cy int) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initChunkRevs()
	k := chunkKey(cx, cy)
	w.ChunkRevs[k]++
	return w.ChunkRevs[k]
}

// ChunkRev returns the current revision of a chunk (0 = pristine).
func (w *World) ChunkRev(cx, cy int) int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.ChunkRevs[chunkKey(cx, cy)]
}

// ChunkSummary lists the revision of every known chunk (for the world doc).
func (w *World) ChunkSummary() []ChunkInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.chunkSummaryLocked()
}

// chunkSummaryLocked lists chunk revisions; caller must hold the read lock.
// Sorted numerically by (cy, cx): sorting the "cx,cy" strings was
// lexicographic (primary key cx, "10,0" before "2,0") and did not match the
// documented (cy, cx) contract.
func (w *World) chunkSummaryLocked() []ChunkInfo {
	out := make([]ChunkInfo, 0, len(w.ChunkRevs))
	for k, rev := range w.ChunkRevs {
		var cx, cy int
		fmt.Sscanf(k, "%d,%d", &cx, &cy)
		out = append(out, ChunkInfo{CX: cx, CY: cy, Rev: rev})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CY != out[j].CY {
			return out[i].CY < out[j].CY
		}
		return out[i].CX < out[j].CX
	})
	return out
}

// GeoJSON is the static portion of the world document (HMF v1 header + tiles
// + chunks revision summary + palette).
func (w *World) GeoJSON() map[string]any {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return map[string]any{
		"id":          w.ID,
		"name":        w.Name,
		"width":       w.Width,
		"height":      w.Height,
		"tiles":       w.TileListLocked(),
		"zones":       w.Zones,
		"portals":     w.Portals,
		"objects":     w.Objects,
		"spawn":       w.Spawn,
		"hmf":         hmfVersion,
		"isPublished": w.IsPublished,
		"isShowcase":  w.IsShowcase,
		"palette":     tileTypeList(),
		"chunks":      w.chunkSummaryLocked(),
	}
}

// WorldJSON is the full world document incl. live entities (GET /api/spaces/{id}).
func (w *World) WorldJSON(entities []*EntitySnap) map[string]any {
	m := w.GeoJSON()
	es := make([]map[string]any, 0, len(entities))
	for _, e := range entities {
		es = append(es, e.PublicJSON())
	}
	m["entities"] = es
	return m
}

// defaultWorld builds the seeded 32x32 town-square hub. Its portals to the
// showcase worlds (garden/lab/hall) are patched in by SeedDefaults after the
// showcase fixtures are seeded (append-if-missing, never clobbering).
func defaultWorld(id, name string) *World {
	w := &World{
		ID: id, Name: name, Width: 32, Height: 32,
		Tiles:      map[string]*Tile{},
		Spawn:      Spawn{X: 16, Y: 16},
		HMFVersion: hmfVersion,
		ChunkRevs:  map[string]int{},
	}
	w.Zones = []Zone{{ID: "main", Name: "Main Hall", X: 0, Y: 0, W: 32, H: 32}}

	// border walls
	for x := 0; x < 32; x++ {
		w.SetTile(x, 0, "wall")
		w.SetTile(x, 31, "wall")
	}
	for y := 0; y < 32; y++ {
		w.SetTile(0, y, "wall")
		w.SetTile(31, y, "wall")
	}
	// a few interior walls
	for y := 4; y <= 12; y++ {
		w.SetTile(8, y, "wall")
	}
	for x := 10; x <= 22; x++ {
		w.SetTile(x, 20, "wall")
	}
	w.SetTile(24, 4, "wall")
	w.SetTile(25, 4, "wall")
	w.SetTile(24, 5, "wall")
	w.SetTile(25, 5, "wall")
	return w
}

// --- persistence helpers ---

func unmarshalTiles(b []byte) (map[string]*Tile, error) {
	var arr []Tile
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, err
	}
	m := map[string]*Tile{}
	for i := range arr {
		m[fmt.Sprintf("%d,%d", arr[i].X, arr[i].Y)] = &arr[i]
	}
	return m, nil
}
