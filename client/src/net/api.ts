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
  /** Server-rendered HMF v1 preview thumbnail URL (T2 directory gravity). */
  thumbnail?: string | null;
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

/** POST /api/worlds — template-seeded draft owned by the device-key user.
 *  Templates: empty_lot (default, no walls) | cozy_room | plaza. */
export async function createWorld(
  name: string,
  deviceKey: string,
  opts: { width?: number; height?: number; template?: string; handle?: string; base?: string } = {},
): Promise<{ id: string; name: string; template?: string; spawn?: { x: number; y: number } } | null> {
  const { width = 24, height = 24, base = '' } = opts;
  try {
    const r = await fetch(`${base}/api/worlds?deviceKey=${encodeURIComponent(deviceKey)}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({
        name: name.trim().slice(0, 40) || 'My World',
        width,
        height,
        template: opts.template && opts.template !== 'empty_lot' ? opts.template : undefined,
        handle: opts.handle?.trim() || undefined,
      }),
    });
    if (!r.ok) return null;
    const body = (await r.json()) as { ok?: boolean; id?: string; name?: string; template?: string; spawn?: { x: number; y: number } };
    if (!body?.ok || !body.id) return null;
    return { id: body.id, name: body.name ?? name, template: body.template, spawn: body.spawn };
  } catch {
    return null;
  }
}

/** One world in the session user's dashboard (/api/me, /api/worlds/mine). */
export interface MyWorld {
  id: string;
  name: string;
  role: 'owner' | 'editor';
  is_showcase?: boolean;
  is_published?: boolean;
  created_at?: string;
}

/** GET /api/worlds/mine — worlds the session user owns or edits. */
export async function myWorlds(): Promise<MyWorld[]> {
  try {
    const r = await fetch('/api/worlds/mine', { headers: { Accept: 'application/json' } });
    if (!r.ok) return [];
    const body = (await r.json()) as { worlds?: MyWorld[] };
    return Array.isArray(body?.worlds) ? body.worlds : [];
  } catch {
    return [];
  }
}

/** POST /api/worlds/{id}/invite — mint a single-use editor invite (owner/editor). */
export async function createInvite(id: string): Promise<string | null> {
  try {
    const r = await fetch(`/api/worlds/${encodeURIComponent(id)}/invite`, {
      method: 'POST',
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return null;
    const body = (await r.json()) as { ok?: boolean; token?: string; url?: string };
    if (!body?.ok || !body.token) return null;
    return body.url ?? body.token;
  } catch {
    return null;
  }
}

/** GET /api/worlds/join?invite=<token> — redeem an invite, granting editor role. */
export async function joinInvite(token: string): Promise<{ worldId: string; name: string } | null> {
  try {
    const r = await fetch(`/api/worlds/join?invite=${encodeURIComponent(token)}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return null;
    const body = (await r.json()) as { ok?: boolean; worldId?: string; name?: string };
    if (!body?.ok || !body.worldId) return null;
    return { worldId: body.worldId, name: body.name ?? '' };
  } catch {
    return null;
  }
}

/** GET /api/me — session identity + the user's worlds. */
export async function fetchMe(): Promise<{ userId: string; name: string; worlds: MyWorld[] } | null> {
  try {
    const r = await fetch('/api/me', { headers: { Accept: 'application/json' } });
    if (!r.ok) return null;
    const body = (await r.json()) as { ok?: boolean; userId?: string; name?: string; worlds?: MyWorld[] };
    if (!body?.ok) return null;
    return { userId: body.userId ?? '', name: body.name ?? '', worlds: Array.isArray(body.worlds) ? body.worlds : [] };
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
