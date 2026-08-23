package media

import (
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// Sink is one subscriber-bound output for a published track: the slot's local
// static track. A published track fans every incoming RTP packet out to all of
// its sinks (selective forwarding — payload untouched).
type Sink struct {
	peerID  string
	slotIdx int
	local   *webrtc.TrackLocalStaticRTP
}

// PublishedTrack is one incoming RTP stream from a publisher (one SSRC, i.e.
// one simulcast rung). The SFU runs exactly one reader goroutine per
// PublishedTrack and forwards packets to every attached sink. TrackRemote
// buffers are fresh per read, so passing the packet straight through is safe.
type PublishedTrack struct {
	key    string // pubID|kind|rung
	pubID  string
	kind   string
	rung   string
	remote *webrtc.TrackRemote

	mu    sync.Mutex
	sinks map[*Sink]bool
	done  chan struct{}
	once  sync.Once
}

// newPublishedTrack starts the fan-out reader. The onClose callback fires once
// when the publisher stops sending (stream ended, PC gone) so the room can
// re-slot (Top-K, screen) and unbind subscribers.
func newPublishedTrack(key, pubID, kind, rung string, remote *webrtc.TrackRemote, onClose func()) *PublishedTrack {
	pt := &PublishedTrack{
		key:    key,
		pubID:  pubID,
		kind:   kind,
		rung:   rung,
		remote: remote,
		sinks:  map[*Sink]bool{},
		done:   make(chan struct{}),
	}
	go pt.run(onClose)
	return pt
}

func (pt *PublishedTrack) run(onClose func()) {
	defer func() {
		pt.once.Do(func() { close(pt.done) })
		onClose()
	}()
	for {
		pkt, _, err := pt.remote.ReadRTP()
		if err != nil {
			return
		}
		pt.mu.Lock()
		for s := range pt.sinks {
			// WriteRTP copies the packet header and synchronously hands the
			// payload to the interceptor chain; unbound tracks no-op.
			_ = s.local.WriteRTP(pkt)
		}
		pt.mu.Unlock()
	}
}

// addSink attaches a subscriber slot to this track.
func (pt *PublishedTrack) addSink(s *Sink) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.sinks[s] = true
}

// removeSink detaches a subscriber slot. Safe to call from a re-slot even
// while the reader is mid-write (one stale packet may slip through; it is
// dropped by the unbound slot's WriteRTP no-op path).
func (pt *PublishedTrack) removeSink(peerID string, slotIdx int) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	for s := range pt.sinks {
		if s.peerID == peerID && s.slotIdx == slotIdx {
			delete(pt.sinks, s)
			return
		}
	}
}

// rtpPacket is a re-export guard so tests can build fake packets without
// importing github.com/pion/rtp themselves.
var _ = rtp.Packet{}
