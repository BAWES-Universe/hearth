package media

import (
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// Slot is one pre-negotiated subscriber m-line. The slot's local static track
// is bound to the transceiver ONCE at join; re-routing a slot = swapping which
// published track's reader writes into slot.local. No SDP renegotiation ever
// happens on the subscriber connection.
type Slot struct {
	idx      int
	kind     string // KindAudio | KindVideo | KindScreen
	tr       *webrtc.RTPTransceiver
	local    *webrtc.TrackLocalStaticRTP
	boundKey string // pubID|kind|rung of the source currently feeding this slot ("" = idle)
	boundPub string
}

// Peer owns one client's two PeerConnections through the SFU:
//
//	publisher: client sends media up; client offers, SFU answers (renegotiation
//	           allowed here — publishing is unlimited).
//	subscriber: SFU sends media down; SFU offers 12+6+2 sendonly m-lines once;
//	           afterwards slots are re-pointed in-process, never renegotiated.
type Peer struct {
	id       string
	room     *Room
	cfg      Config
	joinedAt time.Time

	pub *webrtc.PeerConnection // client -> SFU (answerer)
	sub *webrtc.PeerConnection // SFU -> client (offerer)

	audioSlots  []*Slot
	videoSlots  []*Slot
	screenSlots []*Slot

	mu     sync.Mutex
	closed bool
}

// emit writes one signaling message into the media events stream.
type emitFn func(SignalMsg)

func (p *Peer) logf(format string, args ...any) {
	p.cfg.logf("[peer %s] "+format, append([]any{p.id}, args...)...)
}

// newPeer builds both PeerConnections. The subscriber connection is
// pre-negotiated immediately: placeholder static tracks are added to 20
// transceivers (12 audio + 6 camera + 2 screen) and an offer is emitted on the
// events channel for the server to relay. Returns the peer and the offer.
func newPeer(room *Room, peerID string, emit emitFn, cfg Config) (*Peer, error) {
	p := &Peer{
		id:       peerID,
		room:     room,
		cfg:      cfg,
		joinedAt: time.Now(),
	}
	pcCfg := webrtc.Configuration{ICEServers: cfg.ICEServers}

	pub, err := webrtc.NewPeerConnection(pcCfg)
	if err != nil {
		return nil, err
	}
	p.pub = pub

	sub, err := webrtc.NewPeerConnection(pcCfg)
	if err != nil {
		_ = pub.Close()
		return nil, err
	}
	p.sub = sub

	// --- publisher connection wiring (SFU answers) ---
	pub.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		p.logf("onTrack: kind=%s stream=%q id=%q rid=%q ssrc=%d", tr.Kind(), tr.StreamID(), tr.ID(), tr.RID(), tr.SSRC())
		room.registerRemoteTrack(peerID, tr)
	})
	pub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand := c.ToJSON()
		emit(SignalMsg{Type: SigICE, PC: PCPublisher, PeerID: peerID, Candidate: &cand})
	})
	pub.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		p.logf("publisher conn state: %s", s)
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateDisconnected {
			room.onPeerGone(peerID)
		}
	})

	// --- subscriber connection wiring (SFU offers, never renegotiates) ---
	sub.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		cand := c.ToJSON()
		emit(SignalMsg{Type: SigICE, PC: PCSubscriber, PeerID: peerID, Candidate: &cand})
	})
	sub.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		p.logf("subscriber conn state: %s", s)
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateDisconnected {
			room.onPeerGone(peerID)
		}
	})

	// Pre-negotiated transceivers — 12 audio, 6 camera video, 2 screen.
	audioCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	videoCodec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	init := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}

	for i := 0; i < cfg.AudioSlots; i++ {
		slot, err := p.addSlot(sub, KindAudio, i, audioCodec, init)
		if err != nil {
			_ = pub.Close()
			_ = sub.Close()
			return nil, err
		}
		p.audioSlots = append(p.audioSlots, slot)
	}
	for i := 0; i < cfg.VideoSlots; i++ {
		slot, err := p.addSlot(sub, KindVideo, i, videoCodec, init)
		if err != nil {
			_ = pub.Close()
			_ = sub.Close()
			return nil, err
		}
		p.videoSlots = append(p.videoSlots, slot)
	}
	for i := 0; i < cfg.ScreenSlots; i++ {
		slot, err := p.addSlot(sub, KindScreen, i, videoCodec, init)
		if err != nil {
			_ = pub.Close()
			_ = sub.Close()
			return nil, err
		}
		p.screenSlots = append(p.screenSlots, slot)
	}

	offer, err := sub.CreateOffer(nil)
	if err != nil {
		_ = pub.Close()
		_ = sub.Close()
		return nil, err
	}
	if err := sub.SetLocalDescription(offer); err != nil {
		_ = pub.Close()
		_ = sub.Close()
		return nil, err
	}

	emit(SignalMsg{
		Type:   SigOffer,
		PC:     PCSubscriber,
		PeerID: peerID,
		SDP:    &offer,
		Slots: &SlotLayout{
			Audio:  cfg.AudioSlots,
			Video:  cfg.VideoSlots,
			Screen: cfg.ScreenSlots,
		},
	})
	return p, nil
}

// addSlot adds one sendonly transceiver pre-bound to a placeholder static
// track. The placeholder gives the m-line a stable msid in the offer so the
// client can identify slots ("h-slot-audio-0", "h-slot-camera-2",
// "h-slot-screen-1").
func (p *Peer) addSlot(pc *webrtc.PeerConnection, kind string, idx int, codec webrtc.RTPCodecCapability, init webrtc.RTPTransceiverInit) (*Slot, error) {
	var (
		msid string
		id   string
	)
	switch kind {
	case KindAudio:
		msid, id = fmt.Sprintf("h-slot-audio-%d", idx), "audio"
	case KindVideo:
		msid, id = fmt.Sprintf("h-slot-camera-%d", idx), "camera"
	case KindScreen:
		msid, id = fmt.Sprintf("h-slot-screen-%d", idx), "screen"
	}
	local, err := webrtc.NewTrackLocalStaticRTP(codec, id, msid)
	if err != nil {
		return nil, err
	}
	tr, err := pc.AddTransceiverFromTrack(local, init)
	if err != nil {
		return nil, err
	}
	return &Slot{idx: idx, kind: kind, tr: tr, local: local}, nil
}

// close tears down both connections and removes all published tracks.
func (p *Peer) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	_ = p.pub.Close()
	_ = p.sub.Close()
	p.room.removePeerTracks(p.id)
}
