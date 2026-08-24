// Typed client for the S9 admin REST API (/api/admin/*).
// The operator API key is kept in localStorage and sent as X-API-Key on
// every request; a 401 clears it and asks again.

const KEY_STORAGE = 'hearth_admin_key';

export interface Overview {
  worlds: number;
  worldsPublished: number;
  members: number;
  operators: number;
  auditEvents: number;
  clientsLive: number;
  botsLive: number;
}

export interface AdminWorld {
  id: string;
  name: string;
  owner: { id: string; name: string };
  isPublished: boolean;
  isShowcase: boolean;
  publishedAt: string | null;
  createdAt: string;
  headcount: number;
  gravity: { love: number; reach: number; momentum: number; gravity: number };
}

export interface AdminMember {
  id: string;
  name: string;
  role: 'operator' | 'member';
  createdAt: string;
  lastSeen: string | null;
  online: boolean;
}

export interface AuditEvent {
  id: number;
  worldId: string;
  actor: string;
  role: string;
  kind: string;
  action: string;
  target: string;
  diff: string;
  ip: string;
  ts: string;
}

export interface ServiceToken {
  id: string;
  name: string;
  capabilities: string[];
  createdAt: string;
  createdBy: string;
  lastUsed: string | null;
}

export function getApiKey(): string {
  let key = localStorage.getItem(KEY_STORAGE) ?? '';
  if (!key) {
    key = window.prompt('Enter the Hearth admin API key:') ?? '';
    if (key) localStorage.setItem(KEY_STORAGE, key);
  }
  return key;
}

export function clearApiKey(): void {
  localStorage.removeItem(KEY_STORAGE);
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const key = getApiKey();
  if (!key) throw new Error('admin API key required');
  const res = await fetch(path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      'X-API-Key': key,
      ...(init.headers ?? {}),
    },
  });
  if (res.status === 401) {
    clearApiKey();
    throw new Error('unauthorized — re-enter the admin API key');
  }
  const data = (await res.json().catch(() => ({}))) as { error?: string };
  if (!res.ok) throw new Error(data.error ?? `HTTP ${res.status}`);
  return data as T;
}

export const adminApi = {
  overview: () => request<{ ok: boolean; overview: Overview }>('/api/admin/overview'),
  worlds: () => request<{ ok: boolean; worlds: AdminWorld[] }>('/api/admin/worlds'),
  createWorld: (name: string, opts: { width?: number; height?: number; publish?: boolean } = {}) =>
    request<{ ok: boolean; id: string; name: string }>('/api/admin/worlds', {
      method: 'POST',
      body: JSON.stringify({
        name,
        width: opts.width ?? 32,
        height: opts.height ?? 32,
        publish: opts.publish ?? false,
      }),
    }),
  publishWorld: (id: string) =>
    request<{ ok: boolean }>(`/api/admin/worlds/${encodeURIComponent(id)}/publish`, { method: 'POST' }),
  deleteWorld: (id: string) =>
    request<{ ok: boolean }>(`/api/admin/worlds/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  members: () => request<{ ok: boolean; members: AdminMember[] }>('/api/admin/members'),
  audit: (kind = '') =>
    request<{ ok: boolean; events: AuditEvent[] }>(
      `/api/admin/audit${kind ? `?kind=${encodeURIComponent(kind)}` : ''}`,
    ),
  tokens: () => request<{ ok: boolean; tokens: ServiceToken[] }>('/api/admin/tokens'),
  createToken: (name: string, capabilities: string[]) =>
    request<{ ok: boolean; id: string; token: string }>('/api/admin/tokens', {
      method: 'POST',
      body: JSON.stringify({ name, capabilities }),
    }),
  deleteToken: (id: string) =>
    request<{ ok: boolean }>(`/api/admin/tokens/${encodeURIComponent(id)}`, { method: 'DELETE' }),
};

export function fmtTime(ts: string | null | undefined): string {
  if (!ts) return '—';
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? ts : d.toISOString().slice(0, 19).replace('T', ' ');
}
