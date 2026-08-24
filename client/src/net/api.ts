// REST accessors used before/while the WS connects. Auth: the WS handshake
// sets an httpOnly hearth_session cookie; the ?deviceKey= fallback keeps
// create/publish working even when the cookie is missing (same user via
// UpsertUser). Universal hub: '/' = town-square, directory = GET /api/worlds.

export async function fetchSpace(id: string, base = ''): Promise<unknown | null> {
  try {
    const r = await fetch(`${base}/api/spaces/${encodeURIComponent(id)}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return null;
    return await r.json();
  } catch {
    return null;
  }
}

/** Directory entry as served by GET /api/worlds. */
export interface WorldEntry {
  id: string;
  name: string;
  is_showcase?: boolean;
  is_published?: boolean;
  published_at?: string;
  created_at?: string;
  owner?: { id?: string; name?: string };
  headcount?: number;
  gravity?: { love?: number; reach?: number; momentum?: number; gravity?: number };
  thumbnail?: unknown;
}

/** GET /api/worlds — published worlds, gravity desc. */
export async function listWorlds(q = '', base = ''): Promise<WorldEntry[]> {
  try {
    const qs = q.trim() ? `?q=${encodeURIComponent(q.trim())}` : '';
    const r = await fetch(`${base}/api/worlds${qs}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return [];
    const body = (await r.json()) as { worlds?: WorldEntry[] };
    return Array.isArray(body?.worlds) ? body.worlds : [];
  } catch {
    return [];
  }
}

/** POST /api/worlds — blank-canvas draft owned by the device-key user. */
export async function createWorld(
  name: string,
  deviceKey: string,
  opts: { width?: number; height?: number; base?: string } = {},
): Promise<{ id: string; name: string } | null> {
  const { width = 24, height = 24, base = '' } = opts;
  try {
    const r = await fetch(`${base}/api/worlds?deviceKey=${encodeURIComponent(deviceKey)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ name: name.trim().slice(0, 40) || 'My World', width, height }),
    });
    if (!r.ok) return null;
    const body = (await r.json()) as { ok?: boolean; id?: string; name?: string };
    if (!body?.ok || !body.id) return null;
    return { id: body.id, name: body.name ?? name };
  } catch {
    return null;
  }
}

/** POST /api/worlds/{id}/publish — draft -> published (idempotent). */
export async function publishWorld(id: string, deviceKey: string, base = ''): Promise<boolean> {
  try {
    const r = await fetch(`${base}/api/worlds/${encodeURIComponent(id)}/publish?deviceKey=${encodeURIComponent(deviceKey)}`, {
      method: 'POST',
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return false;
    const body = (await r.json()) as { ok?: boolean };
    return body?.ok === true;
  } catch {
    return false;
  }
}

/** GET /api/worlds/{id} — single world doc (flags + gravity + headcount). */
export async function fetchWorldDoc(id: string, base = ''): Promise<WorldEntry | null> {
  try {
    const r = await fetch(`${base}/api/worlds/${encodeURIComponent(id)}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return null;
    const body = (await r.json()) as { world?: WorldEntry };
    return body?.world ?? null;
  } catch {
    return null;
  }
}
