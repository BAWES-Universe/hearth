import { describe, expect, it } from 'vitest';
import { env, type ChatMsg, type Envelope } from './protocol';

// Wire protocol v0 is FROZEN (PROTOCOL.md). These tests pin the envelope
// contract — any protocol change MUST update PROTOCOL.md + this file in the
// same commit (round+sign rule), and go through the PR workflow.
describe('protocol envelope (PROTOCOL.md v0)', () => {
  it('env() builds a valid v1 envelope', () => {
    const e = env('chat', { channel: 'proximity', text: 'hi' } satisfies ChatMsg);
    expect(e.v).toBe(1);
    expect(e.t).toBe('chat');
    expect(typeof e.id).toBe('string');
    expect(e.id.length).toBeGreaterThan(0);
    expect(typeof e.ts).toBe('number');
    expect(e.d).toMatchObject({ channel: 'proximity', text: 'hi' });
  });

  it('round-trips over JSON.parse, exactly like the WS wire', () => {
    const e = env('move', { x: 12.5, y: 8.2, dir: 'right', seq: 1 });
    const wire = JSON.parse(JSON.stringify(e)) as Envelope;
    expect(wire).toEqual(e);
    expect(wire.d).toMatchObject({ x: 12.5, y: 8.2, dir: 'right', seq: 1 });
  });

  it('server->client chat frame matches the frozen contract', () => {
    const frame: Envelope = JSON.parse(
      '{"v":1,"t":"chat","id":"m1","ts":1700000000000,"d":{"channel":"proximity","from":"u2","text":"hi","seq":77}}',
    );
    expect(frame.v).toBe(1);
    expect(frame.t).toBe('chat');
    expect(frame.d).toMatchObject({ channel: 'proximity', from: 'u2', text: 'hi', seq: 77 });
  });

  it('envelope keys are exactly v,t,id,ts,d', () => {
    const e = env('welcome', { selfId: 'u1' });
    expect(Object.keys(e).sort()).toEqual(['d', 'id', 't', 'ts', 'v']);
  });
});
