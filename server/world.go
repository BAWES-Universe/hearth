package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Tile is a sparse map tile. Only non-floor tiles are stored/serialized.
type Tile struct {
	X int    `json:"x"`
	Y int    `json:"y"`
	T string `json:"t"`
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

// World is a persistent space: tile map + zones + portals + spawn point.
// Live entity positions are RAM-only (see SpaceState in hub.go).
type World struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Width   int              `json:"width"`
	Height  int              `json:"height"`
	Tiles   map[string]*Tile `json:"-"`
	Zones   []Zone           `json:"zones"`
	Portals []Portal         `json:"portals"`
	Spawn   Spawn            `json:"spawn"`
}

func (w *World) tileKey(x, y int) string { return fmt.Sprintf("%d,%d", x, y) }

func (w *World) SetTile(x, y int, t string) {
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
	if t, ok := w.Tiles[w.tileKey(x, y)]; ok {
		return t.T
	}
	return "floor"
}

// TileList returns tiles sorted by y then x (deterministic output).
func (w *World) TileList() []Tile {
	out := make([]Tile, 0, len(w.Tiles))
	for _, t := range w.Tiles {
		out = append(out, *t)
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
	for i := range w.Portals {
		if w.Portals[i].ID == id {
			return &w.Portals[i]
		}
	}
	return nil
}

// GeoJSON is the static portion of the world document.
func (w *World) GeoJSON() map[string]any {
	return map[string]any{
		"id":      w.ID,
		"name":    w.Name,
		"width":   w.Width,
		"height":  w.Height,
		"tiles":   w.TileList(),
		"zones":   w.Zones,
		"portals": w.Portals,
		"spawn":   w.Spawn,
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

// defaultWorld builds the seeded 32x32 world. hearth and garden share the same
// tile file; their portals point at each other (offset landing spots).
func defaultWorld(id, name string) *World {
	w := &World{
		ID: id, Name: name, Width: 32, Height: 32,
		Tiles: map[string]*Tile{},
		Spawn: Spawn{X: 16, Y: 16},
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

	if id == "hearth" {
		w.Portals = []Portal{
			{ID: "garden-east", X: 4, Y: 16, TargetSpace: "garden", TargetX: 24, TargetY: 16},
			{ID: "garden-west", X: 27, Y: 16, TargetSpace: "garden", TargetX: 8, TargetY: 16},
		}
	} else {
		w.Portals = []Portal{
			{ID: "hearth-west", X: 8, Y: 16, TargetSpace: "hearth", TargetX: 4, TargetY: 16},
			{ID: "hearth-east", X: 24, Y: 16, TargetSpace: "hearth", TargetX: 27, TargetY: 16},
		}
	}
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
