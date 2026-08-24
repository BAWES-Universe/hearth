// Curated avatar layer catalog (T1). Mirrors server/avatar.go avatarCatalog.
// Each layer has >= 4 options; npcOnly options exist for bots/NPCs only and
// are hidden from the human picker (server also rejects them for humans).

import { isAssetOption, type AvatarLayerId, type AvatarSpec } from './spec';

export interface AvatarOptionDef {
  id: string;
  label: string;
  /** NPC/bot-only option — never shown in the human picker. */
  npcOnly?: boolean;
  /** Representative color chip / base draw color. */
  swatch: string;
}

export interface AvatarLayerDef {
  id: AvatarLayerId;
  label: string;
  options: AvatarOptionDef[];
}

export const LAYER_CATALOG: AvatarLayerDef[] = [
  {
    id: 'body',
    label: 'Body',
    options: [
      { id: 'round', label: 'Round', swatch: '#fbbf24' },
      { id: 'bean', label: 'Bean', swatch: '#84cc16' },
      { id: 'slim', label: 'Slim', swatch: '#38bdf8' },
      { id: 'square', label: 'Square', swatch: '#a78bfa' },
      { id: 'bot', label: 'Robot', swatch: '#94a3b8', npcOnly: true },
    ],
  },
  {
    id: 'skin',
    label: 'Skin',
    options: [
      { id: 'warm', label: 'Warm', swatch: '#f1c27d' },
      { id: 'fair', label: 'Fair', swatch: '#ffe0bd' },
      { id: 'olive', label: 'Olive', swatch: '#c68642' },
      { id: 'deep', label: 'Deep', swatch: '#8d5524' },
      { id: 'cool', label: 'Cool', swatch: '#e0ac69' },
    ],
  },
  {
    id: 'hair',
    label: 'Hair',
    options: [
      { id: 'bob', label: 'Bob', swatch: '#3b2f2f' },
      { id: 'curls', label: 'Curls', swatch: '#7c4a21' },
      { id: 'mohawk', label: 'Mohawk', swatch: '#f43f5e' },
      { id: 'cap', label: 'Cap', swatch: '#1d4ed8' },
      { id: 'bald', label: 'Bald', swatch: '#d1d5db' },
    ],
  },
  {
    id: 'outfit',
    label: 'Outfit',
    options: [
      { id: 'hoodie', label: 'Hoodie', swatch: '#6366f1' },
      { id: 'tee', label: 'Tee', swatch: '#10b981' },
      { id: 'robe', label: 'Robe', swatch: '#b45309' },
      { id: 'vest', label: 'Vest', swatch: '#0f766e' },
      { id: 'dress', label: 'Dress', swatch: '#db2777' },
    ],
  },
  {
    id: 'accessory',
    label: 'Accessory',
    options: [
      { id: 'none', label: 'None', swatch: '#9ca3af' },
      { id: 'glasses', label: 'Glasses', swatch: '#111827' },
      { id: 'crown', label: 'Crown', swatch: '#facc15' },
      { id: 'scarf', label: 'Scarf', swatch: '#ef4444' },
      { id: 'halo', label: 'Halo', swatch: '#fde68a' },
    ],
  },
];

/** Options visible to humans (npcOnly filtered out). */
export function layerOptions(id: AvatarLayerId): AvatarOptionDef[] {
  const layer = LAYER_CATALOG.find((l) => l.id === id);
  return layer ? layer.options.filter((o) => !o.npcOnly) : [];
}

export function optionOf(id: AvatarLayerId, optionId: string): AvatarOptionDef | undefined {
  if (isAssetOption(optionId)) {
    // custom uploaded asset — synthetic picker entry ("Custom" label)
    return { id: optionId, label: 'Custom', swatch: '#a78bfa' };
  }
  return LAYER_CATALOG.find((l) => l.id === id)?.options.find((o) => o.id === optionId);
}

export function layerLabel(id: AvatarLayerId): string {
  return LAYER_CATALOG.find((l) => l.id === id)?.label ?? id;
}

/** Quick presets — a complete look in one tap (<10s goal). */
export const AVATAR_PRESETS: AvatarSpec[] = [
  { v: 1, body: 'round', skin: 'warm', hair: 'bob', outfit: 'hoodie', accessory: 'none' },
  { v: 1, body: 'slim', skin: 'deep', hair: 'curls', outfit: 'dress', accessory: 'crown' },
  { v: 1, body: 'bean', skin: 'olive', hair: 'mohawk', outfit: 'vest', accessory: 'glasses' },
  { v: 1, body: 'square', skin: 'fair', hair: 'cap', outfit: 'tee', accessory: 'scarf' },
];
