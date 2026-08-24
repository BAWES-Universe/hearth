# MCP v1 — Model Context Protocol endpoint for AI agents

> **Status: new in T2 (2026-08-24).** Additive HTTP route on the existing
> :8090 listener. PROTOCOL.md (frozen wire contract) is untouched — MCP rides
> a separate JSON-RPC transport (`POST /mcp`) and reuses the S7 bot op-log
> contract (`docs/BOT-PROTOCOL.md`) for every mutation.

`/mcp` exposes Hearth to AI agents (Claude Desktop, custom MCP clients, ...)
over the **Model Context Protocol streamable HTTP transport**
(spec revision `2025-06-18`). Agents can inspect published worlds, read their
state, paint/erase/place tiles, send chat as a bot account, see who is in a
space, run multi-op BotScripts, and read the audit feed.

The endpoint is served by the Hearth monolith itself (one binary, `POST /mcp`
on the existing `:8090` listener) — no separate process, no SDK dependency
(stdlib JSON-RPC 2.0).

---

## 1. Pointing an MCP client at Hearth

Any MCP client that speaks the streamable HTTP transport:

```
URL:  http://<host>:8090/mcp
```

For the BAWES box: `http://51.75.74.214:8090/mcp`.

Claude Desktop (claude_desktop_config.json):

```json
{
  "mcpServers": {
    "hearth": {
      "type": "http",
      "url": "http://51.75.74.214:8090/mcp"
    }
  }
}
```

Plain curl also works (stateless requests are tolerated; a session id is
issued on initialize and carried via `Mcp-Session-Id`):

```bash
# initialize (issues Mcp-Session-Id)
curl -s -X POST http://127.0.0.1:8090/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}'

# tools/list
curl -s -X POST http://127.0.0.1:8090/mcp \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
```

Transport notes (streamable HTTP per the spec):

- `initialize` → the response carries a `Mcp-Session-Id` header; subsequent
  requests should echo it. A presented-but-unknown session id gets `404`.
  Requests without a session id are processed anyway (stateless-tolerant).
- Notifications (`notifications/initialized`, id-less requests) are answered
  `202 Accepted` with an empty body.
- `DELETE /mcp` with `Mcp-Session-Id` terminates the session.
- `GET /mcp` returns `405` (no server→client SSE stream is offered).
- Responses honour the client's `Accept`: `application/json` by default,
  `text/event-stream` when requested.

## 2. Tools

| Tool | What it does | Auth |
|---|---|---|
| `worlds.list` | Published-worlds directory (gravity-ranked): id, name, owner, headcount, gravity. Optional `q` name filter. | read-only, none |
| `world.read` | Full HMF v1 world doc: tiles (x, y, `t` name + `tileId`), chunk rev summary, zones, portals, spawn, palette. Drafts are invisible (404-style error). | read-only, none |
| `world.edit` | Paint/erase/place ONE tile through the bot op-log. Returns `applied` / `deduped` + `seq` + audit attribution (`actor` = sha256(deviceKey), `audit` = `role=bot`). | `deviceKey` required |
| `world.chat` | Send a chat message as the bot account into a space (`space` or `proximity` channel). Confirms delivery via the bot's own echo. | `deviceKey` required |
| `presence.list` | Live occupants of a space (players + ambient bots): id, name, x, y, dir, bot flag, avatar, userId. | read-only, none |
| `bot.run` | Execute a full multi-op BotScript (paint/erase/place + rect/ring/line sugar) through the bot op-log. Returns per-run applied/deduped + seqs. | `deviceKey` required |
| `world.activity` | Recent append-only audit feed for a world. Bot/MCP ops appear as `kind=edit, role=bot` rows with actor + idem key. | read-only, none |

### Examples

```bash
# list published worlds
{"jsonrpc":"2.0","id":3,"method":"tools/call",
 "params":{"name":"worlds.list","arguments":{}}}

# read garden's full state
{"jsonrpc":"2.0","id":4,"method":"tools/call",
 "params":{"name":"world.read","arguments":{"worldId":"garden"}}}

# paint a flower at (2,2) in garden as bot-bricky (idempotent run)
{"jsonrpc":"2.0","id":5,"method":"tools/call",
 "params":{"name":"world.edit","arguments":{
   "worldId":"garden","op":"paint","x":2,"y":2,"tile":"flower",
   "deviceKey":"bot-bricky","runId":"bricky-2026-08-24"}}}
# -> {"applied":true,"deduped":false,"seq":42,"actor":"<sha256>","audit":"role=bot","runId":"bricky-2026-08-24"}

# replay the SAME runId -> {"applied":false,"deduped":true,"seq":42,...} (no-op)

# build a 5x3 wall rectangle
{"jsonrpc":"2.0","id":6,"method":"tools/call",
 "params":{"name":"bot.run","arguments":{
   "world":"garden","deviceKey":"bot-bricky","runId":"wall-1",
   "ops":[{"op":"rect","x":10,"y":10,"w":5,"h":3,"tile":"wall"}]}}}

# say hello in garden
{"jsonrpc":"2.0","id":7,"method":"tools/call",
 "params":{"name":"world.chat","arguments":{
   "worldId":"garden","text":"hello from the agent","deviceKey":"bot-bricky"}}}

# who is in garden right now
{"jsonrpc":"2.0","id":8,"method":"tools/call",
 "params":{"name":"presence.list","arguments":{"worldId":"garden"}}}

# audit feed
{"jsonrpc":"2.0","id":9,"method":"tools/call",
 "params":{"name":"world.activity","arguments":{"worldId":"garden","limit":20}}}
```

## 3. Auth — bot deviceKey (S7 convention)

Mutating tools (`world.edit`, `world.chat`, `bot.run`) authenticate with a
**bot deviceKey**, exactly like the S7 headless bot builder
(`docs/BOT-PROTOCOL.md`):

- A deviceKey is just a stable string; convention is `bot-<name>`
  (e.g. `bot-bricky`). There is **no secret to generate** — the account row
  is upserted on first use and every action attributes to
  `actor = sha256(deviceKey)` (raw keys are never stored, same rule as
  humans).
- To mint one: pick `bot-<name>`, pass it as `deviceKey`. Keep the same key
  across runs so all of an agent's builds attribute to one account.
- The key is passed **per tool call** (not per session), so different agents
  can share a server with distinct audit identities.

Authorization (mirrors the WS editor gate, `server/chat.go canEditSpace`):

- **Showcase worlds** (garden/lab/hall/town-square) are the shared
  co-authoring hub — any authenticated bot may edit them.
- **User-owned worlds** — only the bot whose deviceKey maps to the world's
  owner account may edit; everyone else gets `edit_forbidden`.

## 4. Safety notes

- **Idempotent by construction**: every op carries an idem key
  `"<runId>:<idx>"`; replaying the same runId is a no-op (server acks
  `deduped:true` with the original seq — unique partial index on
  `op_log(space_id, idem)`). Agent retries are free and safe.
- **Append-only + audited**: every mutation flows through the same frozen
  `edit` envelope and op_log as humans, attributed to the bot account; idem
  ops additionally emit `activity_events` rows with `role=bot`
  (visible via `world.activity` and `GET /api/worlds/{id}/activity`).
- **Reads are scoped**: `worlds.list` / `world.read` only ever expose
  **published** worlds; drafts are invisible to MCP.
- **Strict palette**: unknown tile names are rejected, never silently mapped
  to floor (a typo cannot turn a paint into an erase).
- **Rate limits / size caps**: chat text ≤ 2048 bytes; request body ≤ 1 MiB;
  MCP sessions idle-expire after 24h (registry capped at 256).
- **No protocol change**: PROTOCOL.md and the WS wire are untouched; MCP is a
  purely additive HTTP surface reusing the existing op-log.

## 5. Implementation

- `server/mcp/mcp.go` — the streamable-HTTP JSON-RPC server (stdlib only):
  `initialize`, `ping`, `tools/list`, `tools/call`, notifications, session
  registry, SSE/JSON response negotiation, tool schemas + validation.
- `server/mcp_adapter.go` — `mcp.Backend` implementation against the
  monolith: read tools via store/hub APIs; mutating tools spawn an in-process
  `BotClient` (`server/bot.go`) that dials this server's own `/ws`, so op
  semantics are reused, never reimplemented.
- `server/main.go` — mounts `POST /mcp` on the existing listener.
- `server/mcp_test.go` — `TestMCPRoundTrip`: real in-process round-trip
  (initialize → tools/list → world.edit mutates a world through the op-log,
  audit row shows `role=bot`, replay dedupes, session/SSE/404 semantics).

Verification: `cd server && go vet ./... && go test ./...` (green), plus the
box e2e (`e2e-test.cjs` 4/4) after deploy.
