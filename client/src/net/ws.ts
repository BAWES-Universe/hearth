// WebSocket connection manager: envelope framing, auto-reconnect with
// exponential backoff + jitter, re-join on open (server re-sends welcome →
// client resyncs full state).

import { env, type BotMsg, type ChatMsg, type EditMsg, type EditOut, type ErrMsg, type PortalMsg, type StateEntry, type Welcome } from './protocol';
import type { AvatarInfo } from '../avatar/spec';

export type NetStatus = 'connecting' | 'online' | 'reconnecting' | 'offline';

export interface JoinInfo {
  name: string;
  lang: string;
  space: string;
  guest: boolean;
  deviceKey?: string;
  avatar?: AvatarInfo;
}

export interface NetHandlers {
  onStatus(s: NetStatus): void;
  onWelcome(d: Welcome): void;
  onState(d: StateEntry[]): void;
  onChat(d: ChatMsg): void;
  onEdit(d: EditMsg): void;
  onPortal?(d: PortalMsg): void;
  onError(d: ErrMsg): void;
  onBotMsg?(d: BotMsg): void;
}

const BASE_DELAY = 500;
const MAX_DELAY = 10000;

export class Net {
  private ws: WebSocket | null = null;
  private join: JoinInfo | null = null;
  private closedByUser = false;
  private attempts = 0;
  private timer: number | null = null;

  constructor(
    private url: string,
    private h: NetHandlers,
  ) {}

  connect(join: JoinInfo): void {
    this.join = join;
    this.closedByUser = false;
    this.attempts = 0;
    this.open();
  }

  private open(): void {
    this.h.onStatus(this.attempts === 0 ? 'connecting' : 'reconnecting');
    let ws: WebSocket;
    try {
      ws = new WebSocket(this.url);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;

    ws.onopen = () => {
      this.attempts = 0;
      this.h.onStatus('online');
      if (this.join) this.send(env('join', this.join));
    };

    ws.onmessage = (ev: MessageEvent) => {
      try {
        const m = JSON.parse(String(ev.data));
        this.dispatch(m);
      } catch {
        /* non-JSON frame — ignore */
      }
    };

    ws.onclose = () => {
      if (this.closedByUser) {
        this.h.onStatus('offline');
      } else {
        this.scheduleReconnect();
      }
    };

    ws.onerror = () => {
      try {
        ws.close();
      } catch {
        /* already closing */
      }
    };
  }

  private scheduleReconnect(): void {
    if (this.closedByUser) return;
    const delay = Math.min(MAX_DELAY, BASE_DELAY * 2 ** this.attempts) + Math.random() * 300;
    this.attempts++;
    this.h.onStatus('reconnecting');
    if (this.timer) window.clearTimeout(this.timer);
    this.timer = window.setTimeout(() => this.open(), delay);
  }

  private dispatch(m: unknown): void {
    if (!m || typeof m !== 'object') return;
    const msg = m as { t?: string; d?: unknown };
    switch (msg.t) {
      case 'welcome':
        this.h.onWelcome((msg.d ?? {}) as Welcome);
        break;
      case 'state':
        this.h.onState(Array.isArray(msg.d) ? (msg.d as StateEntry[]) : []);
        break;
      case 'chat':
        this.h.onChat((msg.d ?? {}) as ChatMsg);
        break;
      case 'edit':
        this.h.onEdit((msg.d ?? {}) as EditMsg);
        break;
      case 'portal':
        this.h.onPortal?.((msg.d ?? {}) as PortalMsg);
        break;
      case 'error':
        this.h.onError((msg.d ?? {}) as ErrMsg);
        break;
      case 'bot_msg':
        this.h.onBotMsg?.((msg.d ?? {}) as BotMsg);
        break;
      default:
        break;
    }
  }

  send(e: { v: number; t: string; id: string; ts: number; d: unknown }): void {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(e));
    }
  }

  sendMove(x: number, y: number, dir: string, seq: number): void {
    this.send(env('move', { x, y, dir, seq }));
  }

  sendChat(channel: string, text: string, nonce: string): void {
    this.send(env('chat', { channel, text, nonce }));
  }

  /** Send one frozen HMF v1 editor op (paint|erase|place|zone|portal|publish). */
  sendEdit(op: EditOut): void {
    this.send(env('edit', op));
  }

  /** Request a portal walk-through (server validates proximity + publish). */
  sendPortal(portalId: string): void {
    this.send(env('portal', { portalId }));
  }

  close(): void {
    this.closedByUser = true;
    if (this.timer) window.clearTimeout(this.timer);
    try {
      this.ws?.close();
    } catch {
      /* ignore */
    }
    this.h.onStatus('offline');
  }
}
