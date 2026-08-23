// Hearth wire protocol v0 — frozen contract (PROTOCOL.md). Envelope + message shapes.

export interface Envelope {
  v: number;
  t: string;
  id: string;
  ts: number;
  d: unknown;
}

export function env(t: string, d: unknown): Envelope {
  return { v: 1, t, id: crypto.randomUUID(), ts: Date.now(), d };
}

export interface RosterEntry {
  id: string;
  name: string;
  x: number;
  y: number;
  dir?: string;
}

export interface Welcome {
  selfId: string;
  space?: string;
  world?: unknown;
  roster?: RosterEntry[];
}

export interface StateEntry {
  id: string;
  x: number;
  y: number;
  dir?: string;
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
  by?: string;
}

export interface ErrMsg {
  code?: string;
  msg?: string;
}

export interface BotMsg {
  botId?: string;
  text?: string;
}
