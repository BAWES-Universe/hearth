// Runtime config: WS endpoint, API base, space id. All same-origin by default
// (Go server embeds client dist). Overridable via ?ws= or localStorage.

const q = new URLSearchParams(window.location.search);

export const SPACE_ID: string =
  q.get('space') || localStorage.getItem('hearth:space') || 'town-square';

export function wsUrl(): string {
  const override = q.get('ws') || localStorage.getItem('hearth:ws');
  if (override) return override;
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${window.location.host}/ws`;
}

export function apiBase(): string {
  return '';
}
