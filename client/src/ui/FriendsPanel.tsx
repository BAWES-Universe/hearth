// T2 social layer: friends panel — who's here, friends with live presence
// dots, incoming requests, add-by-name. Mobile-first bottom sheet matching
// the existing dark UI (ChatSheet pattern).

import { useEffect, useState } from 'preact/hooks';
import { fetchSpace } from '../net/api';
import { fetchActivity, type ActivityRow, type FriendEntry } from '../net/friends';

type Tab = 'here' | 'friends' | 'requests' | 'add';

interface HerePerson {
  id: string;
  name: string;
  bot: boolean;
}

interface Props {
  open: boolean;
  onClose(): void;
  friends: FriendEntry[];
  currentSpace: string;
  onAdd(id: string): void;
  onAccept(id: string): void;
  onDecline(id: string): void;
  onRemove(id: string): void;
}

function activityText(row: ActivityRow, nameOf: (id: string) => string): string {
  const who = () => {
    try {
      if (row.diff) {
        const d = JSON.parse(row.diff) as { name?: string };
        if (d.name) return d.name;
      }
    } catch {
      /* ignore */
    }
    return nameOf(row.actor ?? '') || 'someone';
  };
  switch (row.kind) {
    case 'friend':
      return row.action === 'accept' ? `${who()} is now friends with someone here` : `${who()} ${row.action}d a friend`;
    case 'presence':
      return row.action === 'join' ? `${who()} joined this space` : `${who()} left this space`;
    case 'publish':
      return `${who()} published a world`;
    case 'nav':
      return row.action === 'portal' ? `${who()} stepped through a portal` : `${who()} arrived`;
    case 'edit':
      return `${who()} painted`;
    case 'chat':
      return `${who()} chatted`;
    case 'create':
      return `${who()} created a world`;
    default:
      return `${who()} · ${row.kind ?? ''} ${row.action ?? ''}`;
  }
}

export function FriendsPanel({
  open,
  onClose,
  friends,
  currentSpace,
  onAdd,
  onAccept,
  onDecline,
  onRemove,
}: Props) {
  const [tab, setTab] = useState<Tab>('here');
  const [here, setHere] = useState<HerePerson[]>([]);
  const [activity, setActivity] = useState<ActivityRow[]>([]);
  const [query, setQuery] = useState('');
  const [results, setResults] = useState<{ id: string; name: string; online: boolean }[]>([]);
  const [searching, setSearching] = useState(false);

  const friendIds = new Set(friends.filter((f) => f.status === 'accepted').map((f) => f.friendId));
  const pendingIds = new Set(friends.filter((f) => f.status === 'pending').map((f) => f.friendId));
  const requests = friends.filter((f) => f.status === 'requested');
  const accepted = friends.filter((f) => f.status === 'accepted');

  const nameOf = (id: string) => friends.find((f) => f.friendId === id)?.name ?? '';

  // load who's-here + the space activity feed whenever the panel opens
  useEffect(() => {
    if (!open) return;
    setTab('here');
    setQuery('');
    setResults([]);
    void (async () => {
      const sp = await fetchSpace(currentSpace);
      const ents = sp && typeof sp === 'object' ? (sp as { entities?: HerePerson[] }).entities : null;
      setHere(Array.isArray(ents) ? ents : []);
    })();
    void fetchActivity(currentSpace, 12).then(setActivity);
  }, [open, currentSpace]);

  const doSearch = async (q: string) => {
    setQuery(q);
    if (!q.trim()) {
      setResults([]);
      return;
    }
    setSearching(true);
    const r = await (await import('../net/friends')).searchUsers(q);
    setResults(r);
    setSearching(false);
  };

  if (!open) return null;
  const humans = here.filter((p) => !p.bot);

  return (
    <div class="sheet-wrap" onClick={onClose} role="dialog" aria-label="Friends & who's here">
      <div class="sheet" onClick={(e) => e.stopPropagation()}>
        <div class="sheet-grabber" />
        <div class="sheet-tabs">
          {(
            [
              ['here', `Here · ${humans.length}`],
              ['friends', `Friends · ${accepted.length}`],
              ['requests', requests.length ? `Requests · ${requests.length}` : 'Requests'],
              ['add', 'Add'],
            ] as [Tab, string][]
          ).map(([t, label]) => (
            <button key={t} class={`tab${tab === t ? ' active' : ''}`} onClick={() => setTab(t)}>
              {label}
            </button>
          ))}
        </div>

        {tab === 'here' && (
          <div class="f-tab">
            {humans.length === 0 && <div class="sheet-empty">No one else here yet.</div>}
            <ul class="f-list">
              {humans.map((p) => (
                <li key={p.id} class="f-row">
                  <span class={`f-dot${friendIds.has(p.id) ? ' friend' : ''}`} />
                  <span class="f-name">
                    {p.name}
                    {friendIds.has(p.id) && <span class="f-friend-tag">friend</span>}
                  </span>
                </li>
              ))}
            </ul>
            {activity.length > 0 && (
              <>
                <div class="f-section-title">Recent here</div>
                <ul class="f-list">
                  {activity.map((a) => (
                    <li key={a.id ?? a.ts} class="f-row f-activity">
                      {activityText(a, nameOf)}
                    </li>
                  ))}
                </ul>
              </>
            )}
          </div>
        )}

        {tab === 'friends' && (
          <div class="f-tab">
            {accepted.length === 0 && <div class="sheet-empty">No friends yet — add someone by name.</div>}
            <ul class="f-list">
              {accepted.map((f) => (
                <li key={f.friendId} class="f-row">
                  <span class={`f-dot${f.online ? ' online' : ''}`} />
                  <span class="f-name">
                    {f.name}
                    <span class="f-sub">{f.online ? (f.space ? `in ${f.space}` : 'online') : 'offline'}</span>
                  </span>
                  <button class="f-action" onClick={() => onRemove(f.friendId)} aria-label={`Remove ${f.name}`}>
                    ✕
                  </button>
                </li>
              ))}
            </ul>
            {pendingIds.size > 0 && (
              <>
                <div class="f-section-title">Sent</div>
                <ul class="f-list">
                  {friends
                    .filter((f) => f.status === 'pending')
                    .map((f) => (
                      <li key={f.friendId} class="f-row">
                        <span class="f-dot pending" />
                        <span class="f-name">
                          {f.name}
                          <span class="f-sub">request sent</span>
                        </span>
                      </li>
                    ))}
                </ul>
              </>
            )}
          </div>
        )}

        {tab === 'requests' && (
          <div class="f-tab">
            {requests.length === 0 && <div class="sheet-empty">No pending requests.</div>}
            <ul class="f-list">
              {requests.map((f) => (
                <li key={f.friendId} class="f-row">
                  <span class="f-dot pending" />
                  <span class="f-name">{f.name}</span>
                  <button class="f-action primary" onClick={() => onAccept(f.friendId)}>
                    Accept
                  </button>
                  <button class="f-action" onClick={() => onDecline(f.friendId)}>
                    Decline
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}

        {tab === 'add' && (
          <div class="f-tab">
            <form
              class="sheet-input"
              onSubmit={(e) => {
                e.preventDefault();
                void doSearch(query);
              }}
            >
              <input
                value={query}
                onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
                placeholder="Find by name…"
                maxLength={40}
                enterkeyhint="search"
                autocomplete="off"
              />
              <button type="submit" disabled={!query.trim()}>
                Search
              </button>
            </form>
            {searching && <div class="sheet-empty">Searching…</div>}
            <ul class="f-list">
              {results.map((u) => {
                const isFriend = friendIds.has(u.id);
                const isPending = pendingIds.has(u.id);
                const isRequest = requests.some((r) => r.friendId === u.id);
                return (
                  <li key={u.id} class="f-row">
                    <span class={`f-dot${u.online ? ' online' : ''}`} />
                    <span class="f-name">
                      {u.name}
                      <span class="f-sub">{u.online ? 'online' : 'offline'}</span>
                    </span>
                    {isFriend ? (
                      <span class="f-friend-tag">friend</span>
                    ) : isRequest ? (
                      <button class="f-action primary" onClick={() => onAccept(u.id)}>
                        Accept
                      </button>
                    ) : isPending ? (
                      <span class="f-sub">sent</span>
                    ) : (
                      <button class="f-action primary" onClick={() => onAdd(u.id)}>
                        Add
                      </button>
                    )}
                  </li>
                );
              })}
            </ul>
          </div>
        )}
      </div>
    </div>
  );
}
