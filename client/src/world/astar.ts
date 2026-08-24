// A* pathfinding on the tile grid (8-dir, corner-cut prevention).
// Grid is tiny (≤256x256), so a linear open-list scan is plenty fast.

export interface Pt {
  x: number;
  y: number;
}

interface Node {
  x: number;
  y: number;
  g: number;
  f: number;
  parent: Node | null;
}

const DIRS: ReadonlyArray<readonly [number, number]> = [
  [1, 0],
  [-1, 0],
  [0, 1],
  [0, -1],
  [1, 1],
  [1, -1],
  [-1, 1],
  [-1, -1],
];

/**
 * Walk out to the nearest passable tile (Manhattan rings, deterministic tie
 * break) from a blocked cell. Rescues tap-to-move when the player's own tile
 * is impassable — polluted maps with walls painted on/near a spawn point
 * (town-square live-test bug) would otherwise make A* refuse the blocked
 * start and movement would silently die.
 */
export function nearestPassable(
  passable: Uint8Array,
  w: number,
  h: number,
  x: number,
  y: number,
): Pt | null {
  const key = (cx: number, cy: number) => cy * w + cx;
  const pass = (cx: number, cy: number) =>
    cx >= 0 && cy >= 0 && cx < w && cy < h && passable[key(cx, cy)] === 1;
  if (pass(x, y)) return { x, y };
  const limit = Math.max(w, h);
  for (let r = 1; r <= limit; r++) {
    for (let dy = -r; dy <= r; dy++) {
      for (let dx = -r; dx <= r; dx++) {
        if (Math.abs(dx) !== r && Math.abs(dy) !== r) continue; // ring only
        if (pass(x + dx, y + dy)) return { x: x + dx, y: y + dy };
      }
    }
  }
  return null;
}

/**
 * @param passable Uint8Array: 1 = walkable, 0 = blocked (row-major, w columns)
 * @returns tile path (inclusive of start & goal) or null when unreachable.
 *          Empty array when start === goal.
 */
export function astar(passable: Uint8Array, w: number, h: number, start: Pt, goal: Pt): Pt[] | null {
  const sx = Math.floor(start.x);
  const sy = Math.floor(start.y);
  const gx = Math.floor(goal.x);
  const gy = Math.floor(goal.y);
  if (sx === gx && sy === gy) return [];
  const key = (x: number, y: number) => y * w + x;
  const pass = (x: number, y: number) =>
    x >= 0 && y >= 0 && x < w && y < h && passable[key(x, y)] === 1;
  if (!pass(gx, gy)) return null;
  // A blocked START (spawn on a painted wall — town-square live-test bug)
  // used to kill tap-to-move entirely. Snap to the nearest passable tile so
  // the path walks the player OFF the wall and on toward the goal.
  let psx = sx;
  let psy = sy;
  if (!pass(sx, sy)) {
    const snap = nearestPassable(passable, w, h, sx, sy);
    if (!snap) return null; // fully enclosed — nowhere to walk
    psx = snap.x;
    psy = snap.y;
  }

  const open = new Map<number, Node>();
  const closed = new Set<number>();
  const hc = (x: number, y: number) => Math.abs(x - gx) + Math.abs(y - gy);
  const startNode: Node = { x: psx, y: psy, g: 0, f: hc(psx, psy), parent: null };
  open.set(key(psx, psy), startNode);

  while (open.size) {
    let best: Node | null = null;
    let bestF = Infinity;
    for (const n of open.values()) if (n.f < bestF) {
      bestF = n.f;
      best = n;
    }
    if (!best) break;
    if (best.x === gx && best.y === gy) {
      const out: Pt[] = [];
      let cur: Node | null = best;
      while (cur) {
        out.push({ x: cur.x, y: cur.y });
        cur = cur.parent;
      }
      return out.reverse();
    }
    open.delete(key(best.x, best.y));
    closed.add(key(best.x, best.y));
    for (const [dx, dy] of DIRS) {
      const nx = best.x + dx;
      const ny = best.y + dy;
      if (!pass(nx, ny)) continue;
      if (closed.has(key(nx, ny))) continue;
      // no corner cutting through wall diagonals
      if (dx !== 0 && dy !== 0 && (!pass(best.x + dx, best.y) || !pass(best.x, best.y + dy))) continue;
      const ng = best.g + (dx !== 0 && dy !== 0 ? 1.4142 : 1);
      const existing = open.get(key(nx, ny));
      if (!existing || ng < existing.g) {
        open.set(key(nx, ny), { x: nx, y: ny, g: ng, f: ng + hc(nx, ny), parent: best });
      }
    }
  }
  return null;
}
