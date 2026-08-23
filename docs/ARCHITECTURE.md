# Hearth Architecture

Hearth is a single-binary spatial universe: a Go monolith (`server/`) that owns HTTP + WebSocket + presence simulation + chat + SQLite persistence + guest auth + REST, serves the built PWA client, and hosts a Pion-based SFU library (`media/`) that will carry all voice/video once integrated. The client (`client/`) is a Vite + TypeScript + Preact + PixiJS v8 PWA. All realtime communication runs over one WebSocket per client using the frozen JSON protocol in [PROTOCOL.md](../PROTOCOL.md).

```
Browser (PWA: PixiJS renderer + Preact UI)
   │  HTTPS /api/*         WSS /ws (JSON envelopes)          WebRTC (DTLS-SRTP)
   ▼                          ▼                                ▼
┌─────────────────────────────────────────────┐   ┌──────────────────────┐
│ server/ (Go, :8090)                         │   │ media/ (Pion SFU lib)│
│ mux · WS hub · spatial hash · chat · auth   │◄──┤ rooms · peers · Top-K│
│ SQLite WAL · REST · serves client/dist      │   │ (signaling via WS)   │
└─────────────────────────────────────────────┘   └──────────────────────┘
```

## 1. Module map

### `server/` — Go monolith (module `hearth`, Go 1.22; deps: gorilla/websocket, modernc.org/sqlite — pure Go, no CGO)

| File | Role | Key facts |
|---|---|---|
| `main.go` | Process shell | `//go:embed all:dist` fallback page; `HEARTH_ADDR` (default `0.0.0.0:8090`), `HEARTH_DB` (default `data/hearth.db`); routes `/api/health`, `/api/auth/guest`, `/api/me`, `/api/spaces`, `/api/spaces/{id}`, `/ws`, `/`; `serveClient` prefers `../client/dist` on disk, else embedded; access-log middleware with a `statusRecorder` that forwards `Hijacker/Flusher/Pusher` so WS upgrades and streaming work; graceful shutdown on SIGINT/SIGTERM; version `0.1.0`. |
| `hub.go` | Presence engine | `Entity` (positions **RAM-only**, never persisted), immutable `EntitySnap` copies; `SpatialHash` (8×8 cells) with `Nearby(x,y,radius)` AOI queries; `SpaceState` per space (entities map + hash); `Hub` owns spaces/clients, register/unregister channels, a 12Hz `broadcastStates` tick with per-client coalescing (`shouldSendState`: send only when the AOI entity set changed **or** the 1s heartbeat elapsed), and a 2s bot tick moving ambient wisps (`botsPerSpace = 2`, names `Wisp`/`Ember`). `sendQueueSize = 256` per client; full queues drop frames (next tick retries). |
| `ws.go` | Transport + dispatch + auth + REST | Upgrader accepts any origin (dev). `Client` = WS conn + 256-deep send chan + `TokenBucket` rate limiter + last-known position/entity set for coalescing. Read pump: 64KB read limit, 70s read deadline, ping every 30s, pong handler. Write pump: 10s write deadline. `resolveAuth` at handshake: existing `hearth_session` cookie wins, else `?deviceKey=` creates a user + session and sets the cookie (HttpOnly, SameSite=Lax, MaxAge 30d). Dispatch switch handles `join/move/chat/edit/portal/signal/media/bot_msg/ping`. `handleJoin` defaults space to `hearth`, entity ID = session ID, name falls back to `Guest-<4hex>`, `welcome` carries the world GeoJSON. `handleMedia` is a **pass-through relay** (comment: "media/ package integrates later"). REST: `POST /api/auth/guest` (deviceKey+name → sessionId/userId/name + cookie), `GET /api/me`, `POST /api/spaces` (create, dims clamped 32–512), `GET /api/spaces` (list with live entity counts), `GET /api/spaces/{id}` (world doc + live entities), `GET /api/health` (ok/service/version/uptime/spaces/clients/entities/bots/sessions). |
| `chat.go` | Chat + edits | `TokenBucket{capacity:5, rate:0.5/s}` = burst 5/10s, sustained 1/2s. `handleChat` validates channel `proximity|space|dm`, non-empty text, ≤2048 bytes, rate; routes: `space` → `BroadcastToClients`, `proximity` → only clients inside AOI radius 20, `dm` → `findEntity(target)`; every message persisted via `InsertMessage`. `handleEdit`: x/y required, tile `{t: "floor"|…}`, bounds-checked, `SetTile` + `SaveWorld` + broadcast `edit {x,y,tile,by}` (paint only today). |
| `world.go` | World model | Sparse tile map (only non-floor tiles stored); `Zone`, `Portal`, `Spawn`; `GeoJSON()` = static world doc; `WorldJSON()` = world + live entities; `defaultWorld` seeds 32×32 `hearth` + `garden` with border/interior walls and **mutual portals** (hearth has `garden-east`@x4 and `garden-west`@x27; garden has the reverse pair), spawn (16,16), zone `main`. |
| `db.go` | Storage | modernc SQLite, WAL + busy_timeout 5000 + synchronous NORMAL + foreign_keys, `MaxOpenConns(1)`. Tables: `spaces`, `maps` (tiles/zones/portals/spawn as chunked JSON), `users` (**id = sha256(deviceKey)[:32] hex — raw keys never stored**), `sessions` (uuid), `messages` (space_id/session_id/user_id/name/channel/text/ts + index). `SeedDefaults` inserts hearth+garden on first run. |
| `*_test.go` | Contract tests | `protocol_test.go` (wire envelope parse, dispatch covers every PROTOCOL.md type incl. `ping`, `chatMaxBytes == 2048`), `dispatch_test.go` (mirror of the dispatch switch + channel check), `chat_test.go` (token-bucket burst/refill/cap; spatial-hash insert/nearby/move-across-cells/skip-self). **Green.** |

### `media/` — SFU library (module `hearth/media`, Go 1.24; deps: pion/webrtc v4.2.18, pion/rtp)

A library, not a service: it opens no listeners. The server relays signaling between `Media` and peers over its own WebSocket — the SFU emits `SignalMsg`s on `Events()` (1024-buffered; drops + logs when the server falls behind) and consumes parsed signals via `HandleSignal`.

| File | Role | Key facts |
|---|---|---|
| `config.go` | Defaults | 12 audio / 6 video / 2 screen slots, Top-K 3, hysteresis 500ms; kinds `audio|video|screen`; rungs `low|mid|high` (default `low`); errors `ErrNotFound / ErrNoSlot / ErrBadState`. |
| `media.go` | Facade | One instance per server process; `Join(peerID, roomID)` creates both PeerConnections and emits the pre-negotiated subscriber offer + `joined` + `slots`; `Start` = Join + first publisher offer in one call; `Leave`/`onPeerGone` teardown; `Publish` (bookkeeping hint); `Subscribe` (audio no-op — Top-K managed; video binds a camera slot 0–5 at the requested rung with nearest-lower fallback; screen marks wanted); `Unsubscribe`; `SetTopK(k)`; `SetRung(peer, target, rung)`; `Stats()`. |
| `peer.go` | Two PCs per peer | **Publisher** PC: client offers, SFU answers, renegotiation allowed → publishing is unlimited. **Subscriber** PC: SFU offers 20 sendonly m-lines **exactly once** (12 Opus@48k audio + 6 VP8 camera + 2 VP8 screen), each bound to a placeholder static track with stable msids `h-slot-audio-N`, `h-slot-camera-N`, `h-slot-screen-N`; afterwards slots are re-pointed in-process, **never renegotiated**. `pub.OnTrack` → `room.registerRemoteTrack`; connection-state failures → `room.onPeerGone`. |
| `room.go` | Routing | Single publisher track table keyed `pubID|kind|rung`; `classifyTrack` by msid convention (`mic-<peerId>`, `camera-<rung>` or RID wins, `screen-<peerId>`); audio join-order list feeding Top-K; screen recency order (newest first) with per-peer subscription sets; `subscribeVideo` idempotent; `reslotScreens` caps 2 with oldest-eviction; `removePeerTracks` re-routes everyone on a leave; `snapshot()` for stats. |
| `topk.go` | Top-K audio | Importance = join order today (deterministic, flap-free; placeholder until real audio levels — RFC 6464/RTCP — land, then only `pick()` changes). Hysteresis: a new selection must persist 500ms before `applyAudioSelection`; per-peer list excludes self (you never hear yourself). |
| `track.go` | Fan-out | One SSRC reader goroutine per `PublishedTrack` fans every RTP packet to all attached sinks (selective forwarding — payload untouched); onClose re-slots (Top-K, screens). |
| `*_test.go` | Loopback | `loopback_test.go` runs an in-process test client (plays both the WS relay and the browser): asserts the subscriber offer has **exactly 20 m-lines** and is the **only** offer ever; Top-K audio flows A→B; video subscribe on slot 0 + SetRung mid/high with fallback; screen publish/subscribe; hysteresis (K=1, C joins, selection stays `[A]`, after A leaves → `[C]`); zero subscriber renegotiation end-to-end. `renego_minimal_test.go` isolates plain-pion behavior: an answerer must fire `onTrack` for a track added in a **second** offer. |

> **Current status (verified Aug 2026):** `go vet` clean, but the suite is **red** — `TestLoopback` fails at video subscribe (`media: not found`: camera tracks never reach the room's track table after a renegotiated publisher offer) and `TestRenegoMinimal` fails (plain pion v4.2.18 fires `onTrack` for only 1 of 2 tracks). Reproduced 3/3. The isolation test proves it is pion-level, not our wiring. Audio Top-K and the 20-m-line pre-negotiation pass within the loopback. Fixing this is the day-1 unblock (see [README roadmap](../README.md#roadmap)).

### `client/` — PWA (Vite 6 + TS 5.7 + Preact 10 + PixiJS 8.6; vite-plugin-pwa)

| Area | Facts |
|---|---|
| `src/net/` | `protocol.ts` (Envelope `{v:1, t, id:uuid, ts, d}` + message interfaces, `env()` helper); `ws.ts` (auto-reconnect with 500ms→10s exponential backoff + jitter, re-join on open → server re-sends `welcome` → full resync; dispatches welcome/state/chat/edit/error/bot_msg; NetStatus `connecting|online|reconnecting|offline`); `api.ts` (`fetchSpace` early world fetch before welcome); `config.ts` (SPACE_ID from `?space=` or localStorage, **default `town-square`** — mismatched with the server's seeded `hearth`/`garden`; `wsUrl()` same-origin or `?ws=` override). |
| `src/world/` | `renderer.ts` (PixiJS v8: procedural canvas avatars + tile atlas — no asset files; camera follows local player, pinch-zoom 0.6–2.5, two-finger pan; **tap-to-move = A\* on the passability grid**, walk at 4.5 tiles/s; local moves reported throttled to 84ms ≈ 12Hz with monotonic seq; remotes rendered through `InterpBuffer(100ms)` with cubic ease; `paintTile` for edits; tolerant `parseWorld` with deterministic `genDefaultWorld` fallback); `astar.ts`; `interp.ts`; `tiles.ts` (TILE=32px; floor/wall/water/grass/stone passability). |
| `src/ui/` | `JoinScreen` (guest-first, name ≤24 chars), `Hud` (status pill, space tag, chat FAB with unread badge), `ChatSheet` (bottom-sheet, proximity/space tabs, optimistic pending messages with echo dedupe, 2000-char input). |
| App glue | `App.tsx` (join → loading → world phases; `visualViewport` `--vvh` so layout survives the mobile keyboard; `window.__hearth` debug hook used by integration tests). PWA: `registerSW({immediate:true})`, manifest (standalone, theme `#17111f`), Workbox precache with `/index.html` navigate fallback. Dev proxy: `/api` and `/ws` → the Go server on :8090. |

### Scripts & tests

`test-ws.mjs` (repo root): two WS clients — join → move → state round-trip → chat (space + dm) → rate limit → portal. `verify-rest.sh`: health / static / guest auth (cookie) / me / spaces. `client/scripts/mock-server.mjs`: zero-dep raw-RFC6455 mock on :8090 (dist + welcome + ~4Hz state + chat echo + edit broadcast) for offline client work; `client/scripts/ws-smoke.mjs` smokes it.

## 2. Data flows

### Join → welcome → state@12Hz

1. Client opens `WS /ws`; server resolves auth (cookie, else `?deviceKey=` → user + session + Set-Cookie on the 101 response).
2. Client sends `join {name, lang, space, guest}` → server creates an `Entity` (ID = session ID) in the space's `SpaceState` and spatial hash → replies `welcome {selfId, spaceId, world(GeoJSON), x, y, dir}`.
3. The hub tick runs at 12Hz; per client it computes the AOI set (`Nearby(x, y, radius=20)` over the 8×8 hash), coalesces via `shouldSendState` (send **only** when the AOI entity set changed or the 1s heartbeat elapsed), and pushes `state {spaceId, entities[], t}` with int16-quantized positions. Remote clients render through their 100ms interpolation buffer — steady state is cheap, motion is smooth.

### Move → spatial hash → AOI broadcast

Client walks an A* path locally (4.5 tiles/s) and reports at ≤12Hz with a monotonic `seq` → `handleMove` clamps to world bounds, updates the entity in the spatial hash (cell move), sets `dir` → next tick, neighbors' AOI includes the mover → `state` frames. No per-move broadcast to the whole space; fan-out is O(AOI), not O(space).

### Chat (proximity / space / dm)

`chat {channel, text, nonce}` → token bucket (burst 5/10s, sustained 1/2s) → non-empty, ≤2048 bytes → route: `space` = broadcast to every client in the space; `proximity` = only clients within AOI radius 20; `dm` = direct to the target entity (searched across spaces) → `InsertMessage` persists it → `chat {from, fromId, text, ts}` frames. The client appends optimistically (`pending` → delivered on echo, deduped).

### Portal teleport

`portal {portalId}` → server verifies proximity (squared distance ≤ 9, ~3 tiles) → removes the entity from the source `SpaceState`, inserts into the target, replies `portal {portalId, spaceId, x, y}` → subsequent `state` frames come from the new space.

### Edit

`edit {x, y, tile}` → bounds-checked → `SetTile` (sparse map; `floor` deletes) → `SaveWorld` to SQLite → broadcast `edit {x, y, tile, by}` to all clients in the space; the renderer paints the tile and invalidates any cached path. (Today: paint only. `erase|place|zone|portal` ops and the ≥50-op undo are roadmap day 4.)

### Voice/video (target pipeline; media integration is day 1–2 work)

```
mic → Opus encode (browser) → publisher PC (client offers, SFU answers; renegotiable)
   → pub.OnTrack → room.registerRemoteTrack  (track table key: pubID|kind|rung)
   → Top-K evaluator picks K audio by join order (500ms hysteresis, self excluded)
   → subscriber side: SFU's one-time 20-m-line offer; binding a speaker = attaching a
     sink to that PublishedTrack's SSRC reader → RTP forwarded untouched
   → browser decodes Opus → WebAudio playback
```

- **Audio**: always Top-K routed (K=3 default, `SetTopK(k)` clamped to [1,12]); `Subscribe(kind=audio)` is a no-op.
- **Camera**: explicit `Subscribe` binds target's camera at the default rung (nearest-lower fallback) to a free pre-negotiated slot 0–5; `SetRung` re-points live. Simulcast rungs `low|mid|high` (RID wins over streamID suffix).
- **Screen**: `Subscribe` marks a target's screen as wanted; up to 2 screen sources per subscriber, newest presenter first, oldest evicted.
- **Signaling**: the server relays `signal`/`media` envelopes over the existing WS (`findEntity` target resolution). Today `handleMedia` is a pass-through; the real `Events()` + `HandleSignal` wiring replaces it on day 1–2.

## 3. Protocol summary

Frozen contract, v0 — see [PROTOCOL.md](../PROTOCOL.md) for the full text. Amending it requires **round+sign**, and the contract tests must change in the same commit.

- **Envelope**: `{"v":1, "t":"<type>", "id":"<uuid>", "ts":<ms>, "d":{...}}` — one WS per client, JSON text frames.
- **Client → server**: `join`, `move` (12Hz max, seq monotonic), `chat` (prox|space|dm), `edit` (paint|erase|place|zone|portal), `portal`, `signal` (sdp/ice), `media` (publish|subscribe|unsubscribe|topk; kind audio|video|screen), `bot_msg`, `ping`.
- **Server → client**: `welcome`, `state` (12Hz coalesced, AOI only), `chat`, `edit`, `signal`, `media` (offer with slots `{audio:12, video:6, screen:2}`), `bot_msg`, `error` (code `RATE` etc.), `pong`.
- **Frozen rules**: server-assigned UUIDs; float tile positions int16-quantized; AOI radius 20, 8×8 spatial hash; chat burst 5/10s + sustained 1/2s, ≤2KB; move 12Hz; pre-negotiated transceivers 12+6+2, publish unlimited, display adaptive; edits optimistic-client/server-authoritative, AOI-batched at 20Hz, undo ≥50 ops; SQLite WAL persistence with positions RAM-only (batched persist 2–5s); guest-first auth (device key) + passkeys later; session = httpOnly cookie, 90-day sliding; bots identical protocol (MCP via streamable HTTP, OpenRouter brain).

**Known v0 discrepancies (tracked, don't silently "fix"):**
- The implementation uses `"type"` as the envelope field where PROTOCOL.md says `"t"` (noted in `protocol_test.go`; the test locks what the server actually accepts).
- Session cookie MaxAge is 30 days in code vs 90-day sliding in the protocol (day-3 item).
- Client default space `town-square` vs server-seeded `hearth`/`garden` (use `?space=hearth` today).
- `handleMedia` is a pass-through relay until the SFU integration lands.

## 4. Scaling

**Bubble model.** The unit of everything is the **space** (= one `SpaceState` in the hub = one SFU room once media lands). Spaces are independent: separate entity maps, spatial hashes, SQLite rows, and (later) SFU rooms — so a deployment scales horizontally by sharding spaces across processes; cross-space movement already exists via portals and would need inter-shard routing at that point. Inside a space, the AOI system keeps state fan-out at O(neighbors) and proximity chat at O(AOI); media Top-K caps receive fan-in at K regardless of room size — **subscriber bandwidth is constant per peer**, which is the core trick that makes 50+ crowds viable.

**Horizontal SFU tiering.** `media/` is a library today (in-process). The tiering path: extract standalone SFU nodes (they already speak a clean signal envelope — `SignalMsg`), pin spaces/rooms to SFU nodes, and let the server act as signaling dispatcher. Rooms are the shard key, so a 20-euro box tier per N rooms is the natural ladder.

**Voice capacity estimate (~800–1200 users per 20-euro box, audio-only, K=3).** Per stream: Opus ~24 kbps @ 20ms frames (50 pps) + ~16 kbps RTP/UDP/IP overhead ≈ **40 kbps**. Per peer: 40 kbps up (1 publisher stream) + 120 kbps down (K=3). At N=1000: **40 Mbps ingress / 120 Mbps egress** — well inside a 1 Gbps port. Packet crypto (SRTP decrypt once per ingress, encrypt per sink) ≈ 200k ops/s at N=1000 — trivial for a few AES-NI cores; the real constraints are per-packet allocations/GC, syscall overhead, and RAM (two PeerConnections + buffers ≈ 2–5 MB per peer → 2–5 GB at 1000 peers). So: **~800 realistic, ~1200 optimistic on a 4–8 vCPU / 8 GB / 1 Gbps box; cap ~600–800 in practice for headroom.** Caveats: any heavy camera/screen usage collapses this (each VP8 camera stream is ~1 Mbps × slots × subscribers), and the number is unproven until the SFU is integrated and the day-7 benchmark runs.

## 5. Security model

- **Identity — guest-first, passkeys later.** `POST /api/auth/guest {deviceKey, name}` → user ID is `sha256(deviceKey)[:32]` hex; **raw device keys are never stored**. Sessions are server-side UUIDs. The stated upgrade path is passkeys (protocol rule).
- **Session cookie.** `hearth_session`, **HttpOnly**, SameSite=Lax, 30d MaxAge (protocol says 90-day sliding — day-3 alignment item). WS auth prefers the cookie; the fallback `?deviceKey=` query param works for clients that can't set cookies (note: query params can leak into logs — consider a short-lived ticket later). SameSite=Lax means cross-site WebSocket handshakes won't attach the cookie, which is the main CSWSH mitigation — but the upgrader's `CheckOrigin` currently returns `true` for any origin (dev convenience). **Prod hardening: origin allowlist + TLS/WSS.**
- **Abuse controls.** Chat: token bucket burst 5/10s, sustained 1/2s, payload ≤2KB; `bot_msg` rate-limited with the same 2KB cap; move clamped to world bounds (12Hz is a protocol rule; the coalescer absorbs bursts); edits bounds-checked and server-authoritative; portal gated by proximity; space dims clamped 32–512; name length capped client-side (24).
- **WS hardening.** 64KB read limit, 70s read deadline with ping/pong liveness (30s ping), 10s write deadline, 256-deep send queue with drop-on-full backpressure (bounds memory against slow readers).
- **Data at rest.** SQLite WAL, busy_timeout, `MaxOpenConns(1)`; positions never persisted (RAM-only, batched 2–5s per protocol). Secrets hygiene in `.gitignore` (`*.pat`, `.env`, `or_key.txt`). All queries parameterized.
- **Media plane.** WebRTC's DTLS-SRTP is mandatory on both PeerConnections; signaling is relayed only to resolved entities (`findEntity`). Gap: no explicit authorization check on who may signal whom (proximity gating is TBD) — add before opening media to the public.
- **Embedding/iframe sandbox.** No CSP / X-Frame-Options / `frame-ancestors` are set today. If third-party embedding is desired (WorkAdventure heritage), serve with an explicit `frame-ancestors` policy and require sandboxed iframes (no `allow-same-origin` for untrusted hosts); otherwise ship `X-Frame-Options: DENY`. The client PWA manifest already constrains scope to `/`.

**Known gaps (tracked):** origin allowlist, CSP + frame policy, WS auth ticket, session sliding renewal + rotation, move/edit rate ceilings beyond clamping, media authorization, audit logging.
