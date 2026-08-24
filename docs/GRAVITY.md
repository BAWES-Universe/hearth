# Hearth Directory Gravity (T2)

The world directory (/worlds) feels alive and self-organizing: every
published world and every active member carries a **gravity score** —
Love × Reach × Momentum — recomputed nightly by an in-server cron from the
append-only `activity_events` log. Active worlds rise; quiet worlds settle.
All additions are ADDITIVE — the frozen PROTOCOL.md v0 envelope is untouched.

## Activity events (the fuel)

`activity_events` is the single append-only event log (INSERT only; nothing
is ever updated or deleted). Every write goes through `Store.Emit` /
`Hub.emitActivity`. The T2 live-event wiring means gravity reflects real
engagement, not just admin actions:

| Kind | Action | Emitted when | Counts toward |
|---|---|---|---|
| `chat` | `space` / `proximity` / `global` | A member sends a world-local chat message (dm is private and does NOT emit) | Love |
| `edit` | `paint` / `erase` / `place` / `zone` / `portal` / `publish` | A human or bot applies an editor op | Love |
| `presence` | `join` | A member joins a world (returning visits add Love, capped) | Love + Reach (worlds) / Reach (members) |
| `nav` | `portal` | A member walks through a portal to another world | Love |
| `world` | `publish` | A member publishes their world | Love |
| `admin` | `admin.*` | Operator console mutations (S9 audit) | — |

The event row never carries message text — just `kind`, `action`, `target`,
a small `diff` JSON, and a sanitized IP (/24 or /48 prefix only).

## The formula

For a world (or member) with event rows `E` inside the lookback window:

```
Love     = Σ over (actor, day) buckets of min(count, gravityPerDayCap)
Reach    = distinct visitors of the world (worlds)   — join events, unique actor
           distinct worlds visited (members)         — join events, unique world_id
Momentum = Σ 2^(-ageDays / 14)  over all events in the window   (14-day half-life)
Gravity  = (1 + Love) × (1 + Reach) × (1 + Momentum)
```

Constants (server/gravity.go):

| Constant | Value | Meaning |
|---|---|---|
| `gravityHalfLifeDays` | 14 | Momentum recency half-life |
| `gravityLookbackDays` | 30 | Events older than this contribute ~0 momentum; only window events count for Love/Reach |
| `gravityPerDayCap` | 20 | Max Love contribution events per actor/world/day (or per member/day) |

Design properties:

- **Never zero** — the +1 on every factor means a brand-new published world
  still ranks deterministically against the seeds.
- **Monotone** — more engagement, more visitors, or fresher activity always
  raises gravity.
- **Capped Love** — one hyperactive member can't drown a world (per-day cap).
- **Decaying Momentum** — a world that goes quiet cools off on the
  14-day half-life; the directory keeps reflecting *current* life.

## Nightly cron

The recompute lives INSIDE the server (not a system cron): `Hub.gravityCron`
runs once at startup, then every 24h (`gravityCronEvery`), guarded by
`gravityMu` so a concurrent directory refresh never double-writes. On top of
the nightly pass, `ensureGravityFresh` recomputes when the persisted scores
are missing or older than 5 minutes (`gravityTTL`) — so the directory is
never stale even between cron ticks.

## Determinism (the test contract)

Identical input → identical output:

1. **Same snapshot, same scores** — two worlds (or members) with identical
   event sets produce identical Love/Reach/Momentum/Gravity in one pass.
   Verified by `TestGravityDeterminismAndOrdering` (S1) and
   `TestMemberGravityDeterminismAndFormula` (T2).
2. **Stable ordering** — the directory sorts `gravity desc, recency desc,
   id asc`; the tie-breakers make the ORDER fully deterministic with no
   map-order flapping. Two consecutive directory reads return the same
   ordering.
3. **Across recomputes** — persisted values only drift by nanosecond-scale
   decay between runs (each pass uses its own `now`); ranking stays stable.

The live check is `gravity-test.cjs` (repo root): it generates synthetic
activity over the WS, triggers recompute, and asserts the ranking order AND
that two runs agree.

## Directory & thumbnails

- GET /api/worlds — published worlds ordered by gravity (desc), then
  recency, then id; `?q=` name search still works. Each entry carries
  `gravity {love, reach, momentum, gravity}`, live `headcount` from
  presence, and `thumbnail` — a server-rendered HMF v1 preview.
- GET /api/worlds/{id}/thumbnail — PNG rendered directly from the world's
  HMF v1 tile grid (`server/thumbnail.go`, deterministic, colors mirror the
  client palette). The client renders it lazily in each directory card.
- Per-member scores live in `member_gravity_scores` (one row per member
  with activity, computed from their own event rows). Members with no
  activity simply have no row.

## Stream test (live box)

```
node gravity-test.cjs            # repo root, against http://127.0.0.1:8090
```

Asserts: synthetic chat/edit/join activity → gravity recompute → active
world ranks above quiet world → same ranking on a second run (determinism)
→ headcount reflects live presence. See the script header for the exact
scenario.
