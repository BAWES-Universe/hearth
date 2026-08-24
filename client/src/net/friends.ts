// T2 social layer REST client (docs/SOCIAL.md).
//
// All /api/friends routes require the hearth_session cookie. The browser gets
// that cookie from POST /api/auth/guest (the WS handshake alone does not set
// it — join authenticates in-band), so every call here goes through
// ensureSession() which performs guest auth once per page load.

export interface FriendEntry {
  friendId: string;
  name: string;
  status: 'pending' | 'requested' | 'accepted';
  online: boolean;
  space?: string;
  since?: string;
}

export interface UserHit {
  id: string;
  name: string;
  online: boolean;
}

export interface ActivityRow {
  id?: number;
  worldId?: string;
  actor?: string;
  role?: string;
  kind?: string;
  action?: string;
  target?: string;
  diff?: string;
  ts?: string;
}

let sessionPromise: Promise<boolean> | null = null;

/** Guest-auth once per page load so the httpOnly session cookie exists for REST. */
export function ensureSession(deviceKey: string, name: string, base = ''): Promise<boolean> {
  if (!sessionPromise) {
    sessionPromise = (async () => {
      try {
        const r = await fetch(`${base}/api/auth/guest`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
          body: JSON.stringify({ deviceKey, name }),
          credentials: 'same-origin',
        });
        return r.ok;
      } catch {
        return false;
      }
    })();
  }
  return sessionPromise;
}

async function authFetch(base: string, path: string, init?: RequestInit): Promise<Response> {
  return fetch(`${base}${path}`, { ...init, credentials: 'same-origin' });
}

export async function listFriends(base = ''): Promise<FriendEntry[]> {
  try {
    const r = await authFetch(base, '/api/friends', { headers: { Accept: 'application/json' } });
    if (!r.ok) return [];
    const body = (await r.json()) as { friends?: FriendEntry[] };
    return Array.isArray(body?.friends) ? body.friends : [];
  } catch {
    return [];
  }
}

export async function addFriend(friendId: string, base = ''): Promise<boolean> {
  try {
    const r = await authFetch(base, '/api/friends', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ friendId }),
    });
    return r.ok;
  } catch {
    return false;
  }
}

export async function respondFriend(friendId: string, action: 'accept' | 'decline', base = ''): Promise<boolean> {
  try {
    const r = await authFetch(base, `/api/friends/${encodeURIComponent(friendId)}/${action}`, {
      method: 'POST',
      headers: { Accept: 'application/json' },
    });
    return r.ok;
  } catch {
    return false;
  }
}

export async function removeFriend(friendId: string, base = ''): Promise<boolean> {
  try {
    const r = await authFetch(base, `/api/friends/${encodeURIComponent(friendId)}`, {
      method: 'DELETE',
      headers: { Accept: 'application/json' },
    });
    return r.ok;
  } catch {
    return false;
  }
}

export async function searchUsers(q: string, base = ''): Promise<UserHit[]> {
  try {
    const r = await authFetch(base, `/api/users?q=${encodeURIComponent(q.trim())}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return [];
    const body = (await r.json()) as { users?: UserHit[] };
    return Array.isArray(body?.users) ? body.users : [];
  } catch {
    return [];
  }
}

/** Recent activity rows of a world's feed (friend actions surface here). */
export async function fetchActivity(worldId: string, limit = 12, base = ''): Promise<ActivityRow[]> {
  try {
    const r = await fetch(`${base}/api/worlds/${encodeURIComponent(worldId)}/activity?limit=${limit}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return [];
    const body = (await r.json()) as { events?: ActivityRow[] };
    return Array.isArray(body?.events) ? body.events : [];
  } catch {
    return [];
  }
}
