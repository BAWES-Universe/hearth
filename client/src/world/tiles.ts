// Procedural tile textures (canvas-generated, no asset files), tile semantics,
// tolerant world parsing, and a deterministic default world fallback.

import { Texture } from 'pixi.js';

export const TILE = 32; // world px per tile cell

export interface TileCell {
  x: number;
  y: number;
  tileId: number;
}

export interface WorldData {
  width: number;
  height: number;
  tiles: TileCell[];
}

export const TILE_DEFS: Record<number, { name: string; passable: boolean }> = {
  0: { name: 'floor', passable: true },
  1: { name: 'wall', passable: false },
  2: { name: 'water', passable: false },
  3: { name: 'grass', passable: true },
  4: { name: 'stone', passable: true },
};

export function isPassableTile(tileId: number): boolean {
  return TILE_DEFS[tileId]?.passable ?? true;
}

function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function makeTex(
  size: number,
  paint: (ctx: CanvasRenderingContext2D, rnd: () => number, seed: number) => void,
  seedExtra = 0,
): Texture {
  const c = document.createElement('canvas');
  c.width = size;
  c.height = size;
  const ctx = c.getContext('2d')!;
  paint(ctx, mulberry32(((size * 9301 + 49297) ^ (seedExtra * 2654435761)) >>> 0), seedExtra);
  return Texture.from(c);
}

/**
 * Build the tile atlas: 3 deterministic variants per tileId (32x32 each),
 * keyed `tileId * VARIANTS + variant`. Variant is chosen per cell by
 * `variant = (x*31 + y*17) % 3` — periodic with period 3 in both axes, so a
 * 3x3 repeat pattern of the floor texture matches the formula exactly.
 * Dusk/ember palette per the visual system (§2/§3), warm 1px top edge.
 */
export const VARIANTS = 3;

/** Warm top edge every tile catches — "wet cobbles catching amber light". */
const WARM_EDGE = 'rgba(255,196,107,0.10)';

const PAL = {
  floor: '#241633',
  floorLight: '#2c1c3f',
  floorDark: '#1c1229',
  wall: '#2a2040',
  wallTop: '#3d3059',
  wallDark: '#211836',
  water: '#1c2f52',
  waterHi: '#5a7fae',
  grass: '#2e4a35',
  grassHi: '#4a6b45',
  grassDark: '#223a28',
  stone: '#3a3040',
  stoneDark: '#2e2633',
};

/** Deterministic variant texture key: `tileId*VARIANTS + variant`. */
export function texKey(tileId: number, variant: number): number {
  return tileId * VARIANTS + (variant % VARIANTS);
}

/** Deterministic per-cell variant: (x*31 + y*17) % 3. */
export function tileVariant(x: number, y: number): number {
  return ((x * 31 + y * 17) % VARIANTS + VARIANTS) % VARIANTS;
}

/** Jittered speckle: lighter/darker dither (~±8% tone) for hand-made feel. */
function speckle(ctx: CanvasRenderingContext2D, rnd: () => number, tones: string[], n: number): void {
  for (let i = 0; i < n; i++) {
    ctx.fillStyle = tones[(rnd() * tones.length) | 0];
    ctx.fillRect((rnd() * TILE) | 0, (rnd() * TILE) | 0, 2, 2);
  }
}

export function generateTileTextures(): Record<number, Texture> {
  const S = TILE;
  const tex: Record<number, Texture> = {};

  for (let v = 0; v < VARIANTS; v++) {
    const vShift = (v - 1) * 6; // ±6 lightness drift per variant

    // wall — raised block, warm lip, brick joints shifted per variant
    tex[texKey(1, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.wall, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade(PAL.wallTop, vShift);
      ctx.fillRect(0, 0, S, 4);
      ctx.fillStyle = shade(PAL.wallDark, vShift);
      ctx.fillRect(0, S - 3, S, 3);
      ctx.fillStyle = 'rgba(255,255,255,0.05)';
      ctx.fillRect(0, 4, S, 1);
      ctx.fillStyle = 'rgba(0,0,0,0.22)';
      const jx = v * 5;
      ctx.fillRect(8 + jx, 10, 1, 10);
      ctx.fillRect(20 - jx, 14, 1, 10);
      ctx.fillRect((v * 9) % S, 18, S, 1);
      speckle(ctx, rnd, ['rgba(255,196,107,0.05)', 'rgba(0,0,0,0.12)'], 4);
    });

    // water — deep dusk blue, wave arcs phase-shifted per variant
    tex[texKey(2, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.water, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.strokeStyle = 'rgba(90,127,174,0.55)';
      ctx.lineWidth = 1.5;
      const ph = v * 5;
      for (let i = 0; i < 3; i++) {
        const y = 8 + i * 9;
        ctx.beginPath();
        ctx.moveTo(3, y + ph - 3);
        ctx.quadraticCurveTo(10, y + ph - 6, 17, y + ph - 3);
        ctx.quadraticCurveTo(24, y + ph, 30, y + ph - 3);
        ctx.stroke();
      }
      speckle(ctx, rnd, ['rgba(90,127,174,0.12)', 'rgba(0,0,0,0.2)'], 5);
    });

    // grass — dusk green with lighter blades
    tex[texKey(3, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.grass, vShift);
      ctx.fillRect(0, 0, S, S);
      speckle(ctx, rnd, [PAL.grassHi, PAL.grass, PAL.grassDark], 10);
      ctx.fillStyle = 'rgba(122,196,110,0.14)';
      const bx = v * 7;
      ctx.fillRect(4 + bx, 6, 2, 4);
      ctx.fillRect(18 - bx, 16, 2, 4);
      ctx.fillRect(26, 24 + (v % 2) * 3, 2, 4);
    });

    // stone — slate with cracked joints
    tex[texKey(4, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.stone, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.strokeStyle = 'rgba(0,0,0,0.38)';
      ctx.lineWidth = 1;
      ctx.strokeRect(1.5, 1.5, S - 3, S - 3);
      ctx.fillStyle = 'rgba(255,255,255,0.06)';
      ctx.fillRect(3 + v * 3, 3, 7, 2);
      ctx.strokeStyle = 'rgba(0,0,0,0.25)';
      ctx.beginPath();
      ctx.moveTo(6 + v * 4, 14);
      ctx.lineTo(12 + v * 3, 20);
      ctx.stroke();
      speckle(ctx, rnd, ['rgba(255,255,255,0.05)', 'rgba(0,0,0,0.15)'], 4);
    });

    // unknown tileId — keep the hot-pink checker (obvious mismatch), 3 tints
    tex[texKey(99, v)] = makeTex(S, (ctx) => {
      ctx.fillStyle = '#330011';
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = v === 1 ? '#ff00aa' : v === 2 ? '#ff55cc' : '#cc0088';
      ctx.fillRect(0, 0, S / 2, S / 2);
      ctx.fillRect(S / 2, S / 2, S / 2, S / 2);
    });
  }

  return tex;
}

/**
 * Floor is implicit in the sparse wire format, so it renders as one repeating
 * 3x3-cell pattern (96x96) that exactly matches the per-cell variant formula
 * (period 3 in both axes). Warm top edge + ±8% dither per cell.
 */
export function generateFloorTexture(): Texture {
  const c = document.createElement('canvas');
  c.width = TILE * VARIANTS;
  c.height = TILE * VARIANTS;
  const ctx = c.getContext('2d')!;
  for (let cy = 0; cy < VARIANTS; cy++) {
    for (let cx = 0; cx < VARIANTS; cx++) {
      const v = tileVariant(cx, cy);
      const vShift = (v - 1) * 6;
      const x = cx * TILE;
      const y = cy * TILE;
      ctx.fillStyle = shade(PAL.floor, vShift);
      ctx.fillRect(x, y, TILE, TILE);
      const rnd = mulberry32((x * 9301 + y * 49297 + 11) >>> 0);
      speckle(ctx, rnd, [PAL.floorLight, PAL.floor, PAL.floorDark], 6);
      ctx.fillStyle = WARM_EDGE;
      ctx.fillRect(x, y, TILE, 1);
      ctx.fillStyle = 'rgba(0,0,0,0.22)';
      ctx.fillRect(x, y + TILE - 1, TILE, 1);
    }
  }
  const tex = Texture.from(c);
  tex.source.style.addressMode = 'repeat';
  return tex;
}

/** Simple hex shade shift (can be negative/positive, clamped). */
function shade(hex: string, delta: number): string {
  const n = parseInt(hex.slice(1), 16);
  const r = clamp8(((n >> 16) & 0xff) + delta);
  const g = clamp8(((n >> 8) & 0xff) + delta);
  const b = clamp8((n & 0xff) + delta);
  return `rgb(${r},${g},${b})`;
}

function clamp8(v: number): number {
  return v < 0 ? 0 : v > 255 ? 255 : v;
}

/**
 * Tolerant world parser. Accepts:
 *  - { width, height, tiles: [{x,y,tileId}] }
 *  - { w, h, grid: number[][] } / { grid: number[][] }
 *  - { width, height, tiles: number[] } (row-major)
 *  - { tiles: number[][] }
 * Returns null when unparseable → caller falls back to genDefaultWorld().
 */
export function parseWorld(w: unknown): WorldData | null {
  if (!w || typeof w !== 'object') return null;
  const o = w as Record<string, unknown>;
  const tilesRaw = (o.tiles ?? o.grid ?? o.tilemap) as unknown;
  const arr = Array.isArray(tilesRaw) ? tilesRaw : null;

  let width = typeof o.width === 'number' ? o.width : typeof o.w === 'number' ? o.w : 0;
  let height = typeof o.height === 'number' ? o.height : typeof o.h === 'number' ? o.h : 0;
  if (arr) {
    if (!width && arr.length && typeof arr[0] === 'number') width = Math.round(Math.sqrt(arr.length));
    if (!height && arr.length && typeof arr[0] === 'number') height = Math.ceil(arr.length / width);
    if (!width && arr.length) width = arr.length;
    if (!height && Array.isArray(arr[0])) height = (arr[0] as unknown[]).length;
  }
  if (!width || !height || width > 256 || height > 256) return null;

  const tiles: TileCell[] = [];
  if (arr) {
    if (arr.length && typeof arr[0] === 'number') {
      // flat row-major
      for (let y = 0; y < height; y++)
        for (let x = 0; x < width; x++) {
          const v = (arr as number[])[y * width + x];
          if (v !== undefined && v !== 0) tiles.push({ x, y, tileId: v });
        }
    } else if (Array.isArray(arr[0])) {
      // 2D [y][x]
      for (let y = 0; y < arr.length; y++) {
        const row = arr[y] as number[];
        for (let x = 0; x < row.length; x++) {
          const v = row[x];
          if (v !== undefined && v !== 0) tiles.push({ x, y, tileId: v });
        }
      }
    } else {
      // objects
      for (const t of arr as Record<string, unknown>[]) {
        if (t && typeof t.x === 'number' && typeof t.y === 'number') {
          const tid = typeof t.tileId === 'number' ? t.tileId : typeof t.id === 'number' ? t.id : 0;
          tiles.push({ x: t.x, y: t.y, tileId: tid });
        }
      }
    }
  }
  return { width, height, tiles };
}

/** Deterministic 32x32 default world: walled border, rooms, ponds. */
export function genDefaultWorld(w = 32, h = 32): WorldData {
  const rnd = mulberry32(7);
  const g: number[][] = Array.from({ length: h }, () => new Array<number>(w).fill(0));
  for (let x = 0; x < w; x++) {
    g[0][x] = 1;
    g[h - 1][x] = 1;
  }
  for (let y = 0; y < h; y++) {
    g[y][0] = 1;
    g[y][w - 1] = 1;
  }
  const rooms: [number, number, number, number][] = [
    [5, 5, 10, 10],
    [17, 4, 23, 9],
    [6, 15, 12, 21],
    [18, 17, 25, 24],
  ];
  for (const [x1, y1, x2, y2] of rooms)
    for (let y = y1; y <= y2; y++) for (let x = x1; x <= x2; x++) if (g[y][x] !== 1) g[y][x] = 0;
  for (let i = 0; i < 12; i++) {
    const x = 3 + ((rnd() * (w - 6)) | 0);
    const y = 3 + ((rnd() * (h - 6)) | 0);
    const len = 2 + ((rnd() * 4) | 0);
    const horiz = rnd() > 0.5;
    for (let k = 0; k < len; k++) {
      const xx = horiz ? x + k : x;
      const yy = horiz ? y : y + k;
      if (xx > 0 && xx < w - 1 && yy > 0 && yy < h - 1 && g[yy][xx] === 0) g[yy][xx] = 1;
    }
  }
  const px = (w >> 1) + 1;
  const py = (h >> 1) + 1;
  for (let dy = -2; dy <= 2; dy++)
    for (let dx = -2; dx <= 2; dx++)
      if (Math.abs(dx) + Math.abs(dy) <= 2) {
        const xx = px + dx;
        const yy = py + dy;
        if (g[yy][xx] !== 1) g[yy][xx] = 2;
      }
  const tiles: TileCell[] = [];
  for (let y = 0; y < h; y++) for (let x = 0; x < w; x++) tiles.push({ x, y, tileId: g[y][x] });
  return { width: w, height: h, tiles };
}
