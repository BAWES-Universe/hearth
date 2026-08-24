// T2 avatar platform REST client (docs/AVATARS.md).
//
// All /api/avatars/* routes require the hearth_session cookie — the browser
// gets it from POST /api/auth/guest, so upload/generate/list call
// ensureSession() first (runs once per page load, same pattern as friends).
// The device key is shared with the WS join (localStorage 'hearth:device'),
// so uploaded assets and the joined identity are the same member.

import { ensureSession } from './friends';
import type { AvatarSpec } from '../avatar/spec';

export interface AvatarAsset {
  id: string;
  ownerId: string;
  layer: string;
  name: string;
  kind: string;
  width: number;
  height: number;
  status: string;
  createdAt: string;
}

export interface AvatarSetInfo {
  id: string;
  name: string;
  scope: string;
  worldId?: string;
  version: number;
  createdBy: string;
  createdAt: string;
  items?: string[];
}

/** The same device key App.tsx uses for WS join + guest auth. */
export function avatarDeviceKey(): string {
  let k = localStorage.getItem('hearth:device');
  if (!k) {
    k = typeof crypto !== 'undefined' && crypto.randomUUID ? crypto.randomUUID() : `dk-${Date.now()}-${Math.random().toString(36).slice(2)}`;
    localStorage.setItem('hearth:device', k);
  }
  return k;
}

let sessionReady: Promise<boolean> | null = null;

/** Guest-auth once so the httpOnly session cookie exists for avatar REST. */
function ensureAvatarSession(): Promise<boolean> {
  if (!sessionReady) {
    const key = avatarDeviceKey();
    sessionReady = ensureSession(key, localStorage.getItem('hearth:name') ?? 'Guest');
  }
  return sessionReady;
}

async function avatarFetch(path: string, init?: RequestInit): Promise<Response> {
  await ensureAvatarSession();
  return fetch(path, { ...init, credentials: 'same-origin' });
}

/** POST /api/avatars/assets — upload a base64 image for a layer. */
export async function uploadAvatarAsset(
  layer: string,
  name: string,
  dataBase64: string,
  kind: string,
): Promise<AvatarAsset | null> {
  try {
    const r = await avatarFetch('/api/avatars/assets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ layer, name, kind, data: dataBase64 }),
    });
    const body = (await r.json()) as { ok?: boolean; asset?: AvatarAsset; error?: string };
    return r.ok && body.asset ? body.asset : null;
  } catch {
    return null;
  }
}

/** GET /api/avatars/assets — my active assets (no image bytes). */
export async function listAvatarAssets(): Promise<AvatarAsset[]> {
  try {
    const r = await avatarFetch('/api/avatars/assets', { headers: { Accept: 'application/json' } });
    const body = (await r.json()) as { assets?: AvatarAsset[] };
    return Array.isArray(body?.assets) ? body.assets : [];
  } catch {
    return [];
  }
}

/** DELETE /api/avatars/assets/{id} — safe-archive (409 while worn). */
export async function archiveAvatarAsset(id: string): Promise<{ ok: boolean; error?: string }> {
  try {
    const r = await avatarFetch(`/api/avatars/assets/${encodeURIComponent(id)}`, { method: 'DELETE' });
    const body = (await r.json()) as { ok?: boolean; error?: string };
    return { ok: r.ok && body.ok !== false, error: body.error };
  } catch {
    return { ok: false, error: 'network error' };
  }
}

/** The raw image endpoint for <img>/canvas (same-origin, cookie auth). */
export function avatarAssetUrl(id: string): string {
  return `/api/avatars/assets/${encodeURIComponent(id)}/image`;
}

/** POST /api/avatars/generate — deterministic prompt -> candidate spec. */
export async function generateAvatar(prompt: string): Promise<{ model: string; spec: AvatarSpec } | null> {
  try {
    const r = await avatarFetch('/api/avatars/generate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ prompt }),
    });
    const body = (await r.json()) as { ok?: boolean; model?: string; spec?: AvatarSpec; error?: string };
    return r.ok && body.spec ? { model: body.model ?? 'hearth-catalog-sampler v1', spec: body.spec } : null;
  } catch {
    return null;
  }
}

// ---- sets + grants (governance surface; used by the admin/console surface
// and the live test suite; the picker itself only needs the four above) ----

/** POST /api/avatars/sets — create a versioned collection. */
export async function createAvatarSet(
  name: string,
  scope: string,
  worldId = '',
  items: { layer: string; optionId: string }[] = [],
): Promise<{ ok: boolean; set?: AvatarSetInfo; error?: string }> {
  try {
    const r = await avatarFetch('/api/avatars/sets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ name, scope, worldId, items }),
    });
    const body = (await r.json()) as { ok?: boolean; set?: AvatarSetInfo; error?: string };
    return { ok: r.ok && body.ok !== false, set: body.set, error: body.error };
  } catch {
    return { ok: false, error: 'network error' };
  }
}

/** GET /api/avatars/sets — sets visible to me. */
export async function listAvatarSets(): Promise<AvatarSetInfo[]> {
  try {
    const r = await avatarFetch('/api/avatars/sets', { headers: { Accept: 'application/json' } });
    const body = (await r.json()) as { sets?: AvatarSetInfo[] };
    return Array.isArray(body?.sets) ? body.sets : [];
  } catch {
    return [];
  }
}

/** POST /api/avatars/sets/{id}/items — add an option (bumps version). */
export async function addAvatarSetItem(setId: string, layer: string, optionId: string): Promise<{ ok: boolean; version?: number; error?: string }> {
  try {
    const r = await avatarFetch(`/api/avatars/sets/${encodeURIComponent(setId)}/items`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ layer, optionId }),
    });
    const body = (await r.json()) as { ok?: boolean; version?: number; error?: string };
    return { ok: r.ok && body.ok !== false, version: body.version, error: body.error };
  } catch {
    return { ok: false, error: 'network error' };
  }
}

/** POST /api/avatars/grants — grant a member access to a set's options. */
export async function createAvatarGrant(
  setId: string,
  userId: string,
  kind = 'direct',
  match = '',
  expiresAt = '',
): Promise<{ ok: boolean; grant?: string; error?: string }> {
  try {
    const r = await avatarFetch('/api/avatars/grants', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify({ setId, userId, kind, match, expiresAt }),
    });
    const body = (await r.json()) as { ok?: boolean; grant?: string; error?: string };
    return { ok: r.ok && body.ok !== false, grant: body.grant, error: body.error };
  } catch {
    return { ok: false, error: 'network error' };
  }
}
