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
  ownerName,
  role,
  canEdit,
  friendRequests,
  onOpenChat,
  onOpenWorlds,
  onOpenFriends,
  onOpenByok,
}: {
  status: NetStatus;
  unread: number;
  space: string;
  ownerName?: string;
  role?: 'owner' | 'editor' | 'viewer';
  canEdit?: boolean;
  friendRequests: number;
  onOpenChat(): void;
  onOpenWorlds(): void;
  onOpenFriends(): void;
  onOpenByok(): void;
}) {
  // Showcase worlds are co-editable by anyone with a session, so the role
  // badge reflects the effective permission (canEdit) over the raw role.
  const roleLabel = canEdit ? (role === 'owner' ? 'owner' : 'co-builder') : 'viewer';
  return (
    <>
      <div class="status-pill">
        <span class={`dot ${status}`} />
        {STATUS_LABEL[status]}
      </div>
      <div class="space-tag" title="Space">
        {space}
      </div>
      <div class="world-owner-chip" title="Who owns this world">
        <span class="owner-role">{roleLabel === 'viewer' ? '👁' : roleLabel === 'owner' ? '👑' : '🔧'}</span>
        <span class="owner-name">{ownerName && ownerName !== 'guest' ? ownerName : space}</span>
        <span class={`owner-role-label ${roleLabel}`}>{roleLabel}</span>
      </div>
      <button class="worlds-btn" onClick={onOpenWorlds} aria-label="Browse worlds">
        <svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <circle cx="12" cy="12" r="9" />
          <path d="M3 12h18M12 3a15 15 0 0 1 0 18 15 15 0 0 1 0-18z" />
        </svg>
        <span class="worlds-btn-label">Worlds</span>
      </button>
      <button class="fab" onClick={onOpenByok} aria-label="AI key & contribution" title="Your key, what it brought">
        <svg viewBox="0 0 24 24" width="22" height="22" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M12 2 2 7l10 5 10-5-10-5zM2 17l10 5 10-5M2 12l10 5 10-5" />
        </svg>
      </button>
      <button class="fab friends-fab" onClick={onOpenFriends} aria-label="Friends & who's here">
        <svg viewBox="0 0 24 24" width="24" height="24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
          <circle cx="9" cy="7" r="4" />
          <path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75" />
        </svg>
        {friendRequests > 0 && <span class="fab-badge">{friendRequests > 99 ? '99+' : friendRequests}</span>}
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
