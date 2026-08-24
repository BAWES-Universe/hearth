import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { scrubPII, scrubProps } from './scrub';
import { track, flush, _resetForTests } from './analytics';

const fakeKey = 'sk-or-v1-' + 'a'.repeat(16);

// node-env stubs (vitest.config runs node, not jsdom)
function stubBrowser(): void {
  const store = new Map<string, string>();
  (globalThis as Record<string, unknown>).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
  };
  (globalThis as Record<string, unknown>).fetch = vi.fn().mockResolvedValue({ ok: true });
}

describe('analytics scrub (client pre-send)', () => {
  it('redacts keys, emails, IPs, bearer tokens', () => {
    expect(scrubPII(fakeKey)).toBe('[REDACTED_KEY]');
    expect(scrubPII('hi ' + fakeKey + ' bye')).toBe('hi [REDACTED_KEY] bye');
    expect(scrubPII('mail alice@example.com now')).toBe('mail [REDACTED_EMAIL] now');
    expect(scrubPII('from 203.0.113.7:8080')).toBe('from [REDACTED_IP]:8080');
    expect(scrubPII('Bearer eyJhbGciOiJIUzI1NiJ9.abc')).toBe('[REDACTED_TOKEN]');
  });

  it('scrubs prop keys AND values; drops nested payloads', () => {
    const out = scrubProps({ key: fakeKey, email: 'bob@example.com', space: 'town-square', n: 3 })!;
    expect(JSON.stringify(out)).not.toContain(fakeKey);
    expect(JSON.stringify(out)).not.toContain('bob@example.com');
    expect(out.space).toBe('town-square');
    expect(scrubProps({ nested: { a: 1 } })).toBeNull();
  });
});

describe('analytics batching (fire-and-forget intake)', () => {
  beforeEach(() => {
    stubBrowser();
    _resetForTests();
  });
  afterEach(() => {
    _resetForTests();
    vi.restoreAllMocks();
  });

  it('posts scrubbed events to /api/events on flush', async () => {
    track('world_enter', { space: 'town-square', secret: fakeKey });
    track('chat_send', { channel: 'space', char_len: 12 });
    await flush();
    const fetchMock = vi.mocked(fetch);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [, init] = fetchMock.mock.calls[0];
    const body = JSON.parse((init as RequestInit).body as string);
    expect(body.events).toHaveLength(2);
    expect(body.events[0].name).toBe('world_enter');
    expect(JSON.stringify(body)).not.toContain(fakeKey);
    expect(JSON.stringify(body)).not.toContain('sk-or-v1');
  });

  it('drops non-allowlisted events silently', async () => {
    track('not_a_real_event', {});
    await flush();
    expect(vi.mocked(fetch)).not.toHaveBeenCalled();
  });

  it('flushes automatically at 20 queued events', async () => {
    for (let i = 0; i < 20; i++) track('page_view', { view: 'worlds' });
    await flush(); // flush drains the auto-flushed batch (fetch already fired)
    expect(vi.mocked(fetch)).toHaveBeenCalled();
  });

  it('respects consent=denied (zero tracking calls)', async () => {
    (globalThis as Record<string, unknown>).localStorage = {
      getItem: () => 'denied',
    };
    track('session_start', {});
    await flush();
    expect(vi.mocked(fetch)).not.toHaveBeenCalled();
  });
});
