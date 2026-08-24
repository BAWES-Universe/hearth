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
	Op      *hmf.Op
	Cells   []hmf.CellChange
	Chunks  []ChunkInfo
	Err     string
	Deduped bool // replay-safe skip: an op with the same idem key was already applied
}

// applyEditOp applies one frozen editor op to a space's world (server-
// arbitrated, LWW by arrival order) and persists the deltas. The returned
// ack is broadcast to everyone in the space, including the acting client —
// the client editor uses priorTileId/chunk revs for undo + refetch/replay.
// Authorization (ownership) is enforced by handleEdit BEFORE calling this.
func (h *Hub) applyEditOp(sp *SpaceState, c *Client, op *hmf.Op) *EditAck {
	w := sp.World
	op.SpaceID = w.ID
	op.By = c.Entity.ID
	if c.Session != nil {
		// Actor = the account the op is attributed to in the audit trail
		// (user id = sha256(deviceKey)); bots are attributed to their bot
		// account (docs/BOT-PROTOCOL.md).
		op.Actor = c.Session.UserID
	}
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
	case "object":
		if err := h.applyObjectOp(w, op); err != nil {
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

	// Build the world grid ONCE per op and reuse it for every touched chunk —
	// the previous path rebuilt the full grid again inside each persistChunk.
	changes, grid, err := h.applyGridOp(w, op)
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
		if err := h.persistChunk(w, ci.CX, ci.CY, grid); err != nil {
			log.Printf("persist chunk %s %d,%d: %v", w.ID, ci.CX, ci.CY, err)
		}
	}
	return ack
}

// applyGridOp applies a paint/erase/place op to the world tile map (sparse
// names in RAM; hmf.Grid conversion for the pure op semantics). It returns
// the changed cells AND the post-apply grid snapshot, so the caller can
// persist every touched chunk without rebuilding the grid per chunk.
func (h *Hub) applyGridOp(w *World, op *hmf.Op) ([]hmf.CellChange, hmf.Grid, error) {
	if op.Op != "erase" && op.TileID != hmf.FloorTile && hmf.TileName(op.TileID) == "" {
		return nil, nil, errors.New("edit_rejected: unknown tileId " + itoa(op.TileID))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	grid := hmf.Grid{}
	for _, t := range w.Tiles {
		grid[hmf.Key(t.X, t.Y)] = TileID(t.T)
	}
	changes, err := hmf.ApplyOp(grid, w.Width, w.Height, op)
	if err != nil {
		return nil, nil, errors.New("edit_rejected: " + err.Error())
	}
	if len(changes) == 0 {
		return nil, grid, nil
	}
	for _, ch := range changes {
		w.setTileLocked(ch.X, ch.Y, hmf.TileName(ch.TileID))
	}
	return changes, grid, nil
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
		// same bounds validation the portal path applies: a zone with
		// negative coords or a rect larger than the world must not reach
		// the world document and every client.
		if z.X < 0 || z.Y < 0 || z.W <= 0 || z.H <= 0 ||
			z.X+z.W > w.Width || z.Y+z.H > w.Height {
			return errors.New("edit_rejected: zone outside map bounds")
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

// applyObjectOp upserts or removes a functional object (door|npc|sign|light)
// — RAM (World.Objects) + world_entities row. Object ids are client-chosen
// (or server-generated when empty); kinds are validated against the frozen
// object palette (hmf.ValidObjectKind).
func (h *Hub) applyObjectOp(w *World, op *hmf.Op) error {
	if op.Object != nil {
		o := WorldObject{
			ID: op.Object.ID, Kind: op.Object.Kind, X: op.Object.X, Y: op.Object.Y,
			Name: op.Object.Name, Text: op.Object.Text, Data: op.Object.Data,
		}
		if o.ID == "" {
			o.ID = "obj-" + randHex(6)
		}
		if !hmf.ValidObjectKind(o.Kind) {
			return errors.New("edit_rejected: unknown object kind " + o.Kind)
		}
		if o.X < 0 || o.Y < 0 || o.X >= w.Width || o.Y >= w.Height {
			return errors.New("edit_rejected: object outside map bounds")
		}
		w.UpsertObject(o)
		return h.store.UpsertObject(w.ID, o)
	}
	if op.ObjectID != "" {
		w.DeleteObject(op.ObjectID)
		return h.store.DeleteObject(w.ID, op.ObjectID)
	}
	return errors.New("edit_rejected: object op requires object payload or objectId")
}

// persistChunk re-encodes one chunk from a pre-built grid snapshot and stores
// it with its current revision. The grid is built once per op by applyGridOp
// and shared across all touched chunks (was rebuilt per chunk before).
// Callers must pass a POST-apply snapshot: the rev-guard in SaveChunk
// (excluded.rev > map_chunks.rev) keeps a stale lower-rev write from ever
// clobbering a fresher one when two edits race on the same chunk.
func (h *Hub) persistChunk(w *World, cx, cy int, grid hmf.Grid) error {
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
	// Read the revision under the lock we already hold: w.ChunkRev() takes
	// its own RLock, and sync.RWMutex is not reentrant — a writer waiting
	// between the two RLock calls would deadlock both goroutines.
	rev := w.ChunkRevs[chunkKey(op.CX, op.CY)]
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
		"rev":   rev,
		"rle":   rle,
		"tiles": tiles,
	})
	log.Printf("chunk_get: %s %d,%d rev=%d (%d tiles)", w.ID, op.CX, op.CY, rev, len(tiles))
}

// chunkTimestamp is a stable RFC3339 now (for ack payloads).
func chunkTimestamp() string { return time.Now().UTC().Format(time.RFC3339) }
