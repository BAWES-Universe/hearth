# Hearth Media Plane — SFU voice/video signaling (T2)

Status: **signaling + wiring + UI landed; audio transport proven by in-package
loopback tests + a live-hub offer/answer round-trip test. Headless CI cannot
prove end-to-end RTP audio through two real browsers — that is the day-0
gate (two phones on the same network).**

The media plane is the `media/` Go package (Pion SFU) wired into the live
server via additive WS envelopes. PROTOCOL.md v0 is **frozen and untouched** —
every message below is a new envelope type added under the protocol-extension
policy.

## How a voice bubble works

- A **bubble is per space**: everyone who joins the same space's bubble shares
  one SFU room. Entering a space auto-joins its bubble (client behavior).
- The SFU keeps **one publisher track table per room** and forwards **Top-K
  (default 3) audio sources** to every subscriber — no mesh, no fan-out
  through the hub. Camera video (6 slots) and screen (2 slots) are
  pre-negotiated but not yet surfaced in the UI (mic-first stream).
- Subscriber connections are **pre-negotiated once** (12 recv-audio + 6
  recv-camera + 2 recv-screen m-lines) and are never renegotiated; slots are
  re-pointed in-process. The publisher connection is client-offered and may
  renegotiate (unlimited publishing).
- Audio/video payloads flow **peer-to-peer through the SFU over RTP** — they
  NEVER traverse the WebSocket hub. Only SDP/ICE signaling does.

## Envelope types (additive — PROTOCOL.md v0 untouched)

### client -> server

| type | payload `d` | meaning |
|---|---|---|
| `media_join` | `{space?}` | join the voice bubble of `space` (defaults to the client's current space). Idempotent per space; joining a different space leaves the old bubble first. |
| `media_leave` | `{}` | leave the voice bubble (tears down both PeerConnections). |
| `media_signal` | `{pc, type, sdp?, candidate?}` | one signaling frame toward the SFU. `pc` = `"publisher"` \| `"subscriber"`, `type` = `"offer"` \| `"answer"` \| `"ice"`. `sdp` = `{type, sdp}`; `candidate` = a standard `RTCIceCandidateInit`. |

### server -> client

| type | payload `d` | meaning |
|---|---|---|
| `media_state` | `{joined, space?, peers?}` | bubble ack + membership. `joined:false` on leave. `peers` = `[{id, name}]` of current bubble members (id matches roster/state ids). Broadcast to all bubble members on every join/leave so member counts stay live. |
| `media_signal` | `{peerId, pc, type, sdp?, candidate?, slots?}` | one signaling frame **from the SFU** (relayed 1:1 from `media.Events()`). `type` may also be `joined` \| `left` \| `slots` \| `topk` (bookkeeping). On join the SFU emits: `joined`, `slots` (`{audio:12, video:6, screen:2}`), the pre-negotiated **subscriber offer** (`type:"offer", pc:"subscriber"`), then trickle ICE candidates. |

Errors are reported with the existing `error` envelope
(`code: "media_error" | "bad_media" | "not_joined" | "space_not_found"`).

## Client flow (voice bubble)

1. On world entry (welcome or portal) the client sends `media_join {space}`.
2. The SFU's subscriber offer arrives → client creates its subscriber
   `RTCPeerConnection`, answers, and plays every incoming audio track
   (Top-K selection). No browser renegotiation ever happens on that PC.
3. The mic button (bottom-left, mobile-first) calls `getUserMedia` on first
   use, adds the track to the publisher PC and sends a publisher offer
   (renegotiation allowed). Mute = disable the track (the SFU keeps the
   m-line; Top-K keeps forwarding silence — see limitations).
4. A WebAudio analyser per received track drives the speaking indicator
   (green animated bars on the mic button).
5. Leaving a space or disconnecting sends `media_leave` / the server cleans
   the bubble up on disconnect.

Client code: `client/src/net/voice.ts` (VoiceManager — both PeerConnections,
signaling state machine), `client/src/ui/VoiceBubble.tsx` (UI),
`client/src/net/ws.ts` (envelope plumbing).

## Server wiring

`server/media_bridge.go`: hub-owned `media.Media` instance, a relay goroutine
draining `sfu.Events()` into per-client `media_signal` envelopes, and the
`media_join` / `media_leave` / `media_signal` handlers. Bubble membership is
tracked per peer (`hub.bubbles`), broadcast as `media_state`, and cleaned on
disconnect. `server/go.mod` imports `hearth/media` via
`replace hearth/media => ../media`.

## What is proven (all real executions)

- `media/`: `go vet ./...` + `go test ./...` green — loopback + renegotiation
  tests exercise the SFU routing in-process.
- `server/`: `go vet ./...` + `go test ./...` green, including
  `TestMediaSDPOfferAnswerThroughHub` — a real client-side PeerConnection
  receives the SFU's pre-negotiated subscriber offer over the live WS hub,
  answers it, then offers an Opus mic track and receives the SFU's answer.
  Also `TestMediaBubbleJoinAndMembership` (join/leave + peers broadcasts) and
  the bubble gate (`media_signal` before `media_join` is rejected).
- `client/`: `npm run build` (tsc strict + vite) + `npm test` (15 vitest
  cases incl. the VoiceManager state machine with stubbed WebRTC) green.
- Deployed box: e2e-test.cjs `RESULT: 4 pass, 0 fail`; voice UI screenshots.

## What is NOT yet proven

- **End-to-end audible audio through two real browsers** — headless CI cannot
  drive two phones' mics. This is the PROTOCOL.md day-0 gate: two phones on
  the same network → voice bubbles < 300ms. The transport pieces (SFU loopback,
  hub signaling round-trip, client state machine) are each proven; the last
  hop is a human two-device test.
- **Per-source spatial panning**: Top-K forwards the N most important sources,
  but the client does not yet map audio slots to avatar positions (the SFU
  does not emit slot-binding events). Stereo pan by position is a documented
  follow-up; until then Top-K = "which voices you hear", not "where they
  sound like they are".
- **Mute ≠ Top-K eviction**: a muted client's track stays registered, so it
  keeps its Top-K slot (levels are join-order placeholders — see
  `media/topk.go`). Real audio-level routing (RFC 6464/RTCP) or track-removal
  semantics would fix this.
- **Camera video / screen sharing**: signaling and pre-negotiated slots exist;
  the UI is mic-only this stream.
