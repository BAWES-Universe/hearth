import { useEffect, useState } from 'preact/hooks';
import { adminApi, fmtTime, type AdminMember } from '../api';

export function MembersPage() {
  const [members, setMembers] = useState<AdminMember[] | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    adminApi
      .members()
      .then((r) => setMembers(r.members))
      .catch((e: Error) => setError(e.message));
  }, []);

  if (error) return <div class="admin-error">{error}</div>;
  if (!members) return <div class="admin-muted">Loading…</div>;

  return (
    <section>
      <h1>Members</h1>
      <table class="admin-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>ID</th>
            <th>Role</th>
            <th>Joined</th>
            <th>Last seen</th>
            <th>Online</th>
          </tr>
        </thead>
        <tbody>
          {members.map((m) => (
            <tr key={m.id}>
              <td>{m.name || '—'}</td>
              <td>
                <span class="admin-mono">{m.id}</span>
              </td>
              <td>
                <span class={m.role === 'operator' ? 'admin-badge accent' : 'admin-badge'}>
                  {m.role}
                </span>
              </td>
              <td>{fmtTime(m.createdAt)}</td>
              <td>{fmtTime(m.lastSeen)}</td>
              <td>{m.online ? <span class="admin-badge ok">online</span> : '—'}</td>
            </tr>
          ))}
          {members.length === 0 ? (
            <tr>
              <td colSpan={6} class="admin-muted">
                No members yet.
              </td>
            </tr>
          ) : null}
        </tbody>
      </table>
    </section>
  );
}
