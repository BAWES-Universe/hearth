# Hearth Social Layer (T2)

Friends, presence, and who's-here on top of the existing guest-auth / AOI
presence / activity stack. All additions are ADDITIVE — the frozen
PROTOCOL.md v0 envelope (`{"v":1,"t":..,"d":{..}}`) is untouched.

## REST surface (all JSON)

All `/api/friends` routes require the `hearth_session` cookie
(`POST /api/auth/guest` sets it; the WS handshake authenticates in-band and
does NOT set it — the client calls guest auth once per page load via
`ensureSession()` before using the friends API).

| Method | Route | Body / Query | Description |
|---|---|---|---|
| GET | `/api/friends` | — | My friend list: per-row `friendId, name, status, online, space?, since`. Status is the VIEWER's perspective: `accepted` (mutual), `pending` (outgoing request), `requested` (incoming request). |
| POST | `/api/friends` | `{friendId}` or `{deviceKey}` | Send a friend request. A raw `deviceKey` is hashed server-side exactly like `UpsertUser` (sha256 prefix — the raw key is never stored). Auto-accepts when a request from that user already exists (mutual, one round trip). |
| POST | `/api/friends/{id}/accept` | — | Accept an incoming request. Both sides flip to `accepted`. |
| POST | `/api/friends/{id}/decline` | — | Decline an incoming request (deletes the pair). |
| DELETE | `/api/friends/{id}` | — | Remove the friendship (either direction, any state). |
| GET | `/api/users?q=` | `q` name substring | User search for adding friends: `{id, name, online}` (excludes self, max 20, ordered by name). |

Errors: `401 not authenticated` (no session), `400 friendId or deviceKey
required` / `you cannot friend yourself`, `404 no such user`, `409 request
already sent` / `already friends` / `no incoming request from this user`.

### Status model

One SQLite row PER DIRECTION (`friends(user_id, friend_id, status, ...)`,
PK `(user_id, friend_id)`, migrated in `db.go migrate()`). A request inserts
both rows in one transaction: the requester's row is `pending`, the target's
is `requested`. Accept flips both to `accepted`; decline/remove delete both.
A viewer's own row always carries their perspective, so listing is a single
SELECT.

## WS envelopes (additive, server -> client)

### `{t:'friend'}` — friend-relation status changed for me

```json
{"v":1,"t":"friend","d":{"event":"request|accept|decline|remove",
                        "userId":"<peer>","name":"<peer name>",
                        "status":"pending|requested|accepted"}}
```

Sent to every connected client of the affected user whenever a relation row
changes (a request arrives, is accepted, declined, or removed). The client
refetches `GET /api/friends` on receipt.

### `{t:'friend_presence'}` — a friend joined/left a space or went offline

```json
{"v":1,"t":"friend_presence","d":{"event":"join|leave|offline",
                                  "userId":"<friend>","name":"<name>",
                                  "online":true,"spaceId":"town-square"}}
```

Fired from the existing presence paths — no new machinery:

- `handleJoin` emits `join` (online, with space) after a friend enters a space.
- `handlePortal` emits `leave` (old space) then `join` (new space).
- `removeClient` (disconnect) emits `offline` (online:false).

Fanout is to ACCEPTED friends only (`FriendIDs`), and only to their live
clients. A user online from several devices receives events on all of them.

## Activity feed wiring

Friend actions ride the EXISTING append-only `activity_events` feed
(`Store.Emit` / `emitActivity`) — no new feed:

- A friend publishing a world already emits `kind=publish` into that world's
  feed (S1 `publishWorld`), visible via `GET /api/worlds/{id}/activity`.
- A friend joining your space already emits `kind=presence, action=join`
  into the space's feed (WS `handleJoin`).
- NEW: a friend accept emits `kind=friend, action=accept` into the
  accepter's CURRENT space feed (only when they are in a space — the log is
  world-keyed). Diff carries `{name, friend:{id,name}}`.

The friends panel surfaces these from the same feed
(`GET /api/worlds/{spaceId}/activity`) with friend actors highlighted — the
client renders `kind=friend` / `presence` / `publish` rows for people on the
friend list.

## Client UI

`FriendsPanel` (bottom sheet, mobile-first, matches the existing dark UI;
new FAB in the HUD with a request-count badge):

- **Here** — who's in the current space: headcount + names from
  `GET /api/spaces/{id}` (live roster stream stays the movement source;
  the space doc is authoritative for full-space presence). Friends get a
  highlight. Below it, the space's recent activity feed with friend actions.
- **Friends** — accepted friends with live online dots + current space
  (updated live via `friend_presence` events), pending "sent" section,
  remove (✕) per row.
- **Requests** — incoming requests with Accept / Decline.
- **Add** — search by name (`GET /api/users?q=`), Add / Accept buttons per
  result, friend-state aware.

## Tests & e2e

- `server/social_test.go` (go test): request → accept → remove → list over
  the real REST handlers; validation (self/unknown/duplicate/unauth);
  full WS round trip (real dial): friend accept event, presence `join` +
  `offline` events on the connected peer, `online` flag + space in the REST
  list, and the `friend/accept` activity row in the space feed.
- `client/src/net/protocol.test.ts`: pins the two new envelope shapes.
- `social-test.cjs` (repo root): two deviceKeys become friends over REST
  (guest auth cookies, search, request, accept) and the presence round trip
  over WS (accept event → join event → online in list → offline event).
  Baseline first: `e2e-test.cjs` 4/4.

## Files touched

- `server/db.go` — `friends` table + index (migration).
- `server/friends.go` — store layer, REST handlers, presence fanout, activity wiring.
- `server/ws.go` / `server/hub.go` — presence hooks (join / portal / disconnect).
- `server/main.go` — `/api/friends*`, `/api/users` routes.
- `client/src/net/friends.ts` — REST client + `ensureSession()`.
- `client/src/ui/FriendsPanel.tsx`, `Hud.tsx`, `App.tsx`, `style.css` — panel + wiring.
- `client/src/net/protocol.ts`, `ws.ts` — additive envelope types + dispatch.
