import { useEffect, useState } from 'preact/hooks';
import { adminApi, fmtTime, type AdminWorld } from '../api';

export function WorldsPage() {
  const [worlds, setWorlds] = useState<AdminWorld[] | null>(null);
  const [error, setError] = useState('');
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);

  const load = () =>
    adminApi
      .worlds()
      .then((r) => setWorlds(r.worlds))
      .catch((e: Error) => setError(e.message));

  useEffect(() => {
    load();
  }, []);

  const create = async () => {
    const n = name.trim();
    if (!n || busy) return;
    setBusy(true);
    setError('');
    try {
      await adminApi.createWorld(n);
      setName('');
      await load();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  };

  const publish = async (id: string) => {
    if (busy) return;
    setBusy(true);
    setError('');
    try {
      await adminApi.publishWorld(id);
      await load();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  };

  const del = async (id: string, name_: string) => {
    if (busy) return;
    if (!window.confirm(`Delete world "${name_}" (${id})? This cannot be undone.`)) return;
    setBusy(true);
    setError('');
    try {
      await adminApi.deleteWorld(id);
      await load();
    } catch (e) {
      setError((e as Error).message);
    }
    setBusy(false);
  };

  if (error) return <div class="admin-error">{error}</div>;
  if (!worlds) return <div class="admin-muted">Loading…</div>;

  return (
    <section>
      <h1>Worlds &amp; Rooms</h1>
      <div class="admin-row">
        <input
          class="admin-input"
          placeholder="New world name"
          value={name}
          onInput={(e) => setName((e.target as HTMLInputElement).value)}
        />
        <button class="admin-btn" disabled={busy || !name.trim()} onClick={create}>
          Create world
        </button>
      </div>
      <table class="admin-table">
        <thead>
          <tr>
            <th>World</th>
            <th>Owner</th>
            <th>Status</th>
            <th>Created</th>
            <th>Published</th>
            <th>Gravity</th>
            <th>Live</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {worlds.map((w) => (
            <tr key={w.id}>
              <td>
                <span class="admin-mono">{w.id}</span>
                <div>{w.name}</div>
              </td>
              <td>{w.owner.name || w.owner.id || '—'}</td>
              <td>
                <span class={w.isPublished ? 'admin-badge ok' : 'admin-badge'}>
                  {w.isPublished ? 'published' : 'draft'}
                </span>
                {w.isShowcase ? <span class="admin-badge accent">showcase</span> : null}
              </td>
              <td>{fmtTime(w.createdAt)}</td>
              <td>{fmtTime(w.publishedAt)}</td>
              <td>{w.gravity.gravity.toFixed(2)}</td>
              <td>{w.headcount}</td>
              <td class="admin-actions">
                {!w.isPublished ? (
                  <button class="admin-btn admin-btn-small" disabled={busy} onClick={() => publish(w.id)}>
                    Publish
                  </button>
                ) : null}
                <button
                  class="admin-btn admin-btn-danger admin-btn-small"
                  disabled={busy}
                  onClick={() => del(w.id, w.name)}
                >
                  Delete
                </button>
              </td>
            </tr>
          ))}
          {worlds.length === 0 ? (
            <tr>
              <td colSpan={8} class="admin-muted">
                No worlds yet.
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>
    </section>
  );
}
