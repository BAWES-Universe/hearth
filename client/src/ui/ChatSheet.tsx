import { useEffect, useRef, useState } from 'preact/hooks';
import { cssColor } from '../colors';

export interface ChatMessage {
  id: string;
  channel: 'proximity' | 'space' | 'global';
  from: string;
  text: string;
  ts: number;
  self?: boolean;
  pending?: boolean;
  system?: boolean;
}

interface Props {
  open: boolean;
  onClose(): void;
  channel: 'proximity' | 'space' | 'global';
  onChannel(c: 'proximity' | 'space' | 'global'): void;
  messages: ChatMessage[];
  onSend(text: string): void;
}

function fmtTime(ts: number): string {
  const d = new Date(ts);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

export function ChatSheet({ open, onClose, channel, onChannel, messages, onSend }: Props) {
  const [draft, setDraft] = useState('');
  const listRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = listRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, open]);

  if (!open) return null;
  const visible = messages.filter((m) => m.channel === channel);

  return (
    <div class="sheet-wrap" onClick={onClose} role="dialog" aria-label="Chat">
      <div class="sheet" onClick={(e) => e.stopPropagation()}>
        <div class="sheet-grabber" />
        <div class="sheet-tabs">
          {([
            ['proximity', 'Nearby'],
            ['space', 'Space'],
            ['global', 'All'],
          ] as const).map(([c, label]) => (
            <button
              key={c}
              class={`tab${channel === c ? ' active' : ''}`}
              onClick={() => onChannel(c)}
            >
              {label}
            </button>
          ))}
        </div>
        <div class="sheet-list" ref={listRef}>
          {visible.length === 0 && <div class="sheet-empty">Say hi 👋</div>}
          {visible.map((m) => (
            <div class={`msg${m.self ? ' self' : ''}${m.system ? ' system' : ''}`} key={m.id}>
              <span class="msg-name" style={m.self ? undefined : { color: cssColor(m.from) }}>
                {m.self ? 'you' : m.system ? 'hearth' : m.from}
              </span>
              <span class="msg-text">{m.text}</span>
              {m.pending && <span class="msg-pending">…</span>}
              <span class="msg-time">{fmtTime(m.ts)}</span>
            </div>
          ))}
        </div>
        <form
          class="sheet-input"
          onSubmit={(e) => {
            e.preventDefault();
            const t = draft.trim();
            if (!t) return;
            onSend(t);
            setDraft('');
          }}
        >
          <input
            value={draft}
            onInput={(e) => setDraft((e.target as HTMLInputElement).value)}
            placeholder={`Message ${channel === 'proximity' ? 'nearby' : channel === 'global' ? 'everyone' : 'the space'}…`}
            maxLength={2000}
            enterkeyhint="send"
            autocomplete="off"
          />
          <button type="submit" disabled={!draft.trim()}>
            Send
          </button>
        </form>
      </div>
    </div>
  );
}
