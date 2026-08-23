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
  if (!pass(sx, sy) || !pass(gx, gy)) return null;

  const open = new Map<number, Node>();
  const closed = new Set<number>();
  const hc = (x: number, y: number) => Math.abs(x - gx) + Math.abs(y - gy);
  const startNode: Node = { x: sx, y: sy, g: 0, f: hc(sx, sy), parent: null };
  open.set(key(sx, sy), startNode);

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
