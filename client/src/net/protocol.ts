// Hearth wire protocol v0 — frozen contract (PROTOCOL.md). Envelope + message shapes.

export interface Envelope {
  v: number;
  t: string;
  id: string;
  ts: number;
  d: unknown;
}

// UUID v4 with a Math.random fallback: crypto.randomUUID is undefined on
// non-secure (plain http) origins, which would throw inside env() and kill
// the join before anything is sent (the infinite-loading bug).
export function uuid(): string {
  try {
    if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
      return crypto.randomUUID();
    }
  } catch {
    /* insecure context — fall back below */
  }
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    const v = c === 'x' ? r : (r & 0x3) | 0x8;
    return v.toString(16);
  });
}

export function env(t: string, d: unknown): Envelope {
  return { v: 1, t, id: uuid(), ts: Date.now(), d };
}

import type { AvatarInfo } from '../avatar/spec';

export interface RosterEntry {
  id: string;
  name: string;
  x: number;
  y: number;
  dir?: string;
  avatar?: AvatarInfo;
}

export interface Welcome {
  selfId: string;
  space?: string;
  spaceId?: string;
  world?: unknown;
  roster?: RosterEntry[];
  avatar?: AvatarInfo;
}

export interface StateEntry {
  id: string;
  x: number;
  y: number;
  dir?: string;
  avatar?: AvatarInfo;
}

export interface ChatMsg {
  channel?: 'proximity' | 'space' | 'dm';
  from?: string;
  text: string;
  seq?: number;
  nonce?: string;
  ts?: number;
}

export interface EditMsg {
  op: string;
  x?: number;
  y?: number;
  tileId?: number;
  priorTileId?: number;
  portalId?: string;
  portal?: PortalPayload;
  by?: string;
  seq?: number;
  applied?: boolean;
}

/** HMF v1 portal payload (mirrors server/hmf.Portal JSON). */
export interface PortalPayload {
  id: string;
  x: number;
  y: number;
  targetSpace: string;
  targetX: number;
  targetY: number;
}

/** Outbound editor op (frozen HMF v1 ops: paint|erase|place|zone|portal|publish). */
export interface EditOut {
  op: string;
  x?: number;
  y?: number;
  tileId?: number;
  portal?: PortalPayload;
  portalId?: string;
}

/** Server ack for a portal walk-through: swap to spaceId at (x, y). */
export interface PortalMsg {
  portalId?: string;
  spaceId?: string;
  x?: number;
  y?: number;
}

export interface ErrMsg {
  code?: string;
  msg?: string;
}

export interface BotMsg {
  botId?: string;
  text?: string;
}
