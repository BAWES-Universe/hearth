// Runtime config: WS endpoint, API base, space id. All same-origin by default
// (Go server embeds client dist). Overridable via ?ws= or localStorage.
//
// Universal hub: URL '/' = town-square — the one world every visitor enters.
// There is NO map/space picker; ?space= is honored only as an explicit deep
// link (directory cards, portal sync), never as a browse surface.

const q = new URLSearchParams(window.location.search);

/** The universal spawn — the single world every visitor enters. */
export const SPACE_ID = 'town-square';

export function wsUrl(): string {
  const override = q.get('ws') || localStorage.getItem('hearth:ws');
  if (override) return override;
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}

export function apiBase(): string {
  return '';
}

// Resolve the space id to join: explicit ?space= (deep link) wins, then
// localStorage, then the universal default town-square. A stale explicit id
// falls back to town-square so join never hangs on a deleted world — the hub
// is always reachable.
export async function resolveSpaceId(): Promise<string> {
  const chosen = q.get('space') || localStorage.getItem('hearth:space') || SPACE_ID;
  if (chosen === SPACE_ID) return chosen;
  const exists = await fetch(`${apiBase()}/api/spaces/${encodeURIComponent(chosen)}`, {
    headers: { Accept: 'application/json' },
  })
    .then((r) => r.ok)
    .catch(() => false);
  return exists ? chosen : SPACE_ID;
}
