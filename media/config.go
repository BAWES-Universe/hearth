// Package media implements the Hearth SFU media plane: a Pion-based selective
// forwarding unit with pre-negotiated subscriber transceivers (no SDP
// renegotiation), Top-K audio routing, camera simulcast rung selection and
// screen slots. It is a library consumed by the Hearth Go server; it performs
// no network listening of its own — all signaling flows through the server's
// WebSocket relay via the Events channel and HandleSignal.
package media

import (
	"fmt"
	"time"

	"github.com/pion/webrtc/v4"
)

// Default slot counts and Top-K per PROTOCOL.md (frozen):
//
//	pre-negotiated transceivers: 12 recv-audio, 6 recv-camera-video, 2 recv-screen
//	publish unlimited, display adaptive.
const (
	DefaultAudioSlots  = 12
	DefaultVideoSlots  = 6
	DefaultScreenSlots = 2
	DefaultTopK        = 3
	// DefaultHysteresis is how long a Top-K change must persist before the
	// audio re-slot is applied (anti-flap).
	DefaultHysteresis = 500 * time.Millisecond
)

// Config controls the SFU. Zero values are replaced by the defaults above.
type Config struct {
	// TopK is how many audio sources are forwarded to every subscriber.
	// Clamped to [1, AudioSlots].
	TopK int
	// AudioSlots is the number of pre-negotiated recv-audio m-lines per
	// subscriber. Must be >= 1.
	AudioSlots int
	// VideoSlots is the number of pre-negotiated recv-camera-video m-lines.
	VideoSlots int
	// ScreenSlots is the number of pre-negotiated recv-screen m-lines.
	ScreenSlots int
	// Hysteresis delays Top-K re-slots (see DefaultHysteresis).
	Hysteresis time.Duration
	// ICEServers is passed through to every PeerConnection (e.g. STUN/TURN).
	ICEServers []webrtc.ICEServer
	// Logf receives debug lines; defaults to a silent logger.
	Logf func(format string, args ...any)
}

func (c *Config) fill() {
	if c.AudioSlots < 1 {
		c.AudioSlots = DefaultAudioSlots
	}
	if c.VideoSlots < 1 {
		c.VideoSlots = DefaultVideoSlots
	}
	if c.ScreenSlots < 1 {
		c.ScreenSlots = DefaultScreenSlots
	}
	if c.TopK < 1 {
		c.TopK = DefaultTopK
	}
	if c.TopK > c.AudioSlots {
		c.TopK = c.AudioSlots
	}
	if c.Hysteresis <= 0 {
		c.Hysteresis = DefaultHysteresis
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
}

func (c *Config) logf(format string, args ...any) { c.Logf(format, args...) }

// Kinds published through the SFU, mirroring PROTOCOL.md media kinds.
const (
	KindAudio  = "audio"
	KindVideo  = "video"
	KindScreen = "screen"
)

// Rungs for camera simulcast. DefaultRung is what subscribers get unless the
// publisher only offers another rung (then the nearest lower rung is used).
const (
	RungLow    = "low"
	RungMid    = "mid"
	RungHigh   = "high"
	DefaultRung = RungLow
)

var rungPriority = []string{RungLow, RungMid, RungHigh}

// ErrNotFound is returned when a peer/track is unknown.
var ErrNotFound = fmt.Errorf("media: not found")

// ErrNoSlot is returned when every pre-negotiated slot of a kind is busy.
var ErrNoSlot = fmt.Errorf("media: no free slot (all pre-negotiated m-lines in use)")

// ErrBadState is returned when a signaling step arrives out of order.
var ErrBadState = fmt.Errorf("media: bad signaling state")
