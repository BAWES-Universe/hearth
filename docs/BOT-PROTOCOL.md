# Bot Protocol — the agent-facing op-log contract (v1)

> **Status: frozen as of 2026-08-23 (S7).** This is the agent-facing contract
> for the Hearth editor op-log. It extends the wire envelope of
> `PROTOCOL.md` (frozen) and the HMF v1 editor ops of `docs/HMF-v1.md`
> (frozen). Nothing here changes what humans send; it **adds** the fields and
> conventions that make the op-log addressable by bots/agents: identity
> (`deviceKey`), op schema, idempotency keys, audit attribution, and
> undo-compat.

A headless bot is just a client: it authenticates with a `deviceKey`, joins a
live world over the **same `/ws` path humans use**, and writes
`paint`/`place`/`erase` ops through the **same `edit` envelope humans use**.
The server cannot tell a bot apart from a human at the protocol level — that
is the point. Bots are distinguished in the audit trail by their account
(user id = `sha256(deviceKey)`) and by the `idem` keys they carry.

---

## 1. Transport & envelope

WebSocket, JSON text frames, the frozen envelope both directions:

```json
{"v":1, "t":"<type>", "d":{...}}
```

Join path: `ws://<host>:8090/ws?deviceKey=<bot-key>` (the query param is
accepted at handshake, exactly like a browser client), then:

```json
{"t":"join","d":{"deviceKey":"<bot-key>","name":"Bricky","spaceId":"garden"}}
```

Server replies `welcome` with `selfId` (your entity/session id) and the world
doc (`world`). **`selfId` is the value you must match in edit acks** (`by`).

## 2. Auth — bot identity

- **`deviceKey`** is the bot account. The account id used everywhere in the
  audit trail is `sha256(deviceKey)` hex, first 32 chars. Raw keys are never
  stored (same rule as humans).
- Convention: bot keys are prefixed `bot-` (e.g. `bot-bricky`). A bot keeps
  one stable `deviceKey` so **all of its runs attribute to the same account**.
- The bot's display `name` is upserted on the account row at join.

## 3. Ops — the same frozen HMF v1 stream

Send:

```json
{"t":"edit","d":{
  "op":"paint",            // paint | erase | place | zone | portal | publish | chunk_get
  "x":16,"y":20,           // single-cell target
  "tileId":14,             // numeric palette id (0=floor) — or "tile":{"t":"roof"}
  "idem":"build-1:0",      // idempotency key, optional for humans, REQUIRED for bots
  "undoOf":0               // set when compensating an earlier op (human undo)
}}
```

Batch ops (the editor's rect/fill/line tools) send `cells` instead of a
single x/y:

```json
{"t":"edit","d":{"op":"paint","cells":[{"x":16,"y":20},{"x":17,"y":20}],
  "tileId":14,"idem":"build-1:1"}}
```

### Palette (frozen, 20 tiles)

`floor0 wall1 water2 grass3 stone4 sand5 path6 wood7 lava8 ice9 flower10
bush11 rock12 tree13 roof14 door15 fence16 bridge17 crystal18 dirt19`.
`0` = floor is implicit and never stored. Passable set: floor/grass/stone/
sand/path/wood/ice/flower/dirt/bridge. Full semantics: `docs/HMF-v1.md`.

### Ack (server → everyone in the space, including the bot)

```json
{"t":"edit","d":{
  "op":"paint","by":"<selfId>","seq":42,"spaceId":"garden",
  "applied":true,
  "x":16,"y":20,"tileId":14,"priorTileId":3,   // single cell
  "chunks":[{"cx":1,"cy":1,"rev":4}],
  "idem":"build-1:0"                            // echoed when the client sent one
}}
```

- `by` = the acting entity id — **match `by == selfId`** to correlate your own
  ops (the space is shared; other actors' acks arrive too).
- `applied:true` = the op was applied and appended to the op_log.
- `priorTileId` = the tile that was there before (the undo hook, §6).
- Batch acks carry `cells:[{x,y,tileId,priorTileId}]` instead of x/y.
- `deduped:true` (§4) = replay-safe skip; `seq` is the **original** seq.

## 4. Idempotency keys — replay-safe builds

- Every bot op MUST carry `idem = "<bot_run_id>:<op_index>"`.
- `bot_run_id` is chosen by the caller and must be **stable across replays**
  (e.g. `demo-2026-08-23`). `op_index` is the op's 0-based position in the
  script.
- The server keeps at most **one op per (space, idem)** (unique partial index
  on `op_log(space_id, idem)`). When an op with the same key already exists:
  - it is **not** re-applied, **not** re-appended,
  - the ack comes back `applied:true, deduped:true` with the original `seq`.
- Consequence: **re-running a script with the same `run_id` is a no-op** —
  the structure is not double-painted and the op_log does not grow. Re-running
  with a different `run_id` is a fresh build (LWW: painting the same tile with
  the same tile is itself a no-op).
- Idempotency also makes agent retries safe: a bot that crashes mid-build can
  reconnect and replay the whole script; the already-applied prefix acks
  `deduped`, the rest applies.

## 5. Audit attribution

Two append-only trails record who built what, both keyed to the bot **account**
(user id = `sha256(deviceKey)`), never to a raw key:

1. **`op_log`** (per space, `(space_id, seq)` PK) — the full op stream. Each
   op payload carries `by` (entity/session id) and **`actor`** (account id),
   plus `idem`. This is the build history: replay it to rebuild any world.
2. **`activity_events`** (audit feed, `GET /api/worlds/{id}/activity`) — ops
   carrying an `idem` key emit one append-only row per op:
   `kind:"edit", role:"bot", actor:<account id>, action:<op>, target:<world>,
   diff:{seq,idem,x,y,tileId,cells}`.

Human edits (no `idem`) write op_log with `actor` set but do **not** emit
activity rows (unchanged behaviour — S7 only wires the agent path).

## 6. Undo-compat — human undo works on bot ops

Bot ops live in the **same op stream** and their acks carry `priorTileId`
exactly like human ops, so any client can compensate them with the standard
inverse op:

```json
{"t":"edit","d":{"op":"paint","x":18,"y":24,
  "tileId":<priorTileId>,      // e.g. 1 (wall) — restore what was there
  "undoOf":<bot op seq>}}
```

The server applies the inverse as a normal op (LWW) and records `undoOf` in
the op_log — no special undo machinery server-side. Verified by
`TestHumanUndoOfBotOp`: a human observer receives the bot's ack, sends the
inverse, and the tile returns to its prior state.

## 7. Bot op-sequence scripts (JSON)

The bot client consumes a small script format — ordered ops, both tile forms,
batch sugar expanded client-side into frozen ops:

```json
{
  "v": 1,
  "name": "Bricky",
  "world": "garden",
  "ops": [
    {"op":"rect", "x":16, "y":20, "w":5, "h":2, "tile":"roof"},
    {"op":"rect", "x":16, "y":22, "w":5, "h":3, "tile":"wall"},
    {"op":"paint","x":17, "y":23, "tile":"flower"},
    {"op":"paint","x":18, "y":24, "tile":"door"},
    {"op":"paint","x":28, "y":25, "tileId":10}
  ]
}
```

- `op`: `paint|erase|place` (frozen wire ops) or `rect|ring|line` (sugar:
  expanded to a single batch `paint` op with `cells`).
- `tile` (palette name) and `tileId` (numeric) are interchangeable.
- Each op gets `idem = "<run_id>:<index>"` automatically.

## 8. Running a bot

### CLI (the headless client)

```bash
# built-in demo: 5x5 house + heart of flowers in garden (13 ops)
hearth-server bot build -demo -url ws://127.0.0.1:8090/ws -run-id demo-1

# custom script against the live box
hearth-server bot build -script script.json -world garden \
  -url ws://51.75.74.214:8090/ws -run-id demo-2 -name Bricky -device-key bot-bricky

# replay-safe: re-running demo-1 acks every op deduped:true
```

Flags: `-world` `-script` `-demo` `-name` `-device-key` `-run-id` `-url`
`-interval` `-timeout`. Exit 0 = all ops applied/deduped.

### REST (in-process spawn)

```bash
curl -X POST http://<host>:8090/api/bots \
  -H 'Content-Type: application/json' \
  -d '{"name":"Bricky","world":"garden","action":"build","runId":"api-1"}'
# -> 202 {"ok":true,"status":"spawned","bot":{"runId":"api-1",...}}

curl http://<host>:8090/api/bots            # status list (newest first)
curl http://<host>:8090/api/bots/api-1      # one run status
```

### Programmatic (Go)

`NewBotClient(cfg).Run()` — used by the test suite
(`server/bot_test.go`): build → tiles persisted in `GET /api/spaces/{id}`,
op_log + activity attribute the bot account, replay dedupes, human undo
restores prior tiles.

## 9. Verification (acceptance)

- `go build ./... && go vet ./... && go test ./...` (server) — green.
- `bot_test.go`: `TestBotBuildsHouseInGarden` (spawn → N tiles persisted →
  audit attributes bot), `TestBotReplayIsIdempotent` (replay-safe),
  `TestHumanUndoOfBotOp` (compensating inverse restores prior tile),
  `TestBotScriptParse`, `TestBotSpawnAPI`.
- Live demo on the box: `hearth-server bot build -demo` then
  `curl http://127.0.0.1:8090/api/spaces/garden` shows the house/heart tiles
  and `GET /api/worlds/garden/activity` shows `kind:"edit", role:"bot"` rows.
