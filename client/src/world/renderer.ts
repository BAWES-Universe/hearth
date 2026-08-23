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
const MIN_ZOOM = 0.6;
const MAX_ZOOM = 2.5;

interface Remote {
  sprite: Sprite;
  label: Text;
  buf: InterpBuffer;
  dir: string;
  /** spec cache key when this remote renders the layered composite. */
  specKey: string;
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
  /** Cached composite frames per spec (walk bob pairs per facing). */
  private specFrames = new Map<string, AvatarFrames>();
  private walkT = 0;
  private walkPhase: 0 | 1 = 0;

  private tileSprites = new Map<string, Sprite>();
  private tiles = new Map<string, number>();
  private grid = new Uint8Array(0);
  private worldW = 32;
  private worldH = 32;

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
    this.world.addChild(this.tileLayer, this.playerLayer);
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
    this.grid = new Uint8Array(this.worldW * this.worldH);

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
    if (this.local) {
      this.local.x = clamp(this.local.x, 0.5, this.worldW - 0.5);
      this.local.y = clamp(this.local.y, 0.5, this.worldH - 0.5);
      this.local.dirty = true;
    }
  }

  paintTile(x: number, y: number, tileId: number): void {
    const key = `${x},${y}`;
    const sp = this.tileSprites.get(key);
    if (!sp) return;
    this.tiles.set(key, tileId);
    sp.texture = this.textures[tileId] ?? this.textures[99];
    if (x >= 0 && x < this.worldW && y >= 0 && y < this.worldH) {
      this.grid[y * this.worldW + x] = isPassableTile(tileId) ? 1 : 0;
    }
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
    const ring = new Graphics().circle(0, 0, 22).stroke({ width: 3, color: 0xf59e0b, alpha: 0.9 });
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
      rem = { sprite, label, buf: new InterpBuffer(100), dir: 'down', specKey };
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
        [0, 1].map((ph) => Texture.from(renderAvatarSpec(spec, { dir, phase: ph as 0 | 1 })));
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
        fontSize: 12,
        fill: color,
        stroke: { color: '#0b0812', width: 3 },
      },
    });
    t.anchor.set(0.5, 0);
    return t;
  }

  // ---------------------------------------------------------------- input

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
        this.zoom = clamp(this.zoom * f, MIN_ZOOM, MAX_ZOOM);
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
        this.zoom = clamp((this.zoom * dist) / this.pinch.dist, MIN_ZOOM, MAX_ZOOM);
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
        const wx = (cur.x - this.world.position.x) / this.zoom;
        const wy = (cur.y - this.world.position.y) / this.zoom;
        this.startMoveTo(Math.floor(wx / TILE), Math.floor(wy / TILE));
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
    // walk bob: 2-frame cycle at 4Hz while the local player is moving
    const moving = !!this.local && (!!this.path || !!this.local.dirty);
    if (moving) this.walkT += dt;
    else this.walkT = 0;
    this.walkPhase = (Math.floor(this.walkT / 0.25) % 2) as 0 | 1;
    this.syncLocal();
    for (const r of this.remotes.values()) {
      const s = r.buf.get(now);
      if (s) {
        r.sprite.position.set(s.x * TILE, s.y * TILE);
        r.label.position.set(s.x * TILE, s.y * TILE - 8);
        this.applyCompositePhase(r, this.walkPhase);
      }
      r.buf.prune(now);
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

  private syncLocal(): void {
    const l = this.local;
    if (!l) return;
    l.sprite.position.set(l.x * TILE, l.y * TILE);
    l.ring.position.set(l.x * TILE, l.y * TILE);
    l.label.position.set(l.x * TILE, l.y * TILE - 8);
    if (l.specKey) {
      const frames = this.specFrames.get(l.specKey);
      if (frames) {
        const tex = frames[this.dirKey(l.dir)][this.walkPhase];
        if (l.sprite.texture !== tex) l.sprite.texture = tex;
      }
      return;
    }
    const tex = this.dirTextures[l.dir] ?? this.dirTextures.down;
    if (l.sprite.texture !== tex) l.sprite.texture = tex;
  }

  /** Bobs a composite remote sprite between its two walk frames. */
  private applyCompositePhase(r: Remote, phase: 0 | 1): void {
    if (!r.specKey) return;
    const frames = this.specFrames.get(r.specKey);
    if (!frames) return;
    const tex = frames[this.dirKey(r.dir)][phase];
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
}
