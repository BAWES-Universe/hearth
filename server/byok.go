package main

// BYOK (Bring Your Own Key) — implemented per the Ox Alpha decision of
// 2026-08-24 (/tmp/byok-design.md, decisions in /tmp/byok-decision-1.md):
//
//   • Keys live CLIENT-SIDE (localStorage). The server stores ONLY a
//     fingerprint (sha256(key)[:8]) in byok_status — there is no key column
//     anywhere, so a DB leak exposes nothing billable.
//   • The key transits the server only to be VALIDATED (in-memory, one
//     outbound OpenRouter call, discarded immediately, never logged).
//   • /api/byok/use is the agent-integration point (Mason-style server-side
//     agents): the initiator's key is passed per-request, held in memory for
//     exactly one completion, then discarded. Never persisted.
//
// API surface (all JSON, auth = hearth_session cookie like /api/worlds):
//
//	POST   /api/byok         validate key vs OpenRouter GET /api/v1/key,
//	                         store fp + model + quota snapshot, return masked
//	GET    /api/byok/status  key presence, masked fp, model, quota snapshot
//	DELETE /api/byok         revoke (clears the fp row, row kept for audit)
//	POST   /api/byok/use     one chat completion with the caller's key
//	                         (in-memory only), logs ai_usage, never stores key
//
// Every mutation emits an append-only activity_events row (kind=byok).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// --- schema (added to Store.migrate; idempotent) ---

const byokStatusDDL = `
CREATE TABLE IF NOT EXISTS byok_status (
	user_id      TEXT PRIMARY KEY REFERENCES users(id),
	key_fp       TEXT NOT NULL,                 -- sha256(key)[:8] hex — NOT the key
	model        TEXT NOT NULL DEFAULT 'stealth/ox-alpha',
	validated_at INTEGER NOT NULL DEFAULT 0,
	revoked_at   INTEGER NOT NULL DEFAULT 0,    -- >0 = revoked (row kept for audit)
	quota_limit  REAL NOT NULL DEFAULT 0,
	quota_used   REAL NOT NULL DEFAULT 0,
	is_free_tier INTEGER NOT NULL DEFAULT 0
)`

const aiUsageDDL = `
CREATE TABLE IF NOT EXISTS ai_usage (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id     TEXT NOT NULL,
	feature     TEXT NOT NULL DEFAULT '',       -- 'mason'|'soul'|'dream'|'byok.use'
	tokens_in   INTEGER NOT NULL DEFAULT 0,
	tokens_out  INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL
)`

const aiUsageIdxDDL = `CREATE INDEX IF NOT EXISTS idx_ai_usage_user ON ai_usage(user_id, created_at)`

// ByokStatus is one user's key fingerprint row (never key material).
type ByokStatus struct {
	UserID      string
	KeyFP       string
	Model       string
	ValidatedAt int64
	RevokedAt   int64
	QuotaLimit  float64
	QuotaUsed   float64
	IsFreeTier  bool
}

// --- store methods (same package, co-located with the feature) ---

func (s *Store) upsertByok(st ByokStatus) error {
	_, err := s.db.Exec(`INSERT INTO byok_status (user_id, key_fp, model, validated_at, revoked_at, quota_limit, quota_used, is_free_tier)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET
			key_fp=excluded.key_fp, model=excluded.model, validated_at=excluded.validated_at,
			revoked_at=excluded.revoked_at, quota_limit=excluded.quota_limit,
			quota_used=excluded.quota_used, is_free_tier=excluded.is_free_tier`,
		st.UserID, st.KeyFP, st.Model, st.ValidatedAt, st.RevokedAt,
		st.QuotaLimit, st.QuotaUsed, b2i(st.IsFreeTier))
	return err
}

// getByok returns the user's ACTIVE key row (nil when absent or revoked).
func (s *Store) getByok(userID string) (*ByokStatus, error) {
	var st ByokStatus
	var free int
	err := s.db.QueryRow(`SELECT user_id, key_fp, model, validated_at, revoked_at, quota_limit, quota_used, is_free_tier
		FROM byok_status WHERE user_id = ?`, userID).
		Scan(&st.UserID, &st.KeyFP, &st.Model, &st.ValidatedAt, &st.RevokedAt,
			&st.QuotaLimit, &st.QuotaUsed, &free)
	if err != nil {
		return nil, err // sql.ErrNoRows = no key
	}
	st.IsFreeTier = free != 0
	if st.RevokedAt > 0 {
		return nil, nil // revoked => not active
	}
	return &st, nil
}

// revokeByok marks the row revoked (kept for audit; key_fp stays as the only trace).
func (s *Store) revokeByok(userID string) error {
	_, err := s.db.Exec(`UPDATE byok_status SET revoked_at = ? WHERE user_id = ? AND revoked_at = 0`,
		time.Now().Unix(), userID)
	return err
}

func (s *Store) logAIUsage(userID, feature string, in, out int) error {
	_, err := s.db.Exec(`INSERT INTO ai_usage (user_id, feature, tokens_in, tokens_out, created_at) VALUES (?,?,?,?,?)`,
		userID, feature, in, out, time.Now().Unix())
	return err
}

// byokContribution is the per-key impact summary ("what your key brought").
type byokContribution struct {
	HasKey      bool                       `json:"hasKey"`
	Fp          string                     `json:"fp,omitempty"` // masked 8-hex, never the key
	Model       string                     `json:"model,omitempty"`
	ValidatedAt int64                      `json:"validatedAt,omitempty"`
	Calls       int64                      `json:"calls"`
	TokensIn    int64                      `json:"tokensIn"`
	TokensOut   int64                      `json:"tokensOut"`
	TokensTotal int64                      `json:"tokensTotal"`
	ByFeature   map[string]int64           `json:"byFeature"` // feature -> call count
	LastUsed    int64                      `json:"lastUsed,omitempty"`
	Since       int64                      `json:"since,omitempty"` // unix ts window start
}

// contributionByKey aggregates ai_usage for a user (default: last 30 days).
func (s *Store) contributionByKey(userID string) (*byokContribution, error) {
	window := time.Now().AddDate(0, 0, -30).Unix()
	c := &byokContribution{ByFeature: map[string]int64{}}
	st, err := s.getByok(userID)
	if err == nil && st != nil {
		c.HasKey = true
		c.Fp = st.KeyFP
		c.Model = st.Model
		c.ValidatedAt = st.ValidatedAt
	}
	rows, err := s.db.Query(`SELECT feature, COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0), COALESCE(MAX(created_at),0)
		FROM ai_usage WHERE user_id = ? AND created_at >= ? GROUP BY feature`, userID, window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		var n, tin, tout, last int64
		if err := rows.Scan(&f, &n, &tin, &tout, &last); err != nil {
			return nil, err
		}
		c.ByFeature[f] = n
		c.Calls += n
		c.TokensIn += tin
		c.TokensOut += tout
		if last > c.LastUsed {
			c.LastUsed = last
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.TokensTotal = c.TokensIn + c.TokensOut
	c.Since = window
	return c, nil
}

// --- OpenRouter client (base URL overridable in tests) ---

// byokORBase is the OpenRouter base; tests point it at an httptest server.
var byokORBase = "https://openrouter.ai"

// byokORClient and byokCompletionClient are shared, so every call does not
// pay for a fresh connection pool (CodeRabbit: reuse one http.Client).
var byokORClient = &http.Client{Timeout: 15 * time.Second}
var byokCompletionClient = &http.Client{Timeout: 120 * time.Second}

// byokModelAllowlist is the hardcoded free-tier allowlist (Ox guardrail:
// "free tier only; hardcoded allowlist" — no arbitrary model billing).
var byokModelAllowlist = map[string]bool{
	"stealth/ox-alpha": true,
}

// byokFeatureAllowlist bounds the audit `feature` label to the known set so
// the ai_usage table can't be polluted with arbitrary strings.
var byokFeatureAllowlist = map[string]bool{
	"mason": true, "soul": true, "dream": true, "byok.use": true,
}

// byokLimiter is a tiny per-user token bucket for the OpenRouter-backed
// endpoints (each POST /api/byok and /api/byok/use triggers one outbound
// call, so they get a cheap in-memory rate limit — fail-closed).
type byokLimiter struct {
	mu    sync.Mutex
	limit int
	win   time.Duration
	hits  map[string][]int64
}

func newByokLimiter(limit int, win time.Duration) *byokLimiter {
	return &byokLimiter{limit: limit, win: win, hits: map[string][]int64{}}
}

func (l *byokLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Unix()
	cut := now - int64(l.win.Seconds())
	keep := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t >= cut {
			keep = append(keep, t)
		}
	}
	l.hits[key] = keep
	if len(keep) >= l.limit {
		return false
	}
	l.hits[key] = append(l.hits[key], now)
	return true
}

var (
	byokValidateLimiter = newByokLimiter(5, time.Minute)  // 5 key validations/min/user
	byokUseLimiter      = newByokLimiter(10, time.Minute) // 10 completions/min/user
)

var errInvalidKey = errors.New("invalid OpenRouter key")

// orKeyInfo is the quota-relevant slice of GET /api/v1/key.
type orKeyInfo struct {
	Limit      float64
	Used       float64
	IsFreeTier bool
}

// orCompletion is the slice of POST /api/v1/chat/completions we need.
type orCompletion struct {
	Text   string
	In     int
	Out    int
	Model  string
}

// validateOpenRouterKey calls GET /api/v1/key with the caller's key. The key
// is used only in this outbound Authorization header and never stored/logged.
func validateOpenRouterKey(ctx context.Context, key string) (*orKeyInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, byokORBase+"/api/v1/key", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := byokORClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errInvalidKey
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter key check: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data struct {
			Limit float64 `json:"limit"`
			Usage struct {
				TotalUsage float64 `json:"total_usage"`
				Limit      float64 `json:"limit"`
				IsFreeTier bool    `json:"is_free_tier"`
			} `json:"usage"`
			IsFreeTier bool `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openrouter key check: bad response: %w", err)
	}
	info := &orKeyInfo{
		Limit:      parsed.Data.Usage.Limit,
		Used:       parsed.Data.Usage.TotalUsage,
		IsFreeTier: parsed.Data.Usage.IsFreeTier || parsed.Data.IsFreeTier,
	}
	if info.Limit == 0 {
		info.Limit = parsed.Data.Limit
	}
	return info, nil
}

// completeWithKey runs one chat completion with the caller's key, in-memory.
// The key is attached to the outbound request and discarded on return.
func completeWithKey(ctx context.Context, key, model, prompt string) (*orCompletion, error) {
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 2048,
		"stream":     false,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, byokORBase+"/api/v1/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := byokCompletionClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errInvalidKey
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter completion: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Model  string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("openrouter completion: bad response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, errors.New("openrouter completion: no choices")
	}
	return &orCompletion{
		Text:  parsed.Choices[0].Message.Content,
		In:    parsed.Usage.PromptTokens,
		Out:   parsed.Usage.CompletionTokens,
		Model: parsed.Model,
	}, nil
}

// keyFingerprint is the ONLY thing the server ever keeps from a key.
func keyFingerprint(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])[:8]
}

// keyLooksValid is the client-format gate (sk-or-v1- prefix, sane length).
func keyLooksValid(key string) bool {
	return strings.HasPrefix(key, "sk-or-v1-") && len(key) >= 20 && len(key) <= 256
}

// maskFP renders the display form Ox specified: ••••a3f9.
func maskFP(fp string) string {
	return "••••" + fp
}

// --- handlers ---

// handleByok dispatches /api/byok: POST upsert+validate, GET status,
// GET /api/byok/contribution (per-key impact), DELETE revoke. Auth = hearth_session cookie.
// Each path only accepts its own method(s); anything else is 405.
func (h *Hub) handleByok(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/byok/contribution":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.byokContribution(w, r)
	case "/api/byok/status":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.byokStatus(w, r)
	default: // /api/byok
		switch r.Method {
		case http.MethodPost:
			h.byokUpsert(w, r)
		case http.MethodGet:
			h.byokStatus(w, r)
		case http.MethodDelete:
			h.byokDelete(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

// byokContribution: GET /api/byok/contribution — what this key brought to
// the ecosystem: calls, tokens, features powered, last used (30-day window).
// Returns fingerprints + aggregates only — never the key.
func (h *Hub) byokContribution(w http.ResponseWriter, r *http.Request) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	c, err := h.store.contributionByKey(sess.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "contribution query failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "contribution": c})
}

// byokUpsert: POST /api/byok {key, model?} — validate against OpenRouter,
// store ONLY the fingerprint + quota snapshot, return masked fp.
func (h *Hub) byokUpsert(w http.ResponseWriter, r *http.Request) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	var body struct {
		Key   string `json:"key"`
		Model string `json:"model"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10) // 16KB: a key is ~73 chars
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	key := strings.TrimSpace(body.Key)
	if !keyLooksValid(key) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid key format (expected sk-or-v1-...)"})
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = "stealth/ox-alpha"
	}
	if !byokModelAllowlist[model] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "model not allowed"})
		return
	}
	if !byokValidateLimiter.allow(sess.UserID) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "too many validation attempts, slow down"})
		return
	}
	// Validate against OpenRouter. The key lives only in this request's
	// outbound Authorization header; it is discarded before we write anything.
	info, err := validateOpenRouterKey(r.Context(), key)
	if err != nil {
		if errors.Is(err, errInvalidKey) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid OpenRouter key"})
			return
		}
		log.Printf("byok validate (network): %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "could not reach OpenRouter"})
		return
	}
	// Ox guardrail: this instance is free-tier only. A paid key is rejected
	// rather than silently billed against the user's allowance.
	if !info.IsFreeTier {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "free-tier only: connect a free OpenRouter key"})
		return
	}
	fp := keyFingerprint(key)
	st := ByokStatus{
		UserID:      sess.UserID,
		KeyFP:       fp,
		Model:       model,
		ValidatedAt: time.Now().Unix(),
		QuotaLimit:  info.Limit,
		QuotaUsed:   info.Used,
		IsFreeTier:  info.IsFreeTier,
	}
	if err := h.store.upsertByok(st); err != nil {
		log.Printf("byok upsert: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.emitActivity("", sess.UserID, "member", "byok", "validate", "",
		diffJSON(map[string]any{"fp": fp, "model": model}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "fp": fp, "masked": maskFP(fp), "model": model,
		"validatedAt": st.ValidatedAt,
		"quota": map[string]any{"limit": st.QuotaLimit, "used": st.QuotaUsed, "isFreeTier": st.IsFreeTier},
	})
}

// byokStatus: GET /api/byok/status — key presence + masked fp + quota snapshot.
func (h *Hub) byokStatus(w http.ResponseWriter, r *http.Request) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	st, err := h.store.getByok(sess.UserID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("byok status: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hasKey": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "hasKey": true,
		"fp": st.KeyFP, "masked": maskFP(st.KeyFP), "model": st.Model,
		"validatedAt": st.ValidatedAt,
		"quota": map[string]any{"limit": st.QuotaLimit, "used": st.QuotaUsed, "isFreeTier": st.IsFreeTier},
	})
}

// byokDelete: DELETE /api/byok — revoke (fp row kept for audit).
func (h *Hub) byokDelete(w http.ResponseWriter, r *http.Request) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	if err := h.store.revokeByok(sess.UserID); err != nil {
		log.Printf("byok revoke: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	h.emitActivity("", sess.UserID, "member", "byok", "revoke", "",
		diffJSON(map[string]any{"fp": ""}), sanitizeIP(r.RemoteAddr))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": true})
}

// byokUse: POST /api/byok/use {key, prompt, feature?, model?} — the
// agent-integration point (Ox decision §1.4): server-side agents (Mason)
// run with the initiator's key passed per-request, held in memory for ONE
// completion, then discarded. Key never persisted; usage is audited in
// ai_usage so per-user dashboards exist without touching keys.
func (h *Hub) byokUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	var body struct {
		Key     string `json:"key"`
		Prompt  string `json:"prompt"`
		Feature string `json:"feature"`
		Model   string `json:"model"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64KB: prompt cap
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	key := strings.TrimSpace(body.Key)
	if !keyLooksValid(key) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid key format (expected sk-or-v1-...)"})
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "prompt required"})
		return
	}
	if len(prompt) > 32<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "prompt too long"})
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = "stealth/ox-alpha"
	}
	if !byokModelAllowlist[model] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "model not allowed"})
		return
	}
	feature := strings.TrimSpace(body.Feature)
	if feature == "" {
		feature = "byok.use"
	}
	if len(feature) > 32 || !byokFeatureAllowlist[feature] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "feature not allowed"})
		return
	}
	if !byokUseLimiter.allow(sess.UserID) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "too many AI calls, slow down"})
		return
	}
	comp, err := completeWithKey(r.Context(), key, model, prompt)
	if err != nil {
		if errors.Is(err, errInvalidKey) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid OpenRouter key"})
			return
		}
		log.Printf("byok use (network): %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "OpenRouter request failed"})
		return
	}
	// audit usage — tokens only, never key material
	if err := h.store.logAIUsage(sess.UserID, feature, comp.In, comp.Out); err != nil {
		log.Printf("byok usage log: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "completion": comp.Text, "model": comp.Model,
		"usage": map[string]any{"promptTokens": comp.In, "completionTokens": comp.Out},
	})
}
