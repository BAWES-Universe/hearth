import { describe, expect, it, vi, afterEach } from 'vitest';
import {
  DISTRICT,
  clampLocal,
  districtKey,
  districtWorld,
  fetchDistrict,
  loadFrontier,
  saveFrontier,
  clearFrontier,
} from './frontier';

// BRICK WORLD M1: frontier module contract — server-authoritative district
// fetch, parseWorld-compatible doc conversion, and reload resume persistence.

// Node test env has no DOM — polyfill a minimal localStorage for the resume
// tests (frontier.ts reads the global `localStorage`).
const lsStore = new Map<string, string>();
(globalThis as Record<string, unknown>).localStorage = {
  getItem: (k: string) => lsStore.get(k) ?? null,
  setItem: (k: string, v: string) => {
    lsStore.set(k, v);
  },
  removeItem: (k: string) => {
    lsStore.delete(k);
  },
  clear: () => lsStore.clear(),
};

describe('frontier module (BRICK WORLD M1)', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    lsStore.clear();
  });

  it('fetchDistrict hits the server chunk endpoint and validates the doc', async () => {
    const doc = {
      ok: true,
      world_id: 'town-square',
      cx: 1,
      cy: 0,
      seed: 42,
      district_size: 32,
      width: 32,
      height: 32,
      tiles: [
        { x: 1, y: 1, tileId: 3, t: 'grass' },
        { x: 8, y: 8, tileId: 18, t: 'crystal' },
      ],
      plot: { x: 8, y: 8, claimed: false, claimable: true },
      generated: true,
    };
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => doc,
    });
    vi.stubGlobal('fetch', fetchMock);
    const got = await fetchDistrict('town-square', 1, 0);
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/worlds/town-square/chunk/1/0',
      expect.objectContaining({ headers: { Accept: 'application/json' } }),
    );
    expect(got?.cx).toBe(1);
    expect(got?.plot.claimable).toBe(true);
  });

  it('fetchDistrict returns null on failure — the client never invents terrain', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, json: async () => ({}) }),
    );
    expect(await fetchDistrict('town-square', 1, 0)).toBeNull();
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, json: async () => ({ ok: true, tiles: 'nope' }) }));
    expect(await fetchDistrict('town-square', 1, 0)).toBeNull();
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('offline')));
    expect(await fetchDistrict('town-square', 1, 0)).toBeNull();
  });

  it('districtWorld converts a district doc into a parseWorld-compatible envelope', () => {
    const w = districtWorld({
      ok: true,
      world_id: 'town-square',
      cx: 2,
      cy: -1,
      seed: 7,
      district_size: 32,
      width: 32,
      height: 32,
      tiles: [
        { x: 0, y: 0, tileId: 2, t: 'water' },
        { x: 31, y: 31, tileId: 13, t: 'tree' },
      ],
      plot: { x: 10, y: 10, claimed: false, claimable: true },
      generated: true,
    });
    expect(w.width).toBe(DISTRICT);
    expect(w.height).toBe(DISTRICT);
    expect(w.tiles).toEqual([
      { x: 0, y: 0, tileId: 2 },
      { x: 31, y: 31, tileId: 13 },
    ]);
  });

  it('frontier resume persists and round-trips per world', () => {
    saveFrontier('town-square', { worldId: 'town-square', cx: 1, cy: 0, x: 3.5, y: 14.2 });
    const s = loadFrontier('town-square');
    expect(s).toEqual({ worldId: 'town-square', cx: 1, cy: 0, x: 3.5, y: 14.2 });
    // per-world keying: another world has no saved frontier
    expect(loadFrontier('garden')).toBeNull();
    // corrupt data is ignored, not thrown
    lsStore.set('hearth:frontier:garden', '{nope');
    expect(loadFrontier('garden')).toBeNull();
    clearFrontier('town-square');
    expect(loadFrontier('town-square')).toBeNull();
  });

  it('clampLocal keeps entry points in the walkable interior', () => {
    expect(clampLocal(-5)).toBe(0.6);
    expect(clampLocal(40)).toBe(DISTRICT - 0.6);
    expect(clampLocal(16)).toBe(16);
    expect(districtKey(1, 0)).toBe('1,0');
    expect(districtKey(-2, 3)).toBe('-2,3');
  });
});
