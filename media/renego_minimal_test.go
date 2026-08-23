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

	// offer 1: audio
	audio, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}, "audio", "mic")
	if _, err := offerer.AddTrack(audio); err != nil {
		t.Fatal(err)
	}
	exchange()
	t.Logf("round 1 done")

	// offer 2: video (renegotiation)
	video, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "camera", "camera-low")
	if _, err := offerer.AddTrack(video); err != nil {
		t.Fatal(err)
	}
	exchange()
	t.Logf("round 2 done")

	// write video RTP for 500ms
	done := make(chan struct{})
	defer close(done)
	go func() {
		seq := uint16(0)
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
			}
			_ = video.WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: uint32(seq) * 9000, SSRC: 0xB0000001}, Payload: []byte{1, 2, 3}})
			seq++
		}
	}()

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
	t.Logf("PASS: pion renegotiation delivers the second track")
}
