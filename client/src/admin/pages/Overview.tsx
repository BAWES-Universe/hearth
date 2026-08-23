import { useEffect, useState } from 'preact/hooks';
import { adminApi, type Overview } from '../api';

export function OverviewPage() {
  const [data, setData] = useState<Overview | null>(null);
  const [error, setError] = useState('');

  useEffect(() => {
    adminApi
      .overview()
      .then((r) => setData(r.overview))
      .catch((e: Error) => setError(e.message));
  }, []);

  if (error) return <div class="admin-error">{error}</div>;
  if (!data) return <div class="admin-muted">Loading…</div>;

  const cards: { label: string; value: number }[] = [
    { label: 'Worlds', value: data.worlds },
    { label: 'Published', value: data.worldsPublished },
    { label: 'Members', value: data.members },
    { label: 'Operators', value: data.operators },
    { label: 'Online now', value: data.clientsLive },
    { label: 'Audit events', value: data.auditEvents },
  ];

  return (
    <section>
      <h1>Overview</h1>
      <div class="admin-cards">
        {cards.map((c) => (
          <div class="admin-card" key={c.label}>
            <div class="admin-card-value">{c.value}</div>
            <div class="admin-card-label">{c.label}</div>
          </div>
        ))}
      </div>
      <p class="admin-muted">
        Hearth admin console — worlds, members and the append-only audit log.
      </p>
    </section>
  );
}
