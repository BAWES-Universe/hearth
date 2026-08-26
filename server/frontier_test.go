package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BRICK WORLD M1 gate tests: determinism, district shape, claimable plot,
// and the HTTP route. Pure functions — no DB writes, no RNG.

func TestDistrictDeterminism(t *testing.T) {
	a := generateDistrict("town-square", 1, 0)
	b := generateDistrict("town-square", 1, 0)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("same (world, cx, cy) produced different districts:\n%v\nvs\n%v", string(ja), string(jb))
	}
	c := generateDistrict("town-square", 1, 1)
	if a.Seed == c.Seed {
		t.Fatalf("distinct coords must produce distinct seeds (both %d)", a.Seed)
	}
	d := generateDistrict("garden", 1, 0)
	if a.Seed == d.Seed {
		t.Fatalf("distinct worlds must produce distinct seeds (both %d)", a.Seed)
	}
}

func TestDistrictShape(t *testing.T) {
	doc := generateDistrict("town-square", 2, -1)
	if doc.Width != DistrictSize || doc.Height != DistrictSize {
		t.Fatalf("district must be %dx%d, got %dx%d", DistrictSize, DistrictSize, doc.Width, doc.Height)
	}
	if !doc.Generated || !doc.OK {
		t.Fatalf("district must be marked generated+ok")
	}
	if len(doc.Tiles) == 0 {
		t.Fatalf("district has no terrain tiles")
	}
	// every tile in bounds, non-floor, and dual-format (tileId + t)
	seen := map[string]bool{}
	for _, tl := range doc.Tiles {
		if tl.X < 0 || tl.X >= DistrictSize || tl.Y < 0 || tl.Y >= DistrictSize {
			t.Fatalf("tile out of bounds: %+v", tl)
		}
		if tl.TileID == 0 {
			t.Fatalf("floor must stay implicit (sparse format), got tile %+v", tl)
		}
		if tl.T == "" {
			t.Fatalf("tile %+v missing canonical name", tl)
		}
		seen[fmt.Sprintf("%d,%d", tl.X, tl.Y)] = true
	}
	// edges stay passable: no blocking tile on the border (walls/water/trees…)
	blocking := map[int]bool{1: true, 2: true, 8: true, 9: true, 11: true, 12: true, 13: true, 14: true, 15: true, 16: true, 18: true}
	for y := 0; y < DistrictSize; y++ {
		for _, x := range []int{0, DistrictSize - 1} {
			if blocking[doc.TileAt(x, y)] {
				t.Fatalf("border tile (%d,%d) blocks the frontier edge (tileId %d)", x, y, doc.TileAt(x, y))
			}
		}
	}
	for x := 0; x < DistrictSize; x++ {
		for _, y := range []int{0, DistrictSize - 1} {
			if blocking[doc.TileAt(x, y)] {
				t.Fatalf("border tile (%d,%d) blocks the frontier edge (tileId %d)", x, y, doc.TileAt(x, y))
			}
		}
	}
	if len(seen) != len(doc.Tiles) {
		t.Fatalf("duplicate tiles in district payload")
	}
}

// TileAt is a test helper: tileId at (x,y), 0 = implicit floor.
func (d DistrictDoc) TileAt(x, y int) int {
	for _, tl := range d.Tiles {
		if tl.X == x && tl.Y == y {
			return tl.TileID
		}
	}
	return 0
}

func TestDistrictClaimablePlot(t *testing.T) {
	for _, c := range [][2]int{{1, 0}, {0, 1}, {-1, 2}, {3, -2}} {
		doc := generateDistrict("town-square", c[0], c[1])
		if !doc.Plot.Claimable {
			t.Fatalf("D(%d,%d): plot must be claimable", c[0], c[1])
		}
		if doc.Plot.Claimed {
			t.Fatalf("D(%d,%d): M1 plot must start unclaimed", c[0], c[1])
		}
		px, py := doc.Plot.X, doc.Plot.Y
		if doc.TileAt(px, py) != 18 {
			t.Fatalf("D(%d,%d): plot center (%d,%d) must be a crystal (18), got %d",
				c[0], c[1], px, py, doc.TileAt(px, py))
		}
		// the 5x5 stage around the beacon is cleared floor
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				if dx == 0 && dy == 0 {
					continue
				}
				if tid := doc.TileAt(px+dx, py+dy); tid != 0 {
					t.Fatalf("D(%d,%d): plot stage cell (%d,%d) must be cleared floor, got %d",
						c[0], c[1], px+dx, py+dy, tid)
				}
			}
		}
	}
}

func TestFrontierChunkRoute(t *testing.T) {
	h := newTestHub(t)
	// town-square is seeded by the test store → real world, real district
	req := httptest.NewRequest(http.MethodGet, "/api/worlds/town-square/chunk/1/0", nil)
	rr := httptest.NewRecorder()
	h.handleWorldRoute(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("chunk route: want 200, got %d (%s)", rr.Code, rr.Body.String())
	}
	var doc DistrictDoc
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if !doc.OK || !doc.Generated || doc.WorldID != "town-square" || doc.CX != 1 || doc.CY != 0 {
		t.Fatalf("unexpected district doc: %+v", doc)
	}
	if len(doc.Tiles) == 0 || !doc.Plot.Claimable || doc.Plot.Claimed {
		t.Fatalf("district must carry terrain + an unclaimed claimable plot (tiles=%d plot=%+v)", len(doc.Tiles), doc.Plot)
	}
	// the same URL twice → byte-identical (reload stability, HTTP-level)
	rr2 := httptest.NewRecorder()
	h.handleWorldRoute(rr2, httptest.NewRequest(http.MethodGet, "/api/worlds/town-square/chunk/1/0", nil))
	if rr.Body.String() != rr2.Body.String() {
		t.Fatalf("same chunk URL returned different bodies across requests")
	}

	// unknown world → 404 (walk out of a real world, not the void)
	rr3 := httptest.NewRecorder()
	h.handleWorldRoute(rr3, httptest.NewRequest(http.MethodGet, "/api/worlds/no-such-world/chunk/1/0", nil))
	if rr3.Code != http.StatusNotFound {
		t.Fatalf("unknown world: want 404, got %d", rr3.Code)
	}

	// bad coords → 400
	rr4 := httptest.NewRecorder()
	h.handleWorldRoute(rr4, httptest.NewRequest(http.MethodGet, "/api/worlds/town-square/chunk/abc/0", nil))
	if rr4.Code != http.StatusBadRequest {
		t.Fatalf("bad coords: want 400, got %d", rr4.Code)
	}

	// POST → 405
	rr5 := httptest.NewRecorder()
	h.handleWorldRoute(rr5, httptest.NewRequest(http.MethodPost, "/api/worlds/town-square/chunk/1/0", nil))
	if rr5.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST chunk: want 405, got %d", rr5.Code)
	}

	// dual-format tiles: t names must match the canonical palette
	for _, tl := range doc.Tiles {
		if !strings.Contains("wall water grass stone sand path wood lava ice flower bush rock tree roof door fence bridge crystal dirt", tl.T) {
			t.Fatalf("tile %+v has non-palette name %q", tl, tl.T)
		}
	}
}
