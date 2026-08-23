# Hearth

A self-hosted spatial universe. Send someone a link and they're instantly walking around a shared pixel world in their phone browser — no installs, no accounts, no third-party services.

Hearth replaces our WorkAdventure fork with a from-scratch stack: a single Go binary serving a PixiJS PWA, a frozen JSON wire protocol (v0), proximity/space/DM chat, live collaborative map editing, portals between spaces, and voice/video "bubbles" powered by a built-in Pion SFU with Top-K spatial audio. Bots speak the exact same protocol as humans, so AI residents, guides, and event hosts are just peers on the wire. Guest-first by design: a device key gets you in, and raw keys are never stored (only SHA-256 hashes).

```
client/   Vite + TypeScript + PixiJS v8 + Preact  →  PWA world renderer (tap-to-move, chat bottom-sheet, service worker)
server/   Go monolith (:8090)                      →  WS hub · spatial-hash presence · chat · SQLite WAL · guest auth · REST /api/spaces · serves client dist
media/    Go package, pion/webrtc v4 SFU           →  pre-negotiated 12+6+2 subscriber transceivers · Top-K audio · simulcast rungs · screen slots
PROTOCOL.md  frozen wire contract v0 (round+sign to amend)
```

## Product vision

**One link → a world → voice/video bubbles → bots → events.**

- **One link** — open the URL, a guest session is minted, the world renders, tap-to-move works. Day-0 gate (the Khalid test): two phones on the same network — map renders, movement works, voice bubbles <300ms.
- **Mobile-first** — touch-native (tap-to-move, pinch-zoom, drag-to-pan), keyboard-safe `visualViewport` layout, installable PWA, dark UI.
- **Zero third-party tools** — one Go binary, pure-Go SQLite (no CGO), no external SaaS. Auth, chat, editing, media, and bots all run on your box.
- **Spatial audio via SFU** — pre-negotiated 12-audio-slot subscriber connection, Top-K selection with 500ms hysteresis (you never hear yourself), pure RTP selective forwarding — the server never touches audio payloads.
- **Bots** — same WS protocol as humans; MCP (streamable HTTP) tool surface, OpenRouter brains.
- **Events** — 50+ person crowds are a supported mode, not an edge case.

## Architecture (tl;dr)

Hearth ships as **one Go binary** (`server/`) that embeds the built PWA client and exposes `/ws` (gorilla/websocket, JSON envelopes `{v,t,id,ts,d}`), REST (`/api/*`), and static assets on port 8090. The hub keeps entity positions **RAM-only**, indexes them in an 8×8 spatial hash, and broadcasts **12Hz coalesced AOI state** (20-tile radius) per client; durable state lives in **SQLite WAL** (spaces, chunked-JSON maps, users, sessions, messages). **Media** is a Pion v4 SFU library package: each peer gets a publisher PeerConnection (unlimited publishes, renegotiable) and a subscriber PeerConnection offering **20 sendonly m-lines exactly once** (12 Opus audio + 6 VP8 camera + 2 VP8 screen slots — never renegotiated), with Top-K audio routing and untouched RTP fan-out. The **client** is Vite + TS + Preact + PixiJS v8: A* tap-to-move reporting at ~12Hz with monotonic seq, 100ms interpolation buffers for remote players, optimistic edits with server authority, and exponential-backoff reconnect with full resync on welcome.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the full module map, data flows, scaling, and security model.

## Run it

### Server (Go)

```bash
cd server
go run .          # listens on 0.0.0.0:8090, db at data/hearth.db (auto-created + seeded with hearth & garden)
```

Env: `HEARTH_ADDR` (default `0.0.0.0:8090`), `HEARTH_DB` (default `data/hearth.db`). The DB auto-migrates and seeds two spaces, `hearth` and `garden`, connected by mutual portals.

The server serves the client itself: it prefers the live build at `../client/dist` on disk and falls back to the embedded copy (`//go:embed all:dist`). So the fastest full-stack run is:

```bash
cd client && npm install && npm run build   # tsc --noEmit + vite build → client/dist
cd ../server && go run .                    # open http://localhost:8090
```

### Client dev

```bash
cd client
npm install
npm run dev      # Vite dev server; /api and /ws are proxied to the Go server on :8090
```

Run the Go server in another terminal, then open the Vite URL. For client-only work with no Go server, use the zero-dependency mock server:

```bash
npm run mock     # raw RFC6455 mock on :8090: serves dist, welcome + ~4Hz state, chat echo, edit broadcast
```

> **Day-0 gotcha**: the client defaults to space `town-square` (`src/net/config.ts`) but the server seeds `hearth`/`garden` and defaults joins to `hearth`. Until the default is aligned, open with `?space=hearth` (or `?space=garden`), or clear the `hearth:space` localStorage key.

## Test it

| Suite | Command | Status |
|---|---|---|
| Server (protocol, dispatch, rate limits, spatial hash) | `cd server && go vet ./... && go test -count=1 ./...` | ✅ green |
| Media/SFU (loopback: 20 m-lines, Top-K, rungs, screen, zero renegotiation) | `cd media && go test ./...` | ⚠️ **RED** — `TestLoopback` (video subscribe: `media: not found`) and `TestRenegoMinimal`; reproduced 3/3; isolated to pion v4.2.18 not firing `onTrack` for tracks added in a second offer. Fixing this is day-1 work (see roadmap). |
| Client (typecheck + build + PWA precache) | `cd client && npm run build` | ✅ green |
| Client smoke vs mock | `npm run mock` then `node scripts/ws-smoke.mjs` | raw RFC6455 |
| Wire e2e (join → move → state → chat space+dm → rate limit → portal) | `node test-ws.mjs` (Node ≥ 21, `WS_URL` override) | against a live server |
| REST (health / static / guest auth / me / spaces) | `./verify-rest.sh` | against `127.0.0.1:8090` |

## Roadmap

The converged master plan, day by day. Scope shrinks by deferral, never by deletion (see pillars).

- **Days 1–2 — Voice/video.** Make `TestRenegoMinimal` pass (pion v4.2.18 won't fire `onTrack` for second-offer tracks — pin/patch/upgrade pion until a plain answerer sees both tracks), then `TestLoopback`'s video subscribe. Both green 3/3 before any wiring. Replace the `handleMedia` pass-through relay in `ws.go` with real `media.Events()` + `HandleSignal` integration; client publishes mic/camera/screen on the publisher PC, auto-subscribes Top-K audio, binds camera slots 0–5. **Gate: the Khalid test** — two phones on the same LAN: map renders, tap-to-move, voice bubbles <300ms.
- **Day 3 — Chat.** History backfill endpoint off the existing `messages` table (per space/channel, paginated); DM tab in the ChatSheet (protocol + server already support `dm`; the UI is prox/space only today); surface `error` envelopes (code `RATE`, 2048-byte cap) as toasts; align the session cookie to PROTOCOL.md's 90-day sliding (code uses 30d MaxAge).
- **Day 4 — Editor.** Implement `erase | place | zone | portal` ops in `handleEdit` (today: tile paint only → `SetTile` + `SaveWorld` + broadcast); client undo stack ≥50 ops (protocol requirement); zone brush; portal placement writing mutual pairs (reuse the `garden-east/west` pattern in `world.go`). Server-authoritative, bounds-checked, 20Hz AOI-batched broadcast.
- **Day 5 — Bots.** Bot host speaking the identical WS protocol (`join/move/chat/bot_msg` dispatch already exists; ambient wisps `Wisp`/`Ember` tick every 2s today); MCP streamable-HTTP server exposing hearth tools (move, chat, edit, portal); OpenRouter brain per persona. Acceptance: a bot is indistinguishable from a human on the wire.
- **Day 6 — Push & PWA hardening.** Web Push: subscription REST endpoint, SW push handler (vite-plugin-pwa autoUpdate is already in place), notify on DM/@mention when the tab is hidden, badge sync with the unread counter. Verify backgrounded position persistence (2–5s batch) and reconnect-resync under real mobile network loss.
- **Day 7 — Benchmark gate.** Harness: N bot clients over real WS at 12Hz moves + token-bucket-ceiling chat; N simultaneous SFU publishers. Metrics: entities/space, hub tick CPU, mem/client, `sendQueue` drop count, chat msg/s, SFU track count, voice p50/p95 (<300ms LAN), 50-crowd soak. Publish numbers vs the **WorkAdventure baseline** and the **BAWES universe** — this gate blocks release.

## Design pillars (hard rules)

1. **No feature cuts. Ever.** Scope shrinks by deferral, not deletion.
2. **Screen sharing ≥ current.** 2 screen slots, newest-wins with oldest-eviction (`reslotScreens`), recency ordering. Any change may only increase capacity or quality.
3. **Voice never drops.** The subscriber PC is offered once and never renegotiated; Top-K changes re-route sinks silently; 500ms hysteresis kills flapping; peer loss triggers `removePeerTracks` re-routing, not teardown.
4. **Unlimited publishers.** The publisher PC always accepts renegotiation; display is adaptive — there is no cap language anywhere in code or docs.
5. **50+ crowd events are a first-class mode**, validated by the day-7 soak, not a best-effort case.
6. **Benchmark gate.** Releases must meet-or-beat WorkAdventure and the BAWES universe on the day-7 suite.
7. **PROTOCOL.md is frozen.** Changes require round+sign; server and tests (`protocol_test.go`, `dispatch_test.go`) enforce every type.
8. **Mobile-first, zero third-party, guest-first.** Single binary, pure-Go deps, hashed device keys only — no raw secrets at rest.

## Layout

```
PROTOCOL.md        frozen wire contract v0
server/            Go monolith (main.go, ws.go, hub.go, chat.go, world.go, db.go) + tests
media/             Go SFU library (media.go, peer.go, room.go, topk.go, track.go, signal.go, config.go) + tests
client/            Vite + TS + Preact + PixiJS v8 PWA (src/net, src/world, src/ui) + scripts
test-ws.mjs        wire e2e (two clients)
verify-rest.sh     REST checks
docs/              ARCHITECTURE.md, DEVELOPMENT.md
```

See [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for the dev loop, the TDD rule, and the verification bar.
