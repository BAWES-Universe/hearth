package media

import (
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// TestRenegoMinimal: does a plain pion answerer fire onTrack for a track added
// by the offerer in a SECOND offer (renegotiation)? Isolates pion behavior
// from our SFU wiring.
//
// This documents the publisher side of the frozen design (PROTOCOL.md:
// "publish unlimited"): the SFU ANSWERS publisher offers, so publisher-side
// renegotiation must work in pion for the SFU to rely on it. (The no-
// renegotiation rule is SUBSCRIBER-side only — the loopback test proves that.)
//
// Pion behavior note: OnTrack fires on the FIRST RTP packet for a track, not
// at SDP negotiation time — so this test must write RTP for the round-1
// (audio) track too, or the answerer never observes it.
func TestRenegoMinimal(t *testing.T) {
	cfg := webrtc.Configuration{}

	offerer, _ := webrtc.NewPeerConnection(cfg)
	answerer, _ := webrtc.NewPeerConnection(cfg)
	t.Cleanup(func() { _ = offerer.Close(); _ = answerer.Close() })

	var mu sync.Mutex
	got := []string{}
	answerer.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		mu.Lock()
		got = append(got, tr.Kind().String()+"|"+tr.StreamID())
		mu.Unlock()
		t.Logf("answerer onTrack: %s %s", tr.Kind(), tr.StreamID())
	})

	relay := func(from, to *webrtc.PeerConnection) {
		from.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			ci := c.ToJSON()
			_ = to.AddICECandidate(ci)
		})
	}
	relay(offerer, answerer)
	relay(answerer, offerer)

	exchange := func() {
		offer, err := offerer.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := offerer.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		if err := answerer.SetRemoteDescription(offer); err != nil {
			t.Fatal(err)
		}
		ans, err := answerer.CreateAnswer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := answerer.SetLocalDescription(ans); err != nil {
			t.Fatal(err)
		}
		if err := offerer.SetRemoteDescription(ans); err != nil {
			t.Fatal(err)
		}
	}

	// Pump RTP into a local track until done is closed (stands in for the
	// encoder). OnTrack only fires once RTP arrives, so every track we want
	// to observe must actually be written.
	done := make(chan struct{})
	defer close(done)
	pump := func(tr *webrtc.TrackLocalStaticRTP, pt uint8, ssrc uint32) {
		go func() {
			var seq uint16
			var ts uint32
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
				}
				_ = tr.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}, Payload: []byte{1, 2, 3}})
				seq++
				ts += 9000
			}
		}()
	}

	// offer 1: audio
	audio, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if _, err := offerer.AddTrack(audio); err != nil {
		t.Fatal(err)
	}
	exchange()
	pump(audio, 111, 0xA0000001)
	t.Logf("round 1 done (audio offered)")

	// offer 2: video (renegotiation)
	video, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "camera", "camera-low")
	if _, err := offerer.AddTrack(video); err != nil {
		t.Fatal(err)
	}
	exchange()
	pump(video, 96, 0xB0000001)
	t.Logf("round 2 done (video offered via renegotiation)")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("tracks seen by answerer: %v", got)
	if len(got) < 2 {
		t.Fatalf("FAIL: renegotiated video track not seen; got %v", got)
	}
	t.Logf("PASS: pion renegotiation delivers the second track (round-1 audio + round-2 video)")
}
