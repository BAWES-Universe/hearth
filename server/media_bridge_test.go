package main

// T2 media bridge acceptance tests (docs/MEDIA.md):
//  1. voice-bubble join/leave over the live WS hub — media_state membership
//     broadcasts (joined, peers list) to bubble members.
//  2. SDP offer/answer round-trips through the LIVE WS hub into the media
//     room: the SFU's pre-negotiated subscriber offer reaches the client, the
//     client's answer is accepted, and a real publisher offer comes back with
//     a valid answer — no direct calls to the media package anywhere.
//  3. media_signal before media_join is rejected (bubble membership gate).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// readUntil returns the first message of envelope type typ, draining others.
func (c *testWSClient) readUntil(typ string, timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		remain := time.Until(deadline)
		if remain < 0 {
			break
		}
		m := c.next(remain)
		if m == nil {
			return nil
		}
		if m["t"] == typ {
			return m
		}
	}
	return nil
}

// dMap extracts the envelope payload as map[string]any.
func dMap(m map[string]any) map[string]any {
	d, _ := m["d"].(map[string]any)
	return d
}

// sdpOf extracts {type,sdp} from a media_signal payload.
func sdpOf(d map[string]any) *webrtc.SessionDescription {
	s, ok := d["sdp"].(map[string]any)
	if !ok {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	var sd webrtc.SessionDescription
	if err := json.Unmarshal(b, &sd); err != nil {
		return nil
	}
	return &sd
}

// expectNoError asserts no server error envelope arrives within the window.
func expectNoError(t *testing.T, c *testWSClient, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		m := c.next(time.Until(deadline))
		if m == nil {
			return
		}
		if m["t"] == "error" {
			t.Fatalf("unexpected server error: %v", dMap(m))
		}
	}
}

// joinBubble joins a client's voice bubble and waits for its media_state ack.
func joinBubble(t *testing.T, c *testWSClient) map[string]any {
	t.Helper()
	c.send("media_join", map[string]any{})
	st := c.readUntil("media_state", 8*time.Second)
	if st == nil {
		t.Fatal("no media_state after media_join")
	}
	d := dMap(st)
	if d["joined"] != true {
		t.Fatalf("media_state joined = %v, want true", d["joined"])
	}
	return st
}

// peerNames returns the ids in a media_state peers payload.
func peerIDs(d map[string]any) []string {
	peers, _ := d["peers"].([]any)
	var out []string
	for _, p := range peers {
		if pm, ok := p.(map[string]any); ok {
			if id, ok := pm["id"].(string); ok {
				out = append(out, id)
			}
		}
	}
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestMediaBubbleJoinAndMembership(t *testing.T) {
	_, wsURL := testWSServer(t)

	a := dialTestWS(t, wsURL, "t2-dev-a", "Alice", "garden")
	defer a.close()
	b := dialTestWS(t, wsURL, "t2-dev-b", "Bob", "garden")
	defer b.close()

	// A joins the bubble: state ack carries A alone.
	joinBubble(t, a)
	// B joins: B's ack carries both members; A is broadcast the update.
	stB := dMap(joinBubble(t, b))
	if !contains(peerIDs(stB), a.selfID) || !contains(peerIDs(stB), b.selfID) {
		t.Fatalf("B membership = %v, want both %s and %s", peerIDs(stB), a.selfID, b.selfID)
	}
	stA := dMap(a.readUntil("media_state", 8*time.Second))
	if stA == nil {
		t.Fatal("no media_state broadcast to A")
	}
	if !contains(peerIDs(stA), a.selfID) || !contains(peerIDs(stA), b.selfID) {
		t.Fatalf("A membership = %v, want both %s and %s", peerIDs(stA), a.selfID, b.selfID)
	}

	// A leaves: A gets joined:false, B gets the shrunk peer list.
	a.send("media_leave", map[string]any{})
	stAL := dMap(a.readUntil("media_state", 8*time.Second))
	if stAL == nil || stAL["joined"] != false {
		t.Fatalf("A leave ack = %v, want joined:false", stAL)
	}
	stBL := dMap(b.readUntil("media_state", 8*time.Second))
	if stBL == nil {
		t.Fatal("no media_state after A left")
	}
	if contains(peerIDs(stBL), a.selfID) {
		t.Fatalf("B membership after A left = %v, still contains %s", peerIDs(stBL), a.selfID)
	}
	if !contains(peerIDs(stBL), b.selfID) {
		t.Fatalf("B membership after A left = %v, missing %s", peerIDs(stBL), b.selfID)
	}
}

func TestMediaSDPOfferAnswerThroughHub(t *testing.T) {
	_, wsURL := testWSServer(t)
	a := dialTestWS(t, wsURL, "t2-dev-c", "Cara", "garden")
	defer a.close()

	joinBubble(t, a)

	// 1) The SFU's pre-negotiated subscriber offer (12+6+2) arrives over the
	//    hub as media_signal offer with a slots payload.
	offerMsg := a.readUntil("media_signal", 8*time.Second)
	var subOffer *webrtc.SessionDescription
	for offerMsg != nil {
		d := dMap(offerMsg)
		if d["pc"] == "subscriber" && d["type"] == "offer" {
			subOffer = sdpOf(d)
			if d["slots"] == nil {
				t.Fatal("subscriber offer missing slots payload")
			}
			break
		}
		offerMsg = a.readUntil("media_signal", 8*time.Second)
	}
	if subOffer == nil {
		t.Fatal("no subscriber offer received from SFU over the hub")
	}
	if len(subOffer.SDP) < 100 || subOffer.Type != webrtc.SDPTypeOffer {
		t.Fatalf("subscriber offer malformed: type=%s len=%d", subOffer.Type, len(subOffer.SDP))
	}

	// 2) Client answers the subscriber offer through the hub — the SFU must
	//    accept it (no media_error).
	subPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("sub pc: %v", err)
	}
	defer subPC.Close()
	if err := subPC.SetRemoteDescription(*subOffer); err != nil {
		t.Fatalf("client setRemoteDescription(sub offer): %v", err)
	}
	ans, err := subPC.CreateAnswer(nil)
	if err != nil {
		t.Fatalf("client createAnswer: %v", err)
	}
	if err := subPC.SetLocalDescription(ans); err != nil {
		t.Fatalf("client setLocalDescription(answer): %v", err)
	}
	ab, _ := json.Marshal(ans)
	a.send("media_signal", map[string]any{
		"pc": "subscriber", "type": "answer", "sdp": json.RawMessage(ab),
	})
	expectNoError(t, a, 600*time.Millisecond)

	// 3) Publisher offer round-trip: a real client-side publisher connection
	//    (opus mic track, msid convention "mic-<peerId>") offers through the
	//    hub and receives the SFU's answer.
	pubPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("pub pc: %v", err)
	}
	defer pubPC.Close()
	opus := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	mic, err := webrtc.NewTrackLocalStaticRTP(opus, "audio", "mic-"+a.selfID)
	if err != nil {
		t.Fatalf("mic track: %v", err)
	}
	if _, err := pubPC.AddTrack(mic); err != nil {
		t.Fatalf("addTrack: %v", err)
	}
	pubOffer, err := pubPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("createOffer: %v", err)
	}
	if err := pubPC.SetLocalDescription(pubOffer); err != nil {
		t.Fatalf("setLocalDescription(offer): %v", err)
	}
	ob, _ := json.Marshal(pubOffer)
	a.send("media_signal", map[string]any{
		"pc": "publisher", "type": "offer", "sdp": json.RawMessage(ob),
	})

	var pubAnswer *webrtc.SessionDescription
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		m := a.readUntil("media_signal", time.Until(deadline))
		if m == nil {
			break
		}
		d := dMap(m)
		if d["pc"] == "publisher" && d["type"] == "answer" {
			pubAnswer = sdpOf(d)
			break
		}
		if d["type"] == "error" || m["t"] == "error" {
			t.Fatalf("unexpected error during publisher offer: %v", d)
		}
	}
	if pubAnswer == nil {
		t.Fatal("no publisher answer received through the hub")
	}
	if pubAnswer.Type != webrtc.SDPTypeAnswer || len(pubAnswer.SDP) < 100 {
		t.Fatalf("publisher answer malformed: type=%s len=%d", pubAnswer.Type, len(pubAnswer.SDP))
	}
	if err := pubPC.SetRemoteDescription(*pubAnswer); err != nil {
		t.Fatalf("client setRemoteDescription(pub answer): %v", err)
	}
}

func TestMediaSignalRequiresBubble(t *testing.T) {
	_, wsURL := testWSServer(t)
	c := dialTestWS(t, wsURL, "t2-dev-d", "Dana", "garden")
	defer c.close()

	c.send("media_signal", map[string]any{"pc": "publisher", "type": "offer"})
	m := c.readUntil("error", 5*time.Second)
	if m == nil {
		t.Fatal("expected error for media_signal without media_join")
	}
	d := dMap(m)
	if d["code"] != "media_error" {
		t.Fatalf("error code = %v, want media_error", d["code"])
	}
}

func TestMediaLeaveIdempotent(t *testing.T) {
	_, wsURL := testWSServer(t)
	c := dialTestWS(t, wsURL, "t2-dev-e", "Eli", "garden")
	defer c.close()
	joinBubble(t, c)
	c.send("media_leave", map[string]any{})
	st := c.readUntil("media_state", 8*time.Second)
	if st == nil || dMap(st)["joined"] != false {
		t.Fatalf("leave ack = %v", st)
	}
	// second leave is a no-op: no ack, no error
	c.send("media_leave", map[string]any{})
	expectNoError(t, c, 400*time.Millisecond)
}
