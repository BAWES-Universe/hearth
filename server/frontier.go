// frontier.go — BRICK WORLD, Milestone 1 (the gate): procedural infinite world.
//
// Walking off the edge of an authored world yields a SEEDED, DETERMINISTIC
// frontier district: same (world_id, cx, cy) → identical tiles for every
// client, stable across reloads. Generation is a pure function of
// (world_id, cx, cy) — zero DB writes, zero RNG state, no vendor, no cost.
//
// Districts are server-authoritative WORLD STATE: the client never invents
// terrain, it renders exactly what GET /api/worlds/{id}/chunk/{cx}/{cy}
// returns. M2 (claim flow, frontier persistence, position sync in districts)
// builds on top of this endpoint.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"hearth/hmf"
)

// DistrictSize is the edge length (in tiles) of a generated frontier
// district. 32×32 keeps districts phone-friendly and matches town-square's
// footprint, so the "first district east" feels like the same world.
const DistrictSize = 32

// DistrictTile is one non-floor cell in the wire payload. Dual format like
// the HMF world GeoJSON: numeric tileId (client palette) + canonical name.
type DistrictTile struct {
	X      int    `json:"x"`
	Y      int    `json:"y"`
	TileID int    `json:"tileId"`
	T      string `json:"t"`
}

// PlotMarker is the single claimable plot every district carries (M1 gate:
// presence + claimed=false; the claiming flow itself is M2).
type PlotMarker struct {
	X         int  `json:"x"`
	Y         int  `json:"y"`
	Claimed   bool `json:"claimed"`
	Claimable bool `json:"claimable"`
}

// DistrictDoc is the full deterministic district payload.
type DistrictDoc struct {
	OK           bool           `json:"ok"`
	WorldID      string         `json:"world_id"`
	CX           int            `json:"cx"`
	CY           int            `json:"cy"`
	Seed         uint64         `json:"seed"`
	DistrictSize int            `json:"district_size"`
	Width        int            `json:"width"`
	Height       int            `json:"height"`
	Tiles        []DistrictTile `json:"tiles"`
	Plot         PlotMarker     `json:"plot"`
	Generated    bool           `json:"generated"`
}

// districtSeed: sha256("brickworld:<worldID>:<cx>:<cy>") → uint64.
// The seed is the ONLY input besides coordinates, so every client and every
// reload derives the identical district.
func districtSeed(worldID string, cx, cy int) uint64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("brickworld:%s:%d:%d", worldID, cx, cy)))
	return binary.BigEndian.Uint64(h[:8])
}

// mix64 is a splitmix64-style finalizer: (seed, salt) → well-mixed uint64.
// Deterministic by construction — no math/rand, no global state.
func mix64(seed, salt uint64) uint64 {
	z := seed + salt + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// pondHit reports whether (x,y) is inside one of the district's two ponds.
// Pond centers + radii all derive from the seed.
func pondHit(seed uint64, x, y int) bool {
	p1x := 4 + int((seed>>8)%8)
	p1y := 4 + int((seed>>12)%8)
	p2x := 20 + int((seed>>16)%8)
	p2y := 20 + int((seed>>20)%8)
	r1 := 2 + int(mix64(seed, 0x101)%2)
	r2 := 2 + int(mix64(seed, 0x202)%2)
	dx1, dy1 := x-p1x, y-p1y
	dx2, dy2 := x-p2x, y-p2y
	return dx1*dx1+dy1*dy1 <= r1*r1 || dx2*dx2+dy2*dy2 <= r2*r2
}

// generateDistrict builds the DistrictSize×DistrictSize district for
// (worldID, cx, cy). Pure function — no I/O, no state.
func generateDistrict(worldID string, cx, cy int) DistrictDoc {
	seed := districtSeed(worldID, cx, cy)
	doc := DistrictDoc{
		OK:           true,
		WorldID:      worldID,
		CX:           cx,
		CY:           cy,
		Seed:         seed,
		DistrictSize: DistrictSize,
		Width:        DistrictSize,
		Height:       DistrictSize,
		Tiles:        make([]DistrictTile, 0, 160),
		Generated:    true,
	}
	// District skeleton (all from the seed): path row + column (a crossroads),
	// and the claimable plot position in the central band.
	pathRow := 8 + int((seed>>24)%8) // 8..15
	pathCol := 8 + int((seed>>28)%8) // 8..15
	plotX := 8 + int((seed>>32)%16)  // 8..23
	plotY := 8 + int((seed>>36)%16)  // 8..23
	doc.Plot = PlotMarker{X: plotX, Y: plotY, Claimed: false, Claimable: true}

	for y := 0; y < DistrictSize; y++ {
		for x := 0; x < DistrictSize; x++ {
			tid := 0 // floor is the implicit base — only non-floor cells are emitted
			if pondHit(seed, x, y) {
				tid = 2 // water
			} else if x == pathCol || y == pathRow {
				tid = 6 // path — the district's crossroads
			} else {
				r := mix64(seed, uint64(y*DistrictSize+x))
				switch p := r % 100; {
				case p < 24:
					tid = 3 // grass
				case p < 27:
					tid = 4 // stone
				}
				// decor on top of the base (interior only — edges stay walkable)
				if x >= 1 && x < DistrictSize-1 && y >= 1 && y < DistrictSize-1 {
					d := mix64(seed, 0xABCD+uint64(y*DistrictSize+x))
					dp := d % 100
					if tid == 3 {
						switch {
						case dp < 6:
							tid = 13 // tree on grass
						case dp < 9:
							tid = 10 // flower
						case dp < 12:
							tid = 11 // bush
						}
					} else if tid == 0 && dp >= 96 {
						tid = 12 // lone rock on open floor
					}
				}
			}
			// The claimable plot: a cleared 5×5 stage with a crystal beacon at
			// the center (overrides ponds/path/decor so the plot is always
			// approachable).
			dx, dy := x-plotX, y-plotY
			if dx*dx <= 4 && dy*dy <= 4 {
				tid = 0
			}
			if x == plotX && y == plotY {
				tid = 18 // crystal
			}
			if tid != 0 {
				doc.Tiles = append(doc.Tiles, DistrictTile{X: x, Y: y, TileID: tid, T: hmf.TileName(tid)})
			}
		}
	}
	return doc
}

// handleFrontierChunk: GET /api/worlds/{id}/chunk/{cx}/{cy}
// Deterministic district generation (BRICK WORLD M1). Public read — a pure
// function of world_id + coordinates; no session data involved. Worlds that
// don't exist get a 404 (you walk out of a real world, not the void).
func (h *Hub) handleFrontierChunk(w http.ResponseWriter, r *http.Request, id string) {
	parts := strings.Split(id, "/")
	if len(parts) != 4 || parts[1] != "chunk" {
		http.NotFound(w, r)
		return
	}
	worldID := parts[0]
	cx, err1 := strconv.Atoi(parts[2])
	cy, err2 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "chunk coords must be integers"})
		return
	}
	if _, err := h.store.worldMeta(worldID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "world not found"})
		return
	}
	writeJSON(w, http.StatusOK, generateDistrict(worldID, cx, cy))
}
