// frontier.ts — BRICK WORLD, Milestone 1 (the gate): procedural infinite world.
//
// Walking off the edge of an authored world fetches a SEEDED, DETERMINISTIC
// frontier district from the server (GET /api/worlds/{id}/chunk/{cx}/{cy}).
// The server is the authority: every client and every reload renders the
// exact same tiles for the same (world_id, cx, cy). This module handles the
// fetch, the parseWorld-compatible doc conversion, and localStorage resume so
// a reload drops you back in the same district at the same spot.

import type { WorldData } from './tiles';

/** Edge length (tiles) of a generated frontier district. Must match the
 *  server's DistrictSize — the client uses it for entry-point math only. */
export const DISTRICT = 32;

export interface PlotMarker {
  x: number;
  y: number;
  claimed: boolean;
  claimable: boolean;
}

/** Wire shape of GET /api/worlds/{id}/chunk/{cx}/{cy}. */
export interface DistrictDoc {
  ok: boolean;
  world_id: string;
  cx: number;
  cy: number;
  seed: number;
  district_size: number;
  width: number;
  height: number;
  tiles: { x: number; y: number; tileId: number; t?: string }[];
  plot: PlotMarker;
  generated: boolean;
}

/** The player's persisted frontier location for one world (reload resume). */
export interface FrontierState {
  worldId: string;
  cx: number;
  cy: number;
  x: number;
  y: number;
}

/** Fetch a district from the server. Returns null on any failure — the
 *  caller treats null as "stay put" (never invent terrain client-side). */
export async function fetchDistrict(
  worldId: string,
  cx: number,
  cy: number,
  base = '',
): Promise<DistrictDoc | null> {
  try {
    const r = await fetch(`${base}/api/worlds/${encodeURIComponent(worldId)}/chunk/${cx}/${cy}`, {
      headers: { Accept: 'application/json' },
    });
    if (!r.ok) return null;
    const body = (await r.json()) as DistrictDoc;
    if (!body?.ok || !Array.isArray(body.tiles) || body.generated !== true) return null;
    return body;
  } catch {
    return null;
  }
}

export function districtKey(cx: number, cy: number): string {
  return `${cx},${cy}`;
}

/** Convert a district doc into the parseWorld-compatible world envelope. */
export function districtWorld(d: DistrictDoc): WorldData {
  return {
    width: d.width || DISTRICT,
    height: d.height || DISTRICT,
    tiles: (d.tiles ?? []).map((t) => ({ x: t.x, y: t.y, tileId: t.tileId })),
  };
}

const LS_PREFIX = 'hearth:frontier:';

export function saveFrontier(worldId: string, s: FrontierState): void {
  try {
    localStorage.setItem(LS_PREFIX + worldId, JSON.stringify(s));
  } catch {
    /* private mode / quota — resume is a nicety, not a requirement */
  }
}

export function loadFrontier(worldId: string): FrontierState | null {
  try {
    const raw = localStorage.getItem(LS_PREFIX + worldId);
    if (!raw) return null;
    const s = JSON.parse(raw) as FrontierState;
    if (!s || typeof s.cx !== 'number' || typeof s.cy !== 'number' || typeof s.x !== 'number' || typeof s.y !== 'number') {
      return null;
    }
    return s;
  } catch {
    return null;
  }
}

export function clearFrontier(worldId: string): void {
  try {
    localStorage.removeItem(LS_PREFIX + worldId);
  } catch {
    /* noop */
  }
}

/** Clamp a district-local coordinate into the walkable interior. */
export function clampLocal(v: number): number {
  return Math.min(Math.max(v, 0.6), DISTRICT - 0.6);
}
