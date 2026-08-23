import type { NetStatus } from '../net/ws';

const STATUS_LABEL: Record<NetStatus, string> = {
  connecting: 'Connecting…',
  online: 'Live',
  reconnecting: 'Reconnecting…',
  offline: 'Offline',
};

export function Hud({
  status,
  unread,
  space,
  onOpenChat,
  onOpenWorlds,
}: {
  status: NetStatus;
  unread: number;
  space: string;
  onOpenChat(): void;
  onOpenWorlds(): void;
}) {
  return (
    <>
      <div class="status-pill">
        <span class={`dot ${status}`} />
        {STATUS_LABEL[status]}
      </div>
      <div class="space-tag" title="Space">
        {space}
      </div>
      <button class="worlds-btn" onClick={onOpenWorlds} aria-label="Browse worlds">
        <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="9" />
          <path d="M3 12h18M12 3a15 15 0 0 1 0 18 15 15 0 0 1 0-18z" />
        </svg>
        <span class="worlds-btn-label">Worlds</span>
      </button>
      <button class="fab" onClick={onOpenChat} aria-label="Open chat">
        <svg viewBox="0 0 24 24" width="26" height="26" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
        </svg>
        {unread > 0 && <span class="fab-badge">{unread > 99 ? '99+' : unread}</span>}
      </button>
    </>
  );
}
