package main

import "fmt"

// World templates for new-world creation — replaces the old random-wall
// canvas. Each template is a curated tile layout applied at create time;
// empty_lot is a fully passable open field (ZERO walls). Spawn always lands
// on a passable tile: templates never park players inside geometry.
//
// Tiles follow the frozen HMF v1 palette (server/hmf) and only non-floor
// tiles are stored (floor is implicit). All tiles used here are palette
// names, mirrored by the client editor (client/src/world/tiles.ts).

const (
	tplEmptyLot = "empty_lot"
	tplCozyRoom = "cozy_room"
	tplPlaza    = "plaza"
)

// worldTemplateNames is the ordered template list exposed to the API and the
// client template picker.
var worldTemplateNames = []string{tplEmptyLot, tplCozyRoom, tplPlaza}

// validWorldTemplate reports whether t is a known template ("" == empty_lot
// default).
func validWorldTemplate(t string) bool {
	switch t {
	case "", tplEmptyLot, tplCozyRoom, tplPlaza:
		return true
	}
	return false
}

// applyWorldTemplate seeds w's tiles + spawn from a named template. Callers
// must apply it before SaveWorld / hub registration (or re-save afterwards).
func applyWorldTemplate(w *World, tpl string) error {
	switch tpl {
	case "", tplEmptyLot:
		applyEmptyLot(w)
	case tplCozyRoom:
		applyCozyRoom(w)
	case tplPlaza:
		applyPlaza(w)
	default:
		return fmt.Errorf("unknown world template %q", tpl)
	}
	return nil
}

func (w *World) setTplTile(x, y int, name string) {
	w.Tiles[fmt.Sprintf("%d,%d", x, y)] = &Tile{X: x, Y: y, T: name}
}

// applyEmptyLot: a wide-open field — every tile passable, zero walls. A few
// passable accents (flowers/dirt/grass) so the canvas isn't a void.
func applyEmptyLot(w *World) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for y := 0; y < w.Height; y++ {
		for x := 0; x < w.Width; x++ {
			switch {
			case (x*7+y*13)%23 == 0:
				w.setTplTile(x, y, "flower")
			case (x*11+y*5)%31 == 0:
				w.setTplTile(x, y, "dirt")
			}
		}
	}
	w.Spawn = Spawn{X: w.Width / 2, Y: w.Height / 2}
}

// applyCozyRoom: a walled room — wood floor, wall border with a door opening
// (door tile, passable), fireplace nook + plants as accents. Spawn on wood.
func applyCozyRoom(w *World) {
	w.mu.Lock()
	defer w.mu.Unlock()
	W, H := w.Width, w.Height
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			if x == 0 || y == 0 || x == W-1 || y == H-1 {
				w.setTplTile(x, y, "wall")
			} else {
				w.setTplTile(x, y, "wood")
			}
		}
	}
	dx := W / 2
	w.setTplTile(dx, H-1, "door") // door opening in the bottom wall (passable)
	// fireplace nook (stone, passable)
	w.setTplTile(2, H-3, "stone")
	w.setTplTile(3, H-3, "stone")
	w.setTplTile(2, H-4, "stone")
	// plants
	w.setTplTile(W-3, H-3, "bush")
	w.setTplTile(W-4, H-3, "bush")
	w.setTplTile(2, 2, "flower")
	w.setTplTile(3, 2, "flower")
	// spawn: center on wood, a step above the door
	w.Spawn = Spawn{X: dx, Y: H/2 - 1}
}

// applyPlaza: a village plaza — tree boundary ring, grass field, stone plaza
// block, a central fountain (water, blocked — spawn stays on stone below it)
// and a path to the north edge. Spawn on the plaza stone (passable).
func applyPlaza(w *World) {
	w.mu.Lock()
	defer w.mu.Unlock()
	W, H := w.Width, w.Height
	cx, cy := W/2, H/2
	for y := 0; y < H; y++ {
		for x := 0; x < W; x++ {
			switch {
			case x == 0 || y == 0 || x == W-1 || y == H-1:
				w.setTplTile(x, y, "tree")
			case x >= cx-5 && x <= cx+5 && y >= cy-4 && y <= cy+4:
				w.setTplTile(x, y, "stone")
			default:
				w.setTplTile(x, y, "grass")
			}
		}
	}
	// path from the plaza edge to the north boundary
	for y := 1; y < cy-4; y++ {
		w.setTplTile(cx, y, "path")
	}
	// fountain: water center with a stone rim
	w.setTplTile(cx, cy, "water")
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx != 0 || dy != 0 {
				w.setTplTile(cx+dx, cy+dy, "stone")
			}
		}
	}
	w.Spawn = Spawn{X: cx, Y: cy + 3}
}
