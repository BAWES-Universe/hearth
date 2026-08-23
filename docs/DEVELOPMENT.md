# Hearth Development Guide

How to work on Hearth without breaking the frozen contract. The protocol is the product's spine: **PROTOCOL.md is frozen v0, and the tests are its enforcement mechanism.**

## 1. Repo layout

```
hearth/
├── PROTOCOL.md          frozen wire contract v0 — round+sign to amend
├── server/              Go monolith (module "hearth", Go 1.22)
│   ├── main.go          process shell: embed dist, mux, graceful shutdown, HEARTH_ADDR/HEARTH_DB
│   ├── ws.go            WS transport, auth resolution, message dispatch, REST handlers
│   ├── hub.go           presence: entities, 8×8 spatial hash, AOI, 12Hz coalesced broadcaster, bot sim
│   ├── chat.go          chat routing (proximity/space/dm) + rate limits; edit handling
│   ├── world.go         world model (tiles/zones/portals/spawn) + seeded defaults
│   ├── db.go            SQLite (WAL) store: spaces/maps/users/sessions/messages
│   ├── protocol_test.go contract tests: envelope, dispatch coverage, limits
│   ├── dispatch_test.go mirror of the dispatch switch + channel check (keep in sync!)
│   ├── chat_test.go     token bucket + spatial hash tests
│   └── dist/            embedded fallback client page
├── media/               SFU library (module "hearth/media", Go 1.24, pion/webrtc v4)
│   ├── media.go         facade: Join/Start/Leave/Publish/Subscribe/SetTopK/SetRung/Stats
│   ├── peer.go          publisher PC (renegotiable) + subscriber PC (12+6+2 pre-negotiated, never renegotiated)
│   ├── room.go          track table, Top-K inputs, screen slots, slot binding
│   ├── topk.go          Top-K selection with 500ms hysteresis
│   ├── track.go         SSRC reader → sink fan-out (selective forwarding)
│   ├── signal.go        SignalMsg types + ParseSignal
│   ├── config.go        slot/topk/rung defaults
│   ├── loopback_test.go in-process end-to-end SFU test (the big contract test)
│   └── renego_minimal_test.go  plain-pion renegotiation isolation test
├── client/              PWA (Vite 6 + TS + Preact + PixiJS v8)
│   ├── src/net/         protocol.ts (envelope + types), ws.ts (reconnect), api.ts, config.ts
│   ├── src/world/       renderer.ts (PixiJS), astar.ts, interp.ts (100ms buffer), tiles.ts
│   ├── src/ui/          JoinScreen, Hud, ChatSheet
│   ├── scripts/         mock-server.mjs (zero-dep RFC6455), ws-smoke.mjs, gen-icons.py
│   └── vite.config.ts   dev proxy /api + /ws → Go server :8090; PWA manifest + workbox
├── test-ws.mjs          wire e2e: two clients, join→move→state→chat→rate-limit→portal
├── verify-rest.sh       REST checks (health, static, guest auth, me, spaces)
└── docs/                ARCHITECTURE.md, DEVELOPMENT.md
```

## 2. Dev loop

1. **Server**: `cd server && go run .` — listens on `:8090`, creates/seeds `data/hearth.db` (spaces `hearth` + `garden`).
2. **Client**: `cd client && npm install && npm run dev` — Vite dev server; `/api` and `/ws` are proxied to the Go server on 8090 (see `vite.config.ts`). Edit TSX → hot reload. The renderer has a debug hook: `window.__hearth.getLocal() / .selfId() / .status()` in the console.
3. **No Go server handy?** `cd client && npm run mock` — zero-dep mock server on `:8090` (dist + welcome + ~4Hz state + chat echo + edit broadcast), then `node scripts/ws-smoke.mjs` to smoke the raw protocol. **The mock and the Go server both bind :8090 — run one at a time.**
4. **Wire-level e2e against a live server**: `node test-ws.mjs` (Node ≥ 21; `WS_URL` overrides the endpoint). Two clients join, move, round-trip state, chat space + dm, trip the rate limit, walk a portal.
5. **REST checks**: `./verify-rest.sh` (health / static / guest auth cookie / me / spaces).
6. **Full-stack production-ish run**: `cd client && npm run build` then `cd ../server && go run .` — the server prefers `../client/dist` on disk, falling back to its embedded page.

> **Day-0 gotcha**: the client defaults to space `town-square` but the server seeds `hearth`/`garden`. Open with `?space=hearth`, or clear the `hearth:space` localStorage key. (Aligning the default is a tracked item.)

## 3. The TDD rule — tests lock the contract

The protocol is frozen. The tests are its enforcement mechanism, and they are written to be the **single source of truth for what the server and SFU actually accept**:

- `server/protocol_test.go` — wire envelope parsing, dispatch coverage for **every** PROTOCOL.md client→server type (including `ping`), and hard limits (`chatMaxBytes == 2048`).
- `server/dispatch_test.go` — `serverHandlesType` / `validChannel` mirrors. Header comment: *"Add new protocol types here AND in handleMessage in the same commit."*
- `server/chat_test.go` — token-bucket burst/refill/cap (burst 5/10s, sustained 1/2s) and spatial-hash behavior (insert, nearby radius, move across cells, skip-self).
- `media/loopback_test.go` — the SFU contract: subscriber offer = **exactly 20 m-lines**, delivered **exactly once** (zero renegotiation end-to-end), Top-K audio routing + hysteresis, camera slot binding + rung fallback, screen slots, peer-leave re-routing.
- `media/renego_minimal_test.go` — isolates **plain pion** behavior (answerer must fire `onTrack` for a track added in a second offer) so SFU wiring bugs can't hide pion bugs.

**Amending the protocol means amending the tests in the same commit** (round+sign rule): change `PROTOCOL.md`, the server handler, the client protocol types, AND the mirrors in `dispatch_test.go` / `protocol_test.go` together — one commit, one review. A protocol change that lands without its test update is a revert.

## 4. Verification bar (before push)

```bash
cd server && go build ./... && go vet ./... && go test -count=1 ./...
cd media  && go build ./... && go vet ./... && go test ./...
cd client && npm run build          # tsc --noEmit + vite build + PWA precache
```

**Current honest status (verified Aug 2026):**

| Suite | Status |
|---|---|
| `server`: build + vet + test | ✅ green |
| `media`: build + vet | ✅ clean |
| `media`: test | ⚠️ **RED** — `TestLoopback` fails at video subscribe (`media: not found`) and `TestRenegoMinimal` fails (plain pion v4.2.18 fires `onTrack` for only 1 of 2 tracks across a renegotiated offer). Reproduced 3/3; the isolation test shows it is pion-level, not our wiring. Audio Top-K and the 20-m-line pre-negotiation pass within the loopback. |
| `client`: `npm run build` | ✅ green (tsc + vite + 19 PWA precache entries) |

**So today the bar reads:** *server and client must be green; media must be at least as green as the previous commit, with the red renegotiation tests tracked and the day-1 fix scheduled.* The moment `TestRenegoMinimal` passes 3/3 consecutive runs, media joins the hard-gate list and the RED state is no longer acceptable for any push touching `media/`.

## 5. How to add a feature

Walkthrough for a new wire feature — e.g. a new message type (or a new media kind). Every step is one commit.

1. **Protocol first**: add the type/shape to `PROTOCOL.md` (and get the round+sign). Keep the envelope `{v, t, id, ts, d}`.
2. **Server dispatch**: add the case to `handleMessage` in `server/ws.go` and write the handler (e.g. `handleX`). Update the mirror in `server/dispatch_test.go` (`serverHandlesType`) — the test will fail until you do, by design. Add contract assertions to `server/protocol_test.go`.
3. **Handler tests**: unit-test the handler's validation and routing in a new `server/x_test.go` (bounds, rate limits, routing — copy the `chat_test.go` pattern).
4. **Client types + dispatch**: add the interface to `client/src/net/protocol.ts` and the case in `client/src/net/ws.ts` `dispatch()`; wire the UI in the relevant component.
5. **E2E**: extend `test-ws.mjs` (and `verify-rest.sh` for REST surface) so the new type is exercised on the wire.
6. **Media (if media-affecting)**: extend `media/loopback_test.go` with the new behavior before wiring the server-side integration; keep the "one subscriber offer, never renegotiated" invariant asserted.
7. **Verify**: run the full verification bar above, then push.

For an **SFU-only** change, the loopback test is your whole world: the in-process test client plays both the WS relay and the browser, so you can develop media features with `cd media && go test` alone — no server, no browser.

## 6. Working notes / tracked items

- **pion renegotiation gap (day-1 unblock)**: `TestRenegoMinimal` fails on pion v4.2.18 in this environment. Investigate upstream fixes (pin/patch/upgrade) before touching any other media wiring; the loopback video path depends on it. Candidate directions (proposals, not yet verified): (a) pre-negotiate publisher transceivers at `Join`, mirroring the subscriber PC's never-renegotiate pattern; (b) pin/bump `pion/webrtc` off v4.2.18 once upstream behavior is characterized. Until green, voice/video features are blocked and the "<300ms voice bubbles" day-0 gate cannot be claimed — restrict pushes to non-media paths and carry the red state explicitly.
- **Envelope field**: implementation uses `"type"` where PROTOCOL.md says `"t"` — `protocol_test.go` locks the current behavior; don't "fix" it silently, align it deliberately (round+sign).
- **Cookie MaxAge**: code 30d vs protocol 90-day sliding (day-3 item).
- **Client default space** `town-square` vs server `hearth` (align, don't just document).
- **`handleMedia`** in `ws.go` is a pass-through relay until the SFU integration lands — keep the log line, replace the body on day 1–2.
- Never commit: `node_modules/`, `client/dist/`, `*.db*`, `data/`, the `hearth` binary, `*.pat`, `.env`, `or_key.txt` (all gitignored — keep it that way).
