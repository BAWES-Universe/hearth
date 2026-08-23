package media

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// ---------------------------------------------------------------------------
// In-process test client. It plays the role of both the server's WS relay and
// the browser client: it consumes Media events, applies SDP/ICE to its own two
// PeerConnections, and feeds its own offers/answers/candidates back through
// HandleSignal. No network listeners, no external signaling.
// ---------------------------------------------------------------------------

type testClient struct {
	t  *testing.T
	id string
	m  *Media

	pub *webrtc.PeerConnection // client-side publisher PC (offers media up)
	sub *webrtc.PeerConnection // client-side subscriber PC (answers SFU offer)

	receivedAudio   atomic.Int64 // packets on h-slot-audio-* m-lines
	receivedVideo   atomic.Int64 // packets on h-slot-camera-* m-lines
	receivedScreen  atomic.Int64 // packets on h-slot-screen-* m-lines
	subOffersSeen   atomic.Int64 // subscriber offers relayed to us (must stay 1)
	subMlines       atomic.Int64 // m= lines in the first subscriber offer
	subSDPSeen      atomic.Bool
}

func newTestClient(t *testing.T, m *Media, id string) *testClient {
	t.Helper()
	c := &testClient{t: t, id: id, m: m}
	pcCfg := webrtc.Configuration{}
	var err error

	c.pub, err = webrtc.NewPeerConnection(pcCfg)
	if err != nil {
		t.Fatalf("%s: pub PC: %v", id, err)
	}
	c.sub, err = webrtc.NewPeerConnection(pcCfg)
	if err != nil {
		t.Fatalf("%s: sub PC: %v", id, err)
	}

	// Our candidates -> SFU.
	c.pub.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		_, _ = m.HandleSignal(id, SignalMsg{Type: SigICE, PC: PCPublisher, PeerID: id, Candidate: cand.ToJSON()})
	})
	c.sub.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		_, _ = m.HandleSignal(id, SignalMsg{Type: SigICE, PC: PCSubscriber, PeerID: id, Candidate: cand.ToJSON()})
	})

	// Incoming media from the SFU -> counters, classified by the msid the SFU
	// pre-negotiated (h-slot-audio-*, h-slot-camera-*, h-slot-screen-*).
	c.sub.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver, _ *webrtc.RTPTransceiver) {
		stream := tr.StreamID()
		c.t.Logf("%s: sub onTrack stream=%q id=%q kind=%s", id, stream, tr.ID(), tr.Kind())
		go func() {
			for {
				if _, _, err := tr.ReadRTP(); err != nil {
					return
				}
				switch {
				case strings.HasPrefix(stream, "h-slot-audio"):
					c.receivedAudio.Add(1)
				case strings.HasPrefix(stream, "h-slot-screen"):
					c.receivedScreen.Add(1)
				default:
					c.receivedVideo.Add(1)
				}
			}
		}()
	})

	if err := m.Join(id, "room1"); err != nil {
		t.Fatalf("%s: Join: %v", id, err)
	}
	c.runRelay()
	return c
}

// runRelay consumes Media events addressed to this client and drives its PCs.
func (c *testClient) runRelay() {
	go func() {
		for ev := range c.m.Events() {
			if ev.PeerID != c.id {
				continue
			}
			switch ev.Type {
			case SigOffer:
				if ev.PC != PCSubscriber || ev.SDP == nil {
					continue
				}
				c.subOffersSeen.Add(1)
				mlines := countMlines(ev.SDP.SDP)
				if !c.subSDPSeen.Swap(true) {
					c.subMlines.Store(int64(mlines))
				}
				c.t.Logf("%s: subscriber offer: %d m-lines", c.id, mlines)
				if err := c.sub.SetRemoteDescription(*ev.SDP); err != nil {
					c.t.Errorf("%s: SetRemoteDescription(sub offer): %v", c.id, err)
					return
				}
				ans, err := c.sub.CreateAnswer(nil)
				if err != nil {
					c.t.Errorf("%s: CreateAnswer: %v", c.id, err)
					return
				}
				if err := c.sub.SetLocalDescription(ans); err != nil {
					c.t.Errorf("%s: SetLocalDescription(answer): %v", c.id, err)
					return
				}
				if _, err := c.m.HandleSignal(c.id, SignalMsg{Type: SigAnswer, PC: PCSubscriber, PeerID: c.id, SDP: &ans}); err != nil {
					c.t.Errorf("%s: relay answer: %v", c.id, err)
					return
				}
			case SigICE:
				if ev.Candidate == nil {
					continue
				}
				pc := c.sub
				if ev.PC == PCPublisher {
					pc = c.pub
				}
				if err := pc.AddICECandidate(*ev.Candidate); err != nil {
					c.t.Logf("%s: AddICECandidate(%s): %v", c.id, ev.PC, err)
				}
			}
		}
	}()
}

// addTrackAndOffer publishes one track on the publisher PC (offer/answer
// round-trip through the SFU; renegotiation on the publisher side is allowed).
func (c *testClient) addTrackAndOffer(tr *webrtc.TrackLocalStaticRTP) {
	c.addTracksAndOffer(tr)
}

func (c *testClient) addTracksAndOffer(tracks ...*webrtc.TrackLocalStaticRTP) {
	c.t.Helper()
	for _, tr := range tracks {
		if _, err := c.pub.AddTrack(tr); err != nil {
			c.t.Fatalf("%s: AddTrack: %v", c.id, err)
		}
	}
	offer, err := c.pub.CreateOffer(nil)
	if err != nil {
		c.t.Fatalf("%s: CreateOffer(pub): %v", c.id, err)
	}
	if err := c.pub.SetLocalDescription(offer); err != nil {
		c.t.Fatalf("%s: SetLocalDescription(pub offer): %v", c.id, err)
	}
	out, err := c.m.HandleSignal(c.id, SignalMsg{Type: SigOffer, PC: PCPublisher, PeerID: c.id, SDP: &offer})
	if err != nil {
		c.t.Fatalf("%s: publisher offer -> SFU: %v", c.id, err)
	}
	for _, o := range out {
		if o.Type == SigAnswer && o.SDP != nil {
			if err := c.pub.SetRemoteDescription(*o.SDP); err != nil {
				c.t.Fatalf("%s: SetRemoteDescription(pub answer): %v", c.id, err)
			}
		}
	}
}

// writeFakeRTP pumps synthetic RTP packets into a local track until stop is
// closed — stands in for the client's mic/camera/screen encoder.
func writeFakeRTP(c *testClient, tr *webrtc.TrackLocalStaticRTP, pt uint8, ssrc uint32, tsStep uint32, stop chan struct{}) {
	go func() {
		var seq uint16
		var ts uint32
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version: 2, PayloadType: pt, SequenceNumber: seq,
					Timestamp: ts, SSRC: ssrc,
				},
				Payload: []byte{0xde, 0xad, 0xbe, 0xef, byte(seq & 0xff)},
			}
			if err := tr.WriteRTP(pkt); err != nil {
				c.t.Logf("%s: WriteRTP: %v", c.id, err)
				return
			}
			seq++
			ts += tsStep
		}
	}()
}

func countMlines(sdp string) int {
	n := 0
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(line, "m=") {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

// ---------------------------------------------------------------------------
// The loopback test: two in-process clients through the SFU.
// ---------------------------------------------------------------------------

func TestLoopback(t *testing.T) {
	m := New(Config{TopK: 1, Hysteresis: 250 * time.Millisecond, Logf: t.Logf})

	A := newTestClient(t, m, "A")
	B := newTestClient(t, m, "B")
	t.Cleanup(func() { _ = m.Leave("A"); _ = m.Leave("B") })

	// --- 1. Pre-negotiated subscriber offer: exactly 20 m-lines (12 audio +
	// 6 camera + 2 screen), and it is the ONLY offer the subscriber ever gets.
	waitFor(t, 5*time.Second, func() bool { return B.subMlines.Load() > 0 }, "B subscriber offer")
	if got := B.subMlines.Load(); got != 20 {
		t.Fatalf("FAIL: subscriber offer has %d m-lines, want 20 (12 audio + 6 camera + 2 screen)", got)
	}
	t.Logf("OK: subscriber offer = 20 pre-negotiated m-lines (12 audio + 6 camera + 2 screen)")
	offersAtStart := B.subOffersSeen.Load()

	// --- 2. Audio: A publishes, B receives via Top-K (K=1).
	audio, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "mic-A")
	if err != nil {
		t.Fatal(err)
	}
	A.addTrackAndOffer(audio)
	stopA := make(chan struct{})
	writeFakeRTP(A, audio, 111, 0xA0000001, 480, stopA) // 10ms @ 48kHz

	waitFor(t, 10*time.Second, func() bool { return B.receivedAudio.Load() > 0 }, "B receives A audio via Top-K")
	t.Logf("OK: Top-K audio flows A -> SFU -> B (%d packets)", B.receivedAudio.Load())

	// --- 3. Video: explicit subscribe, simulcast rung fallback + SetRung.
	camLow, err := webrtc.NewTrackLocalStaticRTP(vp8Codec(), "camera", "camera-low")
	if err != nil {
		t.Fatal(err)
	}
	camMid, err := webrtc.NewTrackLocalStaticRTP(vp8Codec(), "camera", "camera-mid")
	if err != nil {
		t.Fatal(err)
	}
	A.addTracksAndOffer(camLow, camMid)
	stopCam := make(chan struct{})
	writeFakeRTP(A, camLow, 96, 0xB0000001, 9000, stopCam)
	writeFakeRTP(A, camMid, 96, 0xB0000002, 9000, stopCam)

	idx, err := m.Subscribe("B", "A", KindVideo)
	if err != nil {
		t.Fatalf("Subscribe video: %v", err)
	}
	if idx != 0 {
		t.Fatalf("video slot = %d, want 0", idx)
	}
	waitFor(t, 10*time.Second, func() bool { return B.receivedVideo.Load() > 0 }, "B receives A camera (low)")
	t.Logf("OK: camera video flows A -> SFU -> B on video slot %d (default rung low, %d packets)", idx, B.receivedVideo.Load())

	// Request rung "mid" (exists) then "high" (absent -> nearest lower).
	if err := m.SetRung("B", "A", RungMid); err != nil {
		t.Fatalf("SetRung(mid): %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	v1 := B.receivedVideo.Load()
	waitFor(t, 10*time.Second, func() bool { return B.receivedVideo.Load() > v1 }, "video still flows after SetRung(mid)")
	if err := m.SetRung("B", "A", RungHigh); err != nil {
		t.Fatalf("SetRung(high): %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	v2 := B.receivedVideo.Load()
	waitFor(t, 10*time.Second, func() bool { return B.receivedVideo.Load() > v2 }, "video still flows after SetRung(high) fallback")
	t.Logf("OK: SetRung mid/high with fallback — video keeps flowing")

	// --- 4. Screen: publish + subscribe; client picks, SFU forwards.
	screen, err := webrtc.NewTrackLocalStaticRTP(vp8Codec(), "screen", "screen-A")
	if err != nil {
		t.Fatal(err)
	}
	A.addTrackAndOffer(screen)
	stopScreen := make(chan struct{})
	writeFakeRTP(A, screen, 96, 0xC0000001, 9000, stopScreen)

	if _, err := m.Subscribe("B", "A", KindScreen); err != nil {
		t.Fatalf("Subscribe screen: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return B.receivedScreen.Load() > 0 }, "B receives A screen")
	t.Logf("OK: screen flows A -> SFU -> B on screen slot (%d packets)", B.receivedScreen.Load())

	// --- 5. Top-K hysteresis: C joins with audio; K=1 keeps A. A leaves; C
	// takes the slot after the hysteresis window.
	C := newTestClient(t, m, "C")
	t.Cleanup(func() { _ = m.Leave("C") })
	audioC, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "mic-C")
	if err != nil {
		t.Fatal(err)
	}
	C.addTrackAndOffer(audioC)
	stopC := make(chan struct{})
	writeFakeRTP(C, audioC, 111, 0xA0000002, 480, stopC)

	time.Sleep(600 * time.Millisecond) // past hysteresis; selection must stay [A]
	st := m.Stats()
	if len(st.Rooms) != 1 || len(st.Rooms[0].AudioTopK) != 1 || st.Rooms[0].AudioTopK[0] != "A" {
		t.Fatalf("FAIL: Top-K selection = %v, want [A] (C must not displace A while A is live)", st.Rooms[0].AudioTopK)
	}
	t.Logf("OK: with K=1 and A+C both live, selection stays [A] (join-order priority)")

	close(stopA) // silence A's writer before it leaves
	_ = m.Leave("A")
	a3 := B.receivedAudio.Load()
	waitFor(t, 10*time.Second, func() bool { return B.receivedAudio.Load() > a3 }, "B receives C audio after A leaves")
	st = m.Stats()
	if len(st.Rooms[0].AudioTopK) != 1 || st.Rooms[0].AudioTopK[0] != "C" {
		t.Fatalf("FAIL: after A leaves, Top-K = %v, want [C]", st.Rooms[0].AudioTopK)
	}
	t.Logf("OK: after A leaves, Top-K re-slots to [C] (%d packets received by B)", B.receivedAudio.Load()-a3)

	// --- 6. Zero renegotiation on the subscriber side, end to end.
	if got := B.subOffersSeen.Load(); got != offersAtStart {
		t.Fatalf("FAIL: subscriber renegotiation detected: offers seen = %d, want %d", got, offersAtStart)
	}
	if got := B.subMlines.Load(); got != 20 {
		t.Fatalf("FAIL: subscriber SDP changed: %d m-lines now, want 20", got)
	}
	t.Logf("OK: zero subscriber renegotiation across audio/video/screen/topk (offer count %d, 20 m-lines stable)", offersAtStart)

	close(stopCam)
	close(stopScreen)
	close(stopC)
	t.Logf("PASS: loopback OK — audio (Top-K), video (rung select), screen, topk re-slot, no renegotiation")
}

func vp8Codec() webrtc.RTPCodecCapability {
	return webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
}

var _ = fmt.Sprintf
