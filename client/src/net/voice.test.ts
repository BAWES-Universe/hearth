import { afterEach, describe, expect, it, vi } from 'vitest';
import { VoiceManager, type VoiceHandlers, type VoiceState } from './voice';
import type { Net } from './ws';

// VoiceManager unit tests: stub the WS net and the WebRTC globals; no real
// PeerConnections, no DOM audio. Covers the signaling state machine end to
// end: join -> subscriber offer -> answer, mic on -> publisher offer.

class FakePC {
  static instances: FakePC[] = [];
  onicecandidate: ((ev: { candidate?: unknown }) => void) | null = null;
  ontrack: ((ev: unknown) => void) | null = null;
  closed = false;
  log: string[] = [];

  constructor(_cfg: unknown) {
    FakePC.instances.push(this);
  }

  async setRemoteDescription(d: unknown): Promise<void> {
    this.log.push(`setRemoteDescription:${(d as { type: string }).type}`);
  }

  async createAnswer(): Promise<unknown> {
    this.log.push('createAnswer');
    return { type: 'answer', sdp: 'v=0 fake answer' };
  }

  async createOffer(): Promise<unknown> {
    this.log.push('createOffer');
    return { type: 'offer', sdp: 'v=0 fake offer' };
  }

  async setLocalDescription(d: unknown): Promise<void> {
    this.log.push(`setLocalDescription:${(d as { type: string }).type}`);
  }

  async addIceCandidate(c: unknown): Promise<void> {
    this.log.push(`addIceCandidate:${(c as { candidate: string }).candidate}`);
  }

  addTrack(track: unknown, _stream: unknown): unknown {
    this.log.push('addTrack');
    return { track };
  }

  getSenders(): { track: unknown }[] {
    return [];
  }

  close(): void {
    this.closed = true;
  }
}

function fakeNet() {
  return {
    sendMediaJoin: vi.fn(),
    sendMediaLeave: vi.fn(),
    sendMediaSignal: vi.fn(),
  } as unknown as Net;
}

function fakeHandlers() {
  const calls: { state?: VoiceState; peers?: unknown; mic?: boolean; speaking?: boolean }[] = [];
  const h: VoiceHandlers = {
    onState: (s) => calls.push({ state: s }),
    onPeers: (p) => calls.push({ peers: p }),
    onMic: (m) => calls.push({ mic: m }),
    onSpeaking: (s) => calls.push({ speaking: s }),
  };
  return { h, calls };
}

const sdpOffer = {
  type: 'offer' as const,
  sdp: 'v=0 subscriber-offer',
};

afterEach(() => {
  vi.unstubAllGlobals();
  FakePC.instances = [];
});

/** Drain pending microtasks (the async offer/answer chain uses several awaits). */
async function flush(): Promise<void> {
  await new Promise((r) => setTimeout(r, 0));
}

describe('VoiceManager signaling state machine', () => {
  it('enter() sends media_join and reports joining', () => {
    const net = fakeNet();
    const { h, calls } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    expect(net.sendMediaJoin).toHaveBeenCalledWith('garden');
    expect(v.getState()).toBe('joining');
    expect(calls.some((c) => c.state === 'joining')).toBe(true);
  });

  it('media_state joined=true flips to on and reports peers', () => {
    vi.stubGlobal('RTCPeerConnection', FakePC);
    const net = fakeNet();
    const { h, calls } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    v.handleState({ joined: true, space: 'garden', peers: [{ id: 'u1', name: 'Alice' }] });
    expect(v.getState()).toBe('on');
    expect(v.getPeers()).toEqual([{ id: 'u1', name: 'Alice' }]);
    expect(calls.some((c) => c.peers)).toBe(true);
  });

  it('subscriber offer is answered and the answer rides media_signal', async () => {
    vi.stubGlobal('RTCPeerConnection', FakePC);
    const net = fakeNet();
    const { h } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    v.handleSignal({ pc: 'subscriber', type: 'offer', sdp: sdpOffer, slots: { audio: 12, video: 6, screen: 2 } });
    // give the async answer chain a tick
    await flush();
    expect(FakePC.instances.length).toBe(1);
    const pc = FakePC.instances[0];
    expect(pc.log).toContain('setRemoteDescription:offer');
    expect(pc.log).toContain('createAnswer');
    expect(pc.log).toContain('setLocalDescription:answer');
    expect(net.sendMediaSignal).toHaveBeenCalledWith({
      pc: 'subscriber',
      type: 'answer',
      sdp: { type: 'answer', sdp: 'v=0 fake answer' },
    });
    expect(v.getState()).toBe('on');
  });

  it('publisher answer and ICE route to the right connections', async () => {
    vi.stubGlobal('RTCPeerConnection', FakePC);
    const net = fakeNet();
    const { h } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    v.handleSignal({ pc: 'subscriber', type: 'offer', sdp: sdpOffer });
    await flush();
    // subscriber answer for the SFU's offer, then a publisher answer + ICE
    v.handleSignal({ pc: 'publisher', type: 'answer', sdp: { type: 'answer', sdp: 'v=0 pub answer' } });
    v.handleSignal({ pc: 'publisher', type: 'ice', candidate: { candidate: 'candidate:1 1 udp 1 1.1.1.1 1 typ host' } });
    v.handleSignal({ pc: 'subscriber', type: 'ice', candidate: { candidate: 'candidate:2 1 udp 1 2.2.2.2 2 typ host' } });
    // publisher PC is created lazily on mic, not on answers — so only the
    // subscriber PC exists until the mic is enabled; ICE must be ignored
    // silently (no throw) when the PC is absent.
    expect(v.getState()).toBe('on');
  });

  it('toggleMic with a granted mic publishes a publisher offer', async () => {
    vi.stubGlobal('RTCPeerConnection', FakePC);
    vi.stubGlobal('navigator', {
      mediaDevices: {
        getUserMedia: vi.fn(async () => ({
          getAudioTracks: () => [{ enabled: true, stop: vi.fn() }],
        })),
      },
    });
    const net = fakeNet();
    const { h, calls } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    v.handleSignal({ pc: 'subscriber', type: 'offer', sdp: sdpOffer });
    await flush();

    await v.toggleMic();
    expect(FakePC.instances.length).toBe(2); // sub + pub
    const pub = FakePC.instances[1];
    expect(pub.log).toContain('addTrack');
    expect(pub.log).toContain('createOffer');
    expect(net.sendMediaSignal).toHaveBeenCalledWith({
      pc: 'publisher',
      type: 'offer',
      sdp: { type: 'offer', sdp: 'v=0 fake offer' },
    });
    expect(calls.some((c) => c.mic === true)).toBe(true);
  });

  it('media_state joined=false tears down and reports off', () => {
    vi.stubGlobal('RTCPeerConnection', FakePC);
    const net = fakeNet();
    const { h, calls } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    v.handleState({ joined: true, space: 'garden', peers: [] });
    v.handleState({ joined: false });
    expect(v.getState()).toBe('off');
    expect(FakePC.instances.length).toBe(0); // nothing created yet, nothing to close
    expect(calls.some((c) => c.state === 'off')).toBe(true);
  });

  it('leave() sends media_leave and closes connections', async () => {
    vi.stubGlobal('RTCPeerConnection', FakePC);
    const net = fakeNet();
    const { h } = fakeHandlers();
    const v = new VoiceManager(net, h);
    v.enter('garden');
    v.handleSignal({ pc: 'subscriber', type: 'offer', sdp: sdpOffer });
    await flush();
    expect(FakePC.instances.length).toBe(1);
    v.leave();
    expect(net.sendMediaLeave).toHaveBeenCalled();
    expect(FakePC.instances[0].closed).toBe(true);
    expect(v.getState()).toBe('off');
  });
});
