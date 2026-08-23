package main

import (
	"errors"
	"log"
	"time"

	"hearth/hmf"
)

// EditAck is the server's authoritative result of applying one editor op:
// the op as recorded (with server seq), the changed cells (with prior tile
// ids for compensating inverse ops / undo), and the touched chunk revisions.
type EditAck struct {
	Op     *hmf.Op
	Cells  []hmf.CellChange
	Chunks []ChunkInfo
	Err    string
}

// applyEditOp applies one frozen editor op to a space's world (server-
// arbitrated, LWW by arrival order) and persists the deltas. The returned
// ack is broadcast to everyone in the space, including the acting client —
// the client editor uses priorTileId/chunk revs for undo + refetch/replay.
func (h *Hub) applyEditOp(sp *SpaceState, c *Client, op *hmf.Op) *EditAck {
	w := sp.World
	op.SpaceID = w.ID
	op.By = c.Entity.ID
	ack := &EditAck{Op: op}

	switch op.Op {
	case "publish":
		w.mu.Lock()
		w.IsPublished = true
		w.mu.Unlock()
		doc := w.GeoJSON()
		if err := h.store.SetPublished(w.ID, true, doc); err != nil {
			log.Printf("publish %s: %v", w.ID, err)
			ack.Err = "publish failed"
			return ack
		}
		return ack
	case "portal":
		if err := h.applyPortalOp(w, op); err != nil {
			ack.Err = err.Error()
			return ack
		}
		return ack
	case "zone":
		if err := h.applyZoneOp(w, op); err != nil {
			ack.Err = err.Error()
			return ack
		}
		return ack
	case "paint", "erase", "place":
		// grid ops below
	default:
		ack.Err = "unknown edit op: " + op.Op
		return ack
	}

	changes, err := h.applyGridOp(w, op)
	if err != nil {
		ack.Err = err.Error()
		return ack
	}
	ack.Cells = changes
	if len(changes) == 0 {
		return ack // no-op (e.g. painting floor on floor) — still seq'd + logged
	}

	// bump per-chunk revisions and persist the touched chunks
	touched := map[string]ChunkInfo{}
	for _, ch := range changes {
		cx, cy := hmf.ChunkOf(ch.X, ch.Y)
		k := chunkKey(cx, cy)
		if _, ok := touched[k]; !ok {
			rev := w.bumpChunkRev(cx, cy)
			touched[k] = ChunkInfo{CX: cx, CY: cy, Rev: rev}
		}
	}
	for _, ci := range touched {
		ack.Chunks = append(ack.Chunks, ci)
		if err := h.persistChunk(w, ci.CX, ci.CY); err != nil {
			log.Printf("persist chunk %s %d,%d: %v", w.ID, ci.CX, ci.CY, err)
		}
	}
	return ack
}

// applyGridOp applies a paint/erase/place op to the world tile map (sparse
// names in RAM; hmf.Grid conversion for the pure op semantics).
func (h *Hub) applyGridOp(w *World, op *hmf.Op) ([]hmf.CellChange, error) {
	if op.Op != "erase" && op.TileID != hmf.FloorTile && hmf.TileName(op.TileID) == "" {
		return nil, errors.New("edit_rejected: unknown tileId " + itoa(op.TileID))
	}
	w.mu.RLock()
	grid := hmf.Grid{}
	for _, t := range w.Tiles {
		grid[hmf.Key(t.X, t.Y)] = TileID(t.T)
	}
	w.mu.RUnlock()
	changes, err := hmf.ApplyOp(grid, w.Width, w.Height, op)
	if err != nil {
		return nil, errors.New("edit_rejected: " + err.Error())
	}
	if len(changes) == 0 {
		return nil, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ch := range changes {
		w.setTileLocked(ch.X, ch.Y, hmf.TileName(ch.TileID))
	}
	return changes, nil
}

// applyPortalOp upserts or removes a portal (RAM + world_portals row).
func (h *Hub) applyPortalOp(w *World, op *hmf.Op) error {
	if op.Portal != nil {
		p := Portal{
			ID: op.Portal.ID, X: op.Portal.X, Y: op.Portal.Y,
			TargetSpace: op.Portal.TargetSpace,
			TargetX:     op.Portal.TargetX, TargetY: op.Portal.TargetY,
		}
		if p.ID == "" {
			return errors.New("edit_rejected: portal op requires id")
		}
		if p.X < 0 || p.Y < 0 || p.X >= w.Width || p.Y >= w.Height {
			return errors.New("edit_rejected: portal outside map bounds")
		}
		w.mu.Lock()
		idx := -1
		for i := range w.Portals {
			if w.Portals[i].ID == p.ID {
				idx = i
				break
			}
		}
		if idx >= 0 {
			w.Portals[idx] = p
		} else {
			w.Portals = append(w.Portals, p)
		}
		w.mu.Unlock()
		return h.store.UpsertPortal(w.ID, p)
	}
	if op.PortalID != "" {
		w.mu.Lock()
		for i := range w.Portals {
			if w.Portals[i].ID == op.PortalID {
				w.Portals = append(w.Portals[:i], w.Portals[i+1:]...)
				break
			}
		}
		w.mu.Unlock()
		return h.store.DeletePortal(w.ID, op.PortalID)
	}
	return errors.New("edit_rejected: portal op requires portal payload or portalId")
}

// applyZoneOp upserts or removes a zone (RAM + world_zones row).
func (h *Hub) applyZoneOp(w *World, op *hmf.Op) error {
	if op.Zone != nil {
		z := Zone{ID: op.Zone.ID, Name: op.Zone.Name, X: op.Zone.X, Y: op.Zone.Y, W: op.Zone.W, H: op.Zone.H}
		if z.ID == "" {
			return errors.New("edit_rejected: zone op requires id")
		}
		w.mu.Lock()
		idx := -1
		for i := range w.Zones {
			if w.Zones[i].ID == z.ID {
				idx = i
				break
			}
		}
		if idx >= 0 {
			w.Zones[idx] = z
		} else {
			w.Zones = append(w.Zones, z)
		}
		w.mu.Unlock()
		return h.store.UpsertZone(w.ID, z)
	}
	if op.ZoneID != "" {
		w.mu.Lock()
		for i := range w.Zones {
			if w.Zones[i].ID == op.ZoneID {
				w.Zones = append(w.Zones[:i], w.Zones[i+1:]...)
				break
			}
		}
		w.mu.Unlock()
		return h.store.DeleteZone(w.ID, op.ZoneID)
	}
	return errors.New("edit_rejected: zone op requires zone payload or zoneId")
}

// persistChunk re-encodes one chunk from the live world and stores it with
// its current revision.
func (h *Hub) persistChunk(w *World, cx, cy int) error {
	w.mu.RLock()
	grid := hmf.Grid{}
	for _, t := range w.Tiles {
		grid[hmf.Key(t.X, t.Y)] = TileID(t.T)
	}
	w.mu.RUnlock()
	chunk := hmf.EncodeChunk(grid, w.Width, w.Height, cx, cy)
	return h.store.SaveChunk(w.ID, cx, cy, w.ChunkRev(cx, cy), hmf.EncodeRLE(chunk))
}

// handleChunkGet answers a chunk fetch (op "chunk_get") with the chunk's
// RLE + decoded tiles + revision — the AOI fetch primitive for refetch+replay
// when a client detects a chunk rev gap.
func (h *Hub) handleChunkGet(sp *SpaceState, c *Client, op *hmf.Op) {
	w := sp.World
	if op.CX < 0 || op.CY < 0 || op.CX*hmf.ChunkSize >= w.Width || op.CY*hmf.ChunkSize >= w.Height {
		c.sendError("chunk_out_of_bounds", "chunk outside world")
		return
	}
	w.mu.RLock()
	grid := hmf.Grid{}
	for _, t := range w.Tiles {
		grid[hmf.Key(t.X, t.Y)] = TileID(t.T)
	}
	rev := w.ChunkRev(op.CX, op.CY)
	w.mu.RUnlock()
	chunk := hmf.EncodeChunk(grid, w.Width, w.Height, op.CX, op.CY)
	rle := hmf.EncodeRLE(chunk)
	cells := hmf.ChunkTileCells(chunk, op.CX, op.CY, w.Width, w.Height)
	tiles := make([]map[string]any, 0, len(cells))
	for _, cell := range cells {
		tiles = append(tiles, map[string]any{"x": cell.X, "y": cell.Y, "tileId": cell.TileID})
	}
	c.emit("chunk", map[string]any{
		"spaceId": w.ID,
		"cx":      op.CX, "cy": op.CY,
		"rev":  rev,
		"rle":  rle,
		"tiles": tiles,
	})
	log.Printf("chunk_get: %s %d,%d rev=%d (%d tiles)", w.ID, op.CX, op.CY, rev, len(tiles))
}

// recordOp allocates the next op seq and appends the op to the op_log.
// Called by handleEdit after a successful apply.
func (h *Hub) recordOp(spaceID string, op *hmf.Op) int64 {
	seq, err := h.store.NextOpSeq(spaceID)
	if err != nil {
		log.Printf("next op seq %s: %v", spaceID, err)
		return 0
	}
	op.Seq = seq
	if err := h.store.AppendOp(spaceID, seq, op); err != nil {
		log.Printf("append op %s: %v", spaceID, err)
	}
	return seq
}

// chunkTimestamp is a stable RFC3339 now (for ack payloads).
func chunkTimestamp() string { return time.Now().UTC().Format(time.RFC3339) }
