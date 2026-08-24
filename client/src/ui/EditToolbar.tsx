// Editor-mode shell (S2): mode tabs Play / Paint / Portal wired to the editor
// bridge in App (which sends frozen HMF v1 ops over WS — server-arbitrated).
// T2 editor v2 adds Objects + Assets (custom image upload → place like a
// tile). When the S3 editor module lands, the bridge swaps to its exports;
// the shell stays the same.

import { TILE_DEFS } from '../world/tiles';
import type { AssetRecord } from '../net/api';

export type EditMode = 'play' | 'paint' | 'portal' | 'objects' | 'assets';

/** Functional object palette (server-validated kinds: door|npc|sign|light). */
export type ObjectKind = 'door' | 'npc' | 'sign' | 'light';

export const OBJECT_KINDS: { kind: ObjectKind; label: string; icon: string }[] = [
  { kind: 'door', label: 'Door', icon: '🚪' },
  { kind: 'npc', label: 'NPC', icon: '🤖' },
  { kind: 'sign', label: 'Sign', icon: '📋' },
  { kind: 'light', label: 'Light', icon: '💡' },
];

/** Swatch colors mirroring generateTileTextures() for quick picking. */
const SWATCH: Record<number, string> = {
  1: '#3b3154', // wall
  2: '#16304e', // water
  3: '#1f331d', // grass
  4: '#2e2b3a', // stone
};

const PAINTABLE = Object.keys(TILE_DEFS)
  .map(Number)
  .filter((id) => id !== 0 && SWATCH[id]);

const MODE_HINT: Record<EditMode, string> = {
  play: 'Tap to move · pinch to zoom · drag to pan',
  paint: 'Paint mode — tap or drag to draw · edits save live for everyone',
  portal: 'Portal mode — tap a tile to drop a portal back to town-square',
  objects: 'Objects mode — tap a tile to place the selected object',
  assets: 'Assets mode — place your uploaded images · edits save live for everyone',
};

export function EditToolbar({
  mode,
  onMode,
  brush,
  onBrush,
  erasing,
  onErasing,
  onUndo,
  canUndo,
  worldName,
  isPublished,
  publishing,
  onPublish,
  onInvite,
  canEdit,
  online,
  mineCount,
  objectKind,
  onObjectKind,
  assets,
  assetBrush,
  onAssetBrush,
  removingAsset,
  onRemovingAsset,
  uploading,
  onUpload,
}: {
  mode: EditMode;
  onMode(m: EditMode): void;
  brush: number;
  onBrush(id: number): void;
  erasing: boolean;
  onErasing(b: boolean): void;
  onUndo(): void;
  canUndo: boolean;
  worldName: string;
  isPublished: boolean;
  publishing: boolean;
  onPublish(): void;
  onInvite(): void;
  canEdit: boolean;
  online: boolean;
  mineCount: number;
  objectKind: ObjectKind;
  onObjectKind(k: ObjectKind): void;
  /** T2 custom asset upload: registry palette + selection + remove toggle. */
  assets: AssetRecord[];
  assetBrush: string | null;
  onAssetBrush(id: string | null): void;
  removingAsset: boolean;
  onRemovingAsset(b: boolean): void;
  uploading: boolean;
  onUpload(file: File): void;
}) {
  const inEdit = mode !== 'play';
  return (
    <>
      {inEdit && (
        <div class="edit-tools">
          {mode === 'paint' && (
            <>
              <div class="palette-row">
                <button
                  class={`swatch eraser${erasing ? ' active' : ''}`}
                  onClick={() => onErasing(!erasing)}
                  title="Eraser — remove a tile"
                  aria-label="Eraser tool"
                >
                  ✕
                </button>
                {PAINTABLE.map((id) => (
                  <button
                    key={id}
                    class={`swatch${!erasing && brush === id ? ' active' : ''}`}
                    style={{ background: SWATCH[id] }}
                    onClick={() => {
                      onErasing(false);
                      onBrush(id);
                    }}
                    title={TILE_DEFS[id].name}
                    aria-label={`Paint ${TILE_DEFS[id].name}`}
                  />
                ))}
              </div>
              <div class="paint-how">
                <span class="mine-count">
                  <span class="mine-dot" aria-hidden="true" /> you painted {mineCount}{' '}
                  {mineCount === 1 ? 'tile' : 'tiles'} here
                </span>
                <span class="paint-how-text">tap or drag to draw · edits save live for everyone</span>
              </div>
            </>
          )}
          {mode === 'portal' && (
            <div class="portal-hint">↓ portal target: town-square (16, 16)</div>
          )}
          {mode === 'objects' && (
            <>
              <div class="palette-row">
                {OBJECT_KINDS.map((o) => (
                  <button
                    key={o.kind}
                    class={`swatch object${objectKind === o.kind ? ' active' : ''}`}
                    onClick={() => onObjectKind(o.kind)}
                    title={`${o.label} — tap a tile to place`}
                    aria-label={`Place ${o.label}`}
                  >
                    {o.icon}
                  </button>
                ))}
              </div>
              <div class="paint-how">
                <span class="paint-how-text">tap a tile to place · doors are passable, npc/sign talk later</span>
              </div>
            </>
          )}
          {mode === 'assets' && (
            <>
              <div class="palette-row asset-row">
                {assets.map((a) => (
                  <button
                    key={a.id}
                    class={`asset-swatch${assetBrush === a.id && !removingAsset ? ' active' : ''}`}
                    onClick={() => {
                      onRemovingAsset(false);
                      onAssetBrush(assetBrush === a.id ? null : a.id);
                    }}
                    title={`${a.name || 'asset'} — tap a tile to place`}
                    aria-label={`Place asset ${a.name || a.id}`}
                  >
                    <img src={a.url} alt={a.name || 'asset'} draggable={false} />
                  </button>
                ))}
                <label
                  class={`asset-swatch upload${uploading ? ' busy' : ''}`}
                  title="Upload an image (png/jpeg/gif/webp, ≤512KB)"
                >
                  <input
                    type="file"
                    accept="image/png,image/jpeg,image/gif,image/webp"
                    onChange={(e) => {
                      const f = (e.target as HTMLInputElement).files?.[0];
                      if (f) onUpload(f);
                      (e.target as HTMLInputElement).value = '';
                    }}
                    hidden
                  />
                  {uploading ? '…' : '⬆'}
                </label>
                <button
                  class={`swatch eraser${removingAsset ? ' active' : ''}`}
                  onClick={() => onRemovingAsset(!removingAsset)}
                  title="Remove — tap a placed asset to remove it"
                  aria-label="Remove asset tool"
                >
                  🗑
                </button>
              </div>
              <div class="paint-how">
                <span class="paint-how-text">
                  {removingAsset
                    ? 'remove mode — tap a placed asset to delete it'
                    : assetBrush
                      ? 'tap a tile to place the selected image · uploads save live for everyone'
                      : 'pick an image (or upload one) then tap a tile to place it'}
                </span>
              </div>
            </>
          )}
          <div class="edit-actions">
            <button class="tool-btn" onClick={onUndo} disabled={!canUndo} title="Undo (compensating inverse op)">
              ↩ Undo
            </button>
            {canEdit && (
              <button class="tool-btn" onClick={onInvite} title="Invite a friend to co-edit this world">
                🔗 Invite
              </button>
            )}
            <button
              class="tool-btn publish"
              onClick={onPublish}
              disabled={!online || publishing || isPublished}
              title={isPublished ? 'Already published' : 'Publish this world to the directory'}
            >
              {isPublished ? '✓ Published' : publishing ? 'Publishing…' : '🚀 Publish'}
            </button>
          </div>
        </div>
      )}
      <div class="mode-dock" role="tablist" aria-label="Editor mode">
        {(['play', 'paint', 'portal', 'objects', 'assets'] as const).map((m) => (
          <button
            key={m}
            role="tab"
            aria-selected={mode === m}
            class={`mode-tab${mode === m ? ' active' : ''}`}
            onClick={() => onMode(m)}
          >
            {m === 'play' ? 'Play' : m === 'paint' ? 'Paint' : m === 'portal' ? 'Portal' : m === 'objects' ? 'Objects' : 'Assets'}
          </button>
        ))}
      </div>
      <div class="hint">{MODE_HINT[mode]}</div>
      {isPublished && (
        <div class="published-tag" title="This world is live in the directory">
          ✓ {worldName}
        </div>
      )}
    </>
  );
}
