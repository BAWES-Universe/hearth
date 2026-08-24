package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"hearth/hmf"
)

// The showcase worlds are dogfood: they must be built by APPLYING editor op
// streams (never hand-authored tile JSON), and the server must replay those
// op streams to the exact committed chunk data.

func TestShowcaseFixturesReplayToCommittedChunks(t *testing.T) {
	worlds, err := loadShowcaseWorlds()
	if err != nil {
		t.Fatalf("load showcase worlds: %v", err)
	}
	if len(worlds) != 3 {
		t.Fatalf("showcase worlds = %d, want 3 (garden/lab/hall)", len(worlds))
	}
	ids := map[string]bool{}
	for _, w := range worlds {
		ids[w.ID] = true
	}
	for _, want := range []string{"garden", "lab", "hall"} {
		if !ids[want] {
			t.Errorf("missing showcase world %q", want)
		}
	}
	// each fixture's op stream must replay to the committed chunks exactly
	entries, _ := showcaseFS.ReadDir("assets/showcase")
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := showcaseFS.ReadFile("assets/showcase/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		var fx showcaseFixture
		if err := json.Unmarshal(b, &fx); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if fx.HMF != "v1" {
			t.Errorf("%s: hmf = %q, want v1", e.Name(), fx.HMF)
		}
		w, err := buildWorldFromOps(&fx)
		if err != nil {
			t.Fatalf("%s: rebuild: %v", e.Name(), err)
		}
		if len(fx.OpStream) == 0 {
			t.Errorf("%s: empty op stream — fixtures must be built from editor ops", e.Name())
		}
		// server-computed chunks must match the committed chunk rows
		got := map[string]string{}
		for _, t2 := range w.TileList() {
			cx, cy := hmf.ChunkOf(t2.X, t2.Y)
			k := chunkKey(cx, cy)
			if _, ok := got[k]; ok {
				continue
			}
			got[k] = mustEncodeChunk(t, w, cx, cy)
		}
		// compare with the committed fixture rows
		committed := map[string]fixtureChunk{}
		for _, ch := range fx.Chunks {
			committed[chunkKey(ch.CX, ch.CY)] = ch
		}
		if len(got) != len(committed) {
			t.Errorf("%s: chunk count %d != committed %d (got=%v committed=%v)",
				e.Name(), len(got), len(committed), got, committed)
		}
		for k, rle := range got {
			ch, ok := committed[k]
			if !ok {
				t.Errorf("%s: chunk %s missing from fixture", e.Name(), k)
				continue
			}
			if ch.RLE != rle {
				t.Errorf("%s: chunk %s rle mismatch:\n  fixture: %s\n  replay:  %s", e.Name(), k, ch.RLE, rle)
			}
		}
		// portal ops must have produced the committed portals
		if len(fx.Portals) != len(w.Portals) {
			t.Errorf("%s: portals %d != committed %d", e.Name(), len(w.Portals), len(fx.Portals))
		}
		// spawn + bounds sanity
		if w.Spawn.X < 0 || w.Spawn.Y < 0 || w.Spawn.X >= w.Width || w.Spawn.Y >= w.Height {
			t.Errorf("%s: spawn out of bounds %+v", e.Name(), w.Spawn)
		}
	}
}

func mustEncodeChunk(t *testing.T, w *World, cx, cy int) string {
	t.Helper()
	w.mu.RLock()
	grid := hmf.Grid{}
	for _, tl := range w.Tiles {
		grid[hmf.Key(tl.X, tl.Y)] = TileID(tl.T)
	}
	w.mu.RUnlock()
	return hmf.EncodeRLE(hmf.EncodeChunk(grid, w.Width, w.Height, cx, cy))
}

// TestRLERoundTrip: encode/decode must round-trip exactly for every showcase
// chunk and for a pathological all-floor chunk.
func TestRLERoundTrip(t *testing.T) {
	cases := []string{
		"",                      // all floor
		"0:256",                 // explicit all floor
		"1:256",                 // all wall
		"0:1,1:1,0:1,1:1,0:252", // alternating
		"1:4,3:12,0:240",        // mixed
		"0:255,19:1",            // single dirt at the end
	}
	for _, c := range cases {
		grid, err := hmf.DecodeRLE(c)
		if err != nil {
			t.Fatalf("decode %q: %v", c, err)
		}
		if got := hmf.EncodeRLE(grid); got != c && !(c == "" && got == "0:256") {
			t.Errorf("round trip %q -> %q", c, got)
		}
	}
	// bad segments must error
	for _, bad := range []string{"1", "1:2:3", "1:257", "x:4", "1:-1", "1:100,2:100"} {
		if _, err := hmf.DecodeRLE(bad); err == nil {
			t.Errorf("decode %q: expected error", bad)
		}
	}
}

// TestFrozenPalette: the palette must stay exactly the frozen 20 tiles.
func TestFrozenPalette(t *testing.T) {
	want := map[string]int{
		"floor": 0, "wall": 1, "water": 2, "grass": 3, "stone": 4,
		"sand": 5, "path": 6, "wood": 7, "lava": 8, "ice": 9,
		"flower": 10, "bush": 11, "rock": 12, "tree": 13, "roof": 14,
		"door": 15, "fence": 16, "bridge": 17, "crystal": 18, "dirt": 19,
	}
	if len(hmf.Palette) != len(want) {
		t.Fatalf("palette size %d, want %d", len(hmf.Palette), len(want))
	}
	for name, id := range want {
		if hmf.TileID(name) != id {
			t.Errorf("TileID(%q) = %d, want %d", name, hmf.TileID(name), id)
		}
		if hmf.TileName(id) != name {
			t.Errorf("TileName(%d) = %q, want %q", id, hmf.TileName(id), name)
		}
	}
	// floor must never be stored: setting floor erases the tile
	w := &World{Tiles: map[string]*Tile{}}
	w.SetTile(3, 4, "wall")
	if w.TileAt(3, 4) != "wall" {
		t.Fatal("wall not set")
	}
	w.SetTile(3, 4, "floor")
	if w.TileAt(3, 4) != "floor" {
		t.Fatal("floor set must erase the tile")
	}
	if len(w.Tiles) != 0 {
		t.Fatal("floor tiles must not be stored")
	}
}

// TestApplyGridOp: paint/erase semantics, batch cells, prior tiles for undo,
// no-op detection, and out-of-bounds rejection.
func TestApplyGridOp(t *testing.T) {
	w := &World{ID: "t", Name: "T", Width: 16, Height: 16, Tiles: map[string]*Tile{}, ChunkRevs: map[string]int{}}
	h := &Hub{store: &Store{}}
	_ = h

	// single paint
	op := &hmf.Op{Op: "paint", X: 2, Y: 3, TileID: 1}
	changes, _, err := h.applyGridOp(w, op)
	if err != nil || len(changes) != 1 {
		t.Fatalf("paint: changes=%v err=%v", changes, err)
	}
	if changes[0].Prior != 0 || changes[0].TileID != 1 {
		t.Errorf("paint change = %+v, want prior=0 tile=1", changes[0])
	}
	if w.TileAt(2, 3) != "wall" {
		t.Error("tile not applied")
	}

	// batch erase (prior tile preserved for compensating inverse); (4,4) is
	// painted first so the batch touches two cells.
	if _, _, err := h.applyGridOp(w, &hmf.Op{Op: "paint", X: 4, Y: 4, TileID: 1}); err != nil {
		t.Fatal(err)
	}
	op = &hmf.Op{Op: "erase", Cells: []hmf.Cell{{X: 2, Y: 3}, {X: 4, Y: 4}}, TileID: 1}
	changes, _, err = h.applyGridOp(w, op)
	if err != nil || len(changes) != 2 {
		t.Fatalf("erase: changes=%v err=%v", changes, err)
	}
	if changes[0].Prior != 1 {
		t.Errorf("erase prior = %d, want 1 (for undo)", changes[0].Prior)
	}
	if w.TileAt(2, 3) != "floor" {
		t.Error("tile not erased")
	}

	// paint same tile twice: second is a no-op (no chunk rev churn)
	op = &hmf.Op{Op: "paint", X: 5, Y: 5, TileID: 3}
	if _, _, err := h.applyGridOp(w, op); err != nil {
		t.Fatal(err)
	}
	op2 := &hmf.Op{Op: "paint", X: 5, Y: 5, TileID: 3}
	changes, _, err = h.applyGridOp(w, op2)
	if err != nil || len(changes) != 0 {
		t.Errorf("same-tile paint: changes=%v err=%v, want no-op", changes, err)
	}

	// out of bounds
	op = &hmf.Op{Op: "paint", X: 99, Y: 99, TileID: 1}
	if _, _, err := h.applyGridOp(w, op); err == nil {
		t.Error("out-of-bounds paint: expected error")
	}

	// unknown tileId
	op = &hmf.Op{Op: "paint", X: 1, Y: 1, TileID: 200}
	if _, _, err := h.applyGridOp(w, op); err == nil {
		t.Error("unknown tileId: expected error")
	}

	// batch too large
	big := make([]hmf.Cell, hmf.MaxCellsPerOp+1)
	for i := range big {
		big[i] = hmf.Cell{X: i % 16, Y: i / 16}
	}
	op = &hmf.Op{Op: "paint", TileID: 1, Cells: big}
	if _, _, err := h.applyGridOp(w, op); err == nil {
		t.Error("oversized batch: expected error")
	}
}

// TestChunkRevsAndSummary: ops bump per-chunk revisions deterministically
// through the full applyEditOp path (RAM bump + chunk persistence).
func TestChunkRevsAndSummary(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "revs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &Hub{store: st}
	w := &World{ID: "t", Name: "T", Width: 32, Height: 32, Tiles: map[string]*Tile{}, ChunkRevs: map[string]int{}}
	sp := NewSpaceState(w)
	c := &Client{Entity: &Entity{ID: "u1", Name: "U1"}}

	// two paints in chunk (0,0), one in chunk (1,0)
	for _, cell := range []struct{ x, y int }{{1, 1}, {2, 1}, {17, 3}} {
		if ack := h.applyEditOp(sp, c, &hmf.Op{Op: "paint", X: cell.x, Y: cell.y, TileID: 1}); ack.Err != "" {
			t.Fatalf("paint %d,%d: %s", cell.x, cell.y, ack.Err)
		}
	}
	if rev := w.ChunkRev(0, 0); rev != 2 {
		t.Errorf("chunk (0,0) rev = %d, want 2", rev)
	}
	if rev := w.ChunkRev(1, 0); rev != 1 {
		t.Errorf("chunk (1,0) rev = %d, want 1", rev)
	}
	sum := w.ChunkSummary()
	if len(sum) != 2 {
		t.Fatalf("chunk summary = %+v, want 2 chunks", sum)
	}
	// deterministic order (cy, cx)
	if sum[0].CX != 0 || sum[0].CY != 0 || sum[1].CX != 1 {
		t.Errorf("chunk summary order = %+v", sum)
	}
	// a no-op paint must NOT bump the revision
	if ack := h.applyEditOp(sp, c, &hmf.Op{Op: "paint", X: 1, Y: 1, TileID: 1}); ack.Err != "" || len(ack.Cells) != 0 {
		t.Fatalf("no-op paint: err=%q cells=%d", ack.Err, len(ack.Cells))
	}
	if rev := w.ChunkRev(0, 0); rev != 2 {
		t.Errorf("no-op paint bumped chunk (0,0) to rev %d, want 2", rev)
	}
}

// TestSeedShowcaseHubPortals: every showcase world has a back-portal to
// town-square and town-square has a portal to each showcase world.
func TestSeedShowcaseHubPortals(t *testing.T) {
	hubPortals := showcaseHubPortals()
	if len(hubPortals) != 3 {
		t.Fatalf("hub portals = %d, want 3", len(hubPortals))
	}
	for _, p := range hubPortals {
		if p.TargetSpace == "" {
			t.Errorf("hub portal %s missing target", p.ID)
		}
	}
	worlds, err := loadShowcaseWorlds()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range worlds {
		found := false
		for _, p := range w.Portals {
			if p.TargetSpace == "town-square" {
				found = true
			}
		}
		if !found {
			t.Errorf("showcase %s has no portal back to town-square", w.ID)
		}
	}
	// landing coords must be inside the target worlds
	for _, p := range hubPortals {
		for _, w := range worlds {
			if w.ID == p.TargetSpace {
				if p.TargetX < 0 || p.TargetY < 0 || p.TargetX >= w.Width || p.TargetY >= w.Height {
					t.Errorf("hub portal %s lands outside %s: %d,%d (world %dx%d)",
						p.ID, w.ID, p.TargetX, p.TargetY, w.Width, w.Height)
				}
			}
		}
	}
}

// TestChunkGetTiles: the chunk_get reply cells carry tile ids (used by the
// client refetch+replay path).
func TestChunkGetTiles(t *testing.T) {
	w := &World{ID: "t", Name: "T", Width: 32, Height: 32, Tiles: map[string]*Tile{}, ChunkRevs: map[string]int{}}
	w.SetTile(3, 4, "wall")
	w.SetTile(20, 20, "tree")
	w.mu.RLock()
	grid := hmf.Grid{}
	for _, tl := range w.Tiles {
		grid[hmf.Key(tl.X, tl.Y)] = TileID(tl.T)
	}
	w.mu.RUnlock()
	chunk := hmf.EncodeChunk(grid, w.Width, w.Height, 0, 0)
	cells := hmf.ChunkTileCells(chunk, 0, 0, w.Width, w.Height)
	if len(cells) != 1 || cells[0].X != 3 || cells[0].Y != 4 || cells[0].TileID != 1 {
		t.Errorf("chunk(0,0) cells = %+v, want [{3,4,1}]", cells)
	}
	chunk2 := hmf.EncodeChunk(grid, w.Width, w.Height, 1, 1)
	cells2 := hmf.ChunkTileCells(chunk2, 1, 1, w.Width, w.Height)
	if len(cells2) != 1 || cells2[0].TileID != 13 {
		t.Errorf("chunk(1,1) cells = %+v, want tree (13)", cells2)
	}
}
