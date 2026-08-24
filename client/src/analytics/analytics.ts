// Analytics v0 — scrubbed product events to the self-hosted intake
// (POST /api/events). Batched (20 events or 15s), fire-and-forget, zero
// user-visible impact (a failed flush is silently dropped).
//
// Consent: localStorage 'hearth:analytics-consent' ∈ granted|denied|unset.
// 'denied' disables all tracking. v0 is self-hosted + double-scrubbed (no
// PII, no third party), so 'unset' defaults to tracking — the ox plan defers
// the consent banner + PostHog cloud to W3 (needs https + keys).
//
// Event payloads are flat + scrubbed here AND on the server; chat text never
// leaves the client (only char_len).

import { scrubProps } from './scrub';

const CONSENT_KEY = 'hearth:analytics-consent';
const BATCH_MAX = 20;
const FLUSH_MS = 15000;

// Mirrors server/analytics.go eventAllowlist (20 core events).
const ALLOWLIST = new Set([
  'join', 'world_enter', 'world_leave', 'portal_transit', 'paint_op', 'publish',
  'chat_send', 'friend_add', 'byok_key_paste', 'byok_key_validate', 'ai_use',
  'epic_pledge', 'orbit_level_up', 'page_view', 'session_start', 'session_end',
  'world_create', 'editor_open', 'editor_save', 'error',
]);

interface PendingEvent {
  name: string;
  props: Record<string, unknown>;
}

let queue: PendingEvent[] = [];
let timer: ReturnType<typeof setTimeout> | null = null;

function consent(): boolean {
  if (typeof localStorage === 'undefined') return true; // SSR / tests
  return localStorage.getItem(CONSENT_KEY) !== 'denied';
}

/** Queue one allowlisted, scrubbed event. No-op when consent is denied. */
export function track(name: string, props: Record<string, unknown> = {}): void {
  if (!ALLOWLIST.has(name) || !consent()) return;
  const scrubbed = scrubProps(props);
  if (!scrubbed) return;
  queue.push({ name, props: scrubbed });
  if (queue.length >= BATCH_MAX) {
    void flush();
  } else if (!timer) {
    timer = setTimeout(() => void flush(), FLUSH_MS);
  }
}

/** POST the pending batch to /api/events. Fire-and-forget: never throws. */
export async function flush(): Promise<void> {
  if (timer) {
    clearTimeout(timer);
    timer = null;
  }
  if (queue.length === 0) return;
  const events = queue;
  queue = [];
  try {
    await fetch('/api/events', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      keepalive: true,
      body: JSON.stringify({ events }),
    });
  } catch {
    // fire-and-forget — analytics must never affect the user
  }
}

/** Install the pagehide beacon + scrubbed client error hooks. Call once. */
export function initAnalytics(): void {
  window.addEventListener('pagehide', () => {
    if (queue.length === 0) return;
    const events = queue;
    queue = [];
    try {
      navigator.sendBeacon('/api/events', new Blob([JSON.stringify({ events })], { type: 'application/json' }));
    } catch {
      // ignore — best-effort on unload
    }
  });
  // Client errors: code + message length ONLY (no stacks, no message text —
  // the plan's `error` event carries code/msg_len, scrubbed).
  window.addEventListener('error', (e) => {
    track('error', { code: 'window_error', msg_len: (e.message ?? '').length });
  });
  window.addEventListener('unhandledrejection', (e) => {
    const msg = e.reason instanceof Error ? e.reason.message : String(e.reason ?? '');
    track('error', { code: 'unhandled_rejection', msg_len: msg.length });
  });
}

// test hook — drain any module-level leftovers between cases
export function _resetForTests(): void {
  if (timer) {
    clearTimeout(timer);
    timer = null;
  }
  queue = [];
}
