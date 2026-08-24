// Composite avatar renderer (T1): draws the 5-layer avatar_spec
// (body/skin/hair/outfit/accessory) procedurally onto a canvas — no asset
// files. Every viewer renders the same spec identically. Minimal animation:
// 2-frame walk bob, plus sit/emote stubs that ride the same pipeline.

import { isAssetOption, isRobotSpec, type AvatarLayerId, type AvatarSpec } from './spec';

export const AV_PX = 128;
/** Re-exported so renderers can style robot labels without importing spec.ts. */
export { isRobotSpec };

// ---- T2: custom uploaded assets -----------------------------------------
// A layer value of "asset:<uuid>" draws the member's uploaded image (fetched
// from /api/avatars/assets/{id}/image — same origin, session cookie, so the
// canvas stays untainted and the SAME bytes render identically for every
// viewer). Loads are cached; avatarAssetRev() bumps when an image lands so
// renderers (picker previews, world sprite caches) can rebuild once fresh.

const assetImgCache = new Map<string, HTMLImageElement>();
let assetRev = 0;
const assetLoadCbs = new Set<() => void>();

/** Monotonic counter bumped whenever a custom asset image finishes loading
 *  (or fails) — cache keys built from it refresh stale render passes. */
export function avatarAssetRev(): number {
  return assetRev;
}

/** Subscribe to asset image load/fail events; returns an unsubscribe fn. */
export function onAvatarAssetLoad(cb: () => void): () => void {
  assetLoadCbs.add(cb);
  return () => {
    assetLoadCbs.delete(cb);
  };
}

/** Returns the cached image when loaded; otherwise kicks off the fetch and
 *  returns undefined (caller draws a placeholder; re-renders on load). */
export function loadAvatarAsset(id: string, base = ''): HTMLImageElement | undefined {
  const hit = assetImgCache.get(id);
  if (hit) return hit.complete && hit.naturalWidth > 0 ? hit : undefined;
  const img = new Image();
  img.crossOrigin = 'anonymous';
  img.onload = () => {
    assetImgCache.set(id, img);
    assetRev++;
    assetLoadCbs.forEach((cb) => cb());
  };
  img.onerror = () => {
    assetImgCache.delete(id);
    assetRev++;
    assetLoadCbs.forEach((cb) => cb());
  };
  img.src = `${base}/api/avatars/assets/${encodeURIComponent(id)}/image`;
  assetImgCache.set(id, img);
  return undefined;
}

/** Cover-fits the asset image into a layer rect (canvas px, 128 space). */
function drawAssetCover(
  ctx: CanvasRenderingContext2D,
  id: string,
  rect: { x: number; y: number; w: number; h: number },
  base = '',
): boolean {
  const img = loadAvatarAsset(id, base);
  if (!img || !img.complete || img.naturalWidth === 0) return false;
  const s = Math.max(rect.w / img.naturalWidth, rect.h / img.naturalHeight);
  const dw = img.naturalWidth * s;
  const dh = img.naturalHeight * s;
  ctx.save();
  ctx.beginPath();
  ctx.rect(rect.x, rect.y, rect.w, rect.h);
  ctx.clip();
  ctx.drawImage(img, rect.x + (rect.w - dw) / 2, rect.y + (rect.h - dh) / 2, dw, dh);
  ctx.restore();
  return true;
}

/** The 128-space rect a layer occupies (asset images are cover-fitted here). */
function layerRect(layer: AvatarLayerId, g: Geom): { x: number; y: number; w: number; h: number } {
  switch (layer) {
    case 'body':
      return { x: g.head.x - 2, y: g.head.y - 2, w: Math.max(g.head.w, g.torso.w) + 6, h: g.torso.y + g.torso.h - g.head.y + 4 };
    case 'skin':
      return { x: g.head.x + 3, y: g.head.y + 3, w: g.head.w - 6, h: g.head.h - 6 };
    case 'hair':
      return { x: g.head.x - 2, y: g.head.y - 1, w: g.head.w + 4, h: Math.round(g.head.h * 0.45) + 3 };
    case 'outfit':
      return { x: g.torso.x, y: g.torso.y, w: g.torso.w, h: g.torso.h };
    case 'accessory':
      return { x: g.head.x - 4, y: g.head.y - 8, w: g.head.w + 8, h: g.head.h + 8 };
  }
}

/** Draws a layer: the uploaded image when the value is an asset (falling
 *  back to the procedural draw until the image loads), else procedural. */
function drawLayer(
  ctx: CanvasRenderingContext2D,
  layer: AvatarLayerId,
  spec: AvatarSpec,
  g: Geom,
  base: string,
  fallback: () => void,
): void {
  const id = layerValueOf(layer, spec);
  if (isAssetOption(id)) {
    if (!drawAssetCover(ctx, id, layerRect(layer, g), base)) fallback();
    return;
  }
  fallback();
}

function layerValueOf(layer: AvatarLayerId, spec: AvatarSpec): string {
  switch (layer) {
    case 'body': return spec.body;
    case 'skin': return spec.skin;
    case 'hair': return spec.hair;
    case 'outfit': return spec.outfit;
    case 'accessory': return spec.accessory;
    default: return '';
  }
}

/** 4-frame walk bob heights (canvas px) — shared with renderers. */
const WALK_BOB = [0, -8, -3, -8] as const;
/** Lateral sway per walk frame so the step reads at small scale. */
const WALK_SWAY = [0, 3, 0, -3] as const;

export interface SpritePose {
  dir?: 'down' | 'up' | 'left' | 'right';
  /** Walk frame: 0 = neutral, 1 = stride up, 2 = passing, 3 = stride up. */
  phase?: 0 | 1 | 2 | 3;
  /** Sit stub: body lowered + legs forward. */
  pose?: 'stand' | 'sit';
  /** Emote stub: wave raises an arm + a "!" bubble. */
  emote?: 'none' | 'wave';
}

type Dir = 'down' | 'up' | 'left' | 'right';

// ---- palettes (option id -> color); swatches in catalog.ts mirror these ----

const SKIN: Record<string, string> = {
  warm: '#f1c27d', fair: '#ffe0bd', olive: '#c68642', deep: '#8d5524', cool: '#e0ac69',
};
const HAIR: Record<string, string> = {
  bob: '#3b2f2f', curls: '#7c4a21', mohawk: '#f43f5e', cap: '#1d4ed8', bald: '#d1d5db',
};
const OUTFIT: Record<string, string> = {
  hoodie: '#6366f1', tee: '#10b981', robe: '#b45309', vest: '#0f766e', dress: '#db2777',
};
const BODY: Record<string, string> = {
  round: '#fbbf24', bean: '#84cc16', slim: '#38bdf8', square: '#a78bfa', bot: '#94a3b8',
};
const BODY_EDGE: Record<string, string> = {
  round: '#b45309', bean: '#4d7c0f', slim: '#0369a1', square: '#7c3aed', bot: '#64748b',
};
const ACCESSORY: Record<string, string> = {
  glasses: '#111827', crown: '#facc15', scarf: '#ef4444', halo: '#fde68a',
};

function color(map: Record<string, string>, id: string, fallback: string): string {
  return map[id] ?? fallback;
}

interface Geom {
  head: { x: number; y: number; w: number; h: number };
  torso: { x: number; y: number; w: number; h: number };
}

function bodyGeom(body: string): Geom {
  switch (body) {
    case 'bean':
      return { head: { x: 53, y: 31, w: 22, h: 24 }, torso: { x: 46, y: 58, w: 36, h: 42 } };
    case 'slim':
      return { head: { x: 54, y: 30, w: 20, h: 24 }, torso: { x: 50, y: 58, w: 28, h: 40 } };
    case 'square':
      return { head: { x: 52, y: 28, w: 24, h: 22 }, torso: { x: 46, y: 52, w: 36, h: 46 } };
    case 'bot':
      return { head: { x: 52, y: 30, w: 24, h: 20 }, torso: { x: 46, y: 54, w: 36, h: 42 } };
    case 'round':
    default:
      return { head: { x: 52, y: 32, w: 24, h: 24 }, torso: { x: 44, y: 58, w: 40, h: 38 } };
  }
}

function roundRect(
  ctx: CanvasRenderingContext2D,
  x: number,
  y: number,
  w: number,
  h: number,
  r: number,
): void {
  const rr = Math.min(r, w / 2, h / 2);
  ctx.beginPath();
  ctx.moveTo(x + rr, y);
  ctx.arcTo(x + w, y, x + w, y + h, rr);
  ctx.arcTo(x + w, y + h, x, y + h, rr);
  ctx.arcTo(x, y + h, x, y, rr);
  ctx.arcTo(x, y, x + w, y, rr);
  ctx.closePath();
}

/** Draws the body silhouette (head + torso blob) in the body fabric color. */
function drawBody(ctx: CanvasRenderingContext2D, body: string, g: Geom): void {
  const fill = color(BODY, body, '#fbbf24');
  const edge = color(BODY_EDGE, body, '#b45309');
  if (body === 'bot') {
    // metallic robot: antenna, panel lines, side rivets
    ctx.strokeStyle = edge;
    ctx.lineWidth = 4;
    ctx.beginPath();
    ctx.moveTo(64, 18);
    ctx.lineTo(64, 30);
    ctx.stroke();
    ctx.fillStyle = fill;
    roundRect(ctx, g.head.x, g.head.y, g.head.w, g.head.h, 5);
    ctx.fill();
    roundRect(ctx, g.torso.x, g.torso.y, g.torso.w, g.torso.h, 5);
    ctx.fill();
    ctx.strokeStyle = 'rgba(15,23,42,0.35)';
    ctx.lineWidth = 2;
    ctx.strokeRect(g.torso.x + 4, g.torso.y + 8, g.torso.w - 8, g.torso.h - 16);
    ctx.fillStyle = '#475569';
    for (const ex of [g.head.x + 4, g.head.x + g.head.w - 8]) {
      ctx.fillRect(ex, g.head.y + g.head.h / 2 - 2, 4, 4);
    }
    return;
  }
  ctx.fillStyle = fill;
  roundRect(ctx, g.head.x, g.head.y, g.head.w, g.head.h, 7);
  ctx.fill();
  roundRect(ctx, g.torso.x, g.torso.y, g.torso.w, g.torso.h, 9);
  ctx.fill();
  // legs peek below the torso (fabric color)
  ctx.fillStyle = fill;
  ctx.fillRect(g.torso.x + g.torso.w * 0.12, g.torso.y + g.torso.h - 4, g.torso.w * 0.3, 8);
  ctx.fillRect(g.torso.x + g.torso.w * 0.58, g.torso.y + g.torso.h - 4, g.torso.w * 0.3, 8);
  ctx.strokeStyle = edge;
  ctx.lineWidth = 2.5;
  roundRect(ctx, g.head.x, g.head.y, g.head.w, g.head.h, 7);
  ctx.stroke();
}

/** Draws the face (skin or robot screen) + eyes/mouth facing dir. */
function drawFace(
  ctx: CanvasRenderingContext2D,
  spec: AvatarSpec,
  g: Geom,
  dir: Dir,
): void {
  const robot = spec.body === 'bot';
  const fx = g.head.x + 3;
  const fy = g.head.y + 3;
  const fw = g.head.w - 6;
  const fh = g.head.h - 6;
  if (robot) {
    ctx.fillStyle = '#0f172a';
    roundRect(ctx, fx, fy, fw, fh, 4);
    ctx.fill();
    const ex = fx + fw / 2;
    const ey = fy + fh / 2;
    const off = 3;
    let dx = 0;
    let dy = 0;
    if (dir === 'left') dx = -off;
    else if (dir === 'right') dx = off;
    else if (dir === 'up') dy = -off;
    else dy = off;
    ctx.fillStyle = '#4ade80';
    ctx.beginPath();
    ctx.arc(ex - 4 + dx, ey + dy, 2.4, 0, Math.PI * 2);
    ctx.arc(ex + 4 + dx, ey + dy, 2.4, 0, Math.PI * 2);
    ctx.fill();
    return;
  }
  const skin = color(SKIN, spec.skin, '#f1c27d');
  ctx.fillStyle = skin;
  roundRect(ctx, fx, fy, fw, fh, 5);
  ctx.fill();
  const ex = fx + fw / 2;
  const ey = fy + fh / 2 + 1;
  const off = 5;
  let dx = 0;
  let dy = 0;
  if (dir === 'left') dx = -off;
  else if (dir === 'right') dx = off;
  else if (dir === 'up') dy = -off;
  else dy = off;
  ctx.fillStyle = '#16121f';
  ctx.beginPath();
  ctx.arc(ex - 4 + dx, ey + dy, 2.2, 0, Math.PI * 2);
  ctx.arc(ex + 4 + dx, ey + dy, 2.2, 0, Math.PI * 2);
  ctx.fill();
  // mouth
  ctx.strokeStyle = '#4c4558';
  ctx.lineWidth = 1.8;
  ctx.lineCap = 'round';
  ctx.beginPath();
  if (dir === 'up') {
    ctx.arc(ex, ey + 5, 3.4, Math.PI * 1.15, Math.PI * 1.85);
  } else if (dir === 'left' || dir === 'right') {
    ctx.moveTo(ex - 3, ey + 6);
    ctx.lineTo(ex + 3, ey + 6);
  } else {
    ctx.arc(ex, ey + 4, 3.4, 0.15 * Math.PI, 0.85 * Math.PI);
  }
  ctx.stroke();
}

/** Draws the outfit over the torso area. */
function drawOutfit(ctx: CanvasRenderingContext2D, spec: AvatarSpec, g: Geom): void {
  if (spec.body === 'bot') return; // robots wear their panels
  const c = color(OUTFIT, spec.outfit, '#6366f1');
  const t = g.torso;
  const x = t.x + 2;
  const y = t.y + 4;
  const w = t.w - 4;
  const h = t.h - 6;
  ctx.fillStyle = c;
  switch (spec.outfit) {
    case 'dress': {
      roundRect(ctx, x + 2, y, w - 4, h * 0.5, 6);
      ctx.fill();
      ctx.beginPath();
      ctx.moveTo(x + 2, y + h * 0.45);
      ctx.lineTo(x + w - 2, y + h * 0.45);
      ctx.lineTo(x + w + 6, y + h + 8);
      ctx.lineTo(x - 6, y + h + 8);
      ctx.closePath();
      ctx.fill();
      break;
    }
    case 'robe': {
      roundRect(ctx, x, y, w, h * 0.55, 6);
      ctx.fill();
      ctx.beginPath();
      ctx.moveTo(x + 2, y + h * 0.5);
      ctx.lineTo(x + w - 2, y + h * 0.5);
      ctx.lineTo(x + w, y + h + 6);
      ctx.lineTo(x, y + h + 6);
      ctx.closePath();
      ctx.fill();
      ctx.strokeStyle = 'rgba(0,0,0,0.35)';
      ctx.lineWidth = 3;
      ctx.beginPath();
      ctx.moveTo(x + 2, y + h * 0.52);
      ctx.lineTo(x + w - 2, y + h * 0.52);
      ctx.stroke();
      break;
    }
    case 'vest': {
      roundRect(ctx, x + 3, y, w - 6, h, 7);
      ctx.fill();
      ctx.fillStyle = 'rgba(0,0,0,0.22)';
      ctx.fillRect(x + w / 2 - 1.5, y + 2, 3, h - 4);
      ctx.fillStyle = c;
      ctx.beginPath();
      ctx.moveTo(x + 3, y + 3);
      ctx.lineTo(x + w / 2, y + 8);
      ctx.lineTo(x + w - 3, y + 3);
      ctx.closePath();
      ctx.fill();
      break;
    }
    case 'tee': {
      roundRect(ctx, x, y, w, h, 8);
      ctx.fill();
      // short sleeves
      ctx.fillRect(x - 7, y + 2, 8, 10);
      ctx.fillRect(x + w - 1, y + 2, 8, 10);
      break;
    }
    case 'hoodie':
    default: {
      roundRect(ctx, x, y, w, h, 9);
      ctx.fill();
      // hood behind neck
      ctx.fillStyle = 'rgba(0,0,0,0.18)';
      roundRect(ctx, x + 6, y - 6, w - 12, 12, 6);
      ctx.fill();
      ctx.fillStyle = c;
      ctx.fillRect(x + 6, y - 6, w - 12, 6);
      // pocket
      ctx.fillStyle = 'rgba(255,255,255,0.25)';
      roundRect(ctx, x + 6, y + h * 0.55, w - 12, h * 0.32, 6);
      ctx.fill();
      // drawstrings
      ctx.strokeStyle = 'rgba(255,255,255,0.8)';
      ctx.lineWidth = 2;
      ctx.beginPath();
      ctx.moveTo(x + w / 2 - 4, y);
      ctx.lineTo(x + w / 2 - 4, y + 8);
      ctx.moveTo(x + w / 2 + 4, y);
      ctx.lineTo(x + w / 2 + 4, y + 8);
      ctx.stroke();
      break;
    }
  }
  // arms in skin tone with outfit shoulder caps
  const robot = spec.body === 'bot';
  const arm = robot ? '#475569' : color(SKIN, spec.skin, '#f1c27d');
  ctx.fillStyle = arm;
  roundRect(ctx, x - 7, y + 2, 7, h - 10, 3.5);
  ctx.fill();
  roundRect(ctx, x + w, y + 2, 7, h - 10, 3.5);
  ctx.fill();
  ctx.fillStyle = c;
  roundRect(ctx, x - 7, y + 2, 7, 8, 3.5);
  ctx.fill();
  roundRect(ctx, x + w, y + 2, 7, 8, 3.5);
  ctx.fill();
}

/** Draws the hair style over the top of the head. */
function drawHair(ctx: CanvasRenderingContext2D, spec: AvatarSpec, g: Geom): void {
  if (spec.body === 'bot' || spec.hair === 'bald') return;
  const c = color(HAIR, spec.hair, '#3b2f2f');
  const hx = g.head.x;
  const hy = g.head.y;
  const hw = g.head.w;
  switch (spec.hair) {
    case 'curls': {
      ctx.fillStyle = c;
      for (const [cx, cy, r] of [
        [hx + 3, hy + 3, 5], [hx + hw / 2, hy + 1, 6], [hx + hw - 3, hy + 3, 5],
        [hx + 1, hy + 8, 4], [hx + hw - 1, hy + 8, 4], [hx + hw / 2, hy + 6, 5],
      ] as const) {
        ctx.beginPath();
        ctx.arc(cx, cy, r, 0, Math.PI * 2);
        ctx.fill();
      }
      break;
    }
    case 'mohawk': {
      ctx.fillStyle = c;
      for (let i = 0; i < 4; i++) {
        ctx.beginPath();
        ctx.moveTo(hx + 4 + i * 5, hy + 8);
        ctx.lineTo(hx + 6 + i * 5, hy - 6 - (i % 2) * 2);
        ctx.lineTo(hx + 9 + i * 5, hy + 8);
        ctx.closePath();
        ctx.fill();
      }
      break;
    }
    case 'cap': {
      ctx.fillStyle = c;
      ctx.beginPath();
      ctx.arc(hx + hw / 2, hy + 3, hw / 2 + 1, Math.PI, 0);
      ctx.closePath();
      ctx.fill();
      ctx.fillRect(hx + hw / 2 - 1, hy + 1, hw / 2 + 2, 5);
      break;
    }
    case 'bob':
    default: {
      ctx.fillStyle = c;
      roundRect(ctx, hx - 2, hy - 1, hw + 4, 11, 6);
      ctx.fill();
      ctx.fillRect(hx - 2, hy + 8, 5, 8);
      ctx.fillRect(hx + hw - 3, hy + 8, 5, 8);
      break;
    }
  }
}

/** Draws the accessory (glasses/crown/scarf/halo) + emote bubble. */
function drawAccessory(
  ctx: CanvasRenderingContext2D,
  spec: AvatarSpec,
  g: Geom,
  emote: 'none' | 'wave',
): void {
  const hx = g.head.x;
  const hy = g.head.y;
  const hw = g.head.w;
  const hh = g.head.h;
  const acc = spec.accessory;
  if (acc === 'crown') {
    ctx.fillStyle = ACCESSORY.crown;
    ctx.beginPath();
    ctx.moveTo(hx - 1, hy);
    ctx.lineTo(hx + 4, hy - 7);
    ctx.lineTo(hx + 7, hy - 1);
    ctx.lineTo(hx + 11, hy - 8);
    ctx.lineTo(hx + hw - 1, hy);
    ctx.closePath();
    ctx.fill();
    ctx.fillStyle = '#ef4444';
    ctx.fillRect(hx + 6, hy - 2, 2, 2);
  } else if (acc === 'glasses') {
    ctx.strokeStyle = ACCESSORY.glasses;
    ctx.lineWidth = 2.5;
    ctx.beginPath();
    ctx.arc(hx + hw * 0.28, hy + hh * 0.48, 5.5, 0, Math.PI * 2);
    ctx.arc(hx + hw * 0.72, hy + hh * 0.48, 5.5, 0, Math.PI * 2);
    ctx.moveTo(hx + hw * 0.28 + 5.5, hy + hh * 0.48);
    ctx.lineTo(hx + hw * 0.72 - 5.5, hy + hh * 0.48);
    ctx.stroke();
  } else if (acc === 'scarf') {
    ctx.fillStyle = ACCESSORY.scarf;
    roundRect(ctx, hx - 2, hy + hh - 6, hw + 4, 8, 4);
    ctx.fill();
    ctx.fillRect(hx + hw - 4, hy + hh - 4, 6, 10);
  } else if (acc === 'halo') {
    ctx.strokeStyle = ACCESSORY.halo;
    ctx.lineWidth = 3;
    ctx.beginPath();
    ctx.ellipse(hx + hw / 2, hy - 3, 11, 4, 0, Math.PI, 0);
    ctx.stroke();
  }
  if (emote === 'wave') {
    // emote stub: raise the right arm + "!" bubble
    ctx.strokeStyle = 'rgba(22,18,31,0.85)';
    ctx.lineWidth = 4;
    ctx.lineCap = 'round';
    ctx.beginPath();
    ctx.moveTo(g.torso.x + g.torso.w + 2, g.torso.y + 8);
    ctx.quadraticCurveTo(g.torso.x + g.torso.w + 14, g.torso.y - 6, g.torso.x + g.torso.w + 8, g.torso.y - 16);
    ctx.stroke();
    ctx.fillStyle = '#ffffff';
    ctx.beginPath();
    ctx.arc(100, 22, 10, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = '#16121f';
    ctx.font = 'bold 13px system-ui, sans-serif';
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText('!', 100, 23);
  }
}

/**
 * Renders the full layered spec onto a fresh canvas (AV_PX square, anchored
 * bottom-center). phase drives a 4-frame walk bob + lateral sway (shadow stays
 * planted); pose 'sit' lowers and squashes; emote 'wave' raises an arm +
 * bubble.
 */
export function renderAvatarSpec(spec: AvatarSpec, pose: SpritePose = {}, base = ''): HTMLCanvasElement {
  const c = document.createElement('canvas');
  c.width = AV_PX;
  c.height = AV_PX;
  const ctx = c.getContext('2d')!;
  const dir: Dir = pose.dir ?? 'down';
  const ph = pose.phase ?? 0;
  const bob = WALK_BOB[ph];
  const sway = WALK_SWAY[ph];
  const sit = pose.pose === 'sit';
  const emote = pose.emote ?? 'none';

  // ground shadow (never bobs)
  ctx.fillStyle = 'rgba(0,0,0,0.35)';
  ctx.beginPath();
  ctx.ellipse(64, 116, 30, 9, 0, 0, Math.PI * 2);
  ctx.fill();

  ctx.save();
  ctx.translate(sway, bob + (sit ? 10 : 0));

  const g = bodyGeom(spec.body);
  if (sit) {
    g.torso.h = Math.round(g.torso.h * 0.8);
    g.torso.y += 6;
  }
  const robot = spec.body === 'bot';
  // body asset replaces the whole silhouette (likely a complete figure)
  drawLayer(ctx, 'body', spec, g, base, () => drawBody(ctx, spec.body, g));
  drawLayer(ctx, 'outfit', spec, g, base, () => {
    if (!robot) drawOutfit(ctx, spec, g);
  });
  // skin asset replaces the face (eyes/mouth stay procedural when skin is a
  // normal catalog option); a body asset already carries its own face
  drawLayer(ctx, 'skin', spec, g, base, () => {
    if (!robot && !isAssetOption(spec.body)) drawFace(ctx, spec, g, dir);
  });
  drawLayer(ctx, 'hair', spec, g, base, () => drawHair(ctx, spec, g));
  drawLayer(ctx, 'accessory', spec, g, base, () => drawAccessory(ctx, spec, g, emote));

  if (sit) {
    // sit stub: legs forward
    ctx.fillStyle = color(BODY, spec.body, '#fbbf24');
    roundRect(ctx, 44, 104, 20, 8, 4);
    ctx.fill();
    roundRect(ctx, 64, 104, 20, 8, 4);
    ctx.fill();
  }
  ctx.restore();
  return c;
}

/** Renders a single layer option as a small square preview (picker swatches).
 *  Custom assets draw their uploaded image cover-fit (procedural fallback
 *  until the image loads). */
export function renderAvatarLayer(layer: AvatarLayerId, optionId: string, size = 56, base = ''): HTMLCanvasElement {
  const c = document.createElement('canvas');
  c.width = size;
  c.height = size;
  const ctx = c.getContext('2d')!;
  const k = size / 128;
  ctx.save();
  ctx.scale(k, k);

  if (isAssetOption(optionId)) {
    if (!drawAssetCover(ctx, optionId, { x: 0, y: 0, w: 128, h: 128 }, base)) {
      // placeholder: neutral silhouette chip while the image loads
      ctx.fillStyle = 'rgba(167,139,250,0.28)';
      ctx.fillRect(0, 0, 128, 128);
      ctx.fillStyle = 'rgba(233,226,245,0.55)';
      ctx.font = 'bold 40px system-ui, sans-serif';
      ctx.textAlign = 'center';
      ctx.textBaseline = 'middle';
      ctx.fillText('…', 64, 68);
    }
  } else if (layer === 'body') {
    drawBody(ctx, optionId, bodyGeom(optionId));
  } else {
    // neutral mini figure, then overlay the chosen layer
    const base: AvatarSpec = { v: 1, body: 'round', skin: 'warm', hair: 'bob', outfit: 'hoodie', accessory: 'none' };
    const g = bodyGeom('round');
    ctx.fillStyle = 'rgba(0,0,0,0.08)';
    roundRect(ctx, g.head.x - 3, g.head.y - 3, g.head.w + 6, g.head.h + 6, 8);
    ctx.fill();
    roundRect(ctx, g.torso.x - 3, g.torso.y - 3, g.torso.w + 6, g.torso.h + 6, 10);
    ctx.fill();
    if (layer === 'skin') {
      const s = { ...base, skin: optionId };
      drawFace(ctx, s, g, 'down');
    } else if (layer === 'hair') {
      const s = { ...base, hair: optionId };
      drawHair(ctx, s, g);
    } else if (layer === 'outfit') {
      const s = { ...base, outfit: optionId };
      drawOutfit(ctx, s, g);
    } else if (layer === 'accessory') {
      const s = { ...base, accessory: optionId };
      drawAccessory(ctx, s, g, 'none');
    }
  }
  ctx.restore();
  return c;
}
