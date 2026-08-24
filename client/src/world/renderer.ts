// World renderer: PixiJS v8 canvas, top-down 2D. Camera follows the local
// player (with free-look pan offset), pinch-zoom 0.6–2.5x, two-finger pan,
// tap-to-move via A*, procedural avatar with 4-dir facing, remote players
// interpolated through a 100ms buffer with cubic ease. Optimistic move
// reporting at 12Hz (seq monotonic).

import { Application, Container, Graphics, Sprite, Text, Texture } from 'pixi.js';
import { tintColor } from '../colors';
import { astar, type Pt } from './astar';
import { InterpBuffer } from './interp';
import { generateTileTextures, genDefaultWorld, isPassableTile, parseWorld, TILE } from './tiles';
import { renderAvatarSpec } from '../avatar/sprites';
import { specAccent, specKey as specKeyOf, type AvatarInfo, type AvatarSpec } from '../avatar/spec';

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
  label: Text;
  buf: InterpBuffer;
  dir: string;
  /** spec cache key when this remote renders the layered composite. */
  specKey: string;
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

/** Grayscale procedural avatar (tinted per player). 4 directions. */
function generateAvatar(dir: string): Texture {
  const c = document.createElement('canvas');
  c.width = AV_PX;
  c.height = AV_PX;
  const ctx = c.getContext('2d')!;
  const S = AV_PX;

  // ground shadow
  ctx.fillStyle = 'rgba(0,0,0,0.35)';
  ctx.beginPath();
  ctx.ellipse(S / 2, S * 0.93, 30, 9, 0, 0, Math.PI * 2);
  ctx.fill();

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

  private textures!: Record<number, Texture>;
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

  constructor(private onLocalMove: (m: LocalMove) => void) {}

  async init(container: HTMLElement): Promise<void> {
    this.textures = generateTileTextures();
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

    this.app.stage.addChild(this.world);
    this.world.addChild(this.tileLayer, this.mineLayer, this.moveDust, this.playerLayer);
    this.bindInput(cv);
    this.app.ticker.add((t) => this.tick(t.deltaMS / 1000));
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
    let parsed = parseWorld(w);
    if (!parsed || parsed.tiles.length === 0) parsed = genDefaultWorld();
    this.worldW = parsed.width;
    this.worldH = parsed.height;

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
      const tex = this.textures[tid] ?? this.textures[99];
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
      .stroke({ width: 2, color: 0x8b5cf6, alpha: 0.45 });
    this.tileLayer.addChild(border);

    this.zoom = 1;
    this.pan = { x: 0, y: 0 };
    this.path = null;
    this.mineLayer.clear();
    this.moveDust.clear();
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
      const ns = new Sprite(this.textures[tileId] ?? this.textures[99]);
      ns.position.set(x * TILE, y * TILE);
      this.tileLayer.addChild(ns);
      this.tileSprites.set(key, ns);
    } else {
      sp.texture = this.textures[tileId] ?? this.textures[99];
    }
    this.tiles.set(key, tileId);
    if (inBounds) this.grid[y * this.worldW + x] = isPassableTile(tileId) ? 1 : 0;
    this.path = null; // path may be invalidated by an edit
  }

  // ------------------------------------------------------------- players

  setSelf(id: string, name: string, avatar?: AvatarInfo): void {
    this.selfId = id;
    if (this.local) {
      this.playerLayer.removeChild(this.local.sprite, this.local.ring, this.local.label);
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
    ring.circle(0, 0, 16).fill({ color: 0xf59e0b, alpha: 0.1 });
    ring.circle(0, 0, 16).stroke({ width: 2, color: 0xf59e0b, alpha: 0.45 });
    const label = this.makeLabel(name || 'you', color);
    this.local = {
      x: this.worldW / 2 + 0.5,
      y: this.worldH / 2 + 0.5,
      dir: 'down',
      seq: 0,
      dirty: true,
      sprite,
      ring,
      label,
      specKey,
    };
    this.playerLayer.addChild(sprite, ring, label);
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
    for (const r of this.remotes.values()) this.playerLayer.removeChild(r.sprite, r.label);
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
      this.playerLayer.addChild(sprite, label);
      rem = { sprite, label, buf: new InterpBuffer(100), dir: 'down', specKey, moving: false, lastX: -1, lastY: -1 };
      this.remotes.set(id, rem);
    } else if (avatar?.spec && rem.specKey !== specKeyOf(avatar.spec)) {
      // layered spec may arrive on a later state tick — upgrade in place
      rem.specKey = specKeyOf(avatar.spec);
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
        fontFamily: 'system-ui, sans-serif',
        fontSize: 11,
        fill: color,
        stroke: { color: '#0b0812', width: 2.5 },
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
    this.updateCamera(dt);
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
    this.maybeSendMove(now);
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
}
