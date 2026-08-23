import { useEffect, useState } from 'preact/hooks';
import { adminApi, fmtTime, type AuditEvent } from '../api';

const KIND_FILTERS = ['', 'admin', 'world', 'presence', 'edit', 'chat'] as const;

export function AuditPage() {
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [kind, setKind] = useState<string>('');
  const [error, setError] = useState('');

  useEffect(() => {
    setEvents(null);
    adminApi
      .audit(kind)
      .then((r) => setEvents(r.events))
      .catch((e: Error) => setError(e.message));
  }, [kind]);

  if (error) return <div class="admin-error">{error}</div>;
  if (!events) return <div class="admin-muted">Loading…</div>;

  return (
    <section>
      <h1>Audit</h1>
      <div class="admin-row">
        {KIND_FILTERS.map((k) => (
          <button
            key={k}
            class={kind === k ? 'admin-btn admin-btn-small active' : 'admin-btn admin-btn-ghost admin-btn-small'}
            onClick={() => setKind(k)}
          >
            {k === '' ? 'all' : k}
          </button>
        ))}
      </div>
      <table class="admin-table">
        <thead>
          <tr>
            <th>Time</th>
            <th>Actor</th>
            <th>Role</th>
            <th>Kind</th>
            <th>Action</th>
            <th>Target</th>
            <th>Diff</th>
            <th>IP</th>
          </tr>
        </thead>
        <tbody>
          {events.map((e) => (
            <tr key={e.id}>
              <td>{fmtTime(e.ts)}</td>
              <td>{e.actor || '—'}</td>
              <td>{e.role || '—'}</td>
              <td>
                <span class="admin-badge">{e.kind}</span>
              </td>
              <td>
                <span class="admin-mono">{e.action}</span>
              </td>
              <td>
                <span class="admin-mono">{e.target || '—'}</span>
              </td>
              <td class="admin-diff">{e.diff || ''}</td>
              <td>{e.ip || '—'}</td>
            </tr>
          ))}
          {events.length === 0 ? (
            <tr>
              <td colSpan={8} class="admin-muted">
                No audit events{kind ? ` for kind "${kind}"` : ''}.
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>
    </section>
  );
}
