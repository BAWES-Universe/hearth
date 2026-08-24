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
  5: { name: 'sand', passable: true },
  6: { name: 'path', passable: true },
  7: { name: 'wood', passable: true },
  8: { name: 'lava', passable: false },
  9: { name: 'ice', passable: true },
  10: { name: 'flower', passable: true },
  11: { name: 'bush', passable: false },
  12: { name: 'rock', passable: false },
  13: { name: 'tree', passable: false },
  14: { name: 'roof', passable: false },
  15: { name: 'door', passable: true },
  16: { name: 'fence', passable: false },
  17: { name: 'bridge', passable: true },
  18: { name: 'crystal', passable: false },
  19: { name: 'dirt', passable: true },
  20: { name: 'torch', passable: false }, // T2 animated
  21: { name: 'glow', passable: true }, // T2 animated
};

/** Server-authoritative animated-tile table (server/hmf AnimatedTiles). The
 *  server embeds frames+fps in every world doc ("anims"); the client derives
 *  playback from that, never from a hardcoded table. */
export interface AnimDef {
  tileId: number;
  name: string;
  frames: number;
  fps: number;
}

export function isPassableTile(tileId: number): boolean {
  return TILE_DEFS[tileId]?.passable ?? true;
}

export function isAnimatedTile(tileId: number): boolean {
  return tileId === 2 || tileId === 8 || tileId === 20 || tileId === 21;
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
  sand: '#4a3d2c',
  sandHi: '#5f4f38',
  sandDark: '#3a3022',
  path: '#3b3345',
  pathHi: '#4c4358',
  wood: '#3a2a22',
  woodHi: '#4c382c',
  woodDark: '#2c201a',
  ice: '#31435c',
  iceHi: '#7fa3c9',
  lava: '#4a1c12',
  lavaHi: '#ff7a2a',
  lavaHot: '#ffc44a',
  flower: '#2e4a35',
  flowerPetal: '#d96a8c',
  flowerCore: '#ffd25e',
  bush: '#1f3a24',
  bushHi: '#2f5635',
  rock: '#38313f',
  rockHi: '#4b4254',
  tree: '#2a241f',
  treeLeaf: '#2f4a30',
  treeLeafHi: '#3f6138',
  roof: '#45344f',
  roofHi: '#5a4566',
  roofDark: '#332636',
  door: '#5a3b26',
  doorHi: '#6f4a30',
  doorDark: '#452d1c',
  fence: '#4a3523',
  fenceHi: '#60452e',
  bridge: '#4a3624',
  bridgeHi: '#604830',
  crystal: '#3a3f5c',
  crystalHi: '#8fa8d9',
  dirt: '#3a2c20',
  dirtHi: '#4a3a2a',
  torch: '#2a2018',
  torchFlame: '#ffb347',
  torchCore: '#fff2c4',
  glow: '#2a2140',
  glowHi: '#c9a8ff',
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

    // sand — warm dunes with darker ripples
    tex[texKey(5, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.sand, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.strokeStyle = 'rgba(0,0,0,0.15)';
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(0, 12 + v * 2);
      ctx.quadraticCurveTo(10, 9 + v * 2, 20, 12 + v * 2);
      ctx.quadraticCurveTo(27, 14 + v * 2, 32, 11 + v * 2);
      ctx.stroke();
      speckle(ctx, rnd, [PAL.sandHi, PAL.sand, PAL.sandDark], 6);
    });

    // path — worn dirt track with pebbles
    tex[texKey(6, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.path, vShift);
      ctx.fillRect(0, 0, S, S);
      speckle(ctx, rnd, [PAL.pathHi, PAL.path, '#2c2634'], 8);
      ctx.fillStyle = 'rgba(0,0,0,0.18)';
      ctx.fillRect(4, 6 + v * 3, 3, 2);
      ctx.fillRect(20, 18 + v, 4, 2);
    });

    // wood — planks with grain
    tex[texKey(7, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.wood, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.strokeStyle = 'rgba(0,0,0,0.3)';
      ctx.lineWidth = 1;
      const off = v * 3;
      ctx.strokeRect(0.5, 8 + off, S - 1, 0);
      ctx.strokeRect(0.5, 20 + off, S - 1, 0);
      ctx.strokeStyle = 'rgba(255,255,255,0.06)';
      ctx.beginPath();
      ctx.moveTo(6, 0);
      ctx.lineTo(10, S);
      ctx.moveTo(22, 0);
      ctx.lineTo(19, S);
      ctx.stroke();
      speckle(ctx, rnd, [PAL.woodHi, PAL.wood, PAL.woodDark], 4);
    });

    // lava — molten cracks, dark rock base
    tex[texKey(8, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.lava, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.strokeStyle = PAL.lavaHi;
      ctx.lineWidth = 2;
      const ph = v * 4;
      ctx.beginPath();
      ctx.moveTo(0, 10 + ph);
      ctx.lineTo(8, 8 + ph);
      ctx.lineTo(16, 13 + ph);
      ctx.lineTo(24, 9 + ph);
      ctx.lineTo(32, 12 + ph);
      ctx.stroke();
      ctx.strokeStyle = PAL.lavaHot;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(4, 22 - ph);
      ctx.lineTo(12, 20 - ph);
      ctx.lineTo(20, 24 - ph);
      ctx.lineTo(28, 21 - ph);
      ctx.stroke();
      speckle(ctx, rnd, ['rgba(255,122,42,0.25)', 'rgba(0,0,0,0.3)'], 5);
    });

    // ice — frozen slab with highlights
    tex[texKey(9, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.ice, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = 'rgba(127,163,201,0.35)';
      ctx.fillRect(3 + v * 2, 4, 10, 2);
      ctx.fillRect(18 - v, 18, 8, 2);
      ctx.strokeStyle = 'rgba(255,255,255,0.18)';
      ctx.strokeRect(1.5, 1.5, S - 3, S - 3);
      speckle(ctx, rnd, ['rgba(127,163,201,0.2)', 'rgba(0,0,0,0.12)'], 4);
    });

    // flower — grass base with a small bloom
    tex[texKey(10, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.flower, vShift);
      ctx.fillRect(0, 0, S, S);
      speckle(ctx, rnd, [PAL.grassHi, PAL.grass, PAL.grassDark], 8);
      const cx = 10 + v * 6;
      const cy = 12 + (v % 2) * 6;
      ctx.fillStyle = PAL.flowerPetal;
      ctx.beginPath();
      ctx.arc(cx, cy, 4, 0, Math.PI * 2);
      ctx.fill();
      ctx.fillStyle = PAL.flowerCore;
      ctx.beginPath();
      ctx.arc(cx, cy, 1.8, 0, Math.PI * 2);
      ctx.fill();
    });

    // bush — dense dark scrub
    tex[texKey(11, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.bush, vShift);
      ctx.fillRect(0, 0, S, S);
      for (let i = 0; i < 5; i++) {
        ctx.fillStyle = i % 2 ? PAL.bushHi : PAL.bush;
        ctx.beginPath();
        ctx.arc(6 + rnd() * 20, 8 + rnd() * 18, 5 + rnd() * 3, 0, Math.PI * 2);
        ctx.fill();
      }
      speckle(ctx, rnd, [PAL.bushHi, PAL.bush], 5);
    });

    // rock — boulder with a lit top edge
    tex[texKey(12, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.rock, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade(PAL.rockHi, vShift);
      ctx.beginPath();
      ctx.ellipse(16, 18, 11, 8, 0, 0, Math.PI * 2);
      ctx.fill();
      ctx.fillStyle = 'rgba(255,196,107,0.14)';
      ctx.beginPath();
      ctx.ellipse(13, 13, 6, 3, -0.4, 0, Math.PI * 2);
      ctx.fill();
      speckle(ctx, rnd, ['rgba(255,255,255,0.06)', 'rgba(0,0,0,0.2)'], 4);
    });

    // tree — trunk + leafy crown
    tex[texKey(13, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.tree, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade('#5a4030', vShift);
      ctx.fillRect(13 + v, 20, 6, 12);
      for (let i = 0; i < 3; i++) {
        ctx.fillStyle = i % 2 ? PAL.treeLeafHi : PAL.treeLeaf;
        ctx.beginPath();
        ctx.arc(16, 10 + i * 5, 9 - i, 0, Math.PI * 2);
        ctx.fill();
      }
      speckle(ctx, rnd, [PAL.treeLeafHi, PAL.treeLeaf], 5);
    });

    // roof — tiled slope
    tex[texKey(14, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.roof, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.strokeStyle = shade(PAL.roofDark, vShift);
      ctx.lineWidth = 1;
      for (let x = 2; x < S; x += 8) {
        ctx.beginPath();
        ctx.moveTo(x, 0);
        ctx.lineTo(x - 6, S);
        ctx.stroke();
      }
      ctx.fillStyle = 'rgba(255,196,107,0.12)';
      ctx.fillRect(0, 0, S, 2);
      speckle(ctx, rnd, [PAL.roofHi, PAL.roof, PAL.roofDark], 4);
    });

    // door — framed entry with handle
    tex[texKey(15, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.floor, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade(PAL.door, vShift);
      ctx.fillRect(4, 2, S - 8, S - 4);
      ctx.fillStyle = shade(PAL.doorHi, vShift);
      ctx.fillRect(4, 2, S - 8, 3);
      ctx.fillStyle = shade(PAL.doorDark, vShift);
      ctx.fillRect(4, S - 6, S - 8, 4);
      ctx.fillStyle = PAL.torchFlame;
      ctx.beginPath();
      ctx.arc(S - 9, 18, 1.6, 0, Math.PI * 2);
      ctx.fill();
      speckle(ctx, rnd, [PAL.doorHi, PAL.door, PAL.doorDark], 3);
    });

    // fence — rail slats
    tex[texKey(16, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.grass, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade(PAL.fence, vShift);
      for (let x = 3; x < S; x += 9) {
        ctx.fillRect(x, 4 + v, 3, S - 8 - v);
      }
      ctx.fillStyle = shade(PAL.fenceHi, vShift);
      ctx.fillRect(0, 10 + v, S, 3);
      ctx.fillRect(0, 20 + v, S, 3);
      speckle(ctx, rnd, ['rgba(255,255,255,0.05)', 'rgba(0,0,0,0.15)'], 3);
    });

    // bridge — planks over floor
    tex[texKey(17, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.bridge, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade(PAL.bridgeHi, vShift);
      ctx.fillRect(0, 2, S, 4);
      ctx.fillRect(0, S - 6, S, 4);
      ctx.strokeStyle = 'rgba(0,0,0,0.3)';
      ctx.lineWidth = 1;
      for (let x = 4 + v * 2; x < S; x += 8) {
        ctx.beginPath();
        ctx.moveTo(x, 0);
        ctx.lineTo(x, S);
        ctx.stroke();
      }
      speckle(ctx, rnd, [PAL.bridgeHi, PAL.bridge], 4);
    });

    // crystal — glowing shard
    tex[texKey(18, v)] = makeTex(S, (ctx, _rnd) => {
      ctx.fillStyle = shade(PAL.crystal, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = PAL.crystalHi;
      ctx.beginPath();
      ctx.moveTo(16, 3);
      ctx.lineTo(24, 22);
      ctx.lineTo(20, 28);
      ctx.lineTo(10, 26);
      ctx.lineTo(9, 18);
      ctx.closePath();
      ctx.fill();
      ctx.fillStyle = 'rgba(255,255,255,0.35)';
      ctx.beginPath();
      ctx.moveTo(16, 5);
      ctx.lineTo(21, 19);
      ctx.lineTo(15, 20);
      ctx.closePath();
      ctx.fill();
    });

    // dirt — bare soil
    tex[texKey(19, v)] = makeTex(S, (ctx, rnd) => {
      ctx.fillStyle = shade(PAL.dirt, vShift);
      ctx.fillRect(0, 0, S, S);
      speckle(ctx, rnd, [PAL.dirtHi, PAL.dirt, '#2e2218'], 9);
      ctx.fillStyle = 'rgba(0,0,0,0.2)';
      ctx.fillRect(5, 8 + v * 2, 4, 2);
      ctx.fillRect(20, 20 + v, 5, 2);
    });

    // torch — wall sconce with flame (animated frames override in play)
    tex[texKey(20, v)] = makeTex(S, (ctx, _rnd) => {
      ctx.fillStyle = shade(PAL.torch, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = shade('#5a4030', vShift);
      ctx.fillRect(13, 18, 6, 14);
      ctx.fillStyle = PAL.torchFlame;
      ctx.beginPath();
      ctx.ellipse(16, 12, 4, 7, 0, 0, Math.PI * 2);
      ctx.fill();
      ctx.fillStyle = PAL.torchCore;
      ctx.beginPath();
      ctx.ellipse(16, 10, 1.8, 3.5, 0, 0, Math.PI * 2);
      ctx.fill();
    });

    // glow — floating light mote (animated frames override in play)
    tex[texKey(21, v)] = makeTex(S, (ctx, _rnd) => {
      ctx.fillStyle = shade(PAL.glow, vShift);
      ctx.fillRect(0, 0, S, S);
      ctx.fillStyle = PAL.glowHi;
      ctx.beginPath();
      ctx.arc(16, 16, 5 + v, 0, Math.PI * 2);
      ctx.fill();
      ctx.fillStyle = 'rgba(255,255,255,0.5)';
      ctx.beginPath();
      ctx.arc(16, 16, 2, 0, Math.PI * 2);
      ctx.fill();
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

/**
 * Animated-tile frame sequences (T2 editor v2). The server is authoritative
 * on WHICH tiles animate and how many frames/fps (world doc "anims"); this
 * generator produces that many distinct 32x32 textures per tile so the
 * renderer can cycle them. Deterministic per (tile, frame).
 */
export function generateAnimatedFrames(tileId: number, frames: number): Texture[] {
  const S = TILE;
  const out: Texture[] = [];
  for (let f = 0; f < Math.max(1, frames); f++) {
    const ph = f; // phase = frame index
    const t = makeTex(S, (ctx, _rnd) => {
      if (tileId === 2) {
        // water — wave arcs sliding downward
        ctx.fillStyle = PAL.water;
        ctx.fillRect(0, 0, S, S);
        ctx.strokeStyle = 'rgba(90,127,174,0.6)';
        ctx.lineWidth = 1.5;
        for (let i = 0; i < 3; i++) {
          const y = ((8 + i * 9 + ph * 3) % 30) - 2;
          ctx.beginPath();
          ctx.moveTo(3, y);
          ctx.quadraticCurveTo(10, y - 3, 17, y);
          ctx.quadraticCurveTo(24, y + 3, 30, y);
          ctx.stroke();
        }
        ctx.fillStyle = 'rgba(90,127,174,0.14)';
        ctx.fillRect((ph * 5) % S, 20, 5, 2);
      } else if (tileId === 8) {
        // lava — bubbling molten pools
        ctx.fillStyle = PAL.lava;
        ctx.fillRect(0, 0, S, S);
        for (let i = 0; i < 3; i++) {
          const bx = (4 + i * 9 + ph * 2) % 26;
          const by = (6 + i * 8 + ((ph * 3 + i * 5) % 6)) % 26;
          ctx.fillStyle = i % 2 ? PAL.lavaHi : PAL.lavaHot;
          ctx.beginPath();
          ctx.arc(bx, by, 2.4 + (i % 2), 0, Math.PI * 2);
          ctx.fill();
        }
        ctx.strokeStyle = PAL.lavaHi;
        ctx.lineWidth = 1.5;
        ctx.beginPath();
        ctx.moveTo(0, (10 + ph * 2) % S);
        ctx.lineTo(S, (8 + ph * 2) % S);
        ctx.stroke();
      } else if (tileId === 20) {
        // torch — flickering flame (height + lean vary per frame)
        ctx.fillStyle = PAL.torch;
        ctx.fillRect(0, 0, S, S);
        ctx.fillStyle = '#5a4030';
        ctx.fillRect(13, 20, 6, 12);
        const lean = (ph % 2 === 0 ? 1 : -1) * (ph % 3);
        const hgt = 7 + (ph % 3) * 2;
        ctx.fillStyle = PAL.torchFlame;
        ctx.beginPath();
        ctx.ellipse(16 + lean, 11, 3.6, hgt, lean * 0.08, 0, Math.PI * 2);
        ctx.fill();
        ctx.fillStyle = PAL.torchCore;
        ctx.beginPath();
        ctx.ellipse(16 + lean, 9, 1.7, 3.2 + (ph % 2), 0, 0, Math.PI * 2);
        ctx.fill();
        // warm halo
        ctx.fillStyle = 'rgba(255,179,71,0.10)';
        ctx.beginPath();
        ctx.arc(16, 13, 11 + (ph % 2), 0, Math.PI * 2);
        ctx.fill();
      } else if (tileId === 21) {
        // glow — pulsing light mote
        ctx.fillStyle = PAL.glow;
        ctx.fillRect(0, 0, S, S);
        const r = 4 + (ph % 3) * 1.6;
        ctx.fillStyle = PAL.glowHi;
        ctx.beginPath();
        ctx.arc(16, 16, r, 0, Math.PI * 2);
        ctx.fill();
        ctx.fillStyle = 'rgba(255,255,255,0.55)';
        ctx.beginPath();
        ctx.arc(16, 16, Math.max(1.6, r * 0.4), 0, Math.PI * 2);
        ctx.fill();
        ctx.fillStyle = `rgba(201,168,255,${0.08 + (ph % 3) * 0.04})`;
        ctx.beginPath();
        ctx.arc(16, 16, r + 5, 0, Math.PI * 2);
        ctx.fill();
      }
    }, f * 7919 + tileId * 131);
    out.push(t);
  }
  return out;
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
