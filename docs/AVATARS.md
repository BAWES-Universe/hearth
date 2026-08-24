# Hearth Avatar Platform (T2)

Custom image asset upload, a generative picker, and versioned avatar
sets with scope + entitlement governance — all on top of the T1 layered
picker (`body/skin/hair/outfit/accessory`, live preview, per-member
`avatar_spec` persistence). Everything here is ADDITIVE: the frozen
PROTOCOL.md v0 envelope (`{"v":1,"t":..,"d":..}`) is untouched.

## REST surface (all JSON, `hearth_session` cookie required)

All `/api/avatars/*` routes authenticate with the `hearth_session` cookie
(`POST /api/auth/guest` sets it; the WS handshake authenticates in-band and
does NOT set it — the client calls guest auth once per page load via
`ensureSession()` before using the avatar API, same pattern as friends).

| Method | Route | Body / Query | Description |
|---|---|---|---|
| POST | `/api/avatars/assets` | `{layer,name,kind,data(base64)}` | Upload a custom image asset for a layer. `layer` ∈ `body/skin/hair/outfit/accessory`. `data` is base64 image bytes; the server sniffs the real type (png/gif/jpeg/webp only) and rejects anything else. Limits: 512 KiB raw, 1024×1024 px. Returns `{ok, asset:{id,ownerId,layer,name,kind,width,height,status,createdAt}}`. |
| GET | `/api/avatars/assets` | — | My active assets (metadata only, no image bytes). |
| GET | `/api/avatars/assets/{id}/image` | — | Raw image bytes (for `<img>`/canvas). |
| DELETE | `/api/avatars/assets/{id}` | — | Safe-archive. **409 while the asset is worn by any live entity**; otherwise soft-archives (old specs re-normalize instead of breaking). |
| POST | `/api/avatars/sets` | `{name,scope,worldId?,items?}` | Create a versioned set. `scope` ∈ `public/universe/world/membership/user-granted/npc-only`. `world` scope requires owning the world; items are `{layer,optionId}` pairs (catalog ids and/or `asset:<id>`); custom-asset items must be your own active assets, one governing set per asset. Returns `{ok,set}` (version starts at 1). |
| GET | `/api/avatars/sets` | — | Sets visible to me (public, universe, world-scope, membership/user-granted where I hold a grant, plus my own). |
| POST | `/api/avatars/sets/{id}/items` | `{layer,optionId}` | Add an option (**bumps the set version**, audited). |
| DELETE | `/api/avatars/sets/{id}/items/{layer}/{optionId}` | — | Remove an option (**bumps the set version**, audited). |
| POST | `/api/avatars/sets/{id}/archive` | — | Safe-archive a set (**409 while an option of it is worn**); otherwise soft-archive. |
| POST | `/api/avatars/grants` | `{setId,userId,kind?,match?,expiresAt?}` | Grant a member access to a set's options. The set creator (or asset owner) may grant. `kind` ∈ `direct/time-limited/tag/sub/email-domain/membership` (default `direct`); `match` carries the tag name / email domain for claim-checked kinds; `expiresAt` is RFC3339 (`''` = never). |

Errors: `401 not authenticated`, `400` validation, `403` not yours / not
entitled, `404` no such asset/set, `409` worn / already governed, `413`
image too large.

## Custom asset upload → sprite pipeline

Uploaded images are stored server-side (BLOB in `avatar_assets`) and their
option id is `asset:<uuid>`. The client keeps an image cache in
`client/src/avatar/sprites.ts`: on first wear it loads the raw bytes from
`/api/avatars/assets/{id}/image`, draws them cover-fit into the layer
canvas (scaled to the layer's atlas cell), and re-renders live avatars
when the image finishes loading (`world/renderer.ts`). This is the
"auto-atlas" step: uploads drop straight into the existing layered sprite
pipeline with no re-pack step.

## Governance: sets, scopes, entitlements

- **Sets** (`avatar_sets`) are versioned collections of options with a
  scope: `public | universe | world | membership | user-granted | npc-only`.
  Every item change bumps the version (audit row via the append-only
  `Store.Emit`, kind=`avatar`). Catalog options (T1) stay free for humans
  (NPC-only aside) — sets gate the **custom assets** they contain.
- **Entitlements** (`avatar_grants`) are per-member grant rows:
  `direct` (that user), `time-limited` (RFC3339 expiry), `tag` /
  `email-domain` / `sub` (claim-checked against `user_claims`),
  `membership` (reserved for the membership tier).
- **Enforcement is server-side, per join AND per `avatar_update`**:
  non-entitled options are normalized to the member's first entitled
  option per layer, and every denial appends an `entitlement.deny` audit
  row. A user may always wear their OWN active asset; wearing someone
  else's requires the asset's governing set to grant them access.
- **Performance**: all tables are mirrored in an in-memory
  `AvatarRegistry` snapshot (invalidated on writes, refreshed lazily), so
  the per-join check is pure map lookups — target p95 < 50ms cached.
- **Safe-archive**: deleting an asset or archiving a set is BLOCKED (409)
  while any live entity wears one of its options; otherwise it is a soft
  archive (`status`/`archived` flag) so old specs re-normalize instead of
  breaking. Every mutation is audited (append-only, kind=`avatar`).

## Generative picker

`POST /api/avatars/generate` — `{prompt}` → `{ok, model, spec}`. T2 uses a
deterministic catalog sampler, NOT an external image model:

```
model = "hearth-catalog-sampler v1 (FNV-1a(prompt,userID) -> xorshift64
         over the entitled layer catalog; deterministic per member,
         no external image model in T2)"
```

Seed = FNV-1a(prompt, userID); xorshift64 walks the layers picking one
ENTITLED option per layer (catalog minus NPC-only, plus the member's own
active assets for that layer). Same prompt + same member + same catalog ⇒
identical candidate; prompts are free-form (default `a friendly traveler`)
and truncated at 500 chars. The picker's Generate tab shows the model
string and the resulting candidate in the live preview.

## WS envelopes (additive)

### `{t:'avatar_update'}` — client → server

```json
{"v":1,"t":"avatar_update","id":"<uuid>","ts":<ms>,"spec":{"body":"round","skin":"warm","hair":"bob","outfit":"tee","accessory":"asset:<uuid>"}}
```

The spec is validated, entitlement-enforced, persisted per member, and
applied to the live entity.

### `{t:'avatar_update'}` — server → client (broadcast + self echo)

```json
{"v":1,"t":"avatar_update","d":{"userId":"<id>","name":"<name>","spec":{...},"self":true|false}}
```

Broadcast to the AOI of the space on every change (with `self:true` on
the sender's own echo — the echo carries the ENFORCED spec, so a denied
option visibly normalizes). The 12Hz `state` stream carries the new look
to every viewer as usual.

## Client

`client/src/avatar/AvatarBuilder.tsx` — three mode tabs:
**Layers** (T1 swatches), **Upload** (file → base64 → POST → thumbnail →
click to wear), **Generate** (prompt → candidate spec, model string
shown). `client/src/net/avatars.ts` is the REST client; `spec.ts`
understands `asset:<uuid>` options; `sprites.ts` caches and draws asset
images cover-fit into the layer atlas.

## Testing

- Server: `go vet ./... && go test ./...` — full suite incl. T2
  (upload/list/image/safe-archive, entitlement deny→allow, user-granted +
  time-limited, tag/email-domain claims, set versioning 1→2→3, world-scope
  ownership, generative determinism, spec round-trip persist→reload,
  image sniffing, WS envelope).
- Live box: `node e2e-test.cjs` (must be `RESULT: 4 pass, 0 fail`) then
  `node avatars-test.cjs` (round-trip, entitlement deny+allow, safe-archive
  worn) against the deployed server.
