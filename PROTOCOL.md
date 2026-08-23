# Hearth Wire Protocol v0 (FROZEN day 0 — do not change without round+sign)

Transport: WebSocket, single connection per client, JSON text frames. Server: Go monolith (HTTP + WS + presence + Pion SFU + SQLite), port 8090. Client: TS + PixiJS v8 + Preact, PWA.

## Envelope
{"v":1, "t":"<type>", "id":"<uuid>", "ts":<ms>, "d":{...}}

## Client -> Server
- {"t":"join","d":{"name":"amber-fox","lang":"en","space":"town-square","guest":true}}
- {"t":"move","d":{"x":12.5,"y":8.2,"dir":"right","seq":1}}       // 12Hz max
- {"t":"chat","d":{"channel":"proximity","text":"hi","nonce":"n1"}} // prox|space|dm
- {"t":"edit","d":{"op":"paint|erase|place|zone|portal","x":0,"y":0,"tileId":7,"entityId":null}}
- {"t":"portal","d":{"target":"garden"}}
- {"t":"signal","d":{"sdp":{},"ice":[]}}                            // WebRTC signaling
- {"t":"media","d":{"action":"publish|subscribe|unsubscribe|topk","kind":"audio|video|screen","peerId":null}}
- {"t":"bot_msg","d":{"botId":"b1","text":"hi"}}

## Server -> Client
- {"t":"welcome","d":{"selfId":"u1","space":"town-square","world":{...},"roster":[{"id":"u2","name":"x","x":1,"y":2}]}}
- {"t":"state","d":[{"id":"u2","x":1.1,"y":2.2,"dir":"left"}...]}     // 12Hz coalesced, AOI only
- {"t":"chat","d":{"channel":"proximity","from":"u2","text":"hi","seq":77}}
- {"t":"edit","d":{"op":"paint","x":0,"y":0,"tileId":7,"by":"u3"}}    // live co-edit broadcast
- {"t":"signal","d":{"peerId":"u2","sdp":{},"ice":[]}}
- {"t":"media","d":{"action":"offer","kind":"audio","peerId":"u2","slots":{"audio":12,"video":6,"screen":2}}}
- {"t":"bot_msg","d":{"botId":"b1","text":"welcome in!"}}
- {"t":"error","d":{"code":"RATE","msg":"slow down"}}

## Rules (frozen)
- IDs: server-assigned uuid. Positions: float tiles, int16-quantized over the wire. AOI: 20-tile radius, 8x8 spatial hash.
- Chat rate limit: burst 5/10s, sustained 1/2s, payload <=2KB. Move: 12Hz, seq monotonic.
- Media: pre-negotiated transceivers — 12 recv-audio, 6 recv-camera-video, 2 recv-screen; publish unlimited, display adaptive (no cap language).
- Edits: optimistic client, server-authoritative, broadcast to AOI at 20Hz batched. Undo >= 50 ops.
- Persistence: SQLite WAL — spaces, maps (chunked JSON), users, messages, events. Positions RAM-only, batched persist 2-5s.
- Auth: guest-first (device key) + passkeys later. Session = httpOnly cookie, 90-day sliding.
- Bots: same protocol as humans (join/move/chat), MCP via streamable HTTP, OpenRouter brain.

## Modules (parallel build units)
- server/: Go — mux, WS hub, presence sim, SFU (pion), chat, editor, sqlite, embed client dist
- client/: Vite+TS — PixiJS world, Preact panels (chat/editor/roster), PWA, MediaDirector
- media/: Go package — Pion SFU: track router, top-K audio, simulcast video, screen slots

## Day-0 gate (Khalid test): two phones on same network -> map renders, tap-to-move works, voice bubbles <300ms.
