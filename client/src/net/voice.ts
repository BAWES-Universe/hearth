// VoiceManager — the client half of the T2 media plane (docs/MEDIA.md).
//
// Two PeerConnections, mirroring the SFU side:
//   publisher:  mic audio up; the client offers (renegotiation allowed).
//   subscriber: SFU offers 12+6+2 pre-negotiated sendonly m-lines once; the
//               client answers and receives Top-K audio on recvonly tracks.
//
// Signaling SDP/ICE rides the hub's additive media_signal envelopes; audio
// payloads flow peer-to-peer through the SFU. All WebRTC objects are created
// lazily and guarded so the class is unit-testable in node (no DOM).

import type { MediaSignalMsg, MediaStateMsg } from './protocol';
import type { Net } from './ws';

export type VoiceState = 'off' | 'joining' | 'on' | 'error';

export interface VoicePeer {
  id: string;
  name: string;
}

export interface VoiceHandlers {
  onState(state: VoiceState, space?: string): void;
  onPeers(peers: VoicePeer[]): void;
  onMic(micOn: boolean): void;
  onSpeaking(speaking: boolean): void;
}

const ICE_SERVERS: RTCConfiguration = {
  iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
};

// audio levels below this RMS threshold count as silence
const SPEAK_THRESHOLD = 0.02;

export class VoiceManager {
  private state: VoiceState = 'off';
  private space = '';
  private peers: VoicePeer[] = [];
  private micOn = false;
  private remoteSpeaking = false;

  private micStream: MediaStream | null = null;
  private pub: RTCPeerConnection | null = null;
  private sub: RTCPeerConnection | null = null;

  private ctx: AudioContext | null = null;
  private analysers: AnalyserNode[] = [];
  private raf = 0;

  constructor(
    private net: Net,
    private h: VoiceHandlers,
  ) {}

  /** Current bubble state (for the UI). */
  getState(): VoiceState {
    return this.state;
  }

  /** Current bubble members. */
  getPeers(): VoicePeer[] {
    return this.peers;
  }

  /** Join the voice bubble of a space. Idempotent; re-join moves bubbles. */
  enter(space: string): void {
    if (this.state !== 'off' && this.state !== 'error') {
      this.teardown(); // leave the old bubble's connections, join fresh
    }
    this.space = space;
    this.setState('joining', space);
    this.net.sendMediaJoin(space);
  }

  /** Leave the voice bubble (server tears down the SFU connections too). */
  leave(): void {
    this.net.sendMediaLeave();
    this.teardown();
    this.setState('off');
  }

  /** Mic toggle — must be called from a user gesture (getUserMedia). */
  async toggleMic(): Promise<void> {
    if (this.micOn) {
      this.micDisable();
    } else {
      await this.micEnable();
    }
  }

  // ---------------------------------------------------------------- inbound

  /** Route one server->client media_signal frame. */
  handleSignal(msg: MediaSignalMsg): void {
    switch (msg.type) {
      case 'offer':
        if (msg.pc === 'subscriber' && msg.sdp) void this.onSubscriberOffer(msg);
        break;
      case 'answer': {
        const pc = this.pcOf(msg.pc);
        if (pc && msg.sdp) {
          void pc.setRemoteDescription(msg.sdp as RTCSessionDescriptionInit).catch((e) =>
            console.warn('[voice] setRemoteDescription(answer) failed', e),
          );
        }
        break;
      }
      case 'ice': {
        const pc = this.pcOf(msg.pc);
        if (pc && msg.candidate) {
          void pc.addIceCandidate(msg.candidate as RTCIceCandidateInit).catch((e) =>
            console.warn('[voice] addIceCandidate failed', e),
          );
        }
        break;
      }
      default:
        break; // joined/left/slots/topk are server bookkeeping — no client action
    }
  }

  /** Route one server->client media_state frame (bubble membership). */
  handleState(msg: MediaStateMsg): void {
    if (msg.joined === false) {
      this.setState('off');
      this.teardown();
      return;
    }
    if (msg.peers) {
      this.peers = msg.peers;
      this.h.onPeers(this.peers);
    }
    if (msg.space) this.space = msg.space;
    this.setState('on', this.space);
  }

  // ---------------------------------------------------------------- SFU side

  /** Subscriber offer from the SFU: answer it. No renegotiation ever here. */
  private async onSubscriberOffer(msg: MediaSignalMsg): Promise<void> {
    const sub = this.ensureSub();
    try {
      await sub.setRemoteDescription(msg.sdp as RTCSessionDescriptionInit);
      const answer = await sub.createAnswer();
      await sub.setLocalDescription(answer);
      this.net.sendMediaSignal({ pc: 'subscriber', type: 'answer', sdp: answer });
      this.setState('on', this.space);
    } catch (e) {
      console.warn('[voice] subscriber offer/answer failed', e);
      this.setState('error');
    }
  }

  private ensureSub(): RTCPeerConnection {
    if (this.sub) return this.sub;
    const sub = new RTCPeerConnection(ICE_SERVERS);
    this.sub = sub;
    sub.onicecandidate = (ev) => {
      if (ev.candidate) {
        this.net.sendMediaSignal({ pc: 'subscriber', type: 'ice', candidate: ev.candidate.toJSON() });
      }
    };
    sub.ontrack = (ev) => {
      const stream = ev.streams[0];
      if (ev.track.kind === 'audio' && stream) this.attachAudio(stream);
    };
    return sub;
  }

  private pcOf(pc: 'publisher' | 'subscriber'): RTCPeerConnection | null {
    return pc === 'publisher' ? this.pub : this.sub;
  }

  private ensurePub(): RTCPeerConnection {
    if (this.pub) return this.pub;
    const pub = new RTCPeerConnection(ICE_SERVERS);
    this.pub = pub;
    pub.onicecandidate = (ev) => {
      if (ev.candidate) {
        this.net.sendMediaSignal({ pc: 'publisher', type: 'ice', candidate: ev.candidate.toJSON() });
      }
    };
    return pub;
  }

  // ---------------------------------------------------------------- mic

  private async micEnable(): Promise<void> {
    const pub = this.ensurePub();
    try {
      if (!this.micStream) {
        this.micStream = await navigator.mediaDevices.getUserMedia({ audio: true });
      }
      const track = this.micStream.getAudioTracks()[0];
      if (!track) throw new Error('getUserMedia returned no audio track');
      // re-adding an already-sent track throws — only add once per stream
      const alreadySent = pub.getSenders().some((s) => s.track === track);
      if (!alreadySent) {
        pub.addTrack(track, this.micStream); // first add triggers renegotiation
      } else {
        track.enabled = true;
      }
      this.micOn = true;
      this.h.onMic(true);
      this.bumpSpeaking();
      // renegotiate: client offers, SFU answers (publisher PC, unlimited)
      const offer = await pub.createOffer();
      await pub.setLocalDescription(offer);
      this.net.sendMediaSignal({ pc: 'publisher', type: 'offer', sdp: offer });
    } catch (e) {
      console.warn('[voice] mic enable failed', e);
      this.setState('error');
    }
  }

  private micDisable(): void {
    // Keep the track on the connection but silenced — the SFU's Top-K keeps
    // forwarding it (join-order placeholder levels). Removing it would need
    // track-removal semantics the SFU doesn't expose yet; documented in
    // docs/MEDIA.md under limitations.
    if (this.micStream) {
      this.micStream.getAudioTracks().forEach((t) => {
        t.enabled = false;
      });
    }
    this.micOn = false;
    this.h.onMic(false);
    this.bumpSpeaking();
  }

  private bumpSpeaking(): void {
    this.h.onSpeaking(this.micOn || this.remoteSpeaking);
  }

  // ---------------------------------------------------------------- audio out

  /** Attach one subscriber audio stream: analyser for the speaking indicator. */
  private attachAudio(stream: MediaStream): void {
    if (typeof AudioContext === 'undefined') return; // node/tests
    if (!this.ctx) this.ctx = new AudioContext();
    const src = this.ctx.createMediaStreamSource(stream);
    const analyser = this.ctx.createAnalyser();
    analyser.fftSize = 256;
    src.connect(analyser);
    analyser.connect(this.ctx.destination);
    this.analysers.push(analyser);
    this.startLevelLoop();
  }

  private startLevelLoop(): void {
    if (this.raf) return;
    const buf = new Uint8Array(128);
    const loop = () => {
      this.raf = requestAnimationFrame(loop);
      let peak = 0;
      for (const a of this.analysers) {
        a.getByteTimeDomainData(buf);
        for (let i = 0; i < buf.length; i++) {
          const v = (buf[i] - 128) / 128;
          const sq = v * v;
          if (sq > peak) peak = sq;
        }
      }
      const speaking = peak > SPEAK_THRESHOLD * SPEAK_THRESHOLD;
      if (speaking !== this.remoteSpeaking) {
        this.remoteSpeaking = speaking;
        this.bumpSpeaking();
      }
    };
    loop();
  }

  // ---------------------------------------------------------------- teardown

  private teardown(): void {
    if (this.raf) {
      cancelAnimationFrame(this.raf);
      this.raf = 0;
    }
    this.analysers = [];
    if (this.ctx && this.ctx.state !== 'closed') void this.ctx.close();
    this.ctx = null;
    if (this.micStream) {
      this.micStream.getAudioTracks().forEach((t) => t.stop());
      this.micStream = null;
    }
    if (this.pub) {
      this.pub.close();
      this.pub = null;
    }
    if (this.sub) {
      this.sub.close();
      this.sub = null;
    }
    this.micOn = false;
    this.remoteSpeaking = false;
    this.h.onMic(false);
    this.h.onSpeaking(false);
  }

  private setState(s: VoiceState, space?: string): void {
    this.state = s;
    this.h.onState(s, space ?? this.space);
  }
}
