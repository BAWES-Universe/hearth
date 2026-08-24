// Layered avatar builder (T1 + T2): 5 layer tabs with option swatches, a live
// composite preview, quick presets and Save; T2 adds two picker modes —
// Upload (custom image assets, auto-atlased into the sprite pipeline,
// persisted server-side, entitlement-checked) and Generate (prompt -> candidate
// seeded from the layer catalog, deterministic per member). Standalone —
// styled inline so it never depends on HUD/global css owned by other streams.

import { useEffect, useRef, useState, type MutableRef } from 'preact/hooks';
import { AVATAR_PRESETS, layerOptions, optionOf } from './catalog';
import {
  loadAvatarAsset,
  onAvatarAssetLoad,
  renderAvatarLayer,
  renderAvatarSpec,
} from './sprites';
import {
  AVATAR_LAYERS,
  defaultAvatarSpec,
  isAssetOption,
  specAccent,
  type AvatarLayerId,
  type AvatarSpec,
} from './spec';
import {
  archiveAvatarAsset,
  generateAvatar,
  listAvatarAssets,
  uploadAvatarAsset,
  type AvatarAsset,
} from '../net/avatars';

const TAB_LABEL: Record<AvatarLayerId, string> = {
  body: 'Body',
  skin: 'Skin',
  hair: 'Hair',
  outfit: 'Outfit',
  accessory: 'Accessory',
};

type Mode = 'layers' | 'upload' | 'generate';

function useCanvasDraw(fn: (ctx: CanvasRenderingContext2D) => void, deps: unknown[]): MutableRef<HTMLCanvasElement | null> {
  const ref = useRef<HTMLCanvasElement | null>(null);
  useEffect(() => {
    const cv = ref.current;
    if (!cv) return;
    const ctx = cv.getContext('2d');
    if (!ctx) return;
    ctx.clearRect(0, 0, cv.width, cv.height);
    fn(ctx);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return ref;
}

const btnBase: Record<string, string> = {
  background: '#1c1626',
  color: '#e9e2f5',
  border: '1px solid #3a2f4d',
  borderRadius: '10px',
  padding: '6px 10px',
  font: 'inherit',
  cursor: 'pointer',
};

const inputBase: Record<string, string> = {
  background: '#1c1626',
  color: '#e9e2f5',
  border: '1px solid #3a2f4d',
  borderRadius: '10px',
  padding: '6px 10px',
  font: 'inherit',
};

export function AvatarBuilder({
  value,
  onChange,
}: {
  value: AvatarSpec;
  onChange(s: AvatarSpec): void;
}) {
  const [mode, setMode] = useState<Mode>('layers');
  const [tab, setTab] = useState<AvatarLayerId>('body');
  const [draft, setDraft] = useState<AvatarSpec>(() => value ?? defaultAvatarSpec());
  const draftRef = useRef(draft);
  draftRef.current = draft;

  // re-render previews whenever a custom asset image lands (or fails)
  const [rev, setRev] = useState(0);
  useEffect(() => onAvatarAssetLoad(() => setRev((r) => r + 1)), []);

  // T2 upload mode state
  const [assets, setAssets] = useState<AvatarAsset[]>([]);
  const [assetLayer, setAssetLayer] = useState<AvatarLayerId>('accessory');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState('');
  const fileRef = useRef<HTMLInputElement | null>(null);

  // T2 generate mode state
  const [prompt, setPrompt] = useState('');
  const [candidate, setCandidate] = useState<AvatarSpec | null>(null);
  const [genModel, setGenModel] = useState('');

  const refreshAssets = async () => {
    setAssets(await listAvatarAssets());
  };
  useEffect(() => {
    if (mode === 'upload') void refreshAssets();
    if (mode === 'generate') {
      setCandidate(null);
      setGenModel('');
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode]);

  const previewRef = useCanvasDraw(
    (ctx) => {
      const cv = ctx.canvas;
      ctx.clearRect(0, 0, cv.width, cv.height);
      ctx.drawImage(renderAvatarSpec(draftRef.current, { dir: 'down' }), 0, 0, cv.width, cv.height);
    },
    [draft, rev],
  );

  const selected = draft[tab];
  const accent = specAccent(draft);

  const pickFile = (f: File | undefined | null) => {
    if (!f) return;
    if (!/^image\/(png|gif|jpeg|webp)$/i.test(f.type)) {
      setNotice('Only PNG / GIF / JPEG / WebP images are accepted.');
      return;
    }
    if (f.size > 512 * 1024) {
      setNotice('Image exceeds 512 KiB.');
      return;
    }
    const rd = new FileReader();
    rd.onload = () => {
      const dataUrl = String(rd.result ?? '');
      const b64 = dataUrl.includes(',') ? dataUrl.slice(dataUrl.indexOf(',') + 1) : dataUrl;
      setNotice('');
      void (async () => {
        setBusy(true);
        try {
          const a = await uploadAvatarAsset(assetLayer, f.name.replace(/\.[^.]+$/, '') || 'custom', b64, f.type);
          if (a) {
            setNotice(`Uploaded "${a.name}" (${a.width}×${a.height}). Click its thumbnail to wear it.`);
            await refreshAssets();
          } else {
            setNotice('Upload failed — check the image (max 1024×1024).');
          }
        } finally {
          setBusy(false);
        }
      })();
    };
    rd.readAsDataURL(f);
  };

  const removeAsset = async (id: string) => {
    const r = await archiveAvatarAsset(id);
    setNotice(r.ok ? 'Archived.' : `Archive blocked: ${r.error ?? 'unknown error'}`);
    // drop the option from the draft if it was worn
    const prefix = `asset:${id}`;
    const nd = { ...draftRef.current };
    let changed = false;
    for (const l of AVATAR_LAYERS) {
      if (nd[l] === prefix) {
        nd[l] = defaultAvatarSpec()[l];
        changed = true;
      }
    }
    if (changed) setDraft(nd);
    await refreshAssets();
  };

  const runGenerate = async () => {
    setBusy(true);
    try {
      const g = await generateAvatar(prompt.trim() || 'a friendly traveler');
      if (g) {
        setCandidate(g.spec);
        setGenModel(g.model);
        setNotice('');
      } else {
        setNotice('Generation failed — try again.');
      }
    } finally {
      setBusy(false);
    }
  };

  const modeBtn = (m: Mode, label: string) => (
    <button
      type="button"
      role="tab"
      aria-selected={mode === m}
      style={{
        ...btnBase,
        background: mode === m ? accent : '#1c1626',
        color: mode === m ? '#140f1c' : '#e9e2f5',
        fontWeight: mode === m ? 700 : 400,
      }}
      onClick={() => setMode(m)}
    >
      {label}
    </button>
  );

  return (
    <div
      class="avatar-builder"
      style={{
        background: '#140f1c',
        border: '1px solid #3a2f4d',
        borderRadius: '14px',
        padding: '12px',
        margin: '10px 0',
      }}
    >
      {/* mode tabs: Layers | Upload | Generate */}
      <div role="tablist" aria-label="Avatar picker modes" style={{ display: 'flex', gap: '6px', marginBottom: '10px' }}>
        {modeBtn('layers', 'Layers')}
        {modeBtn('upload', 'Upload')}
        {modeBtn('generate', 'Generate')}
      </div>

      {mode === 'layers' && (
        <div class="ab-row" style={{ display: 'flex', gap: '14px', alignItems: 'flex-start' }}>
          {/* live preview */}
          <canvas
            ref={previewRef}
            class="ab-preview"
            width={96}
            height={96}
            style={{
              width: 96,
              height: 96,
              borderRadius: '10px',
              background: 'radial-gradient(circle at 50% 60%, #2b2140, #140f1c)',
              border: `2px solid ${accent}`,
              flex: 'none',
            }}
            aria-label="Avatar preview"
          />
          <div style={{ flex: 1, minWidth: 0 }}>
            {/* layer tabs */}
            <div role="tablist" aria-label="Avatar layers" style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
              {AVATAR_LAYERS.map((id) => (
                <button
                  key={id}
                  type="button"
                  role="tab"
                  aria-selected={tab === id}
                  style={{
                    ...btnBase,
                    background: tab === id ? accent : '#1c1626',
                    color: tab === id ? '#140f1c' : '#e9e2f5',
                    fontWeight: tab === id ? 700 : 400,
                  }}
                  onClick={() => setTab(id)}
                >
                  {TAB_LABEL[id]}
                </button>
              ))}
            </div>
            {/* option grid (NPC-only options hidden; worn custom asset shown) */}
            <div
              role="group"
              aria-label={`${TAB_LABEL[tab]} options`}
              class="ab-options"
              style={{ display: 'grid', gridTemplateColumns: 'repeat(5, 1fr)', gap: '6px', marginTop: '8px' }}
            >
              {layerOptions(tab).map((o) => {
                const sel = selected === o.id;
                return (
                  <button
                    key={o.id}
                    type="button"
                    title={o.label}
                    aria-label={o.label}
                    aria-pressed={sel}
                    style={{
                      ...btnBase,
                      padding: '4px',
                      border: sel ? `2px solid ${accent}` : '1px solid #3a2f4d',
                      background: '#1c1626',
                      display: 'flex',
                      flexDirection: 'column',
                      alignItems: 'center',
                      gap: '2px',
                    }}
                    onClick={() => setDraft({ ...draftRef.current, [tab]: o.id })}
                  >
                    <OptionSwatch layer={tab} optionId={o.id} />
                    <span style={{ fontSize: 10, color: sel ? accent : '#b9aed0', maxWidth: '100%', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                      {o.label}
                    </span>
                  </button>
                );
              })}
              {isAssetOption(selected) && (
                <button
                  type="button"
                  title={`Custom asset ${selected}`}
                  aria-label={`Custom asset ${selected}`}
                  aria-pressed
                  style={{
                    ...btnBase,
                    padding: '4px',
                    border: `2px solid ${accent}`,
                    background: '#1c1626',
                    display: 'flex',
                    flexDirection: 'column',
                    alignItems: 'center',
                    gap: '2px',
                  }}
                  onClick={() => setMode('upload')}
                >
                  <OptionSwatch layer={tab} optionId={selected} rev={rev} />
                  <span style={{ fontSize: 10, color: accent }}>Custom</span>
                </button>
              )}
            </div>
          </div>
        </div>
      )}

      {mode === 'upload' && (
        <div class="ab-upload" style={{ display: 'flex', flexDirection: 'column', gap: '10px' }}>
          <div style={{ display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' }}>
            <span style={{ fontSize: 11, color: '#8f83a8' }}>Upload to layer</span>
            {AVATAR_LAYERS.map((id) => (
              <button
                key={id}
                type="button"
                aria-pressed={assetLayer === id}
                style={{
                  ...btnBase,
                  padding: '4px 8px',
                  background: assetLayer === id ? accent : '#1c1626',
                  color: assetLayer === id ? '#140f1c' : '#e9e2f5',
                }}
                onClick={() => setAssetLayer(id)}
              >
                {TAB_LABEL[id]}
              </button>
            ))}
            <span style={{ flex: 1 }} />
            <button
              type="button"
              style={{ ...btnBase, background: accent, color: '#140f1c', fontWeight: 700, padding: '6px 14px' }}
              disabled={busy}
              onClick={() => fileRef.current?.click()}
            >
              {busy ? 'Uploading…' : 'Choose image'}
            </button>
            <input
              ref={fileRef}
              type="file"
              accept="image/png,image/gif,image/jpeg,image/webp"
              style={{ display: 'none' }}
              onChange={(e) => pickFile((e.target as HTMLInputElement).files?.[0])}
            />
          </div>
          <p style={{ fontSize: 11, color: '#8f83a8', margin: 0 }}>
            PNG / GIF / JPEG / WebP up to 512 KiB, 1024×1024. Your image is auto-atlased into the sprite
            pipeline; it renders identically for every viewer and only you can wear it (sharing via sets
            comes from the governance surface).
          </p>
          {notice && <p style={{ fontSize: 11, color: accent, margin: 0 }}>{notice}</p>}
          <div
            role="group"
            aria-label="My uploaded assets"
            style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(76px, 1fr))', gap: '8px' }}
          >
            {assets.length === 0 && !busy && (
              <span style={{ fontSize: 11, color: '#8f83a8' }}>No uploaded assets yet.</span>
            )}
            {assets.map((a) => {
              const worn = draftRef.current[a.layer as AvatarLayerId] === `asset:${a.id}`;
              return (
                <div
                  key={a.id}
                  style={{
                    border: worn ? `2px solid ${accent}` : '1px solid #3a2f4d',
                    borderRadius: '10px',
                    padding: '6px',
                    background: '#1c1626',
                    display: 'flex',
                    flexDirection: 'column',
                    alignItems: 'center',
                    gap: '4px',
                  }}
                >
                  <button
                    type="button"
                    title={`Wear ${a.name} as ${TAB_LABEL[a.layer as AvatarLayerId]}`}
                    aria-label={`Wear ${a.name}`}
                    style={{ ...btnBase, padding: 0, lineHeight: 0, background: 'none', border: 'none' }}
                    onClick={() => {
                      setDraft({ ...draftRef.current, [a.layer as AvatarLayerId]: `asset:${a.id}` });
                      setNotice(`Wearing "${a.name}" as ${TAB_LABEL[a.layer as AvatarLayerId]}.`);
                    }}
                  >
                    <AssetThumb id={a.id} rev={rev} />
                  </button>
                  <span style={{ fontSize: 10, color: '#b9aed0', maxWidth: '100%', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={a.name}>
                    {a.name}
                  </span>
                  <span style={{ fontSize: 9, color: '#8f83a8' }}>
                    {TAB_LABEL[a.layer as AvatarLayerId]} · {worn ? 'worn' : `${a.width}×${a.height}`}
                  </span>
                  <button
                    type="button"
                    style={{ ...btnBase, padding: '2px 8px', fontSize: 11 }}
                    onClick={() => void removeAsset(a.id)}
                  >
                    Remove
                  </button>
                </div>
              );
            })}
          </div>
        </div>
      )}

      {mode === 'generate' && (
        <div class="ab-generate" style={{ display: 'flex', flexDirection: 'column', gap: '10px' }}>
          <div style={{ display: 'flex', gap: '8px' }}>
            <input
              type="text"
              value={prompt}
              placeholder="Describe a look — e.g. a friendly pirate in a cape"
              maxLength={500}
              style={{ ...inputBase, flex: 1 }}
              onInput={(e) => setPrompt((e.target as HTMLInputElement).value)}
              enterkeyhint="go"
            />
            <button
              type="button"
              style={{ ...btnBase, background: accent, color: '#140f1c', fontWeight: 700 }}
              disabled={busy}
              onClick={() => void runGenerate()}
            >
              {busy ? 'Generating…' : 'Generate'}
            </button>
          </div>
          <p style={{ fontSize: 11, color: '#8f83a8', margin: 0 }}>
            Deterministic catalog sampler: your prompt + member id seed the pick across your entitled
            layer catalog — the same prompt always gives you the same look.
          </p>
          {candidate && (
            <div style={{ display: 'flex', gap: '12px', alignItems: 'center' }}>
              <canvas
                width={64}
                height={64}
                style={{ width: 64, height: 64, borderRadius: 8, border: `2px solid ${accent}`, background: '#0d0a14' }}
                ref={(el) => {
                  if (el && candidate && !el.dataset.drawn) {
                    el.dataset.drawn = '1';
                    el.getContext('2d')?.drawImage(renderAvatarSpec(candidate, { dir: 'down' }), 0, 0, 64, 64);
                  }
                }}
                aria-label="Generated candidate preview"
              />
              <div style={{ display: 'flex', flexDirection: 'column', gap: '6px' }}>
                <span style={{ fontSize: 11, color: '#8f83a8' }}>{genModel}</span>
                <button
                  type="button"
                  style={{ ...btnBase, background: accent, color: '#140f1c', fontWeight: 700, alignSelf: 'flex-start' }}
                  onClick={() => {
                    if (candidate) {
                      setDraft(candidate);
                      setMode('layers');
                    }
                  }}
                >
                  Use this look
                </button>
              </div>
            </div>
          )}
          {notice && <p style={{ fontSize: 11, color: accent, margin: 0 }}>{notice}</p>}
        </div>
      )}

      {/* presets + save (shared across modes) */}
      <div class="ab-presets" style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '10px' }}>
        <span style={{ fontSize: 11, color: '#8f83a8' }}>Presets</span>
        {AVATAR_PRESETS.map((p, i) => (
          <button
            key={i}
            type="button"
            title={`Preset ${i + 1}`}
            aria-label={`Preset ${i + 1}`}
            style={{ ...btnBase, padding: 2, lineHeight: 0 }}
            onClick={() => setDraft(p)}
          >
            <canvas
              width={28}
              height={28}
              style={{ width: 28, height: 28, borderRadius: 6, display: 'block' }}
              ref={(el) => {
                if (el && !el.dataset.drawn) {
                  el.dataset.drawn = '1';
                  el.getContext('2d')?.drawImage(renderAvatarSpec(p, { dir: 'down' }), 0, 0, 28, 28);
                }
              }}
            />
          </button>
        ))}
        <span style={{ flex: 1 }} />
        <button
          type="button"
          style={{
            ...btnBase,
            background: accent,
            color: '#140f1c',
            fontWeight: 700,
            padding: '7px 18px',
          }}
          onClick={() => onChange(draftRef.current)}
        >
          Save look
        </button>
      </div>
    </div>
  );
}

function OptionSwatch({ layer, optionId, rev = 0 }: { layer: AvatarLayerId; optionId: string; rev?: number }) {
  const ref = useCanvasDraw(
    (ctx) => {
      const cv = ctx.canvas;
      ctx.clearRect(0, 0, cv.width, cv.height);
      ctx.drawImage(renderAvatarLayer(layer, optionId, 64), 0, 0, cv.width, cv.height);
    },
    [layer, optionId, rev],
  );
  return <canvas ref={ref} width={36} height={36} style={{ width: 36, height: 36, borderRadius: 6, display: 'block' }} />;
}

/** Uploaded-asset thumbnail (kicks off the cached fetch; re-draws on load). */
function AssetThumb({ id, rev }: { id: string; rev: number }) {
  const ref = useCanvasDraw(
    (ctx) => {
      const cv = ctx.canvas;
      ctx.clearRect(0, 0, cv.width, cv.height);
      ctx.drawImage(renderAvatarLayer('accessory', `asset:${id}`, 64), 0, 0, cv.width, cv.height);
    },
    [id, rev],
  );
  void loadAvatarAsset(id); // ensure the fetch starts even before first draw
  return <canvas ref={ref} width={44} height={44} style={{ width: 44, height: 44, borderRadius: 6, display: 'block' }} />;
}

/** Small summary line: shows the current layer picks (used by JoinScreen). */
export function AvatarSummary({ spec }: { spec: AvatarSpec }) {
  const parts = AVATAR_LAYERS.map((id) => {
    const o = optionOf(id, spec[id]);
    return o ? `${o.label}` : spec[id];
  });
  return (
    <div style={{ fontSize: 11, color: '#8f83a8', marginTop: 2 }}>
      {parts.join(' · ')}
    </div>
  );
}
