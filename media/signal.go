package media

import (
	"encoding/json"

	"github.com/pion/webrtc/v4"
)

// PCKind identifies which of a peer's two PeerConnections a signal belongs to.
type PCKind string

const (
	// PCPublisher is the connection the client uses to send media up
	// (client offers; the SFU answers). Adding a track here renegotiates —
	// publishing is unlimited per PROTOCOL.md.
	PCPublisher PCKind = "publisher"
	// PCSubscriber is the connection the SFU uses to send media down
	// (SFU offers 12+6+2 pre-negotiated sendonly m-lines once; afterwards
	// tracks are swapped on existing transceivers — never renegotiated).
	PCSubscriber PCKind = "subscriber"
)

// SignalType is the kind of signaling message carried by SignalMsg.
type SignalType string

const (
	SigOffer  SignalType = "offer"
	SigAnswer SignalType = "answer"
	SigICE    SignalType = "ice"
	SigJoined SignalType = "joined"
	SigLeft   SignalType = "left"
	SigSlots  SignalType = "slots"
	SigTopK   SignalType = "topk"
)

// SignalMsg is one unit of WebRTC signaling. The server serializes these into
// the protocol envelope ({v:1,t:"signal",d:{...}}) and relays them between the
// SFU and the peer over the WebSocket; incoming client signals are parsed back
// into SignalMsg and passed to HandleSignal.
type SignalMsg struct {
	Type   SignalType                 `json:"type"`
	PC     PCKind                     `json:"pc"`
	PeerID string                     `json:"peerId"`
	SDP    *webrtc.SessionDescription `json:"sdp,omitempty"`
	// Candidate is one trickle ICE candidate (Type == "ice").
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	// MID / Slot describe which pre-negotiated m-line a media change applies
	// to (subscriber side), e.g. "h-slot-camera-3" / 3.
	MID  string `json:"mid,omitempty"`
	Slot int    `json:"slot,omitempty"`
	// Kind is "audio"|"video"|"screen" (PROTOCOL.md media kinds).
	Kind string `json:"kind,omitempty"`
	// Rung is the requested simulcast layer ("low" default) for video.
	Rung string `json:"rung,omitempty"`
	// Slots is the negotiated layout emitted once per peer on join:
	// {audio:12, video:6, screen:2}.
	Slots *SlotLayout `json:"slots,omitempty"`
}

// SlotLayout mirrors PROTOCOL.md's slots map so the client knows the
// pre-negotiated transceiver counts.
type SlotLayout struct {
	Audio  int `json:"audio"`
	Video  int `json:"video"`
	Screen int `json:"screen"`
}

// ParseSignal decodes a SignalMsg from JSON (the server's envelope payload).
func ParseSignal(data []byte) (SignalMsg, error) {
	var m SignalMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}
