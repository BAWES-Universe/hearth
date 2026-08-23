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

function makeTex(size: number, paint: (ctx: CanvasRenderingContext2D, rnd: () => number) => void): Texture {
  const c = document.createElement('canvas');
  c.width = size;
  c.height = size;
  const ctx = c.getContext('2d')!;
  paint(ctx, mulberry32((size * 9301 + 49297) >>> 0));
  return Texture.from(c);
}

/** Build the small tile atlas: one shared Texture per tileId (32x32). */
export function generateTileTextures(): Record<number, Texture> {
  const S = TILE;
  const tex: Record<number, Texture> = {};

  tex[0] = makeTex(S, (ctx, rnd) => {
    // floor — dark purple slab with faint speckle
    ctx.fillStyle = '#241b33';
    ctx.fillRect(0, 0, S, S);
    for (let i = 0; i < 6; i++) {
      ctx.fillStyle = '#2c2140';
      ctx.fillRect((rnd() * S) | 0, (rnd() * S) | 0, 2, 2);
    }
    ctx.fillStyle = 'rgba(0,0,0,0.25)';
    ctx.fillRect(0, S - 1, S, 1);
  });

  tex[1] = makeTex(S, (ctx) => {
    // wall — raised block with highlight lip + brick joints
    ctx.fillStyle = '#3b3154';
    ctx.fillRect(0, 0, S, S);
    ctx.fillStyle = '#554a78';
    ctx.fillRect(0, 0, S, 4);
    ctx.fillStyle = '#2a2240';
    ctx.fillRect(0, S - 3, S, 3);
    ctx.fillStyle = 'rgba(255,255,255,0.06)';
    ctx.fillRect(0, 4, S, 1);
    ctx.fillStyle = 'rgba(0,0,0,0.18)';
    ctx.fillRect(8, 10, 1, 12);
    ctx.fillRect(20, 14, 1, 12);
    ctx.fillRect(0, 14, S, 1);
  });

  tex[2] = makeTex(S, (ctx) => {
    // water — deep blue with animated-looking wave arcs
    ctx.fillStyle = '#16304e';
    ctx.fillRect(0, 0, S, S);
    ctx.strokeStyle = 'rgba(120,180,230,0.5)';
    ctx.lineWidth = 1.5;
    for (let i = 0; i < 3; i++) {
      const y = 8 + i * 9;
      ctx.beginPath();
      ctx.moveTo(3, y);
      ctx.quadraticCurveTo(10, y - 3, 17, y);
      ctx.quadraticCurveTo(24, y + 3, 30, y);
      ctx.stroke();
    }
  });

  tex[3] = makeTex(S, (ctx, rnd) => {
    // grass
    ctx.fillStyle = '#1f331d';
    ctx.fillRect(0, 0, S, S);
    for (let i = 0; i < 10; i++) {
      ctx.fillStyle = rnd() > 0.5 ? '#2a4226' : '#182a17';
      ctx.fillRect((rnd() * S) | 0, (rnd() * S) | 0, 2, 2);
    }
  });

  tex[4] = makeTex(S, (ctx) => {
    // stone
    ctx.fillStyle = '#2e2b3a';
    ctx.fillRect(0, 0, S, S);
    ctx.strokeStyle = 'rgba(0,0,0,0.4)';
    ctx.lineWidth = 1;
    ctx.strokeRect(1.5, 1.5, S - 3, S - 3);
    ctx.fillStyle = 'rgba(255,255,255,0.05)';
    ctx.fillRect(3, 3, 8, 3);
  });

  tex[99] = makeTex(S, (ctx) => {
    // unknown tileId — hot pink checker so mismatches are obvious
    ctx.fillStyle = '#330011';
    ctx.fillRect(0, 0, S, S);
    ctx.fillStyle = '#ff00aa';
    ctx.fillRect(0, 0, S / 2, S / 2);
    ctx.fillRect(S / 2, S / 2, S / 2, S / 2);
  });

  return tex;
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
