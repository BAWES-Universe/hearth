# Hearth Editor (v2)

The in-browser world editor for Hearth spaces. T1 shipped the tile-based
editor (Play / Paint / Portal + objects, palette brushes, undo, live
broadcast, publish). T2 (this document) grows it with three additive
capabilities on top of the same frozen HMF v1 op stream:

1. **Freeform drawing** — drag to paint a continuous pixel-level stroke.
2. **Custom asset upload** — upload an image, place it in a world like a tile.
3. **Animated tiles** — a small server-authoritative set of frame sequences.

PROTOCOL.md is untouched. Every T2 feature rides additive envelopes/fields;
old clients ignore what they don't know and keep working.

## Editor modes

| Mode | What it does |
|---|---|
| Play | Move, pinch-zoom, drag-pan. Portal proximity auto-walk-through. |
| Paint | Tap **or drag** a tile brush. Dragging accumulates a freeform stroke (Bresenham-continuous, deduped) and sends it as ONE batch op on pointer release. Eraser (✕) erases by stroke too. Amber corner marks = tiles you painted. |
| Portal | Tap a tile to drop a portal (target town-square). |
| Objects | Tap a tile to place a functional object (door / npc / sign / light). |
| Assets | Tap a tile to place a user-uploaded image; 🗑 remove mode deletes a placed asset; ⬆ uploads a new image. |

Guests (no edit entitlement) see a read-only tag; the server enforces the
same gate on every edit op regardless of UI state.

## Freeform strokes (wire shape)

A stroke is a batch paint/erase op. Cells carry no tile id on the way in —
the op-level `tileId` is the fallback:

```json
{ "t": "edit", "d": { "op": "paint", "tileId": 3, "cells": [ { "x": 4, "y": 5 }, { "x": 5, "y": 5 } ] } }
{ "t": "edit", "d": { "op": "erase", "cells": [ { "x": 4, "y": 5 } ] } }
```

The server acks/broadcasts the applied cells with per-cell prior tiles so
clients can undo a whole stroke with one compensating inverse op:

```json
{ "t": "edit", "d": { "op": "paint", "by": "...", "applied": true,
  "cells": [ { "x": 4, "y": 5, "tileId": 3, "priorTileId": 0 } ] } }
```

Undo of a stroke sends the inverse as cells with **per-cell** `tileId`
(prior tile, 0 = floor). The server treats the presence of the per-cell
key as authoritative for that cell — this is what lets a single inverse op
restore a stroke that crossed cells with different prior tiles:

```json
{ "t": "edit", "d": { "op": "paint", "cells": [ { "x": 4, "y": 5, "tileId": 0 }, { "x": 5, "y": 5, "tileId": 1 } ] } }
```

Undo stack: 100 entries, compensating inverse ops (never replays).

## Custom asset upload

REST surface (session-authenticated; edit entitlement required — same gate
as edit ops):

| Method | Route | Description |
|---|---|---|
| POST | `/api/worlds/{id}/assets` | multipart `file` (png/jpeg/gif/webp, ≤512KB) + optional `name`. Stores bytes in SQLite (`world_assets`), returns the asset record `{id, name, mime, w, h, url}`. |
| GET | `/api/assets/{id}` | Serves the stored image bytes (worlds are public-readable; ids are unguessable uuids, uploads are world-scoped). |
| GET | `/api/worlds/{id}/assets` | Registry listing — the client loads this on world entry so the palette survives reload. |

Placement rides the frozen edit op stream as an additive op kind — server
arbitrated, persisted (`world_asset_placements`), appended to the op log,
and broadcast to everyone in the space:

```json
{ "t": "edit", "d": { "op": "asset", "asset": { "assetId": "<uuid>", "x": 4, "y": 5 } } }
{ "t": "edit", "d": { "op": "asset", "asset": { "assetId": "<uuid>", "x": 4, "y": 5, "remove": true } } }
```

World docs carry `assets: [{assetId, name, url, x, y}]` (denormalized at
load/place time) so clients render placements without a registry fetch.
Undo of a placement is the compensating remove op (and vice versa).

## Animated tiles

The server is authoritative on WHICH tiles animate and how (world doc
`anims` field): water (4f @4fps), lava (4f @5fps), torch (4f @7fps), glow
(6f @5fps). HMF v1 chunks still store plain tile ids — animation is a
derived render property, so the map format is untouched. The client
generates the frame textures deterministically and cycles them in the rAF
loop at the server-declared rate.

## Server authority

Every edit is server-arbitrated (LWW by arrival order), persisted to the
op log, and broadcast. Chunk revs bump per touched chunk; a rev mismatch
makes clients refetch + replay. The publish flow (draft → directory) is
unchanged — a blank canvas can still go from zero to published in under a
minute.

## Where the code lives

- `client/src/App.tsx` — editor state, pointer-stroke handlers, upload dialog
- `client/src/ui/EditToolbar.tsx` — mode dock + palettes (tiles, objects, assets)
- `client/src/world/tiles.ts` — palette defs, procedural textures, anim frames
- `client/src/world/renderer.ts` — tile/asset layers, animated-tile ticking
- `client/src/net/protocol.ts` / `api.ts` — edit envelopes + asset REST calls
- `server/assets.go` — asset upload/serve/place/remove + registry
- `server/hmf/hmf.go` — batch op semantics (per-cell undo), animated-tile table
- `server/hmf_ops.go` / `chat.go` — op routing, validation, broadcast
- `server/db.go` — `world_assets` + `world_asset_placements` tables
- `server/t2_editor_test.go` — stroke round-trip/undo, anims table, upload→place→broadcast→remove
- `editor-test.cjs` (repo root) — live-box stream test
