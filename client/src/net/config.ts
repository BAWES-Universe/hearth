// Runtime config: WS endpoint, API base, space id. All same-origin by default
// (Go server embeds client dist). Overridable via ?ws= or localStorage.

const q = new URLSearchParams(window.location.search);

export const SPACE_ID: string =
  q.get('space') || localStorage.getItem('hearth:space') || 'town-square';

export function wsUrl(): string {
  const override = q.get('ws') || localStorage.getItem('hearth:ws');
  if (override) return override;
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}

export function apiBase(): string {
  return '';
}

// Resolve the space id to join: explicit ?space= wins, then localStorage,
// then the default town-square. If the chosen space no longer exists (404),
// fall back to the server's first space so join never hangs on a stale id.
export async function resolveSpaceId(): Promise<string> {
  const chosen =
    q.get('space') || localStorage.getItem('hearth:space') || 'town-square';
  const exists = await fetch(`${apiBase()}/api/spaces/${encodeURIComponent(chosen)}`, {
    headers: { Accept: 'application/json' },
  })
    .then((r) => r.ok)
    .catch(() => false);
  if (exists) return chosen;
  try {
    const r = await fetch(`${apiBase()}/api/spaces`, {
      headers: { Accept: 'application/json' },
    });
    if (r.ok) {
      const body = (await r.json()) as { spaces?: { id?: string }[] };
      const first = body?.spaces?.[0]?.id;
      if (first) {
        localStorage.setItem('hearth:space', first);
        return first;
      }
    }
  } catch {
    /* offline — fall through to the chosen default */
  }
  return chosen;
}
