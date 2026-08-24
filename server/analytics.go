package main

// Analytics v0 — scrubbed event intake + server-side aggregation + daily pulse.
//
// Flow (ox-decided architecture, /tmp/hearth-analytics-plan.md):
//   - POST /api/events    — batched product events from the client. Every event
//     is allowlisted, PII-scrubbed (OpenRouter keys / emails / IPs redacted in
//     ALL string properties AND keys — defense in depth: the client scrubs
//     before send, the server scrubs again before persist), rate-limited per
//     session, then one scrubbed row lands in analytics_events.
//   - High-volume editor ops NEVER ride this path. handleEdit bumps per-space
//     per-hour counters (analytics_op_counters): 10k paint ops = 1 row, not
//     10k rows. The client's paint_op event (1:50 sampled) exists only for
//     funnel analysis.
//   - GET /api/analytics/pulse — admin-gated (X-API-Key, ADMIN_API_KEY env,
//     constant-time, same gate as /api/admin/*) daily pulse: joins, active,
//     errors, BYOK count, top events, op totals.
//
// No PII is persisted by construction: props are scrubbed before insert and
// chat text never enters props (client sends char_len only).

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// eventAllowlist is the 20 core product events (v0). Names outside this list
// are rejected before anything is persisted.
var eventAllowlist = map[string]bool{
	"join": true, "world_enter": true, "world_leave": true, "portal_transit": true,
	"paint_op": true, "publish": true, "chat_send": true, "friend_add": true,
	"byok_key_paste": true, "byok_key_validate": true, "ai_use": true,
	"epic_pledge": true, "orbit_level_up": true, "page_view": true,
	"session_start": true, "session_end": true, "world_create": true,
	"editor_open": true, "editor_save": true, "error": true,
}

// Intake limits (ox Q3 guardrail numbers). Package vars so tests can lower
// them without touching production behavior.
var (
	maxEventsPerBatch = 50     // events per POST
	maxPropsPerEvent  = 64     // props per event
	maxPropKeyLen     = 64     // prop key bytes
	maxPropValueLen   = 512    // prop string value bytes
	maxBodyBytes      = 1 << 20 // 1 MiB request cap
	// Rate limit: 120 events / 60s sliding window per session (burst 120,
	// sustained 2/s — comfortably above a human, way below a bot).
	evRateLimit   = 120
	evRateWindow  = time.Minute
	evRateMaxKeys = 8192 // bound the limiter's memory
)

// --- PII scrubber (defense in depth: also runs client-side in scrub.ts) ---

var (
	scrubKeyRe    = regexp.MustCompile(`sk-or-v1-[A-Za-z0-9]{8,}`)
	scrubEmailRe  = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`)
	scrubIPv4Re   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	scrubIPv6Re   = regexp.MustCompile(`\b[0-9a-fA-F:]{2,39}:[0-9a-fA-F:]{2,39}\b`)
	scrubBearerRe = regexp.MustCompile(`(?i)bearer\s+[a-z0-9\-._~+/]+=*`)
)

// scrubPII redacts OpenRouter keys, emails, IPs and bearer tokens in a string.
func scrubPII(s string) string {
	s = scrubKeyRe.ReplaceAllString(s, "[REDACTED_KEY]")
	s = scrubEmailRe.ReplaceAllString(s, "[REDACTED_EMAIL]")
	s = scrubIPv4Re.ReplaceAllString(s, "[REDACTED_IP]")
	s = scrubIPv6Re.ReplaceAllString(s, "[REDACTED_IP]")
	s = scrubBearerRe.ReplaceAllString(s, "[REDACTED_TOKEN]")
	return s
}

// scrubProps returns a scrubbed flat copy of props. Only scalar values
// (string/float64/bool/nil) are accepted; any nested object/array marks the
// props invalid (rejected by the caller) — v0 keeps payloads small and flat.
// Keys are scrubbed too (defense in depth).
func scrubProps(props map[string]any) (map[string]any, bool) {
	out := make(map[string]any, len(props))
	for k, v := range props {
		sk := scrubPII(k)
		if len(sk) > maxPropKeyLen {
			return nil, false
		}
		switch t := v.(type) {
		case string:
			st := scrubPII(t)
			if len(st) > maxPropValueLen {
				return nil, false
			}
			out[sk] = st
		case float64, bool, nil:
			out[sk] = t
		default:
			return nil, false // nested objects/arrays rejected in v0
		}
	}
	return out, true
}

// --- per-session sliding-window rate limiter ---

type rateWindow struct {
	mu  sync.Mutex
	win map[string]*rateEntry
}

type rateEntry struct {
	start time.Time
	count int
}

func newRateWindow() *rateWindow {
	return &rateWindow{win: map[string]*rateEntry{}}
}

// allow reports whether key may emit n more events within the window.
func (rw *rateWindow) allow(key string, n int) bool {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	now := time.Now()
	if e, ok := rw.win[key]; ok && now.Sub(e.start) < evRateWindow {
		e.count += n
		return e.count <= evRateLimit
	}
	if len(rw.win) >= evRateMaxKeys {
		// Bound memory: drop expired entries; if still full, evict one
		// arbitrary key (map iteration order is random — fine for a limiter).
		for k, e := range rw.win {
			if now.Sub(e.start) >= evRateWindow {
				delete(rw.win, k)
			}
		}
		if len(rw.win) >= evRateMaxKeys {
			for k := range rw.win {
				delete(rw.win, k)
				break
			}
		}
	}
	rw.win[key] = &rateEntry{start: now, count: n}
	return n <= evRateLimit
}

// --- server error counter (panic recovery) ---

var serverPanicCount atomic.Int64

// recoverPanic wraps the mux: a panicking handler returns a 500 instead of
// killing the process, logs the stack, and bumps the pulse's panic counter.
// (sentry-go capture is the W2 upgrade; v0 keeps the counter + log.)
func recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				serverPanicCount.Add(1)
				log.Printf("PANIC %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- REST handlers ---

// analyticsRateKey picks the rate-limit bucket for an event POST: the session
// cookie when present, else the caller's IP (sanitized, never persisted).
func (h *Hub) analyticsRateKey(r *http.Request) string {
	if ck, err := r.Cookie(cookieName); err == nil && ck.Value != "" {
		return "sess:" + ck.Value
	}
	return "ip:" + sanitizeIP(r.RemoteAddr)
}

// sessionFromCookie resolves the auth session from the hearth_session cookie.
func (h *Hub) sessionFromCookie(r *http.Request) (*Session, error) {
	ck, err := r.Cookie(cookieName)
	if err != nil || ck.Value == "" {
		return nil, nil
	}
	return h.store.GetSession(ck.Value)
}

// handleAnalyticsEvents: POST /api/events — batched, validated, scrubbed,
// rate-limited event intake. Unauthenticated by design (guests must be able
// to report events); abuse is bounded by the per-session limiter.
func (h *Hub) handleAnalyticsEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Events []struct {
			Name  string         `json:"name"`
			Props map[string]any `json:"props"`
		} `json:"events"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(maxBodyBytes)))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	if len(body.Events) == 0 || len(body.Events) > maxEventsPerBatch {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "events must be 1..50"})
		return
	}
	if !h.evRate.allow(h.analyticsRateKey(r), len(body.Events)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "rate limited: 120 events/min"})
		return
	}

	sessionID, userID := "", ""
	if sess, err := h.sessionFromCookie(r); err == nil && sess != nil {
		sessionID, userID = sess.ID, sess.UserID
	}

	accepted, rejected := 0, 0
	for _, ev := range body.Events {
		if !eventAllowlist[ev.Name] || len(ev.Name) > maxEventNameLen {
			rejected++
			continue
		}
		if len(ev.Props) > maxPropsPerEvent {
			rejected++
			continue
		}
		props, ok := scrubProps(ev.Props)
		if !ok {
			rejected++
			continue
		}
		propsJSON, err := json.Marshal(props)
		if err != nil {
			rejected++
			continue
		}
		if err := h.store.InsertAnalyticsEvent(ev.Name, string(propsJSON), sessionID, userID); err != nil {
			log.Printf("analytics insert %s: %v", ev.Name, err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		accepted++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": accepted, "rejected": rejected})
}

// handleAnalyticsPulse: GET /api/analytics/pulse — admin-gated daily pulse.
func (h *Hub) handleAnalyticsPulse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.authorizeAdmin(r) == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authorized"})
		return
	}
	pulse, err := h.store.Pulse(time.Now().UTC())
	if err != nil {
		log.Printf("pulse: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "pulse failed"})
		return
	}
	pulse.ServerPanics = int(serverPanicCount.Load())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pulse": pulse})
}

// adminGate authenticates an analytics admin request. It mirrors the S9
// admin gate (X-API-Key vs ADMIN_API_KEY, constant-time) but lives here so
// the pulse endpoint works even before the admin surface is wired.
func (h *Hub) adminGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := os.Getenv("ADMIN_API_KEY")
		if want == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "admin api key not configured"})
			return
		}
		key := r.Header.Get("X-API-Key")
		if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(want)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid or missing API key"})
			return
		}
		next(w, r)
	}
}

// --- store layer ---

// InsertAnalyticsEvent persists one scrubbed event row.
func (s *Store) InsertAnalyticsEvent(name, propsJSON, sessionID, userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO analytics_events (name, props, session_id, user_id, created_at) VALUES (?,?,?,?,?)`,
		name, propsJSON, sessionID, userID, now)
	return err
}

// BumpOpCounter increments the per-space per-hour op counter for one applied
// editor op (paint|erase|place|zone|portal|publish). High-volume aggregation:
// N ops in an hour = 1 row, never N rows.
func (s *Store) BumpOpCounter(spaceID, kind string) error {
	hour := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO analytics_op_counters (space_id, hour, kind, count) VALUES (?,?,?,1)
		 ON CONFLICT(space_id, hour, kind) DO UPDATE SET count = count + 1`,
		spaceID, hour, kind)
	return err
}

// EventCount is one top-event row in the pulse.
type EventCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// OpCount is one per-space per-kind op total in the pulse.
type OpCount struct {
	Space string `json:"space"`
	Kind  string `json:"kind"`
	Count int64  `json:"count"`
}

// Pulse is the daily pulse payload (ox Q3: THE one dashboard, 6 panels).
type Pulse struct {
	Date         string       `json:"date"`
	JoinsToday   int          `json:"joins_today"`
	ActiveToday  int          `json:"active_today"`
	Active24h    int          `json:"active_24h"`
	ErrorsToday  int          `json:"errors_today"`
	ByokCount    int          `json:"byok_count"`
	OpsToday     int64        `json:"ops_today"`
	ServerPanics int          `json:"server_panics"`
	TopEvents    []EventCount `json:"top_events"`
	OpTotals     []OpCount    `json:"op_totals"`
}

// Pulse aggregates today's + trailing-24h numbers. byok_count is defensive:
// the byok_status table may not exist on a fresh DB (feature branch), so a
// lookup error yields 0 rather than failing the whole pulse.
func (s *Store) Pulse(now time.Time) (*Pulse, error) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayStartStr := dayStart.Format(time.RFC3339)
	dayStr := dayStart.Format("2006-01-02")
	ago24 := now.Add(-24 * time.Hour).Format(time.RFC3339)

	p := &Pulse{Date: dayStr}

	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sessions WHERE created_at >= ?`, dayStartStr).Scan(&p.JoinsToday); err != nil {
		return nil, err
	}
	// active = distinct users who joined a session OR sent a message OR
	// reported an analytics event in the window (bots excluded when their
	// sessions/events carry a user_id — see analytics-substrate note).
	if err := s.db.QueryRow(`
		SELECT COUNT(DISTINCT u) FROM (
			SELECT user_id u FROM sessions WHERE created_at >= ?
			UNION SELECT user_id FROM messages WHERE ts >= ?
			UNION SELECT user_id FROM analytics_events WHERE created_at >= ? AND user_id <> ''
		)`, dayStartStr, dayStartStr, dayStartStr).Scan(&p.ActiveToday); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`
		SELECT COUNT(DISTINCT u) FROM (
			SELECT user_id u FROM sessions WHERE created_at >= ?
			UNION SELECT user_id FROM messages WHERE ts >= ?
			UNION SELECT user_id FROM analytics_events WHERE created_at >= ? AND user_id <> ''
		)`, ago24, ago24, ago24).Scan(&p.Active24h); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM analytics_events WHERE name = 'error' AND created_at >= ?`, dayStartStr).Scan(&p.ErrorsToday); err != nil {
		return nil, err
	}
	// Defensive: byok_status may be absent until the BYOK branch lands.
	_ = s.db.QueryRow(
		`SELECT COUNT(*) FROM byok_status WHERE validated_at IS NOT NULL AND revoked_at IS NULL`).Scan(&p.ByokCount)
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(count),0) FROM analytics_op_counters WHERE hour >= ?`, dayStartStr).Scan(&p.OpsToday); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT name, COUNT(*) FROM analytics_events WHERE created_at >= ? GROUP BY name ORDER BY COUNT(*) DESC LIMIT 10`, dayStartStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ec EventCount
		if err := rows.Scan(&ec.Name, &ec.Count); err != nil {
			return nil, err
		}
		p.TopEvents = append(p.TopEvents, ec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	opRows, err := s.db.Query(
		`SELECT space_id, kind, SUM(count) FROM analytics_op_counters WHERE hour >= ? GROUP BY space_id, kind ORDER BY space_id, kind`, dayStartStr)
	if err != nil {
		return nil, err
	}
	defer opRows.Close()
	for opRows.Next() {
		var oc OpCount
		if err := opRows.Scan(&oc.Space, &oc.Kind, &oc.Count); err != nil {
			return nil, err
		}
		p.OpTotals = append(p.OpTotals, oc)
	}
	if err := opRows.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

// maxEventNameLen bounds allowlisted event names (checked in the handler).
const maxEventNameLen = 64
