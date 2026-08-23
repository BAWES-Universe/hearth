// Remote-player position interpolation: render ~100ms in the past (network
// delay buffer) with cubic ease between the two bracketing server samples.

export interface Sample {
  t: number; // performance.now() ms
  x: number; // tile units
  y: number;
}

export class InterpBuffer {
  private samples: Sample[] = [];

  constructor(public delayMs = 100) {}

  push(t: number, x: number, y: number): void {
    this.samples.push({ t, x, y });
    if (this.samples.length > 64) this.samples.shift();
  }

  /** Position as of (now - delayMs), cubic-eased between bracketing samples. */
  get(now: number): Sample | null {
    const s = this.samples;
    if (!s.length) return null;
    if (s.length === 1) return { ...s[0] };
    const target = now - this.delayMs;
    let i = 0;
    while (i < s.length - 1 && s[i + 1].t <= target) i++;
    const a = s[i];
    const b = s[Math.min(i + 1, s.length - 1)];
    if (a.t === b.t) return { x: a.x, y: a.y, t: target };
    const f = Math.min(1, Math.max(0, (target - a.t) / (b.t - a.t)));
    const e = f * f * (3 - 2 * f); // smoothstep = cubic ease
    return {
      x: a.x + (b.x - a.x) * e,
      y: a.y + (b.y - a.y) * e,
      t: target,
    };
  }

  prune(now: number): void {
    const cutoff = now - 3000;
    while (this.samples.length > 1 && this.samples[0].t < cutoff) this.samples.shift();
  }
}
