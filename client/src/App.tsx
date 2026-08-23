import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { fetchSpace } from './net/api';
import { SPACE_ID, wsUrl } from './net/config';
import { Net, type NetStatus } from './net/ws';
import { WorldRenderer } from './world/renderer';
import { ChatSheet, type ChatMessage } from './ui/ChatSheet';
import { Hud } from './ui/Hud';
import { JoinScreen } from './ui/JoinScreen';

type Phase = 'join' | 'loading' | 'world';

export function App() {
  const mountRef = useRef<HTMLDivElement>(null);
  const rendererRef = useRef<WorldRenderer | null>(null);
  const netRef = useRef<Net | null>(null);
  const selfIdRef = useRef('');
  const selfNameRef = useRef('');

  const [phase, setPhase] = useState<Phase>('join');
  const [status, setStatus] = useState<NetStatus>('connecting');
  const [spaceName, setSpaceName] = useState(SPACE_ID);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [channel, setChannel] = useState<'proximity' | 'space'>('proximity');
  const [sheetOpen, setSheetOpen] = useState(false);
  const [unread, setUnread] = useState(0);

  const sheetOpenRef = useRef(false);
  const openChat = useCallback(() => {
    sheetOpenRef.current = true;
    setSheetOpen(true);
    setUnread(0);
  }, []);
  const closeChat = useCallback(() => {
    sheetOpenRef.current = false;
    setSheetOpen(false);
  }, []);

  // boot the renderer once (canvas sits behind every phase)
  useEffect(() => {
    const mount = mountRef.current;
    if (!mount) return;
    const renderer = new WorldRenderer((m) => netRef.current?.sendMove(m.x, m.y, m.dir, m.seq));
    rendererRef.current = renderer;
    renderer.init(mount).catch((err) => console.error('[hearth] renderer init failed', err));
    return () => {
      renderer.destroy();
      rendererRef.current = null;
    };
  }, []);

  // visualViewport: keep layout inside the visible area when the keyboard opens
  useEffect(() => {
    const vv = window.visualViewport;
    const update = () => {
      const h = vv?.height ?? window.innerHeight;
      document.documentElement.style.setProperty('--vvh', `${h}px`);
    };
    update();
    vv?.addEventListener('resize', update);
    vv?.addEventListener('scroll', update);
    window.addEventListener('resize', update);
    return () => {
      vv?.removeEventListener('resize', update);
      vv?.removeEventListener('scroll', update);
      window.removeEventListener('resize', update);
    };
  }, []);

  const join = useCallback((name: string) => {
    const trimmed = name.trim() || 'guest';
    selfNameRef.current = trimmed;
    localStorage.setItem('hearth:name', trimmed);
    setPhase('loading');

    const net = new Net(wsUrl(), {
      onStatus: setStatus,
      onWelcome: (d) => {
        selfIdRef.current = d.selfId;
        const r = rendererRef.current;
        if (!r) return;
        if (d.world) r.setWorld(d.world);
        r.setSelf(d.selfId, trimmed);
        r.applyRoster(d.roster);
        if (d.space) setSpaceName(d.space);
        setPhase('world');
      },
      onState: (states) => rendererRef.current?.updateState(states),
      onChat: (d) => {
        const from = d.from ?? '?';
        const ch: 'proximity' | 'space' = d.channel === 'space' ? 'space' : 'proximity';
        setMessages((prev) => {
          if (from === selfIdRef.current) {
            // echo of our own optimistic message → mark delivered, don't duplicate
            const idx = prev.findIndex((m) => m.self && m.pending && m.channel === ch && m.text === d.text);
            if (idx >= 0) {
              const copy = prev.slice();
              copy[idx] = { ...copy[idx], pending: false };
              return copy;
            }
          }
          const msg: ChatMessage = {
            id: `${d.ts}-${from}-${d.seq ?? Math.random().toString(36).slice(2, 8)}`,
            channel: ch,
            from,
            text: d.text,
            ts: d.ts ?? Date.now(),
            self: from === selfIdRef.current,
          };
          return [...prev, msg];
        });
        if (from !== selfIdRef.current && !sheetOpenRef.current) {
          setUnread((u) => u + 1);
        }
      },
      onEdit: (d) => {
        if (typeof d.x === 'number' && typeof d.y === 'number' && typeof d.tileId === 'number') {
          rendererRef.current?.paintTile(d.x, d.y, d.tileId);
        }
      },
      onError: (d) => console.warn('[hearth] server:', d.code, d.msg),
      onBotMsg: (d) => {
        const text = d.text;
        if (!text) return;
        setMessages((prev) => [
          ...prev,
          {
            id: `bot-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`,
            channel: 'proximity',
            from: d.botId ?? 'hearth-bot',
            text,
            ts: Date.now(),
            system: true,
          },
        ]);
        if (!sheetOpenRef.current) setUnread((u) => u + 1);
      },
    });
    netRef.current = net;
    net.connect({ name: trimmed, lang: 'en', space: SPACE_ID, guest: true });

    // early world fetch → tiles render before welcome arrives
    fetchSpace(SPACE_ID).then((sp) => {
      const w = sp && typeof sp === 'object' ? (sp as Record<string, unknown>).world ?? sp : null;
      if (w) rendererRef.current?.setWorld(w);
    });
  }, []);

  const sendChat = useCallback(
    (text: string) => {
      const nonce = crypto.randomUUID();
      setMessages((prev) => [
        ...prev,
        {
          id: nonce,
          channel,
          from: selfNameRef.current,
          text,
          ts: Date.now(),
          self: true,
          pending: true,
        },
      ]);
      netRef.current?.sendChat(channel, text, nonce);
    },
    [channel],
  );

  // debug hook (used by integration tests / server team)
  useEffect(() => {
    (window as unknown as Record<string, unknown>).__hearth = {
      getLocal: () => rendererRef.current?.getLocal(),
      selfId: () => selfIdRef.current,
      status: () => status,
    };
  }, [status]);

  return (
    <div class="app">
      <div ref={mountRef} class="world-mount" />
      {phase === 'world' && (
        <Hud status={status} unread={unread} space={spaceName} onOpenChat={openChat} />
      )}
      {phase === 'loading' && (
        <div class="loading">
          <div class="spinner" />
          <p>Entering the Hearth…</p>
        </div>
      )}
      {phase === 'join' && (
        <JoinScreen onJoin={join} initial={localStorage.getItem('hearth:name') ?? ''} />
      )}
      <ChatSheet
        open={sheetOpen}
        onClose={closeChat}
        channel={channel}
        onChannel={setChannel}
        messages={messages}
        onSend={sendChat}
      />
    </div>
  );
}
