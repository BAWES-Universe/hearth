package main

// BYOK backend tests (Ox-decided design): the server must reject bad keys,
// CRUD the fingerprint row, NEVER expose the plaintext key in any response
// or the DB, and proxy one completion via /api/byok/use with in-memory keys
// only. OpenRouter is mocked with an httptest server (byokORBase override).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testKey = "sk-or-v1-testkey0123456789abcdef0123456789abcdef0123456789abcdef"

// mockOpenRouter fakes the two OpenRouter endpoints BYOK uses. A nil handler
// means "401" (invalid key). Returns the server URL.
func mockOpenRouter(t *testing.T, keyHandler, completionHandler http.HandlerFunc) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/key":
			if keyHandler != nil {
				keyHandler(w, r)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		case "/api/v1/chat/completions":
			if completionHandler != nil {
				completionHandler(w, r)
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	old := byokORBase
	byokORBase = ts.URL
	t.Cleanup(func() { byokORBase = old })
}

func jsonDump(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("jsonDump: " + err.Error()) // a dump failure must fail the test, not silently pass
	}
	return string(b)
}

func tableColumns(t *testing.T, h *Hub, table string) []string {
	t.Helper()
	rows, err := h.store.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		cols = append(cols, name)
	}
	return cols
}

func TestByokRequiresAuth(t *testing.T) {
	h := newTestHub(t)
	for _, tc := range []struct {
		name   string
		fn     http.HandlerFunc
		method string
		path   string
		body   any
	}{
		{"upsert", h.handleByok, http.MethodPost, "/api/byok", map[string]any{"key": testKey}},
		{"status", h.handleByok, http.MethodGet, "/api/byok/status", nil},
		{"contribution", h.handleByok, http.MethodGet, "/api/byok/contribution", nil},
		{"delete", h.handleByok, http.MethodDelete, "/api/byok", nil},
		{"use", h.byokUse, http.MethodPost, "/api/byok/use", map[string]any{"key": testKey, "prompt": "hi"}},
	} {
		code, out := doJSON(t, tc.fn, tc.method, tc.path, tc.body, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s without session = %d, want 401: %+v", tc.name, code, out)
		}
	}
}

func TestByokValidateRejectsBadKey(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "byok-alice-key", "Alice")
	mockOpenRouter(t, nil, nil) // key endpoint 401s => invalid key

	// wrong format: rejected before any network call
	code, out := doJSON(t, h.handleByok, http.MethodPost, "/api/byok",
		map[string]any{"key": "not-a-real-key"}, u)
	if code != http.StatusBadRequest {
		t.Fatalf("format reject = %d, want 400: %+v", code, out)
	}
	if !strings.Contains(out["error"].(string), "invalid key format") {
		t.Errorf("format error = %v", out["error"])
	}

	// valid format, invalid key: OpenRouter 401 -> 400
	code, out = doJSON(t, h.handleByok, http.MethodPost, "/api/byok",
		map[string]any{"key": testKey}, u)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid key = %d, want 400: %+v", code, out)
	}
	if out["error"] != "invalid OpenRouter key" {
		t.Errorf("error = %v, want 'invalid OpenRouter key'", out["error"])
	}

	// nothing was stored for a rejected key
	code, out = doJSON(t, h.handleByok, http.MethodGet, "/api/byok/status", nil, u)
	if code != http.StatusOK || out["hasKey"] != false {
		t.Errorf("status after rejects = %d %+v, want hasKey=false", code, out)
	}
}

func TestByokCRUDAndNeverStoresKey(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "byok-bob-key", "Bob")
	mockOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		// the key must arrive only as the Bearer header of this one call
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			t.Errorf("authorization = %q, want Bearer key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"label":"bob","usage":{"total_usage":12.5,"limit":1000,"is_free_tier":true},"is_free_tier":true}}`))
	}, nil)

	// POST: validated, fingerprint returned, quota snapshot stored
	code, out := doJSON(t, h.handleByok, http.MethodPost, "/api/byok",
		map[string]any{"key": testKey}, u)
	if code != http.StatusOK {
		t.Fatalf("upsert = %d, want 200: %+v", code, out)
	}
	fp, _ := out["fp"].(string)
	if fp != keyFingerprint(testKey) {
		t.Errorf("fp = %q, want %q", fp, keyFingerprint(testKey))
	}
	if out["masked"] != maskFP(fp) {
		t.Errorf("masked = %v, want %v", out["masked"], maskFP(fp))
	}
	if body := jsonDump(out); strings.Contains(body, testKey) {
		t.Fatal("PLAINTEXT KEY LEAKED in POST /api/byok response")
	}
	quota := out["quota"].(map[string]any)
	if quota["limit"] != float64(1000) || quota["used"] != 12.5 {
		t.Errorf("quota = %+v, want limit 1000 used 12.5", quota)
	}

	// GET status: hasKey, masked fp, quota snapshot; no plaintext key
	code, out = doJSON(t, h.handleByok, http.MethodGet, "/api/byok/status", nil, u)
	if code != http.StatusOK || out["hasKey"] != true {
		t.Fatalf("status = %d %+v, want hasKey=true", code, out)
	}
	if out["fp"] != fp {
		t.Errorf("status fp = %v, want %v", out["fp"], fp)
	}
	if out["model"] != "stealth/ox-alpha" {
		t.Errorf("model = %v", out["model"])
	}
	if body := jsonDump(out); strings.Contains(body, testKey) {
		t.Fatal("PLAINTEXT KEY LEAKED in GET /api/byok/status response")
	}

	// DB: only the fingerprint, and there is no key column at all
	var storedFP string
	if err := h.store.db.QueryRow(`SELECT key_fp FROM byok_status WHERE user_id = ?`, u.UserID).Scan(&storedFP); err != nil {
		t.Fatalf("read byok_status: %v", err)
	}
	if storedFP != fp || strings.Contains(storedFP, testKey) {
		t.Fatalf("stored fp = %q, want %q (never the key)", storedFP, fp)
	}
	for _, col := range tableColumns(t, h, "byok_status") {
		if col == "key" || col == "key_enc" || col == "api_key" {
			t.Fatalf("byok_status has a key column %q — violates Ox design", col)
		}
	}

	// model allowlist: non-free-tier model rejected
	code, out = doJSON(t, h.handleByok, http.MethodPost, "/api/byok",
		map[string]any{"key": testKey, "model": "gpt-4o"}, u)
	if code != http.StatusBadRequest {
		t.Errorf("model allowlist = %d, want 400: %+v", code, out)
	}

	// DELETE revokes; status flips to no key
	code, out = doJSON(t, h.handleByok, http.MethodDelete, "/api/byok", nil, u)
	if code != http.StatusOK || out["revoked"] != true {
		t.Fatalf("delete = %d %+v", code, out)
	}
	code, out = doJSON(t, h.handleByok, http.MethodGet, "/api/byok/status", nil, u)
	if code != http.StatusOK || out["hasKey"] != false {
		t.Errorf("status after delete = %d %+v, want hasKey=false", code, out)
	}
}

func TestByokUseProxy(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "byok-carol-key", "Carol")
	mockOpenRouter(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized) // invalid key → 401 upstream
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode completion body: %v", err)
		}
		if body["model"] != "stealth/ox-alpha" {
			t.Errorf("completion model = %v, want stealth/ox-alpha", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"stealth/ox-alpha","choices":[{"message":{"content":"hello from ox"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})

	code, out := doJSON(t, h.byokUse, http.MethodPost, "/api/byok/use",
		map[string]any{"key": testKey, "prompt": "build a house", "feature": "mason"}, u)
	if code != http.StatusOK {
		t.Fatalf("use = %d, want 200: %+v", code, out)
	}
	if out["completion"] != "hello from ox" {
		t.Errorf("completion = %v, want 'hello from ox'", out["completion"])
	}
	usage := out["usage"].(map[string]any)
	if usage["promptTokens"] != float64(10) || usage["completionTokens"] != float64(5) {
		t.Errorf("usage = %+v, want prompt 10 completion 5", usage)
	}
	if body := jsonDump(out); strings.Contains(body, testKey) {
		t.Fatal("PLAINTEXT KEY LEAKED in /api/byok/use response")
	}

	// ai_usage row written (tokens only — dashboards without keys)
	var n int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM ai_usage WHERE user_id = ? AND feature = 'mason'`, u.UserID).Scan(&n); err != nil {
		t.Fatalf("ai_usage query: %v", err)
	}
	if n != 1 {
		t.Errorf("ai_usage rows = %d, want 1", n)
	}

	// invalid key through the proxy -> 400, nothing stored (a wrong key is
	// still format-valid, so it must reach the network and come back 401)
	badKey := testKey + "x"
	code, out = doJSON(t, h.byokUse, http.MethodPost, "/api/byok/use",
		map[string]any{"key": badKey, "prompt": "hi", "feature": "soul"}, u)
	if code != http.StatusBadRequest || out["error"] != "invalid OpenRouter key" {
		t.Errorf("use with bad key = %d %+v, want 400 invalid", code, out)
	}
	// the failed use must not have written an ai_usage row (nothing stored)
	var badN int
	if err := h.store.db.QueryRow(`SELECT COUNT(*) FROM ai_usage WHERE user_id = ? AND feature = 'soul'`, u.UserID).Scan(&badN); err != nil {
		t.Fatalf("ai_usage count after bad key: %v", err)
	}
	if badN != 0 {
		t.Errorf("ai_usage rows after bad key = %d, want 0 (nothing stored on rejection)", badN)
	}
}

func TestByokContribution(t *testing.T) {
	h := newTestHub(t)
	u := newTestUser(t, h, "byok-contributor", "Dan")
	if err := h.store.logAIUsage(u.UserID, "mason", 100, 50); err != nil {
		t.Fatalf("log mason usage: %v", err)
	}
	if err := h.store.logAIUsage(u.UserID, "dream", 200, 80); err != nil {
		t.Fatalf("log dream usage: %v", err)
	}
	code, out := doJSON(t, h.handleByok, http.MethodGet, "/api/byok/contribution", nil, u)
	if code != http.StatusOK {
		t.Fatalf("contribution = %d, want 200", code)
	}
	c, ok := out["contribution"].(map[string]any)
	if !ok {
		t.Fatalf("contribution missing: %+v", out)
	}
	if c["calls"] != float64(2) {
		t.Errorf("calls = %v, want 2", c["calls"])
	}
	if c["tokensTotal"] != float64(430) {
		t.Errorf("tokensTotal = %v, want 430", c["tokensTotal"])
	}
	bf, ok := c["byFeature"].(map[string]any)
	if !ok || bf["mason"] != float64(1) || bf["dream"] != float64(1) {
		t.Errorf("byFeature = %+v, want mason:1 dream:1", c["byFeature"])
	}
	if c["hasKey"] != false {
		t.Errorf("hasKey = %v, want false (no key stored)", c["hasKey"])
	}
}
