// /worlds — card-based directory of published user worlds (gravity-ranked by
// the server). New World → blank canvas flow. Searchable via ?q=.

import { useEffect, useRef, useState } from 'preact/hooks';
import type { WorldEntry } from '../net/api';

function fmtHeadcount(n: number | undefined): string {
  const v = n ?? 0;
  return v === 1 ? '1 here' : `${v} here`;
}

function fmtGravity(g: WorldEntry['gravity']): string {
  const v = g?.gravity ?? 0;
  return v >= 1000 ? `${(v / 1000).toFixed(1)}k` : v >= 100 ? `${Math.round(v)}` : v.toFixed(1);
}

export function WorldsDirectory({
  open,
  worlds,
  loading,
  creating,
  currentId,
  onBack,
  onJoin,
  onCreate,
  onSearch,
}: {
  open: boolean;
  worlds: WorldEntry[];
  loading: boolean;
  creating: boolean;
  currentId: string | null;
  onBack(): void;
  onJoin(id: string): void;
  onCreate(name: string, template?: string): void;
  onSearch(q: string): void;
}) {
  const [query, setQuery] = useState('');
  const [showNew, setShowNew] = useState(false);
  const [name, setName] = useState('');
  const [template, setTemplate] = useState('empty_lot');
  const searchTimer = useRef<number | null>(null);

  useEffect(() => {
    if (searchTimer.current) window.clearTimeout(searchTimer.current);
    searchTimer.current = window.setTimeout(() => onSearch(query), 250);
    return () => {
      if (searchTimer.current) window.clearTimeout(searchTimer.current);
    };
  }, [query, onSearch]);

  // reset the new-world form each time the modal opens
  useEffect(() => {
    if (showNew) {
      setName('');
      setTemplate('empty_lot');
      const el = document.getElementById('new-world-name');
      if (el) window.setTimeout(() => el.focus(), 50);
    }
  }, [showNew]);

  if (!open) return null;

  const submit = (e: Event) => {
    e.preventDefault();
    const n = name.trim();
    if (!n) return;
    setShowNew(false);
    onCreate(n, template);
  };

  /** World-creation templates (server-seeded — no random walls). */
  const TEMPLATES: { id: string; label: string; desc: string; icon: string }[] = [
    { id: 'empty_lot', label: 'Empty Lot', desc: 'open field · paint from scratch', icon: '🌱' },
    { id: 'cozy_room', label: 'Cozy Room', desc: 'walled room · door + fireplace', icon: '🛋️' },
    { id: 'plaza', label: 'Plaza', desc: 'stone square · fountain center', icon: '⛲' },
  ];

  return (
    <div class="directory" role="region" aria-label="Worlds directory">
      <div class="directory-header">
        <button class="icon-btn" onClick={onBack} aria-label="Back to town-square">
          ←
        </button>
        <h1 class="directory-title">Worlds</h1>
        <button class="new-world-btn" onClick={() => setShowNew(true)} disabled={creating}>
          {creating ? '…' : '+ New World'}
        </button>
      </div>
      <div class="directory-search">
        <input
          value={query}
          onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
          placeholder="Search worlds…"
          aria-label="Search worlds"
        />
      </div>

      <div class="directory-grid">
        {worlds.map((w) => {
          const cur = w.id === currentId;
          return (
            <div key={w.id} class={`world-card${w.is_showcase ? ' showcase' : ''}${cur ? ' current' : ''}`}>
              <div class="world-thumb" aria-hidden="true">
                {w.thumbnail ? (
                  <img class="world-thumb-img" src={w.thumbnail} alt="" loading="lazy" />
                ) : (
                  <>
                    <span class="world-thumb-letter">{w.name.slice(0, 1).toUpperCase()}</span>
                    <span class="world-thumb-dots" />
                  </>
                )}
              </div>
              <div class="world-card-body">
                <div class="world-card-name" title={w.name}>
                  {w.name}
                  {w.is_showcase && <span class="badge showcase">showcase</span>}
                  {cur && <span class="badge here">you are here</span>}
                </div>
                <div class="world-card-meta">
                  <span>by {w.owner?.name || 'someone'}</span>
                  <span class="dot-sep">·</span>
                  <span>{fmtHeadcount(w.headcount)}</span>
                  {w.gravity && (
                    <>
                      <span class="dot-sep">·</span>
                      <span class="gravity" title="Gravity (love × reach × momentum)">
                        ⚡ {fmtGravity(w.gravity)}
                      </span>
                    </>
                  )}
                </div>
                <button class="world-enter" onClick={() => onJoin(w.id)}>
                  Enter
                </button>
              </div>
            </div>
          );
        })}
      </div>

      {!loading && worlds.length === 0 && (
        <div class="directory-empty">
          <p>{query ? `No worlds match “${query}”.` : 'No worlds yet — be the first!'}</p>
          {!query && (
            <button class="new-world-btn big" onClick={() => setShowNew(true)}>
              + Create your world
            </button>
          )}
        </div>
      )}
      {loading && worlds.length === 0 && (
        <div class="directory-empty">
          <div class="spinner" />
          <p>Loading worlds…</p>
        </div>
      )}

      {showNew && (
        <div class="modal-wrap" onClick={() => setShowNew(false)} role="dialog" aria-modal="true" aria-label="New world">
          <form class="modal" onSubmit={submit} onClick={(e) => e.stopPropagation()}>
            <h2>New World</h2>
            <p class="modal-sub">Pick a template, then paint, drop a portal back to town-square, and publish — under a minute.</p>
            <div class="template-row" role="radiogroup" aria-label="World template">
              {TEMPLATES.map((t) => (
                <button
                  type="button"
                  key={t.id}
                  role="radio"
                  aria-checked={template === t.id}
                  class={`template-card${template === t.id ? ' active' : ''}`}
                  onClick={() => setTemplate(t.id)}
                >
                  <span class="template-icon">{t.icon}</span>
                  <span class="template-label">{t.label}</span>
                  <span class="template-desc">{t.desc}</span>
                </button>
              ))}
            </div>
            <input
              id="new-world-name"
              class="modal-input"
              value={name}
              onInput={(e) => setName((e.target as HTMLInputElement).value)}
              placeholder="Name your world"
              maxLength={40}
              enterkeyhint="go"
            />
            <div class="modal-actions">
              <button type="button" class="tool-btn" onClick={() => setShowNew(false)}>
                Cancel
              </button>
              <button type="submit" class="tool-btn publish" disabled={!name.trim()}>
                Create & Enter
              </button>
            </div>
          </form>
        </div>
      )}
    </div>
  );
}
