// Command showcase builds the HMF v1 showcase fixtures
// (server/assets/showcase/{garden,lab,hall}.json) by EMITTING editor op
// streams and APPLYING them through the hmf package — the same pure op
// semantics the live server uses (server/showcase.go replays the op stream
// at seed time; server/showcase_test.go asserts the fixtures agree).
//
// The showcase worlds are DOGFOOD: they are built the way a user builds a
// world in the browser editor (paint/rect/fill/portal/zone ops), never by
// hand-authoring tile JSON. Their op streams are their build history.
//
// Usage: go run ./cmd/showcase   (from server/)
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"hearth/hmf"
)

// fixture is the on-disk HMF v1 fixture format (mirror of server's
// showcaseFixture struct in showcase.go).
type fixture struct {
	HMF      string       `json:"hmf"`
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Width    int          `json:"width"`
	Height   int          `json:"height"`
	Spawn    spawn        `json:"spawn"`
	Chunks   []chunkInfo  `json:"chunks"`
	Portals  []hmf.Portal `json:"portals"`
	Zones    []hmf.Zone   `json:"zones"`
	OpStream []hmf.Op     `json:"opStream"`
}

type spawn struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type chunkInfo struct {
	CX  int    `json:"cx"`
	CY  int    `json:"cy"`
	Rev int    `json:"rev"`
	RLE string `json:"rle"`
}

// worldBuilder accumulates editor ops and applies them to a grid as it goes.
type worldBuilder struct {
	id, name string
	w, h     int
	spawn    spawn
	grid     hmf.Grid
	ops      []hmf.Op
	portals  []hmf.Portal
	zones    []hmf.Zone
}

func newWorld(id, name string, w, h, sx, sy int) *worldBuilder {
	return &worldBuilder{
		id: id, name: name, w: w, h: h,
		spawn: spawn{X: sx, Y: sy},
		grid:  hmf.Grid{},
	}
}

// apply pushes one op and applies it to the grid (editor: optimistic apply +
// send; here: the same op semantics via hmf.ApplyOp).
func (wb *worldBuilder) apply(op hmf.Op) {
	if _, err := hmf.ApplyOp(wb.grid, wb.w, wb.h, &op); err != nil {
		panic(fmt.Sprintf("%s: apply %+v: %v", wb.id, op, err))
	}
	wb.ops = append(wb.ops, op)
}

// brush paints one cell (single-cell op — the editor's brush stroke).
func (wb *worldBuilder) brush(x, y, tile int) {
	wb.apply(hmf.Op{Op: "paint", X: x, Y: y, TileID: tile})
}

// rect fills a rectangle with one batch op (the editor's rect/fill tool).
// Cells are emitted row-major for a deterministic stream.
func (wb *worldBuilder) rect(x1, y1, x2, y2, tile int) {
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	cells := make([]hmf.Cell, 0, (x2-x1+1)*(y2-y1+1))
	for y := y1; y <= y2; y++ {
		for x := x1; x <= x2; x++ {
			cells = append(cells, hmf.Cell{X: x, Y: y})
		}
	}
	wb.apply(hmf.Op{Op: "paint", TileID: tile, Cells: cells})
}

// line paints a Bresenham line with one batch op (the editor's line tool).
func (wb *worldBuilder) line(x0, y0, x1, y1, tile int) {
	cells := bresenham(x0, y0, x1, y1)
	wb.apply(hmf.Op{Op: "paint", TileID: tile, Cells: cells})
}

// ring paints a hollow rectangle border (one batch op).
func (wb *worldBuilder) ring(x1, y1, x2, y2, tile int) {
	var cells []hmf.Cell
	for x := x1; x <= x2; x++ {
		cells = append(cells, hmf.Cell{X: x, Y: y1}, hmf.Cell{X: x, Y: y2})
	}
	for y := y1 + 1; y < y2; y++ {
		cells = append(cells, hmf.Cell{X: x1, Y: y}, hmf.Cell{X: x2, Y: y})
	}
	wb.apply(hmf.Op{Op: "paint", TileID: tile, Cells: cells})
}

// fill flood-fills the contiguous region of the same tile containing (x,y)
// with one batch op (the editor's fill tool). Returns the changed cells.
func (wb *worldBuilder) fill(x, y, tile int) {
	target := wb.grid[hmf.Key(x, y)]
	if target == tile {
		return
	}
	seen := map[string]bool{}
	var cells []hmf.Cell
	queue := []hmf.Cell{{X: x, Y: y}}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c.X < 0 || c.Y < 0 || c.X >= wb.w || c.Y >= wb.h {
			continue
		}
		k := hmf.Key(c.X, c.Y)
		if seen[k] || wb.grid[k] != target {
			continue
		}
		seen[k] = true
		cells = append(cells, c)
		queue = append(queue,
			hmf.Cell{X: c.X + 1, Y: c.Y}, hmf.Cell{X: c.X - 1, Y: c.Y},
			hmf.Cell{X: c.X, Y: c.Y + 1}, hmf.Cell{X: c.X, Y: c.Y - 1},
		)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].Y != cells[j].Y {
			return cells[i].Y < cells[j].Y
		}
		return cells[i].X < cells[j].X
	})
	wb.apply(hmf.Op{Op: "paint", TileID: tile, Cells: cells})
}

func (wb *worldBuilder) portal(p hmf.Portal) {
	wb.ops = append(wb.ops, hmf.Op{Op: "portal", Portal: &p})
	wb.portals = append(wb.portals, p)
}

func (wb *worldBuilder) zone(z hmf.Zone) {
	wb.ops = append(wb.ops, hmf.Op{Op: "zone", Zone: &z})
	wb.zones = append(wb.zones, z)
}

// bresenham returns the cells of a line between two points (row-major).
func bresenham(x0, y0, x1, y1 int) []hmf.Cell {
	var out []hmf.Cell
	dx := abs(x1 - x0)
	dy := -abs(y1 - y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		out = append(out, hmf.Cell{X: x0, Y: y0})
		if x0 == x1 && y0 == y1 {
			break
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Y != out[j].Y {
			return out[i].Y < out[j].Y
		}
		return out[i].X < out[j].X
	})
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// toFixture serializes the built world as an HMF v1 fixture.
func (wb *worldBuilder) toFixture() fixture {
	// chunk the applied grid exactly like the server does
	chunkSeen := map[string]bool{}
	var chunks []chunkInfo
	for k := range wb.grid {
		var x, y int
		fmt.Sscanf(k, "%d,%d", &x, &y)
		cx, cy := hmf.ChunkOf(x, y)
		ck := fmt.Sprintf("%d,%d", cx, cy)
		if chunkSeen[ck] {
			continue
		}
		chunkSeen[ck] = true
		grid := hmf.EncodeChunk(wb.grid, wb.w, wb.h, cx, cy)
		chunks = append(chunks, chunkInfo{CX: cx, CY: cy, Rev: 1, RLE: hmf.EncodeRLE(grid)})
	}
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].CY != chunks[j].CY {
			return chunks[i].CY < chunks[j].CY
		}
		return chunks[i].CX < chunks[j].CX
	})
	return fixture{
		HMF: "v1", ID: wb.id, Name: wb.name,
		Width: wb.w, Height: wb.h, Spawn: wb.spawn,
		Chunks: chunks, Portals: wb.portals, Zones: wb.zones,
		OpStream: wb.ops,
	}
}

func writeFixture(fx fixture, dir string) error {
	b, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(dir, fx.ID+".json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s: %dx%d, %d chunks, %d ops, %d portals, %d zones\n",
		path, fx.Width, fx.Height, len(fx.Chunks), len(fx.OpStream), len(fx.Portals), len(fx.Zones))
	return nil
}

// --- the three dogfood showcase worlds, built with editor ops ---

// buildGarden: a lush grass garden with a pond, flower patches, tree border
// and a path, plus a portal back to town-square.
func buildGarden() *worldBuilder {
	wb := newWorld("garden", "Garden", 48, 32, 24, 16)
	wb.fill(24, 16, hmf.TileID("grass"))        // base lawn
	wb.ring(1, 1, 46, 30, hmf.TileID("tree"))   // tree border
	wb.rect(28, 8, 34, 14, hmf.TileID("water")) // pond
	wb.ring(27, 7, 35, 15, hmf.TileID("rock"))  // pond rocks
	wb.line(4, 16, 44, 16, hmf.TileID("path"))  // main path
	wb.line(24, 2, 24, 29, hmf.TileID("path"))  // cross path
	wb.rect(8, 4, 12, 6, hmf.TileID("flower"))  // flower bed A
	wb.rect(36, 20, 40, 22, hmf.TileID("flower")) // flower bed B
	wb.rect(6, 22, 8, 25, hmf.TileID("bush"))   // bush cluster
	wb.rect(40, 5, 42, 8, hmf.TileID("bush"))   // bush cluster
	wb.portal(hmf.Portal{ID: "garden-to-town", X: 44, Y: 16, TargetSpace: "town-square", TargetX: 4, TargetY: 16})
	wb.zone(hmf.Zone{ID: "garden", Name: "Garden", X: 0, Y: 0, W: 48, H: 32})
	return wb
}

// buildLab: a stone research lab with walled rooms, crystal clusters, an
// ice corner and a small lava pit, plus a portal back to town-square.
func buildLab() *worldBuilder {
	wb := newWorld("lab", "Lab", 40, 32, 20, 16)
	wb.fill(20, 16, hmf.TileID("stone"))        // stone base
	wb.ring(1, 1, 38, 30, hmf.TileID("wall"))   // outer walls
	wb.line(20, 4, 20, 27, hmf.TileID("wall"))  // central divide
	wb.line(4, 12, 36, 12, hmf.TileID("wall"))  // cross wall A
	wb.line(4, 20, 36, 20, hmf.TileID("wall"))  // cross wall B
	wb.brush(20, 12, hmf.FloorTile)             // door gap (erase)
	wb.brush(20, 20, hmf.FloorTile)             // door gap
	wb.rect(6, 14, 9, 17, hmf.TileID("crystal")) // crystal cluster (east room)
	wb.rect(30, 14, 32, 17, hmf.TileID("crystal")) // crystal cluster
	wb.rect(28, 4, 32, 6, hmf.TileID("ice"))    // ice patch
	wb.rect(6, 24, 10, 27, hmf.TileID("lava"))  // lava pit (west room)
	wb.ring(5, 23, 11, 28, hmf.TileID("wall"))  // lava containment
	wb.portal(hmf.Portal{ID: "lab-to-town", X: 36, Y: 16, TargetSpace: "town-square", TargetX: 27, TargetY: 8})
	wb.zone(hmf.Zone{ID: "lab", Name: "Lab", X: 0, Y: 0, W: 40, H: 32})
	return wb
}

// buildHall: a grand wood-floored hall with roof eaves, pillar columns and a
// carpet path, plus a portal back to town-square.
func buildHall() *worldBuilder {
	wb := newWorld("hall", "Hall", 48, 32, 24, 16)
	wb.fill(24, 16, hmf.TileID("wood"))         // wood base
	wb.ring(1, 1, 46, 30, hmf.TileID("wall"))   // outer walls
	wb.rect(1, 1, 46, 2, hmf.TileID("roof"))    // roof eaves (top)
	wb.line(4, 16, 44, 16, hmf.TileID("path"))  // carpet path
	wb.rect(10, 8, 11, 9, hmf.TileID("wall"))   // pillar
	wb.rect(10, 22, 11, 23, hmf.TileID("wall")) // pillar
	wb.rect(24, 6, 25, 7, hmf.TileID("wall"))   // pillar
	wb.rect(24, 24, 25, 25, hmf.TileID("wall")) // pillar
	wb.rect(36, 8, 37, 9, hmf.TileID("wall"))   // pillar
	wb.rect(36, 22, 37, 23, hmf.TileID("wall")) // pillar
	wb.brush(24, 1, hmf.FloorTile)              // entrance gap in the north wall
	wb.brush(25, 1, hmf.FloorTile)
	wb.brush(24, 2, hmf.FloorTile)
	wb.brush(25, 2, hmf.FloorTile)
	wb.portal(hmf.Portal{ID: "hall-to-town", X: 44, Y: 8, TargetSpace: "town-square", TargetX: 27, TargetY: 24})
	wb.zone(hmf.Zone{ID: "hall", Name: "Hall", X: 0, Y: 0, W: 48, H: 32})
	return wb
}

func main() {
	dir := filepath.Join("assets", "showcase")
	worlds := []*worldBuilder{buildGarden(), buildLab(), buildHall()}
	for _, wb := range worlds {
		if err := writeFixture(wb.toFixture(), dir); err != nil {
			fmt.Fprintln(os.Stderr, "showcase:", err)
			os.Exit(1)
		}
	}
	fmt.Println("showcase fixtures written — run the server test to verify they replay identically")
}
