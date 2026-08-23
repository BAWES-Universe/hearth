package main

import (
	"embed"
	"encoding/json"
	"fmt"

	"hearth/hmf"
)

//go:embed assets/showcase/*.json
var showcaseFS embed.FS

// fixtureChunk is one chunk row in a fixture file (includes the committed
// RLE so tests can verify the server's replay reproduces it exactly).
type fixtureChunk struct {
	CX  int    `json:"cx"`
	CY  int    `json:"cy"`
	Rev int    `json:"rev"`
	RLE string `json:"rle"`
}

// showcaseFixture is the on-disk HMF v1 fixture format: a header + the
// resulting chunk store + the editor op stream that BUILT the world.
// The op stream is the build history (editor session log); the chunks are
// the applied result. Showcase worlds are NEVER hand-authored JSON maps —
// they are produced by cmd/showcase which EMITS editor ops and APPLIES them
// through the same hmf package semantics the live server uses.
type showcaseFixture struct {
	HMF      string         `json:"hmf"`
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Spawn    Spawn          `json:"spawn"`
	Chunks   []fixtureChunk `json:"chunks"`
	Portals  []Portal       `json:"portals"`
	Zones    []Zone         `json:"zones"`
	OpStream []hmf.Op       `json:"opStream"`
}

// loadShowcaseWorlds parses the embedded showcase fixtures and REBUILDS each
// world by applying its editor op stream (the same code path the live server
// uses for editor edits). The chunk data is recomputed, not trusted from the
// fixture file — showcase_test.go asserts they agree.
func loadShowcaseWorlds() ([]*World, error) {
	entries, err := showcaseFS.ReadDir("assets/showcase")
	if err != nil {
		return nil, err
	}
	var out []*World
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := showcaseFS.ReadFile("assets/showcase/" + e.Name())
		if err != nil {
			return nil, err
		}
		var fx showcaseFixture
		if err := json.Unmarshal(b, &fx); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", e.Name(), err)
		}
		// the running server must not replay a fixture with a different HMF
		// version header using v1 semantics (the test used to be the only
		// gate — the loader is the real one).
		if fx.HMF != hmfVersion {
			return nil, fmt.Errorf("fixture %s: hmf = %q, want %q", e.Name(), fx.HMF, hmfVersion)
		}
		w, err := buildWorldFromOps(&fx)
		if err != nil {
			return nil, fmt.Errorf("fixture %s: %w", e.Name(), err)
		}
		out = append(out, w)
	}
	return out, nil
}

// buildWorldFromOps applies a fixture's editor op stream to a blank world —
// the dogfood proof: showcase worlds are built with the editor op stream,
// not hand-authored tile JSON.
func buildWorldFromOps(fx *showcaseFixture) (*World, error) {
	w := &World{
		ID: fx.ID, Name: fx.Name, Width: fx.Width, Height: fx.Height,
		Tiles:      map[string]*Tile{},
		Spawn:      fx.Spawn,
		HMFVersion: hmfVersion,
		IsShowcase: true,
		ChunkRevs:  map[string]int{},
	}
	grid := hmf.Grid{}
	for i := range fx.OpStream {
		op := &fx.OpStream[i]
		switch op.Op {
		case "paint", "erase", "place":
			changes, err := hmf.ApplyOp(grid, w.Width, w.Height, op)
			if err != nil {
				return nil, fmt.Errorf("op %d (%s): %w", i, op.Op, err)
			}
			for _, ch := range changes {
				name := TileName(ch.TileID)
				if name == "" {
					return nil, fmt.Errorf("op %d: unknown tileId %d", i, ch.TileID)
				}
				w.SetTile(ch.X, ch.Y, name)
			}
		case "portal":
			if op.Portal == nil {
				return nil, fmt.Errorf("op %d: portal without payload", i)
			}
			w.Portals = append(w.Portals, Portal{
				ID: op.Portal.ID, X: op.Portal.X, Y: op.Portal.Y,
				TargetSpace: op.Portal.TargetSpace,
				TargetX:     op.Portal.TargetX, TargetY: op.Portal.TargetY,
			})
		case "zone":
			if op.Zone == nil {
				return nil, fmt.Errorf("op %d: zone without payload", i)
			}
			w.Zones = append(w.Zones, Zone{
				ID: op.Zone.ID, Name: op.Zone.Name,
				X: op.Zone.X, Y: op.Zone.Y, W: op.Zone.W, H: op.Zone.H,
			})
		default:
			return nil, fmt.Errorf("op %d: unsupported op %q in fixture %s", i, op.Op, fx.ID)
		}
	}
	// revision 1 for every non-empty chunk (matches the builder's chunks)
	seen := map[string]bool{}
	for _, t := range w.TileList() {
		cx, cy := hmf.ChunkOf(t.X, t.Y)
		k := chunkKey(cx, cy)
		if !seen[k] {
			seen[k] = true
			w.ChunkRevs[k] = 1
		}
	}
	if len(w.Zones) == 0 {
		w.Zones = []Zone{{ID: "main", Name: fx.Name, X: 0, Y: 0, W: fx.Width, H: fx.Height}}
	}
	return w, nil
}
