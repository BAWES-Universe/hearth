package media

import (
	"sync"
	"time"
)

// TopK picks the K most important audio sources of a room and forwards them to
// every subscriber.
//
// Audio-level model (placeholder per build brief): a publisher's importance is
// its join order — earlier joiners rank higher. This is deterministic and
// flap-free by construction; when real audio levels (RFC 6464 header extension
// or RTCP) land, only pick() needs to change.
//
// Hysteresis: whenever the candidate set changes, the new selection is applied
// only after it has persisted for cfg.Hysteresis. A transient join/leave
// within the window cancels the pending re-slot.
type TopK struct {
	room *Room
	mu   sync.Mutex

	k        int
	applied  []string // pubIDs currently bound to audio slots
	pending  []string
	timer    *time.Timer
	debounce time.Duration
}

func newTopK(room *Room, k int, debounce time.Duration) *TopK {
	t := &TopK{room: room, k: k, debounce: debounce}
	return t
}

// SetK changes the number of forwarded audio sources and re-evaluates.
func (t *TopK) SetK(k int) {
	t.mu.Lock()
	t.k = k
	if t.k < 1 {
		t.k = 1
	}
	if t.k > t.room.audioSlots {
		t.k = t.room.audioSlots
	}
	t.mu.Unlock()
	t.evaluate()
}

// K returns the current Top-K value.
func (t *TopK) K() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.k
}

// evaluate recomputes the selection from the room's audio join order. Called
// whenever the set of audio publishers changes (join, leave, new track).
func (t *TopK) evaluate() {
	t.mu.Lock()
	defer t.mu.Unlock()

	sel := t.room.audioSelection(t.k)
	if equalStrings(sel, t.applied) {
		// Stable: cancel any pending change.
		if t.timer != nil {
			t.timer.Stop()
			t.timer = nil
		}
		t.pending = nil
		return
	}
	if equalStrings(sel, t.pending) {
		return // debounce already armed for this exact selection
	}
	t.pending = sel
	if t.timer != nil {
		t.timer.Stop()
	}
	selCopy := append([]string(nil), sel...)
	t.timer = time.AfterFunc(t.debounce, func() {
		t.room.applyAudioSelection(selCopy)
	})
}

// appliedSelection returns the pubIDs currently bound to audio slots.
func (t *TopK) appliedSelection() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.applied...)
}

// setApplied records the selection that was actually bound.
func (t *TopK) setApplied(sel []string) {
	t.mu.Lock()
	t.applied = append([]string(nil), sel...)
	t.pending = nil
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	t.mu.Unlock()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
