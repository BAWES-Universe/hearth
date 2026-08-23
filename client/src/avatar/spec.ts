// avatar_spec: one curated option per layer (T1). Mirrors server/avatar.go.
// The wire shape is exactly this JSON, so server and every viewer render the
// same look — identity travels with the entity, not the device.

export interface AvatarSpec {
  v: number;
  body: string;
  skin: string;
  hair: string;
  outfit: string;
  accessory: string;
}

/** Legacy-free avatar payload carried on join/welcome/state. */
export interface AvatarInfo {
  color?: string;
  icon?: string;
  spec?: AvatarSpec;
}

export const AVATAR_SPEC_V = 1;

export type AvatarLayerId = 'body' | 'skin' | 'hair' | 'outfit' | 'accessory';

export const AVATAR_LAYERS: AvatarLayerId[] = ['body', 'skin', 'hair', 'outfit', 'accessory'];

/** The picker default — a coherent starter look. */
export function defaultAvatarSpec(): AvatarSpec {
  return { v: AVATAR_SPEC_V, body: 'round', skin: 'warm', hair: 'bob', outfit: 'hoodie', accessory: 'none' };
}

/** Normalize a partial/incoming spec: unknown layers fall back per layer. */
export function normalizeSpec(s: Partial<AvatarSpec> | null | undefined): AvatarSpec {
  const base = defaultAvatarSpec();
  if (!s) return base;
  const pick = (layer: AvatarLayerId, fallback: string): string => {
    const v = s[layer];
    return typeof v === 'string' && v.length > 0 && v.length <= 24 ? v : fallback;
  };
  return {
    v: AVATAR_SPEC_V,
    body: pick('body', base.body),
    skin: pick('skin', base.skin),
    hair: pick('hair', base.hair),
    outfit: pick('outfit', base.outfit),
    accessory: pick('accessory', base.accessory),
  };
}

/** Stable cache key for sprite textures. */
export function specKey(s: AvatarSpec): string {
  return `v${s.v}:${s.body}/${s.skin}/${s.hair}/${s.outfit}/${s.accessory}`;
}

/** True when the spec is the NPC-only robot body (bots stay distinct). */
export function isRobotSpec(s: AvatarSpec): boolean {
  return s.body === 'bot';
}

/** Accent color derived from the spec (labels, rings, picker chrome). Bots
 *  get a neutral robot accent so NPCs stay visually distinct. */
export function specAccent(s: AvatarSpec): string {
  if (isRobotSpec(s)) return '#94a3b8';
  const ACCENTS = ['#8b5cf6', '#22d3ee', '#f472b6', '#4ade80', '#fb923c', '#e879f9', '#60a5fa', '#facc15'];
  let h = 0;
  const key = specKey(s);
  for (let i = 0; i < key.length; i++) h = (h * 31 + key.charCodeAt(i)) & 0x7fffffff;
  return ACCENTS[h % ACCENTS.length];
}

const LS_SPEC = 'hearth:avatarSpec';

/** Persisted locally so the join screen restores the last crafted look. */
export function loadSpec(): AvatarSpec {
  try {
    const raw = localStorage.getItem(LS_SPEC);
    if (raw) {
      const p = JSON.parse(raw) as Partial<AvatarSpec>;
      if (p && typeof p === 'object') return normalizeSpec(p);
    }
  } catch {
    /* ignore */
  }
  return defaultAvatarSpec();
}

export function saveSpec(s: AvatarSpec): void {
  try {
    localStorage.setItem(LS_SPEC, JSON.stringify(s));
  } catch {
    /* ignore */
  }
}
