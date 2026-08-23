package media

import (
	"strings"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Room holds all routing state for one space: the single publisher track table
// (PROTOCOL.md: "single publisher track table per room"), the audio join order
// feeding Top-K, the screen recency order and per-peer screen subscriptions.
//
// A single mutex guards all routing state. Slot binding helpers must be called
// with r.mu held; anything that may call back into the room (topk.evaluate ->
// audioSelection) must NOT be called while holding r.mu.
type Room struct {
	id          string
	cfg         Config
	onEvent     emitFn
	peerGone    func(peerID string)
	audioSlots  int
	videoSlots  int
	screenSlots int

	mu     sync.Mutex
	peers  map[string]*Peer
	tracks map[string]*PublishedTrack // key: pubID|kind|rung (single table per room)

	audioOrder  []string                   // pubIDs in join order (Top-K priority)
	audioCount  map[string]int             // pubID -> live audio streams
	screenOrder []string                   // pubIDs publishing screen, most recent first
	screenSubs  map[string]map[string]bool // subPeerID -> set of screen pubIDs subscribed

	topk *TopK
}

func newRoom(id string, cfg Config, onEvent emitFn, peerGone func(peerID string)) *Room {
	r := &Room{
		id:          id,
		cfg:         cfg,
		onEvent:     onEvent,
		peerGone:    peerGone,
		audioSlots:  cfg.AudioSlots,
		videoSlots:  cfg.VideoSlots,
		screenSlots: cfg.ScreenSlots,
		peers:       map[string]*Peer{},
		tracks:      map[string]*PublishedTrack{},
		audioCount:  map[string]int{},
		screenSubs:  map[string]map[string]bool{},
	}
	r.topk = newTopK(r, cfg.TopK, cfg.Hysteresis)
	return r
}

func (r *Room) logf(format string, args ...any) {
	r.cfg.logf("[room %s] "+format, append([]any{r.id}, args...)...)
}

// trackKey builds the table key: pubID|kind|rung.
func trackKey(pubID, kind, rung string) string { return pubID + "|" + kind + "|" + rung }

// ---------------------------------------------------------------------------
// Track registration (from publisher onTrack)

// registerRemoteTrack classifies an incoming TrackRemote (msid / RID based)
// and adds it to the room's track table. Only one reader per SSRC is started.
func (r *Room) registerRemoteTrack(pubID string, tr *webrtc.TrackRemote) {
	kind, rung := classifyTrack(tr)
	key := trackKey(pubID, kind, rung)

	r.mu.Lock()
	if old, ok := r.tracks[key]; ok && old.remote.SSRC() == tr.SSRC() {
		r.mu.Unlock()
		return // duplicate onTrack for the same SSRC
	}
	var pt *PublishedTrack
	pt = newPublishedTrack(key, pubID, kind, rung, tr, func() { r.removeTrack(key, pt) })
	r.tracks[key] = pt

	switch kind {
	case KindAudio:
		if r.audioCount[pubID] == 0 {
			r.audioOrder = append(r.audioOrder, pubID)
		}
		r.audioCount[pubID]++
	case KindScreen:
		// most recent first
		r.screenOrder = append([]string{pubID}, r.screenOrder...)
	}
	r.mu.Unlock()

	r.logf("registered track %s (kind=%s rung=%s)", key, kind, rung)
	r.topk.evaluate()
	r.reslotScreens()
}

// classifyTrack maps a TrackRemote to (kind, rung).
//
// Client msid convention (documented in MEDIA-API.md):
//
//	audio:  streamID "mic-<peerId>"
//	camera: streamID "camera-<rung>" (rung: low|mid|high), or set via RID
//	screen: streamID "screen-<peerId>"
//
// RID (real simulcast header extension) wins over the streamID suffix.
func classifyTrack(tr *webrtc.TrackRemote) (kind, rung string) {
	stream := tr.StreamID()
	switch {
	case tr.Kind() == webrtc.RTPCodecTypeAudio:
		return KindAudio, "main"
	case strings.HasPrefix(stream, "screen") || tr.ID() == "screen":
		return KindScreen, "main"
	default: // camera video
		rid := tr.RID()
		if rid != "" && isRung(rid) {
			return KindVideo, rid
		}
		parts := strings.Split(stream, "-")
		last := parts[len(parts)-1]
		if isRung(last) {
			return KindVideo, last
		}
		return KindVideo, DefaultRung
	}
}

func isRung(s string) bool {
	return s == RungLow || s == RungMid || s == RungHigh
}

// removeTrack drops a published track and re-routes everyone away from it.
func (r *Room) removeTrack(key string, pt *PublishedTrack) {
	r.mu.Lock()
	if r.tracks[key] != pt {
		r.mu.Unlock()
		return
	}
	delete(r.tracks, key)
	pubID, kind := pt.pubID, pt.kind

	switch kind {
	case KindAudio:
		if r.audioCount[pubID] > 0 {
			r.audioCount[pubID]--
		}
		if r.audioCount[pubID] == 0 {
			r.audioOrder = removeString(r.audioOrder, pubID)
		}
	case KindScreen:
		r.screenOrder = removeString(r.screenOrder, pubID)
	}

	// Unbind every slot fed by this track.
	for _, p := range r.peers {
		for _, slot := range p.audioSlots {
			if slot.boundKey == key {
				r.unbindSlot(p, slot)
			}
		}
		for _, slot := range p.videoSlots {
			if slot.boundKey == key {
				r.unbindSlot(p, slot)
			}
		}
	}
	r.mu.Unlock()

	r.logf("removed track %s", key)
	r.topk.evaluate()
	r.reslotScreens()
}

// onPeerGone fires when a peer's connection dies: the peer is removed entirely.
func (r *Room) onPeerGone(peerID string) {
	r.mu.Lock()
	p, ok := r.peers[peerID]
	r.mu.Unlock()
	if !ok {
		return
	}
	p.close() // close() -> removePeerTracks takes r.mu itself; never hold it here
	if r.peerGone != nil {
		r.peerGone(peerID)
	}
}

// removePeerTracks removes a leaving peer's tracks and subscriptions and
// re-routes remaining peers. Called from Peer.close (which is called from
// onPeerGone / Leave).
func (r *Room) removePeerTracks(peerID string) {
	r.mu.Lock()
	if _, ok := r.peers[peerID]; !ok {
		r.mu.Unlock()
		return
	}
	// Tracks die with the connection; remove them from the table.
	for key, pt := range r.tracks {
		if pt.pubID == peerID {
			delete(r.tracks, key)
			switch pt.kind {
			case KindAudio:
				if r.audioCount[peerID] > 0 {
					r.audioCount[peerID]--
				}
				if r.audioCount[peerID] == 0 {
					r.audioOrder = removeString(r.audioOrder, peerID)
				}
			case KindScreen:
				r.screenOrder = removeString(r.screenOrder, peerID)
			}
		}
	}
	delete(r.audioCount, peerID)
	delete(r.peers, peerID)
	// Drop this peer's screen subscriptions from other peers' state.
	for _, other := range r.peers {
		if subs := r.screenSubs[other.id]; subs != nil {
			delete(subs, peerID)
		}
	}
	delete(r.screenSubs, peerID)
	// Unbind slots that referenced the leaving peer across remaining peers.
	for _, p := range r.peers {
		for _, slot := range p.videoSlots {
			if slot.boundPub == peerID {
				r.unbindSlot(p, slot)
			}
		}
		for _, slot := range p.screenSlots {
			if slot.boundPub == peerID {
				r.unbindSlot(p, slot)
			}
		}
	}
	r.mu.Unlock()

	r.topk.evaluate()
	r.reslotScreens()
}

// ---------------------------------------------------------------------------
// Top-K audio

// audioSelection returns the join-ordered pubIDs (capped at k) that currently
// have a live audio track. Requires no lock (takes r.mu itself); never call
// while holding r.mu.
func (r *Room) audioSelection(k int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sel []string
	for _, pubID := range r.audioOrder {
		if r.audioCount[pubID] > 0 {
			sel = append(sel, pubID)
			if len(sel) >= k {
				break
			}
		}
	}
	return sel
}

// applyAudioSelection binds each subscriber's audio slots to the selected
// sources (Top-K forwarded to EVERY subscriber). Runs on the hysteresis timer.
func (r *Room) applyAudioSelection(sel []string) {
	r.mu.Lock()
	for _, p := range r.peers {
		// Standard SFU behavior: a publisher never hears itself. Per-peer
		// desired list = room selection minus the peer's own id.
		desired := make([]string, 0, len(sel))
		for _, id := range sel {
			if id != p.id {
				desired = append(desired, id)
			}
		}
		for i, slot := range p.audioSlots {
			if i < len(desired) {
				pt := r.tracks[trackKey(desired[i], KindAudio, "main")]
				if pt != nil {
					r.bindSlot(p, slot, pt)
					continue
				}
			}
			r.unbindSlot(p, slot)
		}
	}
	r.topk.setApplied(sel)
	r.mu.Unlock()
	r.logf("applied audio selection: %v", sel)
}

// ---------------------------------------------------------------------------
// Slot binding (subscriber side, no renegotiation)

// bindSlot points a subscriber slot at a published track. Caller holds r.mu.
func (r *Room) bindSlot(p *Peer, slot *Slot, pt *PublishedTrack) {
	if slot.boundKey == pt.key {
		return
	}
	r.unbindSlot(p, slot)
	slot.boundKey = pt.key
	slot.boundPub = pt.pubID
	pt.addSink(&Sink{peerID: p.id, slotIdx: slot.idx, local: slot.local})
	r.logf("slot %s/%d -> %s", slot.kind, slot.idx, pt.key)
}

// unbindSlot silences a slot. Caller holds r.mu.
func (r *Room) unbindSlot(p *Peer, slot *Slot) {
	if slot.boundKey == "" {
		return
	}
	if pt := r.tracks[slot.boundKey]; pt != nil {
		pt.removeSink(p.id, slot.idx)
	}
	slot.boundKey = ""
	slot.boundPub = ""
}

// ---------------------------------------------------------------------------
// Camera video (explicit subscribe, rung selectable)

// subscribeVideo binds targetID's camera at the requested rung (nearest lower
// fallback) to the first free camera slot. Idempotent: if the target is
// already on a slot, that slot is returned (rung re-evaluated). Returns the
// slot index (client-facing: video slot 0..5).
func (r *Room) subscribeVideo(p *Peer, targetID, rung string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i, slot := range p.videoSlots {
		if slot.boundPub == targetID {
			if rung != "" {
				if pt := r.videoTrackOf(targetID, rung); pt != nil {
					r.bindSlot(p, slot, pt)
				}
			}
			return i, nil
		}
	}
	pt := r.videoTrackOf(targetID, rung)
	if pt == nil {
		return -1, ErrNotFound
	}
	for i, slot := range p.videoSlots {
		if slot.boundKey == "" {
			r.bindSlot(p, slot, pt)
			return i, nil
		}
	}
	return -1, ErrNoSlot
}

// setRung re-points an existing camera subscription to another simulcast rung.
func (r *Room) setRung(p *Peer, targetID, rung string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, slot := range p.videoSlots {
		if slot.boundPub == targetID {
			pt := r.videoTrackOf(targetID, rung)
			if pt == nil {
				return ErrNotFound
			}
			r.bindSlot(p, slot, pt)
			return nil
		}
	}
	return ErrNotFound
}

// videoTrackOf finds targetID's camera track, preferring the requested rung,
// falling back through low -> mid -> high (nearest available).
func (r *Room) videoTrackOf(targetID, rung string) *PublishedTrack {
	order := rungPriority
	if isRung(rung) {
		order = []string{rung, RungLow, RungMid, RungHigh}
	}
	for _, rg := range order {
		if pt := r.tracks[trackKey(targetID, KindVideo, rg)]; pt != nil {
			return pt
		}
	}
	return nil
}

// unsubscribeVideo detaches targetID's camera from the peer's video slots.
func (r *Room) unsubscribeVideo(p *Peer, targetID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, slot := range p.videoSlots {
		if slot.boundPub == targetID {
			r.unbindSlot(p, slot)
		}
	}
}

// ---------------------------------------------------------------------------
// Screen (2 slots, unlimited publishers, client picks)

// subscribeScreen marks targetID's screen as wanted by p and re-slot screens.
// If both screen slots are busy, the OLDEST-bound screen subscription is
// evicted (recency: newest presenter wins).
func (r *Room) subscribeScreen(p *Peer, targetID string) error {
	r.mu.Lock()
	if r.screenSubs[p.id] == nil {
		r.screenSubs[p.id] = map[string]bool{}
	}
	r.screenSubs[p.id][targetID] = true
	r.mu.Unlock()
	r.reslotScreens()
	return nil
}

// unsubscribeScreen removes targetID's screen from p's wanted set.
func (r *Room) unsubscribeScreen(p *Peer, targetID string) {
	r.mu.Lock()
	if subs := r.screenSubs[p.id]; subs != nil {
		delete(subs, targetID)
	}
	r.mu.Unlock()
	r.reslotScreens()
}

// reslotScreens recomputes every peer's screen slots from their subscriptions
// and the room's screen recency order (newest first, capped at 2).
func (r *Room) reslotScreens() {
	r.mu.Lock()
	for _, p := range r.peers {
		desired := make([]*PublishedTrack, 0, r.screenSlots)
		seen := map[string]bool{}
		for _, pubID := range r.screenOrder {
			if !r.screenSubs[p.id][pubID] || seen[pubID] {
				continue
			}
			pt := r.tracks[trackKey(pubID, KindScreen, "main")]
			if pt == nil {
				continue
			}
			desired = append(desired, pt)
			seen[pubID] = true
			if len(desired) >= r.screenSlots {
				break
			}
		}
		for i, slot := range p.screenSlots {
			if i < len(desired) {
				r.bindSlot(p, slot, desired[i])
			} else {
				r.unbindSlot(p, slot)
			}
		}
	}
	r.mu.Unlock()
}

// ---------------------------------------------------------------------------
// helpers

func removeString(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// statsSnapshot is a point-in-time view for Stats().
type roomStats struct {
	RoomID           string   `json:"roomId"`
	Peers            int      `json:"peers"`
	AudioTopK        []string `json:"audioTopK"`
	ScreenOrder      []string `json:"screenOrder"`
	VideoBounds      []string `json:"videoBounds"`
	ScreenBounds     []string `json:"screenBounds"`
	VideoSlotsInUse  int      `json:"videoSlotsInUse"`
	ScreenSlotsInUse int      `json:"screenSlotsInUse"`
}

func (r *Room) snapshot() roomStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := roomStats{RoomID: r.id, Peers: len(r.peers)}
	s.AudioTopK = r.topk.appliedSelection()
	s.ScreenOrder = append([]string(nil), r.screenOrder...)
	for _, p := range r.peers {
		for _, slot := range p.videoSlots {
			if slot.boundKey != "" {
				s.VideoSlotsInUse++
				s.VideoBounds = append(s.VideoBounds, slot.boundKey)
			}
		}
		for _, slot := range p.screenSlots {
			if slot.boundKey != "" {
				s.ScreenSlotsInUse++
				s.ScreenBounds = append(s.ScreenBounds, slot.boundKey)
			}
		}
	}
	return s
}
