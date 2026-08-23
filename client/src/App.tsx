import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import {
  createWorld,
  fetchSpace,
  listWorlds,
  publishWorld,
  type WorldEntry,
} from './net/api';
import { resolveSpaceId, wsUrl } from './net/config';
import { Net, type NetStatus } from './net/ws';
import { WorldRenderer } from './world/renderer';
import { uuid, type EditMsg, type PortalMsg, type PortalPayload } from './net/protocol';
import { ChatSheet, type ChatMessage } from './ui/ChatSheet';
import { Hud } from './ui/Hud';
import { JoinScreen } from './ui/JoinScreen';
import { EditToolbar, type EditMode } from './ui/EditToolbar';
import { WorldsDirectory } from './ui/WorldsDirectory';
import { loadSpec, type AvatarSpec } from './avatar/spec';

type Phase = 'join' | 'loading' | 'world';
type View = 'world' | 'worlds';

/** Portal as carried in the world document / edit acks. */
interface PortalMarker {
  id: string;
  x: number;
  y: number;
  targetSpace: string;
  targetX: number;
  targetY: number;
}

/** One self-edit recorded for compensating-inverse undo (stack >= 50). */
interface UndoEntry {
  op: 'paint' | 'erase' | 'portal';
  x: number;
  y: number;
  tileId?: number;
  priorTileId?: number;
  portalId?: string;
}

const UNDO_CAP = 100;
const PORTAL_TRIGGER_DIST2 = 2.25; // ~1.5 tiles — "walking into" the portal
const PORTAL_COOLDOWN_MS = 2500;

function viewFromUrl(): View {
  return new URLSearchParams(window.location.search).get('view') === 'worlds' ? 'worlds' : 'world';
}

function extractPortals(w: unknown): PortalMarker[] {
  if (!w || typeof w !== 'object') return [];
  const arr = (w as Record<string, unknown>).portals;
  if (!Array.isArray(arr)) return [];
  return arr.filter(
    (p): p is PortalMarker =>
      !!p &&
      typeof p === 'object' &&
      typeof (p as PortalMarker).id === 'string' &&
      typeof (p as PortalMarker).x === 'number',
  );
}

function deviceKey(): string {
  let k = localStorage.getItem('hearth:device');
  if (!k) {
    k = uuid();
    localStorage.setItem('hearth:device', k);
  }
  return k;
}

export function App() {
  const mountRef = useRef<HTMLDivElement>(null);
  const rendererRef = useRef<WorldRenderer | null>(null);
  const netRef = useRef<Net | null>(null);
  const selfIdRef = useRef('');
  const selfNameRef = useRef('');
  const specRef = useRef<AvatarSpec>(loadSpec());
  const lastSpaceRef = useRef('');

  const [phase, setPhase] = useState<Phase>('join');
  const [status, setStatus] = useState<NetStatus>('connecting');
  const [spaceName, setSpaceName] = useState('town-square');
  const [isPublished, setIsPublished] = useState(false);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [channel, setChannel] = useState<'proximity' | 'space'>('proximity');
  const [sheetOpen, setSheetOpen] = useState(false);
  const [unread, setUnread] = useState(0);

  const [view, setView] = useState<View>(viewFromUrl());
  const [mode, setMode] = useState<EditMode>('play');
  const [brush, setBrush] = useState(1);
  const [erasing, setErasing] = useState(false);
  const [undoCount, setUndoCount] = useState(0);
  const [portals, setPortals] = useState<PortalMarker[]>([]);
  const [teleporting, setTeleporting] = useState(false);
  const [toast, setToast] = useState<string | null>(null);

  const [worlds, setWorlds] = useState<WorldEntry[]>([]);
  const [worldsLoading, setWorldsLoading] = useState(false);
  const [creatingWorld, setCreatingWorld] = useState(false);
  const [publishing, setPublishing] = useState(false);

  // refs mirroring state for stable closures (rAF loops, ws handlers)
  const modeRef = useRef<EditMode>('play');
  const brushRef = useRef(1);
  const erasingRef = useRef(false);
  const portalsRef = useRef<PortalMarker[]>([]);
  const undoRef = useRef<UndoEntry[]>([]);
  const skipRecordRef = useRef(false);
  const portalCooldownRef = useRef(0);
  const toastTimer = useRef<number | null>(null);
  const sheetOpenRef = useRef(false);
  const worldsQueryRef = useRef('');

  modeRef.current = mode;
  brushRef.current = brush;
  erasingRef.current = erasing;

  const showToast = useCallback((t: string) => {
    setToast(t);
    if (toastTimer.current) window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(null), 2600);
  }, []);

  // ------------------------------------------------------------ routing
  useEffect(() => {
    const onPop = () => {
      setView(viewFromUrl());
      setMode('play');
    };
    window.addEventListener('popstate', onPop);
    return () => window.removeEventListener('popstate', onPop);
  }, []);

  const navTo = useCallback((path: string) => {
    if (window.location.pathname + window.location.search !== path) {
      window.history.pushState({}, '', path);
    }
    setView(viewFromUrl());
  }, []);

  const openWorlds = useCallback(() => navTo('/?view=worlds'), [navTo]);
  const closeWorlds = useCallback(() => navTo('/'), [navTo]);

  // ------------------------------------------------------------ boot
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

  // ------------------------------------------------------------ net
  const recordUndo = useCallback((d: EditMsg) => {
    if (skipRecordRef.current) {
      skipRecordRef.current = false;
      return;
    }
    const stack = undoRef.current;
    if (d.op === 'portal') {
      if (d.portal) {
        stack.push({ op: 'portal', x: d.portal.x, y: d.portal.y, portalId: d.portal.id });
      } else if (d.portalId) {
        const gone = portalsRef.current.find((p) => p.id === d.portalId);
        if (gone) stack.push({ op: 'portal', x: gone.x, y: gone.y, portalId: gone.id });
      }
    } else if (typeof d.x === 'number' && typeof d.y === 'number') {
      stack.push({
        op: d.op === 'erase' ? 'erase' : 'paint',
        x: d.x,
        y: d.y,
        tileId: d.tileId,
        priorTileId: d.priorTileId,
      });
    }
    if (stack.length > UNDO_CAP) stack.splice(0, stack.length - UNDO_CAP);
    setUndoCount(stack.length);
  }, []);

  const onEdit = useCallback(
    (d: EditMsg) => {
      const r = rendererRef.current;
      if (!r) return;
      if (d.op !== 'portal' && typeof d.x === 'number' && typeof d.y === 'number' && typeof d.tileId === 'number') {
        r.paintTile(d.x, d.y, d.tileId);
      }
      if (d.op === 'portal') {
        if (d.portal) {
          const list = portalsRef.current.filter((p) => p.id !== d.portal!.id);
          list.push(d.portal);
          portalsRef.current = list;
          setPortals(list);
        } else if (d.portalId) {
          const list = portalsRef.current.filter((p) => p.id !== d.portalId);
          portalsRef.current = list;
          setPortals(list);
        }
      }
      if (d.by && d.by === selfIdRef.current && d.applied) recordUndo(d);
    },
    [recordUndo],
  );

  const handlePortalMsg = useCallback(
    async (d: PortalMsg) => {
      const target = d.spaceId;
      if (!target || target === lastSpaceRef.current) return;
      lastSpaceRef.current = target;
      setTeleporting(true);
      try {
        const sp = await fetchSpace(target);
        const r = rendererRef.current;
        if (sp) {
          portalsRef.current = extractPortals(sp);
          setPortals(portalsRef.current);
          r?.setWorld(sp);
          const doc = sp as { isPublished?: boolean; isShowcase?: boolean };
          setIsPublished(doc.isPublished === true);
        }
        r?.setSelf(selfIdRef.current, selfNameRef.current || 'you', { spec: specRef.current });
        if (typeof d.x === 'number' && typeof d.y === 'number') r?.setLocalPos(d.x, d.y);
        setSpaceName(target);
        // keep the URL as the world's deep link (no reload)
        const q = new URLSearchParams(window.location.search);
        if (q.get('space') !== target) {
          q.set('space', target);
          q.delete('view');
          window.history.pushState({}, '', `${window.location.pathname}?${q.toString()}`);
        }
        setView('world');
        setMode('play');
      } finally {
        window.setTimeout(() => setTeleporting(false), 380);
      }
    },
    [],
  );

  const joinInto = useCallback(
    (space: string) => {
      const name = selfNameRef.current || 'guest';
      const spec = specRef.current;
      lastSpaceRef.current = space;
      setSpaceName(space);
      const net = new Net(wsUrl(), {
        onStatus: setStatus,
        onWelcome: (d) => {
          selfIdRef.current = d.selfId;
          const r = rendererRef.current;
          if (!r) return;
          if (d.world) {
            portalsRef.current = extractPortals(d.world);
            setPortals(portalsRef.current);
            r.setWorld(d.world);
            const doc = d.world as { isPublished?: boolean };
            setIsPublished(doc.isPublished === true);
          }
          r.setSelf(d.selfId, name, { spec });
          r.applyRoster(d.roster);
          const sp = d.space ?? d.spaceId;
          if (sp) {
            lastSpaceRef.current = sp;
            setSpaceName(sp);
          }
          setPhase('world');
          setMode('play');
        },
        onState: (states) => rendererRef.current?.updateState(states),
        onChat: (d) => {
          const from = d.from ?? '?';
          const ch: 'proximity' | 'space' = d.channel === 'space' ? 'space' : 'proximity';
          setMessages((prev) => {
            if (from === selfIdRef.current) {
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
        onEdit,
        onPortal: (d) => void handlePortalMsg(d),
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
      net.connect({ name, lang: 'en', space, guest: true, deviceKey: deviceKey(), avatar: { spec } });

      // early world fetch → tiles + portals render before welcome arrives
      void fetchSpace(space).then((sp) => {
        const w = sp && typeof sp === 'object' ? (sp as Record<string, unknown>).world ?? sp : null;
        if (w) {
          portalsRef.current = extractPortals(w);
          setPortals(portalsRef.current);
          rendererRef.current?.setWorld(w);
          const doc = w as { isPublished?: boolean };
          setIsPublished(doc.isPublished === true);
        }
      });
    },
    [onEdit, handlePortalMsg],
  );

  const join = useCallback(
    (name: string, avatar?: AvatarSpec) => {
      const trimmed = name.trim() || 'guest';
      selfNameRef.current = trimmed;
      specRef.current = avatar ?? loadSpec();
      localStorage.setItem('hearth:name', trimmed);
      setPhase('loading');
      void (async () => {
        const space = await resolveSpaceId();
        joinInto(space);
      })();
    },
    [joinInto],
  );

  /** Switch the live connection to another world (directory card / new world). */
  const enterWorld = useCallback(
    (id: string) => {
      const name = selfNameRef.current || 'guest';
      const spec = specRef.current;
      // fresh Net for the new space (server sends a fresh welcome)
      netRef.current?.close();
      netRef.current = null;
      setTeleporting(true);
      selfNameRef.current = name;
      specRef.current = spec;
      setPhase('world');
      joinInto(id);
      window.setTimeout(() => setTeleporting(false), 500);
    },
    [joinInto],
  );

  // ------------------------------------------------------------ directory
  const refreshWorlds = useCallback(async (q = '') => {
    setWorldsLoading(true);
    const ws = await listWorlds(q);
    setWorlds(ws);
    setWorldsLoading(false);
  }, []);

  useEffect(() => {
    if (view === 'worlds') void refreshWorlds(worldsQueryRef.current);
  }, [view, refreshWorlds]);

  const onSearch = useCallback((q: string) => {
    worldsQueryRef.current = q;
    void refreshWorlds(q);
  }, [refreshWorlds]);

  const createAndEnter = useCallback(
    async (name: string) => {
      setCreatingWorld(true);
      const doc = await createWorld(name, deviceKey());
      setCreatingWorld(false);
      if (!doc) {
        showToast('Could not create world — try again');
        return;
      }
      showToast(`“${doc.name}” created — paint & publish!`);
      navTo(`/?space=${doc.id}`);
      enterWorld(doc.id);
    },
    [enterWorld, navTo, showToast],
  );

  const publish = useCallback(async () => {
    const id = lastSpaceRef.current;
    if (!id) return;
    setPublishing(true);
    const ok = await publishWorld(id, deviceKey());
    setPublishing(false);
    if (ok) {
      setIsPublished(true);
      showToast('Published! Your world is in the directory 🎉');
      void refreshWorlds(worldsQueryRef.current);
    } else {
      showToast('Publish failed — try again');
    }
  }, [refreshWorlds, showToast]);

  // ------------------------------------------------------------ editor
  const undo = useCallback(() => {
    const u = undoRef.current.pop();
    if (!u) return;
    setUndoCount(undoRef.current.length);
    const net = netRef.current;
    if (!net) return;
    skipRecordRef.current = true; // don't re-record the compensating op
    if (u.op === 'portal' && u.portalId) {
      net.sendEdit({ op: 'portal', portalId: u.portalId });
      showToast('Removed portal');
    } else {
      net.sendEdit({ op: 'paint', x: u.x, y: u.y, tileId: u.priorTileId ?? 0 });
      showToast('Undid last edit');
    }
  }, [showToast]);

  const onEditorTap = useCallback(
    (e: PointerEvent) => {
      const r = rendererRef.current;
      const net = netRef.current;
      if (!r || !net) return;
      const el = e.currentTarget as HTMLElement;
      const rect = el.getBoundingClientRect();
      const tile = r.screenToTile(e.clientX - rect.left, e.clientY - rect.top);
      if (!tile) return;
      const m = modeRef.current;
      if (m === 'portal') {
        const portal: PortalPayload = {
          id: `p-${uuid().slice(0, 8)}`,
          x: tile.x,
          y: tile.y,
          targetSpace: 'town-square',
          targetX: 16,
          targetY: 16,
        };
        net.sendEdit({ op: 'portal', portal });
        showToast('Portal placed → town-square');
      } else if (m === 'paint') {
        if (erasingRef.current) {
          net.sendEdit({ op: 'erase', x: tile.x, y: tile.y });
        } else {
          net.sendEdit({ op: 'paint', x: tile.x, y: tile.y, tileId: brushRef.current });
        }
      }
    },
    [showToast],
  );

  // portal proximity auto-walk-through (play mode only, with cooldown)
  useEffect(() => {
    if (phase !== 'world' || mode !== 'play' || view !== 'world') return;
    let raf = 0;
    const loop = () => {
      raf = requestAnimationFrame(loop);
      const r = rendererRef.current;
      if (!r) return;
      const loc = r.getLocal();
      if (!loc) return;
      const now = performance.now();
      if (now < portalCooldownRef.current) return;
      for (const p of portalsRef.current) {
        const dx = loc.x - p.x;
        const dy = loc.y - p.y;
        if (dx * dx + dy * dy <= PORTAL_TRIGGER_DIST2) {
          portalCooldownRef.current = now + PORTAL_COOLDOWN_MS;
          netRef.current?.sendPortal(p.id);
          return;
        }
      }
    };
    loop();
    return () => cancelAnimationFrame(raf);
  }, [phase, mode, view]);

  // ------------------------------------------------------------ chat
  const openChat = useCallback(() => {
    sheetOpenRef.current = true;
    setSheetOpen(true);
    setUnread(0);
  }, []);
  const closeChat = useCallback(() => {
    sheetOpenRef.current = false;
    setSheetOpen(false);
  }, []);

  const sendChat = useCallback(
    (text: string) => {
      const nonce = uuid();
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
      space: () => lastSpaceRef.current,
    };
  }, [status]);

  const inWorld = phase === 'world' && view === 'world';

  return (
    <div class="app">
      <div ref={mountRef} class="world-mount" />
      {inWorld && (
        <Hud status={status} unread={unread} space={spaceName} onOpenChat={openChat} onOpenWorlds={openWorlds} />
      )}
      {inWorld && (
        <EditToolbar
          mode={mode}
          onMode={setMode}
          brush={brush}
          onBrush={setBrush}
          erasing={erasing}
          onErasing={setErasing}
          onUndo={undo}
          canUndo={undoCount > 0}
          worldName={spaceName}
          isPublished={isPublished}
          publishing={publishing}
          onPublish={publish}
          online={status === 'online'}
        />
      )}

      {/* portal markers — world doc portals, projected onto the canvas */}
      {inWorld && mode === 'play' && (
        <PortalLayer portals={portals} rendererRef={rendererRef} onUse={(id) => netRef.current?.sendPortal(id)} />
      )}

      {/* editor tap-capture overlay (paint / portal modes) */}
      {inWorld && mode !== 'play' && <div class="editor-overlay" onPointerDown={onEditorTap} />}

      {phase === 'loading' && (
        <div class="loading">
          <div class="spinner" />
          <p>Entering the Hearth…</p>
        </div>
      )}
      {phase === 'join' && (
        <JoinScreen onJoin={join} initial={localStorage.getItem('hearth:name') ?? ''} />
      )}

      <WorldsDirectory
        open={phase === 'world' && view === 'worlds'}
        worlds={worlds}
        loading={worldsLoading}
        creating={creatingWorld}
        currentId={lastSpaceRef.current}
        onBack={closeWorlds}
        onJoin={(id) => {
          closeWorlds();
          navTo(`/?space=${id}`);
          enterWorld(id);
        }}
        onCreate={createAndEnter}
        onSearch={onSearch}
      />

      <ChatSheet
        open={sheetOpen}
        onClose={closeChat}
        channel={channel}
        onChannel={setChannel}
        messages={messages}
        onSend={sendChat}
      />

      {teleporting && (
        <div class="teleport-fade" aria-hidden="true">
          <div class="teleport-label">Stepping through…</div>
        </div>
      )}
      {toast && <div class="toast" role="status">{toast}</div>}
    </div>
  );
}

/** DOM overlay that projects world-doc portals onto the canvas. */
function PortalLayer({
  portals,
  rendererRef,
  onUse,
}: {
  portals: PortalMarker[];
  rendererRef: { current: WorldRenderer | null };
  onUse(id: string): void;
}) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const [, force] = useState(0);

  useEffect(() => {
    let raf = 0;
    const loop = () => {
      raf = requestAnimationFrame(loop);
      force((n) => n + 1);
    };
    loop();
    return () => cancelAnimationFrame(raf);
  }, [portals]);

  const r = rendererRef.current;
  return (
    <div class="portal-layer" ref={wrapRef} aria-hidden="true">
      {portals.map((p) => {
        const s = r ? r.project(p.x, p.y) : null;
        if (!s) return null;
        return (
          <button
            key={p.id}
            class="portal-marker"
            style={{ left: s.x, top: s.y }}
            onClick={(e) => {
              e.stopPropagation();
              onUse(p.id);
            }}
            title={`${p.targetSpace} portal — click to step through`}
          >
            <span class="portal-ring" />
            <span class="portal-label">→ {p.targetSpace}</span>
          </button>
        );
      })}
    </div>
  );
}
