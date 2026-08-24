package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// T2 social layer — the friendship graph, friend presence, and who's-here.
//
// Wire contract (PROTOCOL.md untouched — these are ADDITIVE envelopes):
//
//	server -> client {t:'friend', d:{event, userId, name, status}}
//	             a friend-relation status changed for the recipient:
//	             event = request|accept|decline|remove
//	server -> client {t:'friend_presence', d:{userId, name, online, spaceId}}
//	             a friend joined/left a space or went offline/online
//
// REST surface (all require the hearth_session cookie, see sessionFromRequest):
//
//	GET    /api/friends            my friend list (per-row status, online+space)
//	POST   /api/friends            {friendId} | {deviceKey} -> send a request
//	                               (auto-accepts when an incoming request exists)
//	POST   /api/friends/{id}/accept    accept an incoming request
//	POST   /api/friends/{id}/decline   decline an incoming request
//	DELETE /api/friends/{id}           remove the friendship (either direction)
//	GET    /api/users?q=               user search by name (for adding)
//
// Persistence: the friends table (db.go migrate) holds one row PER DIRECTION:
// the requester's row is 'pending', the target's row is 'requested', and both
// flip to 'accepted' on accept. A viewer's own row therefore always carries
// their perspective, so GET /api/friends is a single SELECT.

const (
	friendPending   = "pending"   // outgoing request, waiting on the other side
	friendRequested = "requested" // incoming request, awaiting my decision
	friendAccepted  = "accepted"  // mutual
)

var (
	errAlreadyFriends    = errors.New("already friends")
	errDuplicateRequest  = errors.New("request already sent")
	errNoIncomingRequest = errors.New("no incoming request from this user")
)

// FriendRow is one friends-table row (viewer perspective).
type FriendRow struct {
	UserID   string `json:"userId"`
	FriendID string `json:"friendId"`
	Status   string `json:"status"`
	Since    string `json:"since"`
}

// FriendEntry is a friend-list item enriched for the client: display name,
// live presence (online + current space), resolved from the Hub at read time.
type FriendEntry struct {
	FriendID string `json:"friendId"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Online   bool   `json:"online"`
	SpaceID  string `json:"space,omitempty"`
	Since    string `json:"since"`
}

// --- store layer ---

// AddFriendRequest creates a bidirectional request pair (requester -> target).
// Returns an error when the pair is already accepted or the request already
// exists. Both rows land in one transaction so the pair can never split.
func (s *Store) AddFriendRequest(requester, target string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	err = tx.QueryRow(`SELECT status FROM friends WHERE user_id = ? AND friend_id = ?`, requester, target).Scan(&status)
	if err == nil {
		if status == friendAccepted {
			return errAlreadyFriends
		}
		return errDuplicateRequest
	}
	if _, err := tx.Exec(`INSERT INTO friends (user_id, friend_id, status, created_at, updated_at) VALUES (?,?,?,?,?)`,
		requester, target, friendPending, now, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO friends (user_id, friend_id, status, created_at, updated_at) VALUES (?,?,?,?,?)`,
		target, requester, friendRequested, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AcceptFriend flips an incoming request to accepted on both sides.
// accepter is the user acting; requester is the one who sent the request.
// Only the 'requested' row may be accepted — a user cannot accept their own
// outgoing request (that row is 'pending', never 'requested').
func (s *Store) AcceptFriend(accepter, requester string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE friends SET status = ?, updated_at = ? WHERE user_id = ? AND friend_id = ? AND status = ?`,
		friendAccepted, now, accepter, requester, friendRequested)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errNoIncomingRequest
	}
	if _, err := tx.Exec(`UPDATE friends SET status = ?, updated_at = ? WHERE user_id = ? AND friend_id = ? AND status = ?`,
		friendAccepted, now, requester, accepter, friendPending); err != nil {
		return err
	}
	return tx.Commit()
}

// DeclineFriend deletes an incoming request pair (requested + pending rows).
func (s *Store) DeclineFriend(accepter, requester string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM friends WHERE user_id = ? AND friend_id = ? AND status = ?`,
		accepter, requester, friendRequested)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errNoIncomingRequest
	}
	if _, err := tx.Exec(`DELETE FROM friends WHERE user_id = ? AND friend_id = ? AND status = ?`,
		requester, accepter, friendPending); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveFriend deletes the pair in ANY state (accepted friendships and
// pending/requested pairs both go). Deleting an accepted friendship is
// one-sided: the other side loses it too (the client refetches on the
// {t:'friend'} event).
func (s *Store) RemoveFriend(a, b string) error {
	_, err := s.db.Exec(`DELETE FROM friends WHERE (user_id = ? AND friend_id = ?) OR (user_id = ? AND friend_id = ?)`,
		a, b, b, a)
	return err
}

// ListFriends returns the viewer's own rows (their perspective per row).
func (s *Store) ListFriends(userID string) ([]FriendRow, error) {
	rows, err := s.db.Query(`SELECT user_id, friend_id, status, created_at FROM friends WHERE user_id = ? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FriendRow
	for rows.Next() {
		var r FriendRow
		if err := rows.Scan(&r.UserID, &r.FriendID, &r.Status, &r.Since); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FriendIDs returns the accepted friend ids of a user (presence fanout).
func (s *Store) FriendIDs(userID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT friend_id FROM friends WHERE user_id = ? AND status = ?`, userID, friendAccepted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AreFriends reports a mutual accepted friendship.
func (s *Store) AreFriends(a, b string) bool {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM friends WHERE user_id = ? AND friend_id = ? AND status = ?`,
		a, b, friendAccepted).Scan(&n)
	return err == nil && n > 0
}

// --- hub layer: live presence + fanout ---

// userPresence returns (online, spaceID) for a user: online when at least one
// of their clients has joined a space (entity present).
func (h *Hub) userPresence(userID string) (bool, string) {
	if userID == "" {
		return false, ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		c.mu.Lock()
		sess, spaceID, ent := c.Session, c.spaceID, c.Entity
		c.mu.Unlock()
		if sess != nil && sess.UserID == userID && ent != nil {
			return true, spaceID
		}
	}
	return false, ""
}

// clientsOfUser snapshots every connected client of a user (a user may be
// online from several devices at once — all of them receive friend events).
func (h *Hub) clientsOfUser(userID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*Client
	for c := range h.clients {
		c.mu.Lock()
		sess := c.Session
		c.mu.Unlock()
		if sess != nil && sess.UserID == userID {
			out = append(out, c)
		}
	}
	return out
}

// notifyFriendStatus fans a friend-relation change out to every connected
// client of targetUserID ({t:'friend'}, additive envelope).
func (h *Hub) notifyFriendStatus(targetUserID string, ev map[string]any) {
	for _, c := range h.clientsOfUser(targetUserID) {
		c.emit("friend", ev)
	}
}

// notifyFriendPresence fans a presence change (join/leave/offline) out to the
// accepted friends of a user ({t:'friend_presence'}, additive envelope).
func (h *Hub) notifyFriendPresence(userID string, ev map[string]any) {
	friends, err := h.store.FriendIDs(userID)
	if err != nil {
		log.Printf("friend presence fanout (%s): %v", userID, err)
		return
	}
	for _, f := range friends {
		for _, c := range h.clientsOfUser(f) {
			c.emit("friend_presence", ev)
		}
	}
}

// --- REST handlers ---

// handleFriends: GET list / POST request (or auto-accept when a request from
// the target already exists).
func (h *Hub) handleFriends(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listFriends(w, r)
	case http.MethodPost:
		h.requestFriend(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFriendRoute: /api/friends/{id} + /api/friends/{id}/accept|decline.
func (h *Hub) handleFriendRoute(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/friends/")
	id = strings.TrimSuffix(id, "/")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(id, "/accept") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.respondFriend(w, r, strings.TrimSuffix(id, "/accept"), "accept")
		return
	}
	if strings.HasSuffix(id, "/decline") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.respondFriend(w, r, strings.TrimSuffix(id, "/decline"), "decline")
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.removeFriend(w, r, id)
}

// friendSession resolves the acting user or 401s.
func (h *Hub) friendSession(w http.ResponseWriter, r *http.Request) (*Session, bool) {
	sess := h.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return nil, false
	}
	return sess, true
}

func (h *Hub) listFriends(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.friendSession(w, r)
	if !ok {
		return
	}
	rows, err := h.store.ListFriends(sess.UserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	out := make([]FriendEntry, 0, len(rows))
	for _, row := range rows {
		online, spaceID := h.userPresence(row.FriendID)
		out = append(out, FriendEntry{
			FriendID: row.FriendID,
			Name:     h.store.userDisplay(row.FriendID),
			Status:   row.Status,
			Online:   online,
			SpaceID:  spaceID,
			Since:    row.Since,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "friends": out})
}

func (h *Hub) requestFriend(w http.ResponseWriter, r *http.Request) {
	sess, ok := h.friendSession(w, r)
	if !ok {
		return
	}
	var body struct {
		FriendID  string `json:"friendId"`
		DeviceKey string `json:"deviceKey"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
		return
	}
	target := strings.TrimSpace(body.FriendID)
	if target == "" && strings.TrimSpace(body.DeviceKey) != "" {
		// raw deviceKey: hash it the same way UpsertUser does — the raw key
		// is never stored, only its sha256 prefix.
		target = hashDeviceKey(strings.TrimSpace(body.DeviceKey))
	}
	if target == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "friendId or deviceKey required"})
		return
	}
	if target == sess.UserID {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "you cannot friend yourself"})
		return
	}
	if h.store.userDisplay(target) == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "no such user"})
		return
	}
	// Friendly UX: an incoming request from the target is accepted by a POST
	// (mutual without a second round-trip).
	var existing string
	if err := h.store.db.QueryRow(`SELECT status FROM friends WHERE user_id = ? AND friend_id = ?`,
		sess.UserID, target).Scan(&existing); err == nil && existing == friendRequested {
		if err := h.store.AcceptFriend(sess.UserID, target); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		h.friendAccepted(sess.UserID, target, sanitizeIP(r.RemoteAddr))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": friendAccepted, "friendId": target})
		return
	}
	if err := h.store.AddFriendRequest(sess.UserID, target); err != nil {
		if err == errDuplicateRequest || err == errAlreadyFriends {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	// notify the target of the incoming request
	h.notifyFriendStatus(target, map[string]any{
		"event": "request", "userId": sess.UserID,
		"name": h.store.userDisplay(sess.UserID), "status": friendRequested,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "status": friendPending, "friendId": target})
}

// respondFriend: accept or decline an incoming request from friendID.
func (h *Hub) respondFriend(w http.ResponseWriter, r *http.Request, friendID, action string) {
	sess, ok := h.friendSession(w, r)
	if !ok {
		return
	}
	var err error
	switch action {
	case "accept":
		err = h.store.AcceptFriend(sess.UserID, friendID)
	case "decline":
		err = h.store.DeclineFriend(sess.UserID, friendID)
	}
	if err != nil {
		if err == errNoIncomingRequest {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "no incoming request from this user"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	if action == "accept" {
		h.friendAccepted(sess.UserID, friendID, sanitizeIP(r.RemoteAddr))
	} else {
		// the requester learns the request was declined
		h.notifyFriendStatus(friendID, map[string]any{
			"event": "decline", "userId": sess.UserID,
			"name": h.store.userDisplay(sess.UserID), "status": friendPending,
		})
	}
	status := ""
	if action == "accept" {
		status = friendAccepted
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status, "friendId": friendID})
}

// friendAccepted wires the accept through the whole stack: WS notify both
// sides + an activity row on the accepter's current space (social events ride
// the existing activity feed, docs/SOCIAL.md).
func (h *Hub) friendAccepted(accepter, requester, ipSource string) {
	// both sides see the new status
	h.notifyFriendStatus(requester, map[string]any{
		"event": "accept", "userId": accepter,
		"name": h.store.userDisplay(accepter), "status": friendAccepted,
	})
	h.notifyFriendStatus(accepter, map[string]any{
		"event": "accept", "userId": requester,
		"name": h.store.userDisplay(requester), "status": friendAccepted,
	})
	// social activity: append a 'friend' row to the accepter's current space
	// feed (only when they are actually in a space — the activity log is
	// world-keyed).
	if _, spaceID := h.userPresence(accepter); spaceID != "" {
		h.emitActivity(spaceID, accepter, "member", "friend", "accept", requester,
			diffJSON(map[string]any{
				"name": h.store.userDisplay(accepter),
				"friend": map[string]any{
					"id": requester, "name": h.store.userDisplay(requester),
				},
			}),
			ipSource)
	}
}

func (h *Hub) removeFriend(w http.ResponseWriter, r *http.Request, friendID string) {
	sess, ok := h.friendSession(w, r)
	if !ok {
		return
	}
	if err := h.store.RemoveFriend(sess.UserID, friendID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	// the other side loses the friend too — they refetch on this event
	h.notifyFriendStatus(friendID, map[string]any{
		"event": "remove", "userId": sess.UserID,
		"name": h.store.userDisplay(sess.UserID), "status": friendPending,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleUsers: GET /api/users?q= — name search for adding friends (excludes
// self; returns live presence so the client can show who's around).
func (h *Hub) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := h.sessionFromRequest(r)
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": []any{}})
		return
	}
	rows, err := h.store.db.Query(`SELECT id, name FROM users WHERE lower(name) LIKE ? ORDER BY name LIMIT 20`, "%"+q+"%")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	defer rows.Close()
	type userHit struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Online bool   `json:"online"`
	}
	out := []userHit{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		if sess != nil && id == sess.UserID {
			continue
		}
		online, _ := h.userPresence(id)
		out = append(out, userHit{ID: id, Name: name, Online: online})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "users": out})
}
