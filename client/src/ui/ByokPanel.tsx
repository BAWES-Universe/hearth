import { useState } from 'preact/hooks';

/** BYOK panel — paste your own OpenRouter key; the app shows what it brought.
 *  Ox-decided: key lives in localStorage, server stores only a fingerprint. */
export interface ByokStatus {
  ok: boolean;
  hasKey?: boolean;
  fp?: string;
  model?: string;
  validatedAt?: number;
  quota?: { limit: number; used: number; isFreeTier: boolean };
  error?: string;
}

export interface ByokContribution {
  hasKey: boolean;
  fp?: string;
  model?: string;
  validatedAt?: number;
  calls: number;
  tokensIn: number;
  tokensOut: number;
  tokensTotal: number;
  byFeature: Record<string, number>;
  lastUsed?: number;
  since?: number;
}

async function j<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, {
    credentials: 'include',
    headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  });
  return r.json() as Promise<T>;
}

export function ByokPanel({ open, onClose }: { open: boolean; onClose(): void }) {
  const [key, setKey] = useState('');
  const [status, setStatus] = useState<ByokStatus | null>(null);
  const [contrib, setContrib] = useState<ByokContribution | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const refresh = async () => {
    const [s, c] = await Promise.all([
      j<ByokStatus>('/api/byok/status'),
      j<{ ok: boolean; contribution?: ByokContribution }>('/api/byok/contribution'),
    ]);
    setStatus(s);
    setContrib(c.contribution ?? null);
  };

  if (open && !status) refresh();

  const save = async () => {
    if (!key.startsWith('sk-or-')) { setErr('That doesn’t look like an OpenRouter key (sk-or-…).'); return; }
    setBusy(true); setErr('');
    try {
      const s = await j<ByokStatus>('/api/byok', {
        method: 'POST',
        body: JSON.stringify({ key, model: 'stealth/ox-alpha' }),
      });
      if (!s.ok) { setErr(s.error ?? 'validation failed'); setBusy(false); return; }
      setKey(''); // never keep the key in the input after vaulting
      setStatus(s);
      const c = await j<{ ok: boolean; contribution?: ByokContribution }>('/api/byok/contribution');
      setContrib(c.contribution ?? null);
    } catch (e) {
      setErr(String(e));
    }
    setBusy(false);
  };

  const remove = async () => {
    setBusy(true);
    try {
      localStorage.removeItem('hearth.byok.v1');
      await j('/api/byok', { method: 'DELETE' });
      setStatus(null); setContrib(null);
    } catch (e) { setErr(String(e)); }
    setBusy(false);
  };

  if (!open) return null;

  const active = status?.hasKey;
  const featureNames: Record<string, string> = { mason: 'Mason builds', soul: 'NPC souls', dream: 'Dream Rooms', 'byok.use': 'AI actions' };

  return (
    <div class="sheet-overlay" onClick={onClose}>
      <div class="sheet" onClick={(e) => e.stopPropagation()}>
        <div class="sheet-grabber" />
        <h3 style="margin:0 0 4px;font-size:15px">🔑 Hearth AI — bring your own key</h3>
        <p style="margin:0 0 12px;color:#aaa;font-size:12px;line-height:1.5">
          Paste your OpenRouter key to power in-world AI (Mason, souls, Dream Rooms).
          The key lives in <em>your browser</em> — the server stores only a fingerprint, never the key.
        </p>

        {!active && (
          <>
            <input
              type="password" autocomplete="off" placeholder="sk-or-v1-…"
              value={key}
              onInput={(e) => setKey((e.target as HTMLInputElement).value)}
              style="width:100%;box-sizing:border-box;padding:10px;border-radius:8px;border:1px solid #444;background:#222;color:#eee;font-size:13px"
            />
            {err && <p style="color:#e77;font-size:12px;margin:6px 0 0">{err}</p>}
            <button
              onClick={save} disabled={busy || !key}
              style="width:100%;margin-top:8px;padding:10px;border-radius:8px;border:none;background:#4a7;color:#111;font-weight:bold;cursor:pointer"
            >
              {busy ? 'Validating…' : 'Validate & enable'}
            </button>
            <p style="color:#888;font-size:11px;margin:10px 0 0">
              No key? Everything else still works — AI features just show this prompt. Get a free key at openrouter.ai.
            </p>
          </>
        )}

        {active && status && (
          <>
            <div style="display:flex;align-items:center;gap:8px;padding:10px;border-radius:8px;background:#13281c;border:1px solid #2a5a3a">
              <span style="width:8px;height:8px;border-radius:50%;background:#4a7;display:inline-block" />
              <span style="font-size:13px;font-weight:bold;color:#8d8">Active</span>
              <span style="font-size:12px;color:#aaa">· {status.model}</span>
              <span style="font-size:11px;color:#888;margin-left:auto">fp {status.fp}</span>
            </div>

            {contrib && contrib.calls > 0 && (
              <div style="margin-top:12px;padding:10px;border-radius:8px;background:#1a1a1a;border:1px solid #333">
                <h4 style="margin:0 0 8px;font-size:13px;color:#eee">✨ What your key brought (30 days)</h4>
                <div style="display:grid;grid-template-columns:1fr 1fr;gap:6px;font-size:12px">
                  <div style="color:#aaa">AI calls</div><div style="text-align:right;color:#eee;font-weight:bold">{contrib.calls}</div>
                  <div style="color:#aaa">Tokens (in+out)</div><div style="text-align:right;color:#eee;font-weight:bold">{contrib.tokensTotal.toLocaleString()}</div>
                  <div style="color:#aaa">Last used</div>
                  <div style="text-align:right;color:#eee">{contrib.lastUsed ? new Date(contrib.lastUsed * 1000).toLocaleDateString() : '—'}</div>
                </div>
                {Object.keys(contrib.byFeature).length > 0 && (
                  <div style="margin-top:8px;font-size:12px">
                    <div style="color:#aaa;margin-bottom:4px">Powered</div>
                    {Object.entries(contrib.byFeature).map(([f, n]) => (
                      <div key={f} style="display:flex;justify-content:space-between;color:#ccc">
                        <span>{featureNames[f] ?? f}</span><span style="font-weight:bold;color:#eee">{n}×</span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            )}
            {contrib && contrib.calls === 0 && (
              <p style="color:#888;font-size:12px;margin-top:10px">No AI calls yet — build something with Mason or a Dream Room and watch the numbers appear here.</p>
            )}

            <button
              onClick={remove} disabled={busy}
              style="width:100%;margin-top:12px;padding:10px;border-radius:8px;border:1px solid #733;background:#2a1212;color:#e88;font-weight:bold;cursor:pointer"
            >
              Remove key
            </button>
          </>
        )}
      </div>
    </div>
  );
}
