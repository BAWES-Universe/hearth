import { useState } from 'preact/hooks';
import { clearApiKey, getApiKey } from './api';
import { OverviewPage } from './pages/Overview';
import { WorldsPage } from './pages/Worlds';
import { MembersPage } from './pages/Members';
import { AuditPage } from './pages/Audit';

type Page = 'overview' | 'worlds' | 'members' | 'audit';

const NAV: { id: Page; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'worlds', label: 'Worlds & Rooms' },
  { id: 'members', label: 'Members' },
  { id: 'audit', label: 'Audit' },
];

// AdminApp is the embedded S9 admin console shell: sidebar nav + pages that
// read live data from /api/admin/* (API key via localStorage prompt).
export function AdminApp() {
  const [page, setPage] = useState<Page>('overview');
  const [key, setKey] = useState<string>(() => getApiKey());

  return (
    <div class="admin">
      <aside class="admin-side">
        <div class="admin-brand">
          Hearth <span>Admin</span>
        </div>
        <nav class="admin-nav">
          {NAV.map((n) => (
            <button
              key={n.id}
              class={page === n.id ? 'admin-nav-item active' : 'admin-nav-item'}
              onClick={() => setPage(n.id)}
            >
              {n.label}
            </button>
          ))}
        </nav>
        <div class="admin-side-foot">
          <div class="admin-key" title="API key in use">
            key: {key ? key.slice(0, 10) + '…' : 'none'}
          </div>
          <button
            class="admin-btn admin-btn-ghost"
            onClick={() => {
              clearApiKey();
              setKey('');
              window.location.reload();
            }}
          >
            Reset key
          </button>
        </div>
      </aside>
      <main class="admin-main">
        {page === 'overview' && <OverviewPage />}
        {page === 'worlds' && <WorldsPage />}
        {page === 'members' && <MembersPage />}
        {page === 'audit' && <AuditPage />}
      </main>
    </div>
  );
}
