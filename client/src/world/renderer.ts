// World renderer: PixiJS v8 canvas, top-down 2D. Camera follows the local
// player (with free-look pan offset), pinch-zoom 0.6–2.5x, two-finger pan,
// tap-to-move via A*, procedural avatar with 4-dir facing, remote players
// interpolated through a 100ms buffer with cubic ease. Optimistic move
// reporting at 12Hz (seq monotonic).

import { Application, Container, Graphics, Sprite, Text, Texture, TilingSprite } from 'pixi.js';
import { tintColor } from '../colors';
import { astar, type Pt } from './astar';
import { InterpBuffer } from './interp';
import {
  generateFloorTexture,
  generateTileTextures,
  genDefaultWorld,
  isPassableTile,
  parseWorld,
  TILE,
  texKey,
  tileVariant,
} from './tiles';
import { onAvatarAssetLoad, renderAvatarSpec } from '../avatar/sprites';
import { isAssetOption, specAccent, specKey as specKeyOf, type AvatarInfo, type AvatarSpec } from '../avatar/spec';

export interface LocalMove {
  x: number;
  y: number;
  dir: string;
  seq: number;
}

export interface RemoteState {
  id: string;
  x: number;
  y: number;
  dir?: string;
  avatar?: AvatarInfo;
}

export interface RosterEntry {
  id: string;
  name: string;
  x: number;
  y: number;
  dir?: string;
  avatar?: AvatarInfo;
}

const SPEED = 4.5; // tiles / sec
const MOVE_INTERVAL = 84; // ms (~12Hz)
const AV_PX = 128;
const AV_SCALE = 0.3; // 128 * 0.3 ≈ 38px on screen
/** Default zoom-in cap. The floor is dynamic (zoomFloor) so a small phone can
 *  still zoom out far enough to see the whole map as a coherent overview. */
const MIN_ZOOM = 0.6;
const MAX_ZOOM = 2.5;
const MIN_ZOOM_FLOOR = 0.12;
/** Below this zoom, per-player labels + ownership markers fade out so the
 *  zoomed-out map reads cleanly instead of a pile of floating text. */
const LABEL_FADE_ZOOM = 0.4;

type WalkPhase = 0 | 1 | 2 | 3;

interface Remote {
  sprite: Sprite;
  shadow: Sprite;
  label: Text;
  buf: InterpBuffer;
  dir: string;
  /** spec cache key when this remote renders the layered composite. */
  specKey: string;
  /** the layered spec itself (kept so asset images can refresh the sprite). */
  spec?: AvatarSpec;
  /** true while the interpolated position is actually changing. */
  moving: boolean;
  lastX: number;
  lastY: number;
}

interface LocalPlayer {
  x: number;
  y: number;
  dir: string;
  seq: number;
  dirty: boolean;
  sprite: Sprite;
  shadow: Sprite;
  ring: Graphics;
  label: Text;
  specKey: string;
}

interface AvatarFrames {
  down: Texture[];
  up: Texture[];
  right: Texture[];
}

function hexToNum(h: string): number {
  return /^#[0-9a-fA-F]{6}$/.test(h) ? parseInt(h.slice(1), 16) : 0x8b5cf6;
}

function clamp(v: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, v));
}

function roundRectPath(ctx: CanvasRenderingContext2D, x: number, y: number, w: number, h: number, r: number): void {
  ctx.beginPath();
  ctx.moveTo(x + r, y);
  ctx.arcTo(x + w, y, x + w, y + h, r);
  ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r);
  ctx.arcTo(x, y, x + w, y, r);
  ctx.closePath();
}

// ---------------------------------------------------------------- atmosphere
// All atmosphere textures are tiny canvases generated once at boot (design §3):
// ~15 new textures total, no per-frame allocations. Sky/wash/vignette are
// screen-space sprites; glows + fireflies live in the world container.

/** (h) Dusk sky gradient — 2x256, stretched full-screen BELOW the tiles. */
function makeSkyTexture(): Texture {
  const c = document.createElement('canvas');
  c.width = 2;
  c.height = 256;
  const ctx = c.getContext('2d')!;
  const g = ctx.createLinearGradient(0, 0, 0, 256);
  g.addColorStop(0, '#1a1033');
  g.addColorStop(0.5, '#2b1a4a');
  g.addColorStop(1, '#3d2247');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, 2, 256);
  return Texture.from(c);
}

/** (c) Shared 128px radial glow — white core → warm 55% @ 25% → 0. Tinted per light. */
function makeGlowTexture(): Texture {
  const S = 128;
  const c = document.createElement('canvas');
  c.width = S;
  c.height = S;
  const ctx = c.getContext('2d')!;
  const g = ctx.createRadialGradient(S / 2, S / 2, 0, S / 2, S / 2, S / 2);
  g.addColorStop(0, 'rgba(255,244,224,0.9)');
  g.addColorStop(0.25, 'rgba(255,200,120,0.55)');
  g.addColorStop(1, 'rgba(255,200,120,0)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, S, S);
  return Texture.from(c);
}

/** (d) Shared 64x24 ellipse avatar shadow — rgba(0,0,0,.35) → 0, at feet y+2. */
function makeShadowTexture(): Texture {
  const c = document.createElement('canvas');
  c.width = 64;
  c.height = 24;
  const ctx = c.getContext('2d')!;
  const g = ctx.createRadialGradient(32, 12, 2, 32, 12, 30);
  g.addColorStop(0, 'rgba(0,0,0,0.35)');
  g.addColorStop(1, 'rgba(0,0,0,0)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, 64, 24);
  return Texture.from(c);
}

/** (a) Warm wash — #ff6b3d @ 0.06 at the bottom fading to 0 at the top (overlay). */
function makeWashTexture(): Texture {
  const c = document.createElement('canvas');
  c.width = 2;
  c.height = 256;
  const ctx = c.getContext('2d')!;
  const g = ctx.createLinearGradient(0, 0, 0, 256);
  g.addColorStop(0, 'rgba(255,107,61,0)');
  g.addColorStop(1, 'rgba(255,107,61,0.06)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, 2, 256);
  return Texture.from(c);
}

/** (a) Vignette — 512px radial → #0d0716 @ 0.45 at the corners. */
function makeVignetteTexture(): Texture {
  const S = 512;
  const c = document.createElement('canvas');
  c.width = S;
  c.height = S;
  const ctx = c.getContext('2d')!;
  const g = ctx.createRadialGradient(S / 2, S / 2, S * 0.1, S / 2, S / 2, S / 2);
  g.addColorStop(0, 'rgba(13,7,22,0)');
  g.addColorStop(0.62, 'rgba(13,7,22,0)');
  g.addColorStop(0.85, 'rgba(13,7,22,0.26)');
  g.addColorStop(1, 'rgba(13,7,22,0.45)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, S, S);
  return Texture.from(c);
}

/** (e) 16px firefly dot — #fff3c4 core → #ffc46b → 0. Additive. */
function makeFireflyTexture(): Texture {
  const S = 16;
  const c = document.createElement('canvas');
  c.width = S;
  c.height = S;
  const ctx = c.getContext('2d')!;
  const g = ctx.createRadialGradient(S / 2, S / 2, 0, S / 2, S / 2, S / 2);
  g.addColorStop(0, 'rgba(255,243,196,0.95)');
  g.addColorStop(0.4, 'rgba(255,196,107,0.6)');
  g.addColorStop(1, 'rgba(255,196,107,0)');
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, S, S);
  return Texture.from(c);
}

interface GlowState {
  sprite: Sprite;
  /** tiles across */
  size: number;
  speed: number;
  phase: number;
  flicker: boolean;
}

interface Firefly {
  sprite: Sprite;
  homeX: number;
  homeY: number;
  r: number;
  s: number;
  p: number;
  life: number;
}

const FIREFLY_COUNT = 32;

/** Grayscale procedural avatar (tinted per player). 4 directions. */
function generateAvatar(dir: string): Texture {
  const c = document.createElement('canvas');
  c.width = AV_PX;
  c.height = AV_PX;
  const ctx = c.getContext('2d')!;
  const S = AV_PX;

  // body blob
  const g = ctx.createLinearGradient(0, S * 0.4, 0, S * 0.92);
  g.addColorStop(0, '#f2f0f6');
  g.addColorStop(1, '#a9a4b8');
  ctx.fillStyle = g;
  roundRectPath(ctx, S * 0.24, S * 0.4, S * 0.52, S * 0.52, 24);
  ctx.fill();

  // eyes offset by facing
  const ex = S / 2;
  const ey = S * 0.56;
  const off = 17;
  let dx = 0;
  let dy = 0;
  if (dir === 'left') dx = -off;
  else if (dir === 'right') dx = off;
  else if (dir === 'up') dy = -off;
  else dy = off;
  for (const s of [-1, 1]) {
    ctx.fillStyle = '#ffffff';
    ctx.beginPath();
    ctx.arc(ex + s * 18, ey, 9, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = '#16121f';
    ctx.beginPath();
    ctx.arc(ex + s * 18 + dx * 0.55, ey + dy * 0.55, 4.6, 0, Math.PI * 2);
    ctx.fill();
  }

  // mouth
  ctx.strokeStyle = '#4c4558';
  ctx.lineWidth = 3;
  ctx.lineCap = 'round';
  ctx.beginPath();
  if (dir === 'up') {
    ctx.arc(ex, ey + 16, 9, Math.PI * 1.15, Math.PI * 1.85);
  } else if (dir === 'left' || dir === 'right') {
    ctx.moveTo(ex - 8, ey + 20);
    ctx.lineTo(ex + 8, ey + 20);
  } else {
    ctx.arc(ex, ey + 13, 9, 0.15 * Math.PI, 0.85 * Math.PI);
  }
  ctx.stroke();

  return Texture.from(c);
}

export class WorldRenderer {
  private app!: Application;
  private world = new Container();
  private tileLayer = new Container();
  private playerLayer = new Container();
  private glowLayer = new Container();
  /** BRICK WORLD M1: claimable-plot marker layer (frontier districts). */
  private plotLayer = new Container();
  private plotRing: Graphics | null = null;
  private floorSprite: TilingSprite | null = null;

  // screen-space atmosphere (sky < world < wash < vignette)
  private skySprite: Sprite | null = null;
  private washSprite: Sprite | null = null;
  private vignetteSprite: Sprite | null = null;

  private textures!: Record<number, Texture>;
  private glowTex!: Texture;
  private shadowTex!: Texture;
  private fireflyTex!: Texture;
  private dirTextures!: Record<string, Texture>;
  /** Cached composite frames per spec (4 walk-bob frames per facing). */
  private specFrames = new Map<string, AvatarFrames>();
  private walkT = 0;
  private idleT = 0;
  private walkPhase: WalkPhase = 0;
  private idlePhase: 0 | 1 = 0;

  private tileSprites = new Map<string, Sprite>();
  private tiles = new Map<string, number>();
  private grid = new Uint8Array(0);
  private worldW = 32;
  private worldH = 32;
  /** Raw world envelope from the last setWorld (portals live here). */
  private lastWorld: unknown = null;

  /** Additive glow sprites: portals + hearth (rebuilt per world). */
  private glows: GlowState[] = [];
  /** Light anchor points for fireflies to gather near (tiles). */
  private lightSpots: { x: number; y: number }[] = [];
  private fireflies: Firefly[] = [];
  private atmoT = 0;

  /** Thin amber corner ticks on tiles painted by the local player this session. */
  private mineLayer = new Graphics();
  /** Tiny motion streaks behind the local player while walking. */
  private moveDust = new Graphics();

  private selfId = '';
  private local: LocalPlayer | null = null;
  private remotes = new Map<string, Remote>();

  private path: Pt[] | null = null;
  private pathIdx = 0;

  private zoom = 1;
  private pan = { x: 0, y: 0 };

  private pointers = new Map<number, { x: number; y: number }>();
  private pinch: { dist: number; midX: number; midY: number } | null = null;
  private dragStart: { x: number; y: number; t: number } | null = null;
  private panning = false;

  private lastMoveSent = 0;

  constructor(private onLocalMove: (m: LocalMove) => void) {
    // T2: when a custom asset image lands (or fails), drop cached composite
    // frames and refresh any remote whose look uses assets — placeholder
    // frames swap to the real image without waiting for the next state tick.
    onAvatarAssetLoad(() => {
      this.specFrames.clear();
      for (const r of this.remotes.values()) {
        if (r.spec && [r.spec.body, r.spec.skin, r.spec.hair, r.spec.outfit, r.spec.accessory].some(isAssetOption)) {
          r.sprite.texture = this.avatarFrames(r.spec).down[0];
        }
      }
    });
  }

  async init(container: HTMLElement): Promise<void> {
    this.textures = generateTileTextures();
    this.glowTex = makeGlowTexture();
    this.shadowTex = makeShadowTexture();
    this.fireflyTex = makeFireflyTexture();
    this.dirTextures = {
      down: generateAvatar('down'),
      up: generateAvatar('up'),
      left: generateAvatar('left'),
      right: generateAvatar('right'),
    };

    this.app = new Application();
    await this.app.init({
      resizeTo: container,
      background: '#0b0812',
      antialias: true,
      resolution: Math.min(window.devicePixelRatio || 1, 2),
      autoDensity: true,
    });
    const cv = this.app.canvas;
    cv.style.position = 'absolute';
    cv.style.inset = '0';
    cv.style.touchAction = 'none';
    container.appendChild(cv);

    // (h) dusk sky — full-screen, BELOW the world
    const sky = new Sprite(makeSkyTexture());
    sky.anchor.set(0, 0);
    this.skySprite = sky;
    // (a) warm wash + vignette — full-screen overlays ABOVE the world
    const wash = new Sprite(makeWashTexture());
    wash.anchor.set(0, 0);
    wash.blendMode = 'overlay';
    this.washSprite = wash;
    const vig = new Sprite(makeVignetteTexture());
    vig.anchor.set(0, 0);
    this.vignetteSprite = vig;

    this.app.stage.addChild(sky, this.world, wash, vig);
    this.world.addChild(this.tileLayer, this.mineLayer, this.moveDust, this.playerLayer, this.plotLayer, this.glowLayer);
    this.bindInput(cv);
    this.app.ticker.add((t) => this.tick(t.deltaMS / 1000));
    this.app.renderer.on('resize', () => this.resizeOverlays());
    this.resizeOverlays();

    // warm fonts for canvas labels once the webfonts land (labels re-render)
    if (typeof document !== 'undefined' && document.fonts?.ready) {
      void document.fonts.ready.then(() => this.refreshTextFonts());
    }
  }

  /** Screen-space overlays track the canvas size (resize-only, cheap). */
  private resizeOverlays(): void {
    if (!this.app) return;
    const w = this.app.screen.width;
    const h = this.app.screen.height;
    if (this.skySprite) {
      this.skySprite.width = w;
      this.skySprite.height = h;
    }
    if (this.washSprite) {
      this.washSprite.width = w;
      this.washSprite.height = h;
    }
    if (this.vignetteSprite) {
      this.vignetteSprite.width = w;
      this.vignetteSprite.height = h;
    }
  }

  /** Force canvas Text objects to re-render now that webfonts are ready. */
  private refreshTextFonts(): void {
    const bump = (t: Text) => {
      t.style.fontSize = t.style.fontSize + 0.01;
      t.style.fontSize = t.style.fontSize - 0.01;
    };
    if (this.local) bump(this.local.label);
    for (const r of this.remotes.values()) bump(r.label);
    for (const b of this.bubbles.values()) {
      bump(b.text);
      b.bg.alpha = b.bg.alpha; // no-op keeps TS happy; text refresh is enough
    }
  }

  // ------------------------------------------------------------ atmosphere

  /** Rebuild additive glows for the current world (portals + hearth). */
  private rebuildGlows(): void {
    for (const g of this.glows) this.glowLayer.removeChild(g.sprite);
    this.glows.length = 0;
    this.lightSpots.length = 0;

    const cx = this.worldW / 2 + 0.5;
    const cy = this.worldH / 2 + 0.5;

    // hearth double glow — the fire at the heart of the square (design §3c)
    const addGlow = (x: number, y: number, size: number, tint: number, speed: number, flicker: boolean): GlowState => {
      const s = new Sprite(this.glowTex);
      s.anchor.set(0.5, 0.5);
      s.position.set(x * TILE, y * TILE);
      s.width = size * TILE;
      s.height = size * TILE;
      s.tint = tint;
      s.blendMode = 'add';
      this.glowLayer.addChild(s);
      const g: GlowState = { sprite: s, size, speed, phase: Math.random() * Math.PI * 2, flicker };
      this.glows.push(g);
      return g;
    };
    addGlow(cx, cy, 3.2, 0xff6b3d, 1.7, true);
    addGlow(cx, cy, 1.7, 0xffc46b, 2.6, true);

    // lanterns — warm pools strung around the square (deterministic offsets)
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2 + 0.4;
      const r = 5.5 + (i % 3) * 1.7;
      const lx = cx + Math.cos(a) * r;
      const ly = cy + Math.sin(a) * r;
      if (lx < 1 || ly < 1 || lx > this.worldW - 1 || ly > this.worldH - 1) continue;
      addGlow(lx, ly, 1.2, 0xffc46b, 2.0 + (i % 3) * 0.5, false);
    }

    // portals — warm ember doorways (2 tiles), from the world envelope
    const raw = (this.lastWorld as Record<string, unknown> | null) ?? null;
    const arr = raw ? (raw.portals as unknown) : null;
    if (Array.isArray(arr)) {
      for (const p of arr) {
        if (!p || typeof p !== 'object') continue;
        const px = (p as { x?: unknown }).x;
        const py = (p as { y?: unknown }).y;
        if (typeof px !== 'number' || typeof py !== 'number') continue;
        addGlow(px + 0.5, py + 0.5, 2, 0xff6b3d, 1.9, true);
        this.lightSpots.push({ x: px + 0.5, y: py + 0.5 });
      }
    }
    this.lightSpots.push({ x: cx, y: cy });
    for (let i = 0; i < 6; i++) {
      const a = (i / 6) * Math.PI * 2 + 0.4;
      const r = 5.5 + (i % 3) * 1.7;
      const lx = cx + Math.cos(a) * r;
      const ly = cy + Math.sin(a) * r;
      if (lx >= 1 && ly >= 1 && lx <= this.worldW - 1 && ly <= this.worldH - 1) {
        this.lightSpots.push({ x: lx, y: ly });
      }
    }
  }

  /** (e) Firefly pool — 32 sprites, 60% near lights, 40% random, recycled. */
  private initFireflies(): void {
    for (const f of this.fireflies) this.glowLayer.removeChild(f.sprite);
    this.fireflies.length = 0;
    for (let i = 0; i < FIREFLY_COUNT; i++) {
      const s = new Sprite(this.fireflyTex);
      s.anchor.set(0.5, 0.5);
      s.blendMode = 'add';
      s.scale.set(0.22 + Math.random() * 0.18);
      this.glowLayer.addChild(s);
      const f: Firefly = { sprite: s, homeX: 0, homeY: 0, r: 0, s: 0, p: 0, life: 0 };
      this.fireflies.push(f);
      this.respawnFirefly(f, i);
    }
  }

  private respawnFirefly(f: Firefly, seed: number): void {
    const nearLight = seed % 10 < 6 && this.lightSpots.length > 0;
    if (nearLight) {
      const spot = this.lightSpots[(seed * 7) % this.lightSpots.length];
      f.homeX = spot.x + (Math.random() - 0.5) * 6;
      f.homeY = spot.y + (Math.random() - 0.5) * 6;
    } else {
      f.homeX = 1 + Math.random() * (this.worldW - 2);
      f.homeY = 1 + Math.random() * (this.worldH - 2);
    }
    f.r = 0.5 + Math.random() * 2.6;
    f.s = 0.3 + Math.random() * 0.9;
    f.p = Math.random() * Math.PI * 2;
    f.life = 6 + Math.random() * 4;
    f.sprite.alpha = 0.3 + Math.random() * 0.5;
  }

  /** (c) Glow pulse + firefly drift. Zero allocations. */
  private tickAtmosphere(dt: number): void {
    this.atmoT += dt;
    const t = this.atmoT;
    const glowsVisible = this.zoom >= 0.9;
    const w = this.app.screen.width;
    const h = this.app.screen.height;
    const wx = this.world.position.x;
    const wy = this.world.position.y;
    const z = this.zoom;

    for (const g of this.glows) {
      let alpha = 0.75 + Math.sin(t * g.speed + g.phase) * 0.15;
      if (g.flicker) alpha += (Math.random() - 0.5) * 0.08;
      g.sprite.alpha = alpha;
      if (!glowsVisible) {
        g.sprite.visible = false;
        continue;
      }
      // off-screen cull (world-space → screen-space via world transform)
      const sx = wx + g.sprite.position.x * z;
      const sy = wy + g.sprite.position.y * z;
      const m = g.size * TILE * z * 0.75;
      g.sprite.visible = sx > -m && sx < w + m && sy > -m && sy < h + m;
    }

    const ffVisible = this.zoom >= 0.8;
    for (const f of this.fireflies) {
      f.life -= dt;
      if (f.life <= 0) this.respawnFirefly(f, (Math.random() * 1e9) | 0);
      const px = f.homeX + Math.sin(t * f.s + f.p) * f.r;
      const py = f.homeY + Math.cos(t * f.s * 0.7 + f.p) * f.r * 0.6;
      f.sprite.position.set(px * TILE, py * TILE);
      f.sprite.alpha = (0.3 + 0.5 * Math.sin(t * 2 + f.p)) * (ffVisible ? 1 : 0);
    }
  }

  destroy(): void {
    try {
      this.app?.destroy(true, { children: true, texture: true });
    } catch {
      /* ignore */
    }
  }

  // ---------------------------------------------------------------- world

  setWorld(w: unknown): void {
    this.lastWorld = w;
    let parsed = parseWorld(w);
    if (!parsed || parsed.tiles.length === 0) parsed = genDefaultWorld();
    this.worldW = parsed.width;
    this.worldH = parsed.height;

    // (b) implicit floor → one repeating 3x3-variant TilingSprite below tiles
    if (!this.floorSprite) {
      this.floorSprite = new TilingSprite({
        texture: generateFloorTexture(),
        width: this.worldW * TILE,
        height: this.worldH * TILE,
      });
      this.floorSprite.position.set(0, 0);
      this.world.addChildAt(this.floorSprite, 0);
    } else {
      this.floorSprite.texture = generateFloorTexture();
      this.floorSprite.width = this.worldW * TILE;
      this.floorSprite.height = this.worldH * TILE;
    }

    this.tileLayer.removeChildren();
    this.tileSprites.clear();
    this.tiles.clear();
    // Sparse wire format: only non-floor tiles are serialized, so a fresh grid
    // must default to PASSABLE (1) — otherwise every floor cell reads as
    // impassable and tap-to-move silently fails everywhere.
    this.grid = new Uint8Array(this.worldW * this.worldH).fill(1);

    for (const t of parsed.tiles) {
      const key = `${t.x},${t.y}`;
      const tid = t.tileId;
      this.tiles.set(key, tid);
      const tex = this.textures[texKey(tid, tileVariant(t.x, t.y))] ?? this.textures[texKey(99, 0)];
      const sp = new Sprite(tex);
      sp.position.set(t.x * TILE, t.y * TILE);
      this.tileLayer.addChild(sp);
      this.tileSprites.set(key, sp);
      if (t.x >= 0 && t.x < this.worldW && t.y >= 0 && t.y < this.worldH) {
        this.grid[t.y * this.worldW + t.x] = isPassableTile(tid) ? 1 : 0;
      }
    }

    // world border frame
    const border = new Graphics()
      .rect(0, 0, this.worldW * TILE, this.worldH * TILE)
      .stroke({ width: 2, color: 0xff6b3d, alpha: 0.28 });
    this.tileLayer.addChild(border);

    this.zoom = 1;
    this.pan = { x: 0, y: 0 };
    this.path = null;
    this.mineLayer.clear();
    this.clearPlot();
    this.moveDust.clear();
    this.rebuildGlows();
    this.initFireflies();
    if (this.local) {
      this.local.x = clamp(this.local.x, 0.5, this.worldW - 0.5);
      this.local.y = clamp(this.local.y, 0.5, this.worldH - 0.5);
      this.local.dirty = true;
    }
  }

  /** Paint (or erase, tileId 0) a single cell. Creates/removes sprites so
   *  painting onto implicit floor actually shows up locally. */
  paintTile(x: number, y: number, tileId: number): void {
    const key = `${x},${y}`;
    const inBounds = x >= 0 && x < this.worldW && y >= 0 && y < this.worldH;
    const sp = this.tileSprites.get(key);
    if (tileId === 0) {
      // erase → remove the sprite (floor is implicit)
      if (sp) {
        this.tileLayer.removeChild(sp);
        this.tileSprites.delete(key);
      }
      this.tiles.delete(key);
      if (inBounds) this.grid[y * this.worldW + x] = 1;
      this.path = null;
      return;
    }
    if (!sp) {
      const ns = new Sprite(this.textures[texKey(tileId, tileVariant(x, y))] ?? this.textures[texKey(99, 0)]);
      ns.position.set(x * TILE, y * TILE);
      this.tileLayer.addChild(ns);
      this.tileSprites.set(key, ns);
    } else {
      sp.texture = this.textures[texKey(tileId, tileVariant(x, y))] ?? this.textures[texKey(99, 0)];
    }
    this.tiles.set(key, tileId);
    if (inBounds) this.grid[y * this.worldW + x] = isPassableTile(tileId) ? 1 : 0;
    this.path = null; // path may be invalidated by an edit
  }

  // ------------------------------------------------------------- players

  /** Shared ellipse ground shadow (design §3d) — anchored at the feet. */
  private makeShadow(): Sprite {
    const s = new Sprite(this.shadowTex);
    s.anchor.set(0.5, 0.5);
    s.scale.set(AV_SCALE * 1.15);
    s.alpha = 0.9;
    return s;
  }

  setSelf(id: string, name: string, avatar?: AvatarInfo): void {
    this.selfId = id;
    if (this.local) {
      this.playerLayer.removeChild(this.local.sprite, this.local.shadow, this.local.ring, this.local.label);
    }
    this.clearRemotes();
    const spec = avatar?.spec;
    const color = spec ? hexToNum(specAccent(spec)) : tintColor(name || 'you');
    let sprite: Sprite;
    let specKey = '';
    if (spec) {
      specKey = specKeyOf(spec);
      sprite = this.makeCompositeSprite(spec, 'down');
    } else {
      sprite = new Sprite(this.dirTextures.down);
      sprite.anchor.set(0.5, 0.92);
      sprite.scale.set(AV_SCALE);
      sprite.tint = color;
    }
    const ring = new Graphics();
    ring.circle(0, 0, 16).fill({ color: 0xffc46b, alpha: 0.1 });
    ring.circle(0, 0, 16).stroke({ width: 2, color: 0xffc46b, alpha: 0.45 });
    const label = this.makeLabel(name || 'you', color);
    const shadow = this.makeShadow();
    this.local = {
      x: this.worldW / 2 + 0.5,
      y: this.worldH / 2 + 0.5,
      dir: 'down',
      seq: 0,
      dirty: true,
      sprite,
      shadow,
      ring,
      label,
      specKey,
    };
    this.playerLayer.addChild(shadow, sprite, ring, label);
  }

  applyRoster(roster: RosterEntry[] | undefined): void {
    if (!Array.isArray(roster)) return;
    const now = performance.now();
    for (const r of roster) {
      if (!r || r.id === this.selfId) continue;
      const rem = this.ensureRemote(r.id, r.name, r.avatar);
      rem.buf.push(now, r.x, r.y);
      rem.label.text = r.name || r.id.slice(0, 6);
    }
  }

  updateState(states: RemoteState[]): void {
    const now = performance.now();
    for (const s of states) {
      if (!s || s.id === this.selfId) continue;
      const rem = this.ensureRemote(s.id, undefined, s.avatar);
      rem.buf.push(now, s.x, s.y);
      if (s.dir) {
        rem.dir = s.dir;
        rem.sprite.scale.x = s.dir === 'left' ? -AV_SCALE : AV_SCALE;
      }
    }
  }

  private clearRemotes(): void {
    for (const r of this.remotes.values()) this.playerLayer.removeChild(r.sprite, r.shadow, r.label);
    this.remotes.clear();
  }

  private ensureRemote(id: string, name = '', avatar?: AvatarInfo): Remote {
    let rem = this.remotes.get(id);
    if (!rem) {
      const spec = avatar?.spec;
      let sprite: Sprite;
      let color: number;
      let specKey = '';
      if (spec) {
        specKey = specKeyOf(spec);
        sprite = this.makeCompositeSprite(spec, 'down');
        color = hexToNum(specAccent(spec));
      } else {
        sprite = new Sprite(this.dirTextures.down);
        sprite.anchor.set(0.5, 0.92);
        sprite.scale.set(AV_SCALE);
        color = tintColor(name || id);
        sprite.tint = color;
      }
      const label = this.makeLabel(name || id.slice(0, 6), color);
      const shadow = this.makeShadow();
      this.playerLayer.addChild(shadow, sprite, label);
      rem = { sprite, shadow, label, buf: new InterpBuffer(100), dir: 'down', specKey, spec, moving: false, lastX: -1, lastY: -1 };
      this.remotes.set(id, rem);
    } else if (avatar?.spec && rem.specKey !== specKeyOf(avatar.spec)) {
      // layered spec may arrive on a later state tick — upgrade in place
      rem.specKey = specKeyOf(avatar.spec);
      rem.spec = avatar.spec;
      rem.sprite.texture = this.avatarFrames(avatar.spec).down[0];
      rem.sprite.tint = 0xffffff;
      rem.label.style.fill = hexToNum(specAccent(avatar.spec));
    } else if (avatar && !rem.specKey) {
      // legacy color/icon arrives late — apply tint (keeps old clients working)
      if (avatar.color && /^#[0-9a-fA-F]{6}$/.test(avatar.color)) {
        const c = parseInt(avatar.color.slice(1), 16);
        rem.sprite.tint = c;
        rem.label.style.fill = c;
      }
    }
    return rem;
  }

  /** Builds a sprite using the layered composite frames (colors are baked in). */
  private makeCompositeSprite(spec: AvatarSpec, dir: string): Sprite {
    const sprite = new Sprite(this.avatarFrames(spec)[this.dirKey(dir)][0]);
    sprite.anchor.set(0.5, 0.92);
    sprite.scale.set(AV_SCALE);
    return sprite;
  }

  private avatarFrames(spec: AvatarSpec): AvatarFrames {
    // custom assets bake image bytes into the frames; the cache is cleared
    // wholesale when an asset image loads (see constructor), so the key is
    // just the spec key — no unbounded rev-keyed growth
    const key = specKeyOf(spec);
    let f = this.specFrames.get(key);
    if (!f) {
      const mk = (dir: 'down' | 'up' | 'right') =>
        [0, 1, 2, 3].map((ph) => Texture.from(renderAvatarSpec(spec, { dir, phase: ph as WalkPhase })));
      f = { down: mk('down'), up: mk('up'), right: mk('right') };
      this.specFrames.set(key, f);
    }
    return f;
  }

  private dirKey(dir: string): 'down' | 'up' | 'right' {
    if (dir === 'up') return 'up';
    if (dir === 'left' || dir === 'right') return 'right';
    return 'down';
  }

  private makeLabel(text: string, color: number): Text {
    const t = new Text({
      text,
      style: {
        fontFamily: 'Nunito, system-ui, sans-serif',
        fontSize: 11,
        fill: color,
        stroke: { color: '#0b0812', width: 2.5 },
        dropShadow: {
          color: '#0d0716',
          alpha: 0.9,
          blur: 2,
          distance: 1,
        },
      },
    });
    t.anchor.set(0.5, 0);
    t.alpha = 0.9;
    return t;
  }

  // ---------------------------------------------------------------- input

  /**
   * Smallest zoom that still lets the whole world fit the screen (capped at
   * MIN_ZOOM so desktop stays reasonable). Lets mobile users zoom out to a
   * coherent map overview instead of a wall of tiles.
   */
  private zoomFloor(): number {
    const w = this.app?.screen.width ?? 800;
    const h = this.app?.screen.height ?? 600;
    const fit = Math.min(w / (this.worldW * TILE), h / (this.worldH * TILE)) * 0.96;
    return clamp(fit, MIN_ZOOM_FLOOR, MIN_ZOOM);
  }

  private bindInput(cv: HTMLCanvasElement): void {
    cv.addEventListener('pointerdown', (e) => this.onDown(cv, e));
    cv.addEventListener('pointermove', (e) => this.onMove(cv, e));
    cv.addEventListener('pointerup', (e) => this.onUp(cv, e));
    cv.addEventListener('pointercancel', (e) => this.onUp(cv, e));
    cv.addEventListener(
      'wheel',
      (e) => {
        e.preventDefault();
        const f = Math.exp(-e.deltaY * 0.0012);
        this.zoom = clamp(this.zoom * f, this.zoomFloor(), MAX_ZOOM);
      },
      { passive: false },
    );
    cv.addEventListener('contextmenu', (e) => e.preventDefault());
  }

  private pos(cv: HTMLCanvasElement, e: PointerEvent): { x: number; y: number } {
    const r = cv.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  }

  private onDown(cv: HTMLCanvasElement, e: PointerEvent): void {
    cv.setPointerCapture(e.pointerId);
    const p = this.pos(cv, e);
    this.pointers.set(e.pointerId, p);
    if (this.pointers.size === 2) {
      const [a, b] = [...this.pointers.values()];
      this.pinch = {
        dist: Math.hypot(a.x - b.x, a.y - b.y),
        midX: (a.x + b.x) / 2,
        midY: (a.y + b.y) / 2,
      };
      this.dragStart = null;
    } else if (this.pointers.size === 1) {
      this.dragStart = { x: p.x, y: p.y, t: performance.now() };
    }
  }

  private onMove(cv: HTMLCanvasElement, e: PointerEvent): void {
    const prev = this.pointers.get(e.pointerId);
    if (!prev) return;
    const cur = this.pos(cv, e);
    this.pointers.set(e.pointerId, cur);

    if (this.pointers.size >= 2 && this.pinch) {
      const [a, b] = [...this.pointers.values()];
      const dist = Math.hypot(a.x - b.x, a.y - b.y);
      const midX = (a.x + b.x) / 2;
      const midY = (a.y + b.y) / 2;
      if (this.pinch.dist > 0) {
        this.zoom = clamp((this.zoom * dist) / this.pinch.dist, this.zoomFloor(), MAX_ZOOM);
      }
      this.pan.x += (midX - this.pinch.midX) / this.zoom;
      this.pan.y += (midY - this.pinch.midY) / this.zoom;
      this.pinch = { dist, midX, midY };
      this.panning = true;
      return;
    }

    if (this.pointers.size === 1) {
      const dx = cur.x - prev.x;
      const dy = cur.y - prev.y;
      if (this.dragStart && Math.hypot(cur.x - this.dragStart.x, cur.y - this.dragStart.y) > 8) {
        this.panning = true;
      }
      if (this.panning) {
        this.pan.x += dx / this.zoom;
        this.pan.y += dy / this.zoom;
      }
    }
  }

  private onUp(cv: HTMLCanvasElement, e: PointerEvent): void {
    const wasPinch = this.pointers.size >= 2;
    this.pointers.delete(e.pointerId);
    if (wasPinch && this.pointers.size < 2) this.pinch = null;

    if (this.pointers.size === 0 && this.dragStart && !this.panning) {
      const cur = this.pos(cv, e);
      const moved = Math.hypot(cur.x - this.dragStart.x, cur.y - this.dragStart.y);
      if (moved < 10 && performance.now() - this.dragStart.t < 500) {
        // screenToTile is the single source of truth for world<->screen math
        const tile = this.screenToTile(cur.x, cur.y);
        if (tile) this.startMoveTo(tile.x, tile.y);
      }
    }
    if (this.pointers.size === 0) {
      this.dragStart = null;
      this.panning = false;
    }
  }

  // ----------------------------------------------------------------- tick

  private tick(dt: number): void {
    const now = performance.now();
    this.stepPath(dt);
    // BRICK WORLD M1: pulse the claimable-plot ring.
    if (this.plotRing) this.plotRing.alpha = 0.7 + Math.sin(now * 0.004) * 0.3;
    this.updateCamera(dt);
    this.tickAtmosphere(dt);
    // walk: 4-frame cycle at 8Hz (0.125s/frame) while moving; gentle 2-frame
    // idle bob (0.45s/frame) while standing — both local and remote.
    const moving = !!this.local && (!!this.path || !!this.local.dirty);
    if (moving) {
      this.walkT += dt;
      this.idleT = 0;
    } else {
      this.walkT = 0;
      this.idleT += dt;
    }
    this.walkPhase = (Math.floor(this.walkT / 0.125) % 4) as WalkPhase;
    this.idlePhase = (Math.floor(this.idleT / 0.45) % 2) as 0 | 1;

    const labelsVisible = this.zoom >= LABEL_FADE_ZOOM;
    this.mineLayer.alpha = labelsVisible ? 1 : 0;
    const ph = moving ? this.walkPhase : (this.idlePhase as WalkPhase);
    this.syncLocal(ph, moving, labelsVisible);
    for (const r of this.remotes.values()) {
      const s = r.buf.get(now);
      if (s) {
        r.sprite.position.set(s.x * TILE, s.y * TILE);
        r.shadow.position.set(s.x * TILE, s.y * TILE + 2);
        r.label.position.set(s.x * TILE, s.y * TILE - 8);
        const dx = s.x - r.lastX;
        const dy = s.y - r.lastY;
        r.moving = dx * dx + dy * dy > 1e-6;
        r.lastX = s.x;
        r.lastY = s.y;
        this.applyCompositePhase(r, r.moving ? this.walkPhase : (this.idlePhase as WalkPhase));
      }
      r.buf.prune(now);
      r.label.alpha = labelsVisible ? 0.9 : 0;
    }
    this.tickBubbles(now);
    this.maybeSendMove(now);
  }

  // ------------------------------------------------------------ bubbles

  private bubbles = new Map<string, { text: Text; bg: Graphics; until: number }>();

  /** Show a speech bubble above the given player (remote or self) for ~5s. */
  showBubble(entityId: string, text: string): void {
    const existing = this.bubbles.get(entityId);
    if (existing) {
      existing.text.text = text;
      existing.until = performance.now() + 5000;
      return;
    }
    const textObj = new Text(text, {
      fontFamily: 'system-ui, sans-serif',
      fontSize: 13,
      fill: 0x111111,
      wordWrap: true,
      wordWrapWidth: 180,
    });
    const bg = new Graphics()
      .roundRect(-6, -6, textObj.width + 12, textObj.height + 10, 8)
      .fill({ color: 0xffffff, alpha: 0.95 });
    bg.position.set(0, 0);
    this.playerLayer.addChild(bg, textObj);
    this.bubbles.set(entityId, { text: textObj, bg, until: performance.now() + 5000 });
  }

  private tickBubbles(now: number): void {
    for (const [id, b] of this.bubbles) {
      const holder = id === this.selfId ? this.local : this.remotes.get(id);
      if (holder && holder.sprite) {
        const x = holder.sprite.position.x;
        const y = holder.sprite.position.y - TILE * 1.35;
        b.bg.position.set(x, y);
        b.text.position.set(x, y - 2);
        const left = b.until - now;
        const alpha = left < 800 ? left / 800 : 1;
        b.bg.alpha = 0.95 * alpha;
        b.text.alpha = alpha;
        if (left <= 0) {
          this.playerLayer.removeChild(b.bg, b.text);
          this.bubbles.delete(id);
        }
      } else if (now > b.until) {
        this.playerLayer.removeChild(b.bg, b.text);
        this.bubbles.delete(id);
      }
    }
  }

  private stepPath(dt: number): void {
    const path = this.path;
    const l = this.local;
    if (!path || !l) return;
    while (this.pathIdx < path.length) {
      const g = path[this.pathIdx];
      const gx = g.x + 0.5;
      const gy = g.y + 0.5;
      const dx = gx - l.x;
      const dy = gy - l.y;
      const dist = Math.hypot(dx, dy);
      const step = SPEED * dt;
      if (dist <= step || dist === 0) {
        l.x = gx;
        l.y = gy;
        l.dirty = true;
        this.pathIdx++;
        if (this.pathIdx >= path.length) this.path = null;
        continue;
      }
      l.x += (dx / dist) * step;
      l.y += (dy / dist) * step;
      l.dir = Math.abs(dx) > Math.abs(dy) ? (dx > 0 ? 'right' : 'left') : dy > 0 ? 'down' : 'up';
      l.dirty = true;
      break;
    }
  }

  private updateCamera(dt: number): void {
    // ease free-look pan back to the player after a tap-to-move
    const k = Math.min(1, dt * 4);
    this.pan.x += (0 - this.pan.x) * k;
    this.pan.y += (0 - this.pan.y) * k;

    const cx = this.local ? this.local.x * TILE : (this.worldW * TILE) / 2;
    const cy = this.local ? this.local.y * TILE : (this.worldH * TILE) / 2;
    const z = this.zoom;
    const w = this.app.screen.width;
    const h = this.app.screen.height;
    const ww = this.worldW * TILE * z;
    const wh = this.worldH * TILE * z;
    let ox = w / 2 - (cx + this.pan.x) * z;
    let oy = h / 2 - (cy + this.pan.y) * z;
    if (ww > w) ox = clamp(ox, w - ww, 0);
    else ox = (w - ww) / 2;
    if (wh > h) oy = clamp(oy, h - wh, 0);
    else oy = (h - wh) / 2;
    this.world.position.set(ox, oy);
    this.world.scale.set(z);
  }

  private syncLocal(ph: WalkPhase, moving: boolean, labelsVisible: boolean): void {
    const l = this.local;
    if (!l) return;
    l.sprite.position.set(l.x * TILE, l.y * TILE);
    l.shadow.position.set(l.x * TILE, l.y * TILE + 2);
    l.ring.position.set(l.x * TILE, l.y * TILE);
    l.label.position.set(l.x * TILE, l.y * TILE - 8);
    l.label.alpha = labelsVisible ? 0.9 : 0;
    // motion dust: small streaks behind the player while walking
    this.moveDust.clear();
    if (moving) {
      const bx = l.x * TILE;
      const by = l.y * TILE;
      const d: Record<string, [number, number]> = {
        down: [0, 1],
        up: [0, -1],
        left: [-1, 0],
        right: [1, 0],
      };
      const [dx, dy] = d[l.dir] ?? [0, 1];
      const step = this.walkPhase % 2 === 0 ? 0 : 1;
      this.moveDust
        .circle(bx - dx * 9 - dy * 5, by + 4 - dy * 4 + step * 2, 2.4)
        .fill({ color: 0xf59e0b, alpha: 0.35 });
      this.moveDust
        .circle(bx - dx * 15 - dy * 8, by + 2 - dy * 2 - step * 2, 1.8)
        .fill({ color: 0xf59e0b, alpha: 0.22 });
    }
    if (l.specKey) {
      const frames = this.specFrames.get(l.specKey);
      if (frames) {
        const tex = frames[this.dirKey(l.dir)][ph];
        if (l.sprite.texture !== tex) l.sprite.texture = tex;
      }
      return;
    }
    const tex = this.dirTextures[l.dir] ?? this.dirTextures.down;
    if (l.sprite.texture !== tex) l.sprite.texture = tex;
  }

  /** Cycles a composite remote sprite through its walk/idle frames. */
  private applyCompositePhase(r: Remote, ph: WalkPhase): void {
    if (!r.specKey) return;
    const frames = this.specFrames.get(r.specKey);
    if (!frames) return;
    const tex = frames[this.dirKey(r.dir)][ph];
    if (r.sprite.texture !== tex) r.sprite.texture = tex;
  }

  private maybeSendMove(now: number): void {
    const l = this.local;
    if (!l || !l.dirty) return;
    if (now - this.lastMoveSent < MOVE_INTERVAL) return;
    this.lastMoveSent = now;
    l.dirty = false;
    this.onLocalMove({
      x: Math.round(l.x * 100) / 100,
      y: Math.round(l.y * 100) / 100,
      dir: l.dir,
      seq: ++l.seq,
    });
  }

  /** Tap-to-move: A* to the tapped tile, then walk. Returns false if blocked. */
  startMoveTo(tx: number, ty: number): boolean {
    if (!this.local) return false;
    if (tx < 0 || ty < 0 || tx >= this.worldW || ty >= this.worldH) return false;
    if (this.grid[ty * this.worldW + tx] !== 1) return false;
    const p = astar(
      this.grid,
      this.worldW,
      this.worldH,
      { x: Math.floor(this.local.x), y: Math.floor(this.local.y) },
      { x: tx, y: ty },
    );
    if (!p) return false;
    this.path = p.length <= 1 ? null : p;
    this.pathIdx = 0;
    this.local.dirty = true;
    return true;
  }

  getLocal(): { x: number; y: number; dir: string } | null {
    return this.local ? { x: this.local.x, y: this.local.y, dir: this.local.dir } : null;
  }

  // ------------------------------------------- additive camera helpers (S2)
  // Read-only world<->screen projection used by the S2 editor overlay + portal
  // markers. Purely additive: no renderer behavior changes.

  /** World tile center -> screen pixels (CSS px, relative to the canvas). */
  project(tx: number, ty: number): { x: number; y: number } | null {
    if (!this.app) return null;
    const z = this.world.scale.x;
    const p = this.world.position;
    return { x: p.x + (tx + 0.5) * TILE * z, y: p.y + (ty + 0.5) * TILE * z };
  }

  /** Screen pixels -> world tile cell (or null when outside the world). */
  screenToTile(px: number, py: number): { x: number; y: number } | null {
    if (!this.app) return null;
    const z = this.world.scale.x;
    const p = this.world.position;
    const x = Math.floor(((px - p.x) / z) / TILE);
    const y = Math.floor(((py - p.y) / z) / TILE);
    if (x < 0 || y < 0 || x >= this.worldW || y >= this.worldH) return null;
    return { x, y };
  }

  getWorldSize(): { w: number; h: number } {
    return { w: this.worldW, h: this.worldH };
  }

  getZoom(): number {
    return this.zoom;
  }

  /**
   * Redraws the "you painted this tile" corner ticks. Keys are "x,y" strings.
   * Fades out automatically when zoomed far out (see tick).
   */
  setMineTiles(keys: Iterable<string>): void {
    this.mineLayer.clear();
    const L = 5; // tick arm length
    const G = 3; // inset from tile corner
    for (const k of keys) {
      const [x, y] = k.split(',').map(Number);
      if (!Number.isFinite(x) || !Number.isFinite(y)) continue;
      const px = x * TILE;
      const py = y * TILE;
      // four corner L-ticks
      this.mineLayer
        .moveTo(px + G, py + G + L)
        .lineTo(px + G, py + G)
        .lineTo(px + G + L, py + G)
        .moveTo(px + TILE - G - L, py + G)
        .lineTo(px + TILE - G, py + G)
        .lineTo(px + TILE - G, py + G + L)
        .moveTo(px + G, py + TILE - G - L)
        .lineTo(px + G, py + TILE - G)
        .lineTo(px + G + L, py + TILE - G)
        .moveTo(px + TILE - G - L, py + TILE - G)
        .lineTo(px + TILE - G, py + TILE - G)
        .lineTo(px + TILE - G, py + TILE - G - L);
    }
    this.mineLayer.stroke({ width: 1.5, color: 0xf59e0b, alpha: 0.8 });
  }

  /** Teleport the local player to a world tile (portal arrival), clamped. */
  setLocalPos(x: number, y: number): void {
    if (!this.local) return;
    this.local.x = clamp(x, 0.5, this.worldW - 0.5);
    this.local.y = clamp(y, 0.5, this.worldH - 0.5);
    this.local.dirty = true;
    this.path = null;
    this.pathIdx = 0;
  }

  /** BRICK WORLD M1: draw the claimable-plot marker for a frontier district.
   *  A pulsing golden ring + label on its own layer above the players. */
  setPlot(x: number, y: number, label: string): void {
    this.clearPlot();
    const px = x * TILE + TILE / 2;
    const py = y * TILE + TILE / 2;
    const g = new Graphics();
    g.circle(px, py, TILE * 0.9).stroke({ width: 2, color: 0xffc46b, alpha: 0.9 });
    g.circle(px, py, TILE * 0.55).stroke({ width: 1.5, color: 0xffc46b, alpha: 0.55 });
    g.circle(px, py, 3).fill({ color: 0xffc46b, alpha: 0.9 });
    this.plotRing = g;
    const t = new Text(label, {
      fontFamily: 'Nunito, system-ui, sans-serif',
      fontSize: 11,
      fontWeight: '700',
      fill: 0xffc46b,
    });
    t.anchor.set(0.5, 0);
    t.position.set(px, py + TILE * 0.7);
    this.plotLayer.addChild(g, t);
  }

  clearPlot(): void {
    this.plotLayer.removeChildren();
    this.plotRing = null;
  }
}
