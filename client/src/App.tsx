import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import {
  createInvite,
  createWorld,
  fetchSpace,
  listWorlds,
  listWorldAssets,
  publishWorld,
  uploadAsset,
  type AssetRecord,
  type WorldEntry,
} from './net/api';
import {
  addFriend,
  ensureSession,
  listFriends,
  removeFriend,
  respondFriend,
  type FriendEntry,
} from './net/friends';
import { resolveSpaceId, wsUrl } from './net/config';
import { Net, type NetStatus } from './net/ws';
import { WorldRenderer } from './world/renderer';
import { uuid, type EditMsg, type FriendMsg, type FriendPresenceMsg, type PortalMsg, type PortalPayload } from './net/protocol';
import { ChatSheet, type ChatMessage } from './ui/ChatSheet';
import { Hud } from './ui/Hud';
import { JoinScreen } from './ui/JoinScreen';
import { EditToolbar, type EditMode, type ObjectKind } from './ui/EditToolbar';
import { WorldsDirectory } from './ui/WorldsDirectory';
import { VoiceBubble } from './ui/VoiceBubble';
import { ByokPanel } from './ui/ByokPanel';
import { VoiceManager, type VoicePeer, type VoiceState } from './net/voice';
import { FriendsPanel } from './ui/FriendsPanel';
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
  op: 'paint' | 'erase' | 'portal' | 'object' | 'asset';
  x: number;
  y: number;
  tileId?: number;
  priorTileId?: number;
  portalId?: string;
  objectId?: string;
  /** Freeform stroke: per-cell prior tiles for a single batch inverse op. */
  cells?: { x: number; y: number; tileId: number }[];
  /** Asset placement/removal (op 'asset'). */
  asset?: { assetId: string; x: number; y: number; remove?: boolean };
}

/** One in-flight freeform stroke (paint mode drag). */
interface StrokeState {
  active: boolean;
  cells: Map<string, { x: number; y: number }>;
  lastX: number;
  lastY: number;
  pointerId: number;
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

/** Bresenham walk between two tile coords (inclusive) — stroke continuity. */
function lineCells(x0: number, y0: number, x1: number, y1: number): { x: number; y: number }[] {
  const out: { x: number; y: number }[] = [];
  let dx = Math.abs(x1 - x0);
  let dy = -Math.abs(y1 - y0);
  const sx = x0 < x1 ? 1 : -1;
  const sy = y0 < y1 ? 1 : -1;
  let err = dx + dy;
  let x = x0;
  let y = y0;
  for (;;) {
    out.push({ x, y });
    if (x === x1 && y === y1) break;
    const e2 = 2 * err;
    if (e2 >= dy) {
      err += dy;
      x += sx;
    }
    if (e2 <= dx) {
      err += dx;
      y += sy;
    }
    if (out.length > 4096) break; // safety: cap a single stroke op
  }
  return out;
}

export function App() {
  const mountRef = useRef<HTMLDivElement>(null);
  const rendererRef = useRef<WorldRenderer | null>(null);
  const netRef = useRef<Net | null>(null);
  const voiceRef = useRef<VoiceManager | null>(null);
  const selfIdRef = useRef('');
  const selfNameRef = useRef('');
  const specRef = useRef<AvatarSpec>(loadSpec());
  const lastSpaceRef = useRef('');

  const [phase, setPhase] = useState<Phase>('join');
  const [status, setStatus] = useState<NetStatus>('connecting');
  const [spaceName, setSpaceName] = useState('town-square');
  const [isPublished, setIsPublished] = useState(false);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [channel, setChannel] = useState<'proximity' | 'space' | 'global'>('proximity');
  const [sheetOpen, setSheetOpen] = useState(false);
  const [unread, setUnread] = useState(0);

  const [view, setView] = useState<View>(viewFromUrl());
  const [mode, setMode] = useState<EditMode>('play');
  const [brush, setBrush] = useState(1);
  const [erasing, setErasing] = useState(false);
  const [objectKind, setObjectKind] = useState<ObjectKind>('door');
  // T2 custom asset upload: registry (palette) + selected asset + remove toggle
  const [assets, setAssets] = useState<AssetRecord[]>([]);
  const [assetBrush, setAssetBrush] = useState<string | null>(null);
  const [removingAsset, setRemovingAsset] = useState(false);
  const [uploading, setUploading] = useState(false);
  /** Upload dialog: picked file awaiting a name + confirm. */
  const [uploadOpen, setUploadOpen] = useState(false);
  const [pendingFile, setPendingFile] = useState<File | null>(null);
  const [assetName, setAssetName] = useState('');
  /** Server-arbitrated edit permission (welcome + spaces REST envelope). */
  const [canEdit, setCanEdit] = useState(true);
  const [undoCount, setUndoCount] = useState(0);
  const [portals, setPortals] = useState<PortalMarker[]>([]);
  const [teleporting, setTeleporting] = useState(false);
  const [toast, setToast] = useState<string | null>(null);
  const [mineCount, setMineCount] = useState(0);
  /** One-time paint-mode onboarding bubble (dismissed → stored). */
  const [paintTip, setPaintTip] = useState(() => localStorage.getItem('hearth:paint-tip') !== '1');

  const [worlds, setWorlds] = useState<WorldEntry[]>([]);
  const [worldsLoading, setWorldsLoading] = useState(false);
  const [creatingWorld, setCreatingWorld] = useState(false);
  const [publishing, setPublishing] = useState(false);

  // T2 voice bubble (media plane — docs/MEDIA.md)
  const [voiceState, setVoiceState] = useState<VoiceState>('off');
  const [voicePeers, setVoicePeers] = useState<VoicePeer[]>([]);
  const [micOn, setMicOn] = useState(false);
  const [speaking, setSpeaking] = useState(false);
  // T2 social: friends list + panel visibility (mirrored in refs for the WS
  // handlers, which are built once per join).
  const [friends, setFriends] = useState<FriendEntry[]>([]);
  const [friendsOpen, setFriendsOpen] = useState(false);
  const [byokOpen, setByokOpen] = useState(false);
  const friendsOpenRef = useRef(false);

  // refs mirroring state for stable closures (rAF loops, ws handlers)
  const modeRef = useRef<EditMode>('play');
  const brushRef = useRef(1);
  const erasingRef = useRef(false);
  const objectKindRef = useRef<ObjectKind>('door');
  const canEditRef = useRef(true);
  const portalsRef = useRef<PortalMarker[]>([]);
  const undoRef = useRef<UndoEntry[]>([]);
  /** "x,y" tiles whose current state is a paint by the local player. */
  const mineRef = useRef<Set<string>>(new Set());
  const skipRecordRef = useRef(false);
  const portalCooldownRef = useRef(0);
  const toastTimer = useRef<number | null>(null);
  const sheetOpenRef = useRef(false);
  const worldsQueryRef = useRef('');
  const assetBrushRef = useRef<string | null>(null);
  const removingAssetRef = useRef(false);
  /** Active freeform stroke (paint mode drag), accumulated per pointer. */
  const strokeRef = useRef<StrokeState>({ active: false, cells: new Map(), lastX: -1, lastY: -1, pointerId: -1 });

  modeRef.current = mode;
  brushRef.current = brush;
  erasingRef.current = erasing;
  objectKindRef.current = objectKind;
  canEditRef.current = canEdit;
  assetBrushRef.current = assetBrush;
  removingAssetRef.current = removingAsset;

  const showToast = useCallback((t: string) => {
    setToast(t);
    if (toastTimer.current) window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(null), 2600);
  }, []);

  /** T2: refresh the world's uploaded-asset registry (palette survives reload). */
  const loadAssets = useCallback(async (space: string) => {
    if (!space) return;
    const list = await listWorldAssets(space);
    setAssets(list);
    setAssetBrush((prev) => (prev && list.some((a) => a.id === prev) ? prev : null));
  }, []);

  /** File picked in the toolbar → open the upload dialog (name + confirm). */
  const onPickUpload = useCallback(
    (f: File) => {
      if (!/^image\/(png|jpeg|gif|webp)$/.test(f.type) && !/\.(png|jpe?g|gif|webp)$/i.test(f.name)) {
        showToast('Upload an image: png/jpeg/gif/webp');
        return;
      }
      setPendingFile(f);
      setAssetName(f.name.replace(/\.[^.]+$/, '').slice(0, 40));
      setUploadOpen(true);
    },
    [showToast],
  );

  const confirmUpload = useCallback(async () => {
    const space = lastSpaceRef.current;
    const f = pendingFile;
    if (!space || !f) return;
    setUploading(true);
    const rec = await uploadAsset(space, f, assetName);
    setUploading(false);
    setUploadOpen(false);
    setPendingFile(null);
    if (rec) {
      setAssets((prev) => [...prev.filter((a) => a.id !== rec.id), rec]);
      setAssetBrush(rec.id);
      setRemovingAsset(false);
      showToast('Asset uploaded — tap a tile to place it');
    } else {
      showToast('Upload failed — png/jpeg/gif/webp, ≤512KB');
    }
  }, [pendingFile, assetName, showToast]);

  const cancelUpload = useCallback(() => {
    setUploadOpen(false);
    setPendingFile(null);
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

  // ------------------------------------------------------------ friends
  const loadFriends = useCallback(async () => {
    const fs = await listFriends();
    setFriends(fs);
  }, []);

  const openFriends = useCallback(() => {
    friendsOpenRef.current = true;
    setFriendsOpen(true);
    void loadFriends();
  }, [loadFriends]);
  const closeFriends = useCallback(() => {
    friendsOpenRef.current = false;
    setFriendsOpen(false);
  }, []);

  const onAddFriend = useCallback(
    async (id: string) => {
      const ok = await addFriend(id);
      showToast(ok ? 'Friend request sent' : 'Could not send request');
      void loadFriends();
    },
    [loadFriends, showToast],
  );
  const onAcceptFriend = useCallback(
    async (id: string) => {
      const ok = await respondFriend(id, 'accept');
      showToast(ok ? 'Friend added 🎉' : 'Could not accept');
      void loadFriends();
    },
    [loadFriends, showToast],
  );
  const onDeclineFriend = useCallback(
    async (id: string) => {
      await respondFriend(id, 'decline');
      void loadFriends();
    },
    [loadFriends],
  );
  const onRemoveFriend = useCallback(
    async (id: string) => {
      await removeFriend(id);
      showToast('Friend removed');
      void loadFriends();
    },
    [loadFriends, showToast],
  );

  const onFriendEvent = useCallback(
    (d: FriendMsg) => {
      if (d.event === 'request') {
        showToast(`New friend request from ${d.name ?? 'someone'}`);
      }
      void loadFriends();
    },
    [loadFriends, showToast],
  );

  const onFriendPresence = useCallback(
    (d: FriendPresenceMsg) => {
      const id = d.userId;
      if (!id) return;
      setFriends((prev) =>
        prev.map((f) => (f.friendId === id ? { ...f, online: !!d.online, space: d.spaceId } : f)),
      );
      if (d.event === 'join' && d.spaceId === lastSpaceRef.current && !friendsOpenRef.current) {
        showToast(`${d.name ?? 'A friend'} joined this space`);
      }
    },
    [showToast],
  );

  // ------------------------------------------------------------ net
  const recordUndo = useCallback((d: EditMsg) => {
    if (skipRecordRef.current) {
      skipRecordRef.current = false;
      return;
    }
    const stack = undoRef.current;
    if (d.op === 'object') {
      // functional object placement → undo removes by id (server delete op)
      const oid = d.object?.id ?? d.objectId;
      const ox = d.object?.x ?? d.x ?? 0;
      const oy = d.object?.y ?? d.y ?? 0;
      if (oid) stack.push({ op: 'object', x: ox, y: oy, objectId: oid });
    } else if (d.op === 'asset') {
      // asset placement → undo = compensating removal (and vice versa)
      const a = d.asset;
      if (a && typeof a.x === 'number' && typeof a.y === 'number') {
        stack.push({ op: 'asset', x: a.x, y: a.y, asset: { assetId: a.assetId, x: a.x, y: a.y, remove: !a.remove } });
      }
    } else if (d.op === 'portal') {
      if (d.portal) {
        stack.push({ op: 'portal', x: d.portal.x, y: d.portal.y, portalId: d.portal.id });
      } else if (d.portalId) {
        const gone = portalsRef.current.find((p) => p.id === d.portalId);
        if (gone) stack.push({ op: 'portal', x: gone.x, y: gone.y, portalId: gone.id });
      }
    } else if (Array.isArray(d.cells) && d.cells.length > 0) {
      // freeform stroke: one undo entry restoring every cell to its own prior
      const cells = d.cells
        .filter((c) => c && typeof c.x === 'number' && typeof c.y === 'number')
        .map((c) => ({ x: c.x, y: c.y, tileId: c.priorTileId ?? 0 }));
      if (cells.length > 0) {
        const first = cells[0];
        stack.push({ op: 'paint', x: first.x, y: first.y, cells });
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

  const syncMine = useCallback(() => {
    const r = rendererRef.current;
    if (!r) return;
    r.setMineTiles(mineRef.current);
    setMineCount(mineRef.current.size);
  }, []);

  const resetMine = useCallback(() => {
    mineRef.current = new Set();
    syncMine();
  }, [syncMine]);

  const onEdit = useCallback(
    (d: EditMsg) => {
      const r = rendererRef.current;
      if (!r) return;
      if (d.op === 'asset' && d.asset) {
        // T2 custom asset: place or remove the image sprite
        const a = d.asset;
        if (a.remove) r.removeAsset(a.assetId, a.x, a.y);
        else r.placeAsset({ assetId: a.assetId, name: a.name, url: a.url, x: a.x, y: a.y });
      } else if (Array.isArray(d.cells) && d.cells.length > 0) {
        // freeform stroke ack: apply every cell (per-cell tileId wins)
        for (const c of d.cells) {
          if (c && typeof c.x === 'number' && typeof c.y === 'number' && typeof c.tileId === 'number') {
            r.paintTile(c.x, c.y, c.tileId);
            const key = `${c.x},${c.y}`;
            if (d.by === selfIdRef.current) {
              if (c.tileId === 0) mineRef.current.delete(key);
              else mineRef.current.add(key);
            } else {
              mineRef.current.delete(key);
            }
          }
        }
        syncMine();
      } else if (d.op !== 'portal' && typeof d.x === 'number' && typeof d.y === 'number' && typeof d.tileId === 'number') {
        r.paintTile(d.x, d.y, d.tileId);
        // ownership: tiles whose current state is my last paint
        const key = `${d.x},${d.y}`;
        if (d.by === selfIdRef.current) {
          if (d.op === 'erase' || d.tileId === 0) mineRef.current.delete(key);
          else mineRef.current.add(key);
        } else {
          mineRef.current.delete(key); // someone else owns it now
        }
        syncMine();
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
    [recordUndo, syncMine],
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
          resetMine();
          void loadAssets(target);
          const doc = sp as { isPublished?: boolean; isShowcase?: boolean; canEdit?: boolean };
          setIsPublished(doc.isPublished === true);
          if (typeof doc.canEdit === 'boolean') setCanEdit(doc.canEdit);
        }
        r?.setSelf(selfIdRef.current, selfNameRef.current || 'you', { spec: specRef.current });
        if (typeof d.x === 'number' && typeof d.y === 'number') r?.setLocalPos(d.x, d.y);
        setSpaceName(target);
        // move the voice bubble to the new space (server re-joins the room)
        voiceRef.current?.enter(target);
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
    [loadAssets, resetMine],
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
          setCanEdit(d.canEdit !== false);
          if (d.world) {
            portalsRef.current = extractPortals(d.world);
            setPortals(portalsRef.current);
            r.setWorld(d.world);
            resetMine();
            const doc = d.world as { isPublished?: boolean };
            setIsPublished(doc.isPublished === true);
          }
          r.setSelf(d.selfId, name, { spec });
          r.applyRoster(d.roster);
          const sp = d.space ?? d.spaceId;
          if (sp) {
            lastSpaceRef.current = sp;
            setSpaceName(sp);
            voiceRef.current?.enter(sp);
            void loadAssets(sp);
          }
          setPhase('world');
          setMode('play');
        },
        onState: (states) => rendererRef.current?.updateState(states),
        onChat: (d) => {
          const from = d.from ?? '?';
          const ch: 'proximity' | 'space' | 'global' = d.channel === 'space' ? 'space' : d.channel === 'global' ? 'global' : 'proximity';
          const fromId = d.fromId ?? from;
          // proximity chat from others → speech bubble above their avatar
          if (ch === 'proximity' && fromId !== selfIdRef.current) {
            rendererRef.current?.showBubble(fromId, d.text);
          }
          setMessages((prev) => {
            // echo of our own message: match by nonce (exact) or pending+text+channel
            if (fromId === selfIdRef.current) {
              const idx = prev.findIndex((m) =>
                m.self && m.pending && m.channel === ch &&
                (d.nonce ? m.id === d.nonce : m.text === d.text),
              );
              if (idx >= 0) {
                const copy = prev.slice();
                copy[idx] = { ...copy[idx], pending: false, id: d.nonce ?? copy[idx].id };
                return copy;
              }
            }
            const msg: ChatMessage = {
              id: d.nonce ?? `${d.ts}-${fromId}-${d.seq ?? Math.random().toString(36).slice(2, 8)}`,
              channel: ch,
              from,
              text: d.text,
              ts: d.ts ?? Date.now(),
              self: fromId === selfIdRef.current,
            };
            return [...prev, msg];
          });
          if (fromId !== selfIdRef.current && !sheetOpenRef.current) {
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
        onMediaSignal: (d) => voiceRef.current?.handleSignal(d),
        onMediaState: (d) => voiceRef.current?.handleState(d),
        onFriend: onFriendEvent,
        onFriendPresence,
      });
      netRef.current = net;
      voiceRef.current?.leave(); // drop any stale bubble from a previous net
      voiceRef.current = new VoiceManager(net, {
        onState: (s) => setVoiceState(s),
        onPeers: (p) => setVoicePeers(p),
        onMic: (m) => setMicOn(m),
        onSpeaking: (sp) => setSpeaking(sp),
      });
      net.connect({ name, lang: 'en', space, guest: true, deviceKey: deviceKey(), avatar: { spec } });

      // T2 social: ensure the session cookie exists (REST friends calls need
      // it — the WS handshake authenticates in-band without setting it), then
      // load the friend list.
      void ensureSession(deviceKey(), name).then(() => loadFriends());

      // early world fetch → tiles + portals render before welcome arrives
      void fetchSpace(space).then((sp) => {
        const w = sp && typeof sp === 'object' ? (sp as Record<string, unknown>).world ?? sp : null;
        if (w) {
          portalsRef.current = extractPortals(w);
          setPortals(portalsRef.current);
          rendererRef.current?.setWorld(w);
          resetMine();
          void loadAssets(space);
          const doc = w as { isPublished?: boolean };
          setIsPublished(doc.isPublished === true);
        }
      });
    },
    [loadAssets, onEdit, handlePortalMsg, onFriendEvent, onFriendPresence],
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
    async (name: string, template = 'empty_lot') => {
      setCreatingWorld(true);
      const doc = await createWorld(name, deviceKey(), { template });
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

  const invite = useCallback(async () => {
    const id = lastSpaceRef.current;
    if (!id) return;
    const token = await createInvite(id);
    if (!token) {
      showToast('Invite failed — try again');
      return;
    }
    const link = `${window.location.origin}/?space=${encodeURIComponent(id)}&invite=${encodeURIComponent(token)}`;
    try {
      await navigator.clipboard.writeText(link);
      showToast('Invite link copied to clipboard');
    } catch {
      showToast(`Invite: ${link}`);
    }
  }, [showToast]);

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
    if (u.op === 'object' && u.objectId) {
      net.sendEdit({ op: 'object', objectId: u.objectId });
      showToast('Removed object');
    } else if (u.op === 'portal' && u.portalId) {
      net.sendEdit({ op: 'portal', portalId: u.portalId });
      showToast('Removed portal');
    } else if (u.op === 'asset' && u.asset) {
      net.sendEdit({ op: 'asset', asset: u.asset });
      showToast(u.asset.remove ? 'Undid asset placement' : 'Restored asset');
    } else if (u.cells && u.cells.length > 0) {
      // freeform undo: single batch inverse op restoring each cell's prior
      net.sendEdit({ op: 'paint', cells: u.cells });
      showToast(`Undid stroke (${u.cells.length} tiles)`);
    } else {
      net.sendEdit({ op: 'paint', x: u.x, y: u.y, tileId: u.priorTileId ?? 0 });
      showToast('Undid last edit');
    }
  }, [showToast]);

  /** Editor overlay pointer-down: tap ops (portal/objects/assets) or stroke
   *  begin (paint). Freeform strokes accumulate cells in strokeRef and are
   *  sent as one batch op on pointer-up (deduped, Bresenham-continuous). */
  const onEditorPointerDown = useCallback(
    (e: PointerEvent) => {
      const r = rendererRef.current;
      const net = netRef.current;
      if (!r || !net) return;
      const el = e.currentTarget as HTMLElement;
      const rect = el.getBoundingClientRect();
      const tile = r.screenToTile(e.clientX - rect.left, e.clientY - rect.top);
      if (!tile) return;
      const m = modeRef.current;
      // guests are read-only: no edit ops (server also rejects, this is UI gating)
      if (!canEditRef.current && m !== 'play') {
        showToast('Read-only — you are a guest here');
        return;
      }
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
        return;
      }
      if (m === 'objects') {
        // server-validated functional object (door|npc|sign|light)
        net.sendEdit({
          op: 'object',
          object: {
            id: `obj-${uuid().slice(0, 8)}`,
            kind: objectKindRef.current,
            x: tile.x,
            y: tile.y,
          },
        });
        showToast(`${objectKindRef.current} placed — tap Undo to remove`);
        return;
      }
      if (m === 'assets') {
        // T2 custom asset: place (or remove) the selected uploaded image
        const ab = assetBrushRef.current;
        if (!ab) {
          showToast('Pick an asset from the palette first');
          return;
        }
        if (removingAssetRef.current) {
          net.sendEdit({ op: 'asset', asset: { assetId: ab, x: tile.x, y: tile.y, remove: true } });
        } else {
          net.sendEdit({ op: 'asset', asset: { assetId: ab, x: tile.x, y: tile.y } });
        }
        return;
      }
      // m === 'paint': begin a freeform stroke (accumulate → batch on release)
      try {
        el.setPointerCapture(e.pointerId);
      } catch {
        /* capture unsupported — moves outside the overlay still fire */
      }
      const cells = new Map<string, { x: number; y: number }>();
      cells.set(`${tile.x},${tile.y}`, { x: tile.x, y: tile.y });
      strokeRef.current = { active: true, cells, lastX: tile.x, lastY: tile.y, pointerId: e.pointerId };
    },
    [showToast],
  );

  /** Stroke continuation: Bresenham-walk from the last cell to the current
   *  pointer cell, adding every cell along the way (deduped via the Map). */
  const onEditorPointerMove = useCallback((e: PointerEvent) => {
    const s = strokeRef.current;
    if (!s.active || e.pointerId !== s.pointerId) return;
    const r = rendererRef.current;
    if (!r) return;
    const el = e.currentTarget as HTMLElement;
    const rect = el.getBoundingClientRect();
    const tile = r.screenToTile(e.clientX - rect.left, e.clientY - rect.top);
    if (!tile || (tile.x === s.lastX && tile.y === s.lastY)) return;
    for (const c of lineCells(s.lastX, s.lastY, tile.x, tile.y)) {
      if (s.cells.size >= 4096) break; // safety: cap a single stroke op
      s.cells.set(`${c.x},${c.y}`, { x: c.x, y: c.y });
    }
    s.lastX = tile.x;
    s.lastY = tile.y;
  }, []);

  /** Stroke end: flush the accumulated cells as one batch op (paint or
   *  erase), then reset the stroke state. */
  const onEditorPointerUp = useCallback(
    (e: PointerEvent) => {
      const s = strokeRef.current;
      if (!s.active || e.pointerId !== s.pointerId) return;
      const cells = [...s.cells.values()];
      strokeRef.current = { active: false, cells: new Map(), lastX: -1, lastY: -1, pointerId: -1 };
      const el = e.currentTarget as HTMLElement;
      try {
        el.releasePointerCapture(e.pointerId);
      } catch {
        /* noop */
      }
      if (cells.length === 0) return;
      const net = netRef.current;
      if (!net) return;
      if (erasingRef.current) {
        net.sendEdit({ op: 'erase', cells });
        if (cells.length > 1) showToast(`Erased ${cells.length} tiles`);
      } else {
        net.sendEdit({ op: 'paint', tileId: brushRef.current, cells });
        if (cells.length > 1) showToast(`Stroke painted (${cells.length} tiles)`);
      }
    },
    [showToast],
  );

  /** Stroke cancel (pointer lost): discard the accumulated cells. */
  const onEditorPointerCancel = useCallback((e: PointerEvent) => {
    const s = strokeRef.current;
    if (!s.active || e.pointerId !== s.pointerId) return;
    strokeRef.current = { active: false, cells: new Map(), lastX: -1, lastY: -1, pointerId: -1 };
  }, []);

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
      voice: () => voiceRef.current?.getState(),
    };
  }, [status]);

  const inWorld = phase === 'world' && view === 'world';

  return (
    <div class="app">
      <div ref={mountRef} class="world-mount" />
      {inWorld && (
        <Hud
          status={status}
          unread={unread}
          space={spaceName}
          friendRequests={friends.filter((f) => f.status === 'requested').length}
          onOpenChat={openChat}
          onOpenWorlds={openWorlds}
          onOpenFriends={openFriends}
          onOpenByok={() => setByokOpen(true)}
        />
      )}
      <ByokPanel open={byokOpen} onClose={() => setByokOpen(false)} />
      {inWorld && (
        <VoiceBubble
          state={voiceState}
          peers={voicePeers}
          micOn={micOn}
          speaking={speaking}
          onToggleMic={() => void voiceRef.current?.toggleMic()}
        />
      )}
      {inWorld && canEdit && (
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
          onInvite={invite}
          canEdit={canEdit}
          online={status === 'online'}
          mineCount={mineCount}
          objectKind={objectKind}
          onObjectKind={setObjectKind}
          assets={assets}
          assetBrush={assetBrush}
          onAssetBrush={(id) => {
            setAssetBrush(id);
            if (!id) setRemovingAsset(false);
          }}
          removingAsset={removingAsset}
          onRemovingAsset={setRemovingAsset}
          uploading={uploading}
          onUpload={onPickUpload}
        />
      )}
      {inWorld && !canEdit && (
        <div class="readonly-tag" role="status" title="This world is owned by someone else — you can explore but not edit">
          👁 read-only — guest
        </div>
      )}

      {/* paint-mode onboarding (one-time) */}
      {inWorld && mode === 'paint' && paintTip && (
        <div class="paint-tip" role="status">
          <div class="paint-tip-title">🖌 Paint mode</div>
          <p class="paint-tip-body">
            Tap or drag to paint tiles with the selected brush — dragging draws a continuous
            freeform stroke. Your edits save live and are visible to everyone — amber corner
            marks show tiles <em>you</em> painted.
          </p>
          <button
            class="paint-tip-dismiss"
            onClick={() => {
              localStorage.setItem('hearth:paint-tip', '1');
              setPaintTip(false);
            }}
          >
            Got it
          </button>
        </div>
      )}

      {/* portal markers — world doc portals, projected onto the canvas */}
      {inWorld && mode === 'play' && (
        <PortalLayer portals={portals} rendererRef={rendererRef} onUse={(id) => netRef.current?.sendPortal(id)} />
      )}

      {/* editor tap-capture overlay (paint / portal / objects / assets modes) */}
      {inWorld && mode !== 'play' && (
        <div
          class="editor-overlay"
          onPointerDown={onEditorPointerDown}
          onPointerMove={onEditorPointerMove}
          onPointerUp={onEditorPointerUp}
          onPointerCancel={onEditorPointerCancel}
        />
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

      <FriendsPanel
        open={friendsOpen}
        onClose={closeFriends}
        friends={friends}
        currentSpace={lastSpaceRef.current}
        onAdd={onAddFriend}
        onAccept={onAcceptFriend}
        onDecline={onDeclineFriend}
        onRemove={onRemoveFriend}
      />

      {uploadOpen && (
        <div class="upload-dialog-backdrop" role="dialog" aria-modal="true" aria-label="Upload asset">
          <div class="upload-dialog">
            <div class="upload-dialog-title">⬆ Upload an image</div>
            <p class="upload-dialog-sub">
              Stored in this world · placeable like a tile · png/jpeg/gif/webp ≤512KB
            </p>
            <input
              class="upload-name"
              value={assetName}
              onInput={(e) => setAssetName((e.target as HTMLInputElement).value)}
              placeholder="Asset name"
              maxLength={40}
            />
            <div class="upload-actions">
              <button class="tool-btn" onClick={cancelUpload} disabled={uploading}>
                Cancel
              </button>
              <button
                class="tool-btn publish"
                onClick={() => void confirmUpload()}
                disabled={uploading || !assetName.trim()}
              >
                {uploading ? 'Uploading…' : 'Upload'}
              </button>
            </div>
          </div>
        </div>
      )}

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
      // fade portal markers out when zoomed out to map-overview distance
      const z = rendererRef.current?.getZoom() ?? 1;
      if (wrapRef.current) wrapRef.current.style.opacity = z >= 0.4 ? '1' : '0.12';
      force((n) => n + 1);
    };
    loop();
    return () => cancelAnimationFrame(raf);
  }, [portals, rendererRef]);

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
