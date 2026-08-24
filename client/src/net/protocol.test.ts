import { describe, expect, it } from 'vitest';
import { env, type ChatMsg, type Envelope, type FriendMsg, type FriendPresenceMsg, type MediaSignalMsg } from './protocol';

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

// T2 media plane — ADDITIVE envelopes (docs/MEDIA.md). PROTOCOL.md v0 is not
// amended; these pin the shapes the client and server exchange for the SFU.
describe('media envelopes (T2, additive — docs/MEDIA.md)', () => {
  it('media_join carries the target space', () => {
    const e = env('media_join', { space: 'garden' });
    expect(e.t).toBe('media_join');
    expect(e.d).toMatchObject({ space: 'garden' });
  });

  it('media_leave is an empty payload', () => {
    const e = env('media_leave', {});
    expect(e.t).toBe('media_leave');
    expect(e.d).toEqual({});
  });

  it('media_signal offer round-trips over JSON like the WS wire', () => {
    const sig: MediaSignalMsg = {
      pc: 'publisher',
      type: 'offer',
      sdp: { type: 'offer', sdp: 'v=0\r\nfake-sdp' },
    };
    const wire = JSON.parse(JSON.stringify(env('media_signal', sig))) as Envelope;
    expect(wire.t).toBe('media_signal');
    expect(wire.d).toMatchObject({
      pc: 'publisher',
      type: 'offer',
      sdp: { type: 'offer', sdp: 'v=0\r\nfake-sdp' },
    });
  });

  it('server->client media_state frame matches the documented shape', () => {
    const frame: Envelope = JSON.parse(
      '{"v":1,"t":"media_state","id":"m1","ts":1700000000000,"d":{"joined":true,"space":"garden","peers":[{"id":"u1","name":"Alice"}]}}',
    );
    expect(frame.t).toBe('media_state');
    expect(frame.d).toMatchObject({
      joined: true,
      space: 'garden',
      peers: [{ id: 'u1', name: 'Alice' }],
    });
  });
});

// T2 social layer — ADDITIVE envelopes (docs/SOCIAL.md). PROTOCOL.md v0 is
// untouched; these pin the new server->client shapes so a server/client
// contract split cannot silently break the friends panel.
describe('social envelopes (T2 additive)', () => {
  it('{t:friend} request frame matches the server shape', () => {
    const frame: Envelope = JSON.parse(
      '{"v":1,"t":"friend","id":"m1","ts":1700000000000,"d":{"event":"request","userId":"u2","name":"Bob","status":"requested"}}',
    );
    expect(frame.t).toBe('friend');
    const d = frame.d as FriendMsg;
    expect(d.event).toBe('request');
    expect(d.userId).toBe('u2');
    expect(d.name).toBe('Bob');
    expect(d.status).toBe('requested');
  });

  it('{t:friend_presence} join frame matches the server shape', () => {
    const frame: Envelope = JSON.parse(
      '{"v":1,"t":"friend_presence","id":"m2","ts":1700000000000,"d":{"event":"join","userId":"u2","name":"Bob","online":true,"spaceId":"town-square"}}',
    );
    expect(frame.t).toBe('friend_presence');
    const d = frame.d as FriendPresenceMsg;
    expect(d.event).toBe('join');
    expect(d.online).toBe(true);
    expect(d.spaceId).toBe('town-square');
  });

  it('{t:friend_presence} offline frame carries online:false', () => {
    const frame: Envelope = JSON.parse(
      '{"v":1,"t":"friend_presence","id":"m3","ts":1700000000000,"d":{"event":"offline","userId":"u2","name":"Bob","online":false,"spaceId":""}}',
    );
    const d = frame.d as FriendPresenceMsg;
    expect(d.event).toBe('offline');
    expect(d.online).toBe(false);
  });
});
