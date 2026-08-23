<!--
  PR workflow (hearth):
  1. CI must be green: server (Go vet+test+coverage), media (Go vet+test), client (build+test).
  2. CodeRabbit AI review must pass (profile: assertive — it may request changes).
  3. TDD rule: tests ship in the SAME commit as the code they cover.
  4. Protocol rule: PROTOCOL.md is FROZEN (day 0). Any wire change = round+sign
     (amend protocol tests in the same commit + note it below).
-->

## Summary
<!-- What does this PR do, and why? Link the issue/ticket. Keep it to 2–3 sentences. -->

## What changed
- [ ] **server/** (Go: WS hub, chat, editor, SFU, sqlite) — describe:
- [ ] **media/** (Go: Pion SFU — track router, top-K, simulcast) — describe:
- [ ] **client/** (TS: PixiJS world, Preact UI, PWA) — describe:
- [ ] **other** (docs, infra, CI) — describe:

## Tests run
<!-- Paste local results; CI reruns all of these. -->
- [ ] `go vet ./... && go test ./...` in `server/`
- [ ] `go vet ./... && go test ./...` in `media/`
- [ ] `npm run build && npm test` in `client/`
- [ ] Smoke: `verify-rest.sh` / `test-ws.mjs` (or N/A)

## Protocol changes (round+sign rule)
- [ ] **No protocol changes** (envelopes, message types, wire behavior untouched)
- [ ] **Protocol changed** — required, same commit:
  - What changed:
  - Why:
  - [ ] PROTOCOL.md updated
  - [ ] Protocol tests amended (server/*_test.go, client/src/net/protocol.test.ts)
  - Signed off by:

## Benchmark / performance impact
<!-- e.g. move/state 12Hz sim, chat rate limits, SFU top-K, bundle size. "N/A" if none. -->

## Screenshots
<!-- Required for any client/ UI change — before/after. Drag & drop here. -->
