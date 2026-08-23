package main

import (
	"testing"
	"time"
)

// TokenBucket rate limiter tests (PROTOCOL.md: burst 5/10s, sustained 1/2s).
func TestTokenBucketBurstThenRefill(t *testing.T) {
	tb := NewTokenBucket(5, 0.5)
	// Burst: 5 allowed immediately.
	for i := 0; i < 5; i++ {
		if !tb.Allow() {
			t.Fatalf("allow %d: expected true in burst", i)
		}
	}
	// 6th in the same instant is denied.
	if tb.Allow() {
		t.Fatal("allow 6: expected false (burst exhausted)")
	}
	// After 2s, 1 token refilled (rate 0.5/s).
	time.Sleep(2 * time.Second)
	if !tb.Allow() {
		t.Fatal("allow after refill: expected true")
	}
	if tb.Allow() {
		t.Fatal("allow immediately after refill: expected false (only 1 refilled)")
	}
}

func TestTokenBucketCapsAtCapacity(t *testing.T) {
	tb := NewTokenBucket(2, 100) // fast refill
	_ = tb.Allow()
	_ = tb.Allow()
	// Long sleep: tokens must cap at 2, never exceed capacity.
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if !tb.Allow() {
			t.Fatalf("allow %d: expected true", i)
		}
	}
	if tb.Allow() {
		t.Fatal("allow past capacity: expected false (capped)")
	}
}

// SpatialHash: insert, move across cells, nearby radius, remove (AOI = 20 tiles).
func TestSpatialHashInsertAndNearby(t *testing.T) {
	sh := NewSpatialHash(cellSize)
	a := &Entity{ID: "a", Name: "A", X: 10, Y: 10}
	b := &Entity{ID: "b", Name: "B", X: 14, Y: 12} // within radius 5 of (10,10): dist ~4.5
	c := &Entity{ID: "c", Name: "C", X: 40, Y: 40} // far
	sh.Insert(a)
	sh.Insert(b)
	sh.Insert(c)

	near := sh.Nearby(10, 10, 5, "")
	if len(near) != 2 {
		t.Fatalf("nearby = %d entities, want 2 (a,b)", len(near))
	}
	found := map[string]bool{}
	for _, e := range near {
		found[e.ID] = true
	}
	if !found["a"] || !found["b"] {
		t.Errorf("nearby missing a/b: %+v", found)
	}
}

func TestSpatialHashMoveAcrossCells(t *testing.T) {
	sh := NewSpatialHash(cellSize)
	e := &Entity{ID: "m", Name: "M", X: 2, Y: 2}
	sh.Insert(e)
	sh.Move(e, 50, 50) // crosses cell boundary (cellSize=8)
	if e.X != 50 || e.Y != 50 {
		t.Fatalf("entity not moved: %d,%d", e.X, e.Y)
	}
	// Old cell must not contain it; new cell must.
	if len(sh.Nearby(2, 2, 2, "")) != 0 {
		t.Error("entity still visible at old position")
	}
	near := sh.Nearby(50, 50, 2, "")
	if len(near) != 1 || near[0].ID != "m" {
		t.Errorf("entity not found at new position: %+v", near)
	}
}

func TestSpatialHashNearbySkipsSelf(t *testing.T) {
	sh := NewSpatialHash(cellSize)
	e := &Entity{ID: "self", Name: "S", X: 5, Y: 5}
	sh.Insert(e)
	near := sh.Nearby(5, 5, 3, "self")
	if len(near) != 0 {
		t.Errorf("nearby with skip=self returned %d, want 0", len(near))
	}
}
