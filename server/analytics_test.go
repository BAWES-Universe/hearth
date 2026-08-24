package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeORKey builds a realistic-but-fake OpenRouter key at runtime so the
// literal secret pattern never appears in the repo (scanners + output
// redaction stay quiet; the scrubber still matches it).
func fakeORKey() string { return "sk-or-v1-" + strings.Repeat("a", 16) }

// --- scrubPII: keys, emails, IPs, bearer tokens must be redacted ---
func TestScrubPIIRedactsSecrets(t *testing.T) {
	k := fakeORKey()
	cases := []struct{ in, want string }{
		{k, "[REDACTED_KEY]"},
		{"my key is " + k + " in text", "my key is [REDACTED_KEY] in text"},
		{"email me at alice@example.com ok", "email me at [REDACTED_EMAIL] ok"},
		{"from 203.0.113.7:8080", "from [REDACTED_IP]:8080"}, // port survives; IP redacted
		{"Bearer eyJhbGciOiJIUzI1NiJ9.abc123def456", "[REDACTED_TOKEN]"},
		{"no secrets here", "no secrets here"},
	}
	for _, c := range cases {
		if got := scrubPII(c.in); got != c.want {
			t.Errorf("scrubPII(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- scrubProps: flat scalars, scrubbed keys AND values, no nested objects ---
func TestScrubProps(t *testing.T) {
	k := fakeORKey()
	props := map[string]any{
		"key":   k,
		"email": "bob@example.com",
		"space": "town-square",
		"count": float64(3),
	}
	scrubbed, ok := scrubProps(props)
	if !ok {
		t.Fatal("scrubProps returned !ok")
	}
	if strings.Contains(jsonDump(scrubbed), k) {
		t.Errorf("raw key survived scrub: %s", jsonDump(scrubbed))
	}
	if strings.Contains(jsonDump(scrubbed), "bob@example.com") {
		t.Errorf("raw email survived scrub: %s", jsonDump(scrubbed))
	}
	if scrubbed["space"] != "town-square" {
		t.Errorf("benign prop lost: %+v", scrubbed)
	}
	// nested objects must be rejected
	if _, ok := scrubProps(map[string]any{"nested": map[string]any{"a": 1}}); ok {
		t.Errorf("nested object accepted, want reject")
	}
	// oversized prop values must be rejected (bounds check)
	if _, ok := scrubProps(map[string]any{"big": strings.Repeat("x", maxPropValueLen+1)}); ok {
		t.Errorf("oversized prop accepted, want reject")
	}
}

// --- /api/events: batch accepted; name key is what the handler reads ---
func TestAnalyticsEventsBatch(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "ana-tester", "Ana")
	body := map[string]any{
		"events": []map[string]any{
			{"name": "world_enter", "props": map[string]any{"space": "town-square"}},
			{"name": "chat_send", "props": map[string]any{"channel": "space", "char_len": float64(12)}},
		},
	}
	code, out := doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events", body, u)
	if code != http.StatusOK {
		t.Fatalf("events = %d, want 200: %+v", code, out)
	}
	if out["accepted"] != float64(2) {
		t.Errorf("accepted = %v, want 2", out["accepted"])
	}
	if out["rejected"] != float64(0) {
		t.Errorf("rejected = %v, want 0", out["rejected"])
	}
}

// --- intake validation: non-allowlisted names rejected, garbage body 400 ---
func TestAnalyticsEventsRejectsBad(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "ana-bad", "Ana")
	code, out := doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events",
		map[string]any{"events": []map[string]any{{"name": "not_real_event", "props": map[string]any{}}}}, u)
	if code != http.StatusOK || out["rejected"] != float64(1) {
		t.Errorf("bad event = %d %+v, want 200 with rejected:1", code, out)
	}
	code, _ = doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events", "not json", u)
	if code != http.StatusBadRequest {
		t.Errorf("garbage body = %d, want 400", code)
	}
	// empty batch rejected
	code, _ = doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events",
		map[string]any{"events": []map[string]any{}}, u)
	if code != http.StatusBadRequest {
		t.Errorf("empty batch = %d, want 400", code)
	}
}

// --- rate limit: burst over 120 events/min per session => 429 ---
func TestAnalyticsRateLimit(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "ana-rapid", "Ana")
	batch := func() map[string]any {
		evs := make([]map[string]any, 10)
		for i := range evs {
			evs[i] = map[string]any{"name": "world_enter", "props": map[string]any{"i": i}}
		}
		return map[string]any{"events": evs}
	}
	var lastCode int
	for i := 0; i < 13; i++ { // 13 * 10 = 130 > 120 limit
		lastCode, _ = doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events", batch(), u)
		if i < 12 && lastCode != http.StatusOK {
			t.Fatalf("burst i=%d = %d, want 200", i, lastCode)
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("over-limit burst = %d, want 429", lastCode)
	}
}

// --- key never persisted: sk-or- key in props is scrubbed at rest ---
func TestAnalyticsKeyScrubbedAtRest(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "ana-secret", "Ana")
	k := fakeORKey()
	doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events",
		map[string]any{"events": []map[string]any{{"name": "byok_key_paste", "props": map[string]any{"key": k}}}}, u)
	var raw string
	err := h.store.db.QueryRow(`SELECT props FROM analytics_events WHERE user_id = ? LIMIT 1`, u.UserID).Scan(&raw)
	if err != nil {
		t.Fatalf("query stored event: %v", err)
	}
	if strings.Contains(raw, k) {
		t.Fatalf("RAW KEY IN DB: %s", raw)
	}
	if !strings.Contains(raw, "[REDACTED_KEY]") {
		t.Errorf("expected redaction marker in stored props, got: %s", raw)
	}
}

// --- pulse gate: 401 without admin key, 200 + data with ADMIN_API_KEY ---
func TestAnalyticsPulseAdminGate(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "ana-pulse", "Ana")
	code, _ := doJSON(t, h.handleAnalyticsPulse, http.MethodGet, "/api/analytics/pulse", nil, u)
	if code != http.StatusUnauthorized {
		t.Errorf("pulse without admin key = %d, want 401", code)
	}
}

func TestAnalyticsPulseWithAdminKey(t *testing.T) {
	t.Setenv("ADMIN_API_KEY", "test-admin-key-123")
	h := newTestHub(t)
	u := newTestUser(t, h, "ana-pulse-ok", "Ana")
	// seed an event so the pulse has data to report
	doJSON(t, h.handleAnalyticsEvents, http.MethodPost, "/api/events",
		map[string]any{"events": []map[string]any{{"name": "world_enter", "props": map[string]any{"space": "town-square"}}}}, u)

	req := httptest.NewRequest(http.MethodGet, "/api/analytics/pulse", nil)
	req.Header.Set("X-API-Key", "test-admin-key-123")
	rr := httptest.NewRecorder()
	h.handleAnalyticsPulse(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("pulse with admin key = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pulse: %v", err)
	}
	pulse, ok := out["pulse"].(map[string]any)
	if !ok {
		t.Fatalf("pulse payload missing: %+v", out)
	}
	if pulse["date"] == "" {
		t.Errorf("pulse.date empty")
	}
	if pulse["joins_today"].(float64) < 1 {
		t.Errorf("joins_today = %v, want >= 1 (test session)", pulse["joins_today"])
	}
	found := false
	if tops, ok := pulse["top_events"].([]any); ok {
		for _, te := range tops {
			if m, ok := te.(map[string]any); ok && m["name"] == "world_enter" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("world_enter missing from pulse.top_events: %s", rr.Body.String())
	}
}

// silence unused-import guard if jsonDump moves (jsonDump itself lives in
// byok_test.go, same package).
var _ = json.Marshal
