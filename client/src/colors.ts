// Deterministic per-name accent colors (purple/amber family + friends).

const PALETTE = [
  '#8b5cf6', // purple
  '#22d3ee', // cyan
  '#f472b6', // pink
  '#4ade80', // green
  '#fb923c', // orange
  '#e879f9', // fuchsia
  '#60a5fa', // blue
  '#facc15', // yellow
];

export function colorIndex(name: string): number {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return h % PALETTE.length;
}

export function cssColor(name: string): string {
  return PALETTE[colorIndex(name)];
}

/** PixiJS tint (0xRRGGBB number) for a player color. */
export function tintColor(name: string): number {
  return parseInt(cssColor(name).slice(1), 16);
}
