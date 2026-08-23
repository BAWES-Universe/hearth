package media

import (
	"fmt"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Media is the Hearth SFU media plane. One instance serves all rooms/spaces of
// a server process. It is a library: no listeners, no goroutine pumps beyond
// per-track readers; the server relays signaling between Media and peers over
// its WebSocket using Events() + HandleSignal.
type Media struct {
	cfg     Config
	events  chan SignalMsg
	mu      sync.Mutex
	rooms   map[string]*Room
	peers   map[string]*Peer // peerID -> peer (all rooms)
}

// New creates the SFU. Config zero values fall back to protocol defaults
// (12/6/2 slots, Top-K 3, 500ms hysteresis).
func New(cfg Config) *Media {
	cfg.fill()
	return &Media{
		cfg:    cfg,
		events: make(chan SignalMsg, 1024),
		rooms:  map[string]*Room{},
		peers:  map[string]*Peer{},
	}
}

// Events is the single relay source for the server's WS: every offer, answer
// and ICE candidate the SFU produces for any peer arrives here. The server
// wraps each SignalMsg into the protocol envelope and sends it to the peer;
// incoming client signals are parsed with ParseSignal and fed to HandleSignal.
// The channel is buffered (1024); if the server falls behind, messages are
// dropped and logged rather than blocking the SFU.
func (m *Media) Events() <-chan SignalMsg { return m.events }

func (m *Media) emit(msg SignalMsg) {
	select {
	case m.events <- msg:
	default:
		m.cfg.logf("events queue full — dropped %s for peer %s", msg.Type, msg.PeerID)
	}
}

// Join creates a peer's publisher + subscriber PeerConnections and immediately
// emits the pre-negotiated subscriber offer (12 audio + 6 camera + 2 screen
// m-lines) on Events. The server relays that offer to the client; the client's
// answer comes back through HandleSignal. Until then the peer's subscriber
// connection is inert (it still gathers ICE, which is relayed).
func (m *Media) Join(peerID, roomID string) error {
	m.mu.Lock()
	if _, dup := m.peers[peerID]; dup {
		m.mu.Unlock()
		return fmt.Errorf("media: peer %q already joined", peerID)
	}
	room := m.rooms[roomID]
	if room == nil {
		room = newRoom(roomID, m.cfg, m.emit, m.onPeerGone)
		m.rooms[roomID] = room
	}
	peer, err := newPeer(room, peerID, m.emit, m.cfg)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.peers[peerID] = peer
	room.mu.Lock()
	room.peers[peerID] = peer
	if room.screenSubs[peerID] == nil {
		room.screenSubs[peerID] = map[string]bool{}
	}
	room.mu.Unlock()
	m.mu.Unlock()

	m.emit(SignalMsg{Type: SigJoined, PC: PCSubscriber, PeerID: peerID})
	m.emit(SignalMsg{Type: SigSlots, PC: PCSubscriber, PeerID: peerID, Slots: &SlotLayout{
		Audio: m.cfg.AudioSlots, Video: m.cfg.VideoSlots, Screen: m.cfg.ScreenSlots,
	}})
	return nil
}

// Start is the one-call join-and-publish entry point: joins the peer and, if
// offerSDP (the client's first publisher offer) is given, applies it and
// returns the answer to relay back. Equivalent to Join + HandleSignal(offer).
func (m *Media) Start(peerID, roomID string, offerSDP *webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	if err := m.Join(peerID, roomID); err != nil {
		return nil, err
	}
	if offerSDP == nil {
		return nil, nil
	}
	out, err := m.HandleSignal(peerID, SignalMsg{Type: SigOffer, PC: PCPublisher, PeerID: peerID, SDP: offerSDP})
	if err != nil {
		return nil, err
	}
	for _, o := range out {
		if o.Type == SigAnswer && o.SDP != nil {
			return o.SDP, nil
		}
	}
	return nil, nil
}

// Leave tears down a peer's connections, removes its published tracks and
// re-routes everyone (Top-K, screens). Emits SigLeft.
func (m *Media) Leave(peerID string) error {
	m.mu.Lock()
	peer := m.peers[peerID]
	if peer == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.peers, peerID)
	m.mu.Unlock()

	peer.close()
	m.emit(SignalMsg{Type: SigLeft, PC: PCSubscriber, PeerID: peerID})
	return nil
}

// onPeerGone is invoked by rooms when a connection dies on its own (network
// failure, client close): same cleanup as Leave, driven by the room.
func (m *Media) onPeerGone(peerID string) {
	m.mu.Lock()
	_, ok := m.peers[peerID]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.peers, peerID)
	m.mu.Unlock()

	m.emit(SignalMsg{Type: SigLeft, PC: PCSubscriber, PeerID: peerID})
}

// HandleSignal processes one inbound signaling message from a peer (via the
// server relay). It returns any SignalMsg produced synchronously (the answer
// to a publisher offer) — the same message is also emitted on Events, so a
// server that consumes Events can ignore the return value.
func (m *Media) HandleSignal(peerID string, msg SignalMsg) ([]SignalMsg, error) {
	m.mu.Lock()
	peer := m.peers[peerID]
	m.mu.Unlock()
	if peer == nil {
		return nil, ErrNotFound
	}

	switch msg.Type {
	case SigOffer:
		if msg.PC != PCPublisher || msg.SDP == nil {
			return nil, ErrBadState
		}
		if err := peer.pub.SetRemoteDescription(*msg.SDP); err != nil {
			return nil, err
		}
		answer, err := peer.pub.CreateAnswer(nil)
		if err != nil {
			return nil, err
		}
		if err := peer.pub.SetLocalDescription(answer); err != nil {
			return nil, err
		}
		out := SignalMsg{Type: SigAnswer, PC: PCPublisher, PeerID: peerID, SDP: &answer}
		m.emit(out)
		return []SignalMsg{out}, nil

	case SigAnswer:
		if msg.PC != PCSubscriber || msg.SDP == nil {
			return nil, ErrBadState
		}
		if err := peer.sub.SetRemoteDescription(*msg.SDP); err != nil {
			return nil, err
		}
		return nil, nil

	case SigICE:
		if msg.Candidate == nil {
			return nil, ErrBadState
		}
		switch msg.PC {
		case PCPublisher:
			return nil, peer.pub.AddICECandidate(*msg.Candidate)
		case PCSubscriber:
			return nil, peer.sub.AddICECandidate(*msg.Candidate)
		}
		return nil, ErrBadState
	}
	return nil, ErrBadState
}

// Publish records a client's publish intent (PROTOCOL.md media action
// "publish"). Actual track registration happens automatically when the client
// offers the m-line and RTP starts flowing (onTrack); this call is a
// bookkeeping hint (logged) and validates that the peer exists.
func (m *Media) Publish(peerID, kind string) error {
	m.mu.Lock()
	peer := m.peers[peerID]
	m.mu.Unlock()
	if peer == nil {
		return ErrNotFound
	}
	switch kind {
	case KindAudio, KindVideo, KindScreen:
		peer.logf("publish intent: %s", kind)
		return nil
	}
	return fmt.Errorf("media: unknown kind %q", kind)
}

// Subscribe binds a target's published track to the peer's next free
// pre-negotiated slot of the kind. Never triggers renegotiation.
//
//   - KindAudio: audio is always Top-K routed; this is a no-op (returns 0).
//   - KindVideo: binds target's camera at the default rung (low, or nearest
//     available). Returns the video slot index (0..5) or ErrNoSlot when all 6
//     are busy, ErrNotFound when the target publishes no camera.
//   - KindScreen: marks the target's screen as wanted (client picks which to
//     render); up to 2 screen sources are forwarded per subscriber, newest
//     first. Returns 0 on success.
func (m *Media) Subscribe(peerID, targetID, kind string) (int, error) {
	m.mu.Lock()
	peer := m.peers[peerID]
	m.mu.Unlock()
	if peer == nil {
		return -1, ErrNotFound
	}
	switch kind {
	case KindAudio:
		return 0, nil
	case KindVideo:
		return peer.room.subscribeVideo(peer, targetID, DefaultRung)
	case KindScreen:
		return 0, peer.room.subscribeScreen(peer, targetID)
	}
	return -1, fmt.Errorf("media: unknown kind %q", kind)
}

// Unsubscribe detaches a target's track from the peer's slots (video/screen).
// Audio is Top-K managed and ignores this call.
func (m *Media) Unsubscribe(peerID, targetID, kind string) error {
	m.mu.Lock()
	peer := m.peers[peerID]
	m.mu.Unlock()
	if peer == nil {
		return ErrNotFound
	}
	switch kind {
	case KindAudio:
		return nil
	case KindVideo:
		peer.room.unsubscribeVideo(peer, targetID)
		return nil
	case KindScreen:
		peer.room.unsubscribeScreen(peer, targetID)
		return nil
	}
	return fmt.Errorf("media: unknown kind %q", kind)
}

// SetTopK changes how many audio sources are forwarded to every subscriber
// (clamped to [1, AudioSlots]) and re-evaluates with hysteresis.
func (m *Media) SetTopK(k int) {
	m.mu.Lock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.mu.Unlock()
	for _, r := range rooms {
		r.topk.SetK(k)
	}
}

// SetRung switches an existing camera subscription to another simulcast rung
// (low|mid|high). Falls back to the nearest lower rung the publisher offers.
// Returns ErrNotFound if the peer has no camera subscription to targetID.
func (m *Media) SetRung(peerID, targetID, rung string) error {
	m.mu.Lock()
	peer := m.peers[peerID]
	m.mu.Unlock()
	if peer == nil {
		return ErrNotFound
	}
	return peer.room.setRung(peer, targetID, rung)
}

// Stats is a point-in-time view used by the server for /health and debugging.
type Stats struct {
	Rooms []roomStats `json:"rooms"`
}

func (m *Media) Stats() Stats {
	m.mu.Lock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.mu.Unlock()
	var st Stats
	for _, r := range rooms {
		st.Rooms = append(st.Rooms, r.snapshot())
	}
	return st
}
