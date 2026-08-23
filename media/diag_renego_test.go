package media

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// DiagRenegoThroughSFU: publish audio (offer 1), then camera tracks (offer 2,
// renegotiation) through the real SFU, wait 3s, and check whether the video
// tracks eventually register. Distinguishes "race" (they register, just late)
// from "renegotiation broken through SFU" (they never register).
func TestDiagRenegoThroughSFU(t *testing.T) {
	m := New(Config{TopK: 1, Hysteresis: 250 * time.Millisecond, Logf: t.Logf})
	A := newTestClient(t, m, "A")
	t.Cleanup(func() { _ = m.Leave("A") })

	audio, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "mic-A")
	if err != nil {
		t.Fatal(err)
	}
	A.addTrackAndOffer(audio)
	stopA := make(chan struct{})
	writeFakeRTP(A, audio, 111, 0xA0000001, 480, stopA)
	waitFor(t, 5*time.Second, func() bool {
		st := m.Stats()
		return len(st.Rooms) > 0 && len(st.Rooms[0].AudioTopK) > 0
	}, "audio registered")

	// offer 2: renegotiation, 1 -> 3 m-lines
	camLow, _ := webrtc.NewTrackLocalStaticRTP(vp8Codec(), "camera", "camera-low")
	camMid, _ := webrtc.NewTrackLocalStaticRTP(vp8Codec(), "camera", "camera-mid")
	A.addTracksAndOffer(camLow, camMid)
	stopCam := make(chan struct{})
	writeFakeRTP(A, camLow, 96, 0xB0000001, 9000, stopCam)
	writeFakeRTP(A, camMid, 96, 0xB0000002, 9000, stopCam)

	time.Sleep(3 * time.Second)
	st := m.Stats()
	t.Logf("video bounds after 3s: %v (inUse=%d)", st.Rooms[0].VideoBounds, st.Rooms[0].VideoSlotsInUse)
	if len(st.Rooms[0].VideoBounds) == 0 {
		t.Fatalf("FAIL: renegotiated video tracks never registered through SFU")
	}
	t.Logf("PASS: renegotiated video tracks registered (late) through SFU")
}
