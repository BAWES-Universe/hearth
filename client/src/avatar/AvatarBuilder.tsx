// Layered avatar builder (T1): 5 layer tabs with option swatches, a live
// composite preview, quick presets and Save. Standalone — styled inline so it
// never depends on HUD/global css owned by other streams.

import { useEffect, useRef, useState, type MutableRef } from 'preact/hooks';
import { AVATAR_PRESETS, layerOptions, optionOf } from './catalog';
import { renderAvatarLayer, renderAvatarSpec } from './sprites';
import {
  AVATAR_LAYERS,
  defaultAvatarSpec,
  specAccent,
  type AvatarLayerId,
  type AvatarSpec,
} from './spec';

const TAB_LABEL: Record<AvatarLayerId, string> = {
  body: 'Body',
  skin: 'Skin',
  hair: 'Hair',
  outfit: 'Outfit',
  accessory: 'Accessory',
};

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

export function AvatarBuilder({
  value,
  onChange,
}: {
  value: AvatarSpec;
  onChange(s: AvatarSpec): void;
}) {
  const [tab, setTab] = useState<AvatarLayerId>('body');
  const [draft, setDraft] = useState<AvatarSpec>(() => value ?? defaultAvatarSpec());
  const draftRef = useRef(draft);
  draftRef.current = draft;

  const previewRef = useCanvasDraw(
    (ctx) => {
      const cv = ctx.canvas;
      ctx.clearRect(0, 0, cv.width, cv.height);
      ctx.drawImage(renderAvatarSpec(draftRef.current, { dir: 'down' }), 0, 0, cv.width, cv.height);
    },
    [draft],
  );

  const selected = draft[tab];
  const accent = specAccent(draft);

  return (
    <div
      style={{
        background: '#140f1c',
        border: '1px solid #3a2f4d',
        borderRadius: '14px',
        padding: '12px',
        margin: '10px 0',
      }}
    >
      <div style={{ display: 'flex', gap: '14px', alignItems: 'flex-start' }}>
        {/* live preview */}
        <canvas
          ref={previewRef}
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
          {/* option grid (NPC-only options hidden) */}
          <div role="group" aria-label={`${TAB_LABEL[tab]} options`} style={{ display: 'grid', gridTemplateColumns: 'repeat(5, 1fr)', gap: '6px', marginTop: '8px' }}>
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
          </div>
        </div>
      </div>
      {/* presets + save */}
      <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginTop: '10px' }}>
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

function OptionSwatch({ layer, optionId }: { layer: AvatarLayerId; optionId: string }) {
  const ref = useCanvasDraw(
    (ctx) => {
      const cv = ctx.canvas;
      ctx.clearRect(0, 0, cv.width, cv.height);
      ctx.drawImage(renderAvatarLayer(layer, optionId, 64), 0, 0, cv.width, cv.height);
    },
    [layer, optionId],
  );
  return <canvas ref={ref} width={36} height={36} style={{ width: 36, height: 36, borderRadius: 6, display: 'block' }} />;
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
