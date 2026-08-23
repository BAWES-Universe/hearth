package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsReadLimit    = 64 * 1024
	wsReadTimeout  = 70 * time.Second
	wsWriteTimeout = 10 * time.Second
	pingEvery      = 30 * time.Second
	cookieName     = "hearth_session"
	chatMaxBytes   = 2048
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// v0: allow any origin (the client team serves from any origin in dev).
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client is one WebSocket connection.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte

	mu            sync.Mutex
	Session       *Session
	Entity        *Entity
	spaceID       string
	lastX, lastY  int
	rate          *TokenBucket
	lastEntities  map[string]entityPos
	lastSpace     string
	lastStateSent time.Time
}

func newClient(h *Hub, conn *websocket.Conn) *Client {
	return &Client{
		hub:          h,
		conn:         conn,
		send:         make(chan []byte, sendQueueSize),
		rate:         NewTokenBucket(5, 0.5), // burst 5 / 10s, sustained 1 / 2s
		lastEntities: map[string]entityPos{},
	}
}

func (c *Client) setEntity(e *Entity) {
	c.mu.Lock()
	c.Entity = e
	c.mu.Unlock()
}

func (c *Client) setSpace(id string) {
	c.mu.Lock()
	c.spaceID = id
	c.mu.Unlock()
}

func (c *Client) setPos(x, y int) {
	c.mu.Lock()
	c.lastX, c.lastY = x, y
	c.mu.Unlock()
}

// pos returns the client's last confirmed position (cross-goroutine safe).
func (c *Client) pos() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastX, c.lastY
}

func (c *Client) enqueue(b []byte) {
	select {
	case c.send <- b:
	default:
		// queue full — drop; next 12Hz tick retries
	}
}

func (c *Client) enqueueJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("marshal outbound: %v", err)
		return
	}
	c.enqueue(b)
}

// emit sends a PROTOCOL.md envelope: {"v":1,"t":"<type>","d":{...}}.
// All server->client messages must go through this so the wire format stays
// the frozen contract. (Legacy flat "type" messages are removed.)
func (c *Client) emit(t string, d map[string]any) {
	c.enqueueJSON(map[string]any{"v": 1, "t": t, "d": d})
}

func (c *Client) sendError(code, msg string) {
	c.emit("error", map[string]any{"code": code, "message": msg})
}

// handleWS upgrades the connection, resolves guest auth (cookie or ?deviceKey=)
// and starts the pumps.
func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	session, _ := h.resolveAuth(w, r)

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	c := newClient(h, conn)
	c.Session = session
	log.Printf("ws connect: %s (session=%s)", r.RemoteAddr, sessionIDOr(c))
	go c.writePump()
	go c.readPump()
}

func sessionIDOr(c *Client) string {
	if c.Session != nil {
		return c.Session.ID
	}
	return "none"
}

// resolveAuth authenticates at handshake time: existing httpOnly cookie wins,
// otherwise a ?deviceKey= query param creates a session and sets the cookie on
// the 101 response. Join can also authenticate later (sessionId/deviceKey msg).
func (h *Hub) resolveAuth(w http.ResponseWriter, r *http.Request) (*Session, *User) {
	if ck, err := r.Cookie(cookieName); err == nil && ck.Value != "" {
		if s, err := h.store.GetSession(ck.Value); err == nil && s != nil {
			return s, s.User
		}
	}
	deviceKey := r.URL.Query().Get("deviceKey")
	if deviceKey == "" {
		return nil, nil
	}
	u, err := h.store.UpsertUser(deviceKey, "")
	if err != nil {
		return nil, nil
	}
	s, err := h.store.CreateSession(u)
	if err != nil {
		return nil, nil
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.ID, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 30 * 24 * 3600,
	})
	return s, u
}

// --- pumps ---

func (c *Client) writePump() {
	ticker := time.NewTicker(pingEvery)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(wsReadLimit)
	c.conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		return nil
	})
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.handleMessage(raw)
	}
}

// --- dispatch ---

func (c *Client) handleMessage(raw []byte) {
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.sendError("bad_json", "invalid JSON")
		return
	}
	// PROTOCOL.md envelope: {"v":1,"t":"<type>","id":..,"ts":..,"d":{...}}.
	// Accept both "t" (frozen contract) and legacy "type" (compat), and merge
	// the "d" payload into the top level so handlers read fields uniformly.
	t, _ := msg["t"].(string)
	if t == "" {
		t, _ = msg["type"].(string)
	}
	if d, ok := msg["d"].(map[string]any); ok {
		for k, v := range d {
			if _, exists := msg[k]; !exists {
				msg[k] = v
			}
		}
	}
	switch t {
	case "join":
		c.handleJoin(msg)
	case "move":
		c.handleMove(msg)
	case "chat":
		c.handleChat(msg)
	case "edit":
		c.handleEdit(msg)
	case "portal":
		c.handlePortal(msg)
	case "signal":
		c.handleSignal(msg)
	case "media":
		c.handleMedia(msg)
	case "bot_msg":
		c.handleBotMsg(msg)
	case "ping":
		c.emit("pong", map[string]any{"t": time.Now().UnixMilli()})
	default:
		c.sendError("unknown_type", "unknown message type: "+t)
	}
}

func getString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func getInt(m map[string]any, k string) (int, bool) {
	f, ok := m[k].(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// --- handlers ---

func (c *Client) handleJoin(msg map[string]any) {
	deviceKey := getString(msg, "deviceKey")
	sessionID := getString(msg, "sessionId")
	name := getString(msg, "name")
	spaceID := getString(msg, "spaceId")
	if spaceID == "" {
		spaceID = getString(msg, "space") // client sends "space"
	}
	if spaceID == "" {
		// Universal spawn: '/' (no spaceId) routes to town-square — the one
		// world every visitor enters. No map picker, no room-of-the-day.
		spaceID = "town-square"
	}

	c.mu.Lock()
	sess := c.Session
	c.mu.Unlock()
	if sess == nil {
		if sessionID != "" {
			if s, err := c.hub.store.GetSession(sessionID); err == nil && s != nil {
				sess = s
			}
		}
		if sess == nil && deviceKey != "" {
			u, err := c.hub.store.UpsertUser(deviceKey, name)
			if err == nil {
				sess, _ = c.hub.store.CreateSession(u)
			}
		}
		if sess == nil {
			c.sendError("auth_required", "join requires deviceKey or sessionId")
			return
		}
		c.mu.Lock()
		c.Session = sess
		c.mu.Unlock()
	}

	sp := c.hub.space(spaceID)
	if sp == nil {
		c.sendError("space_not_found", "no such space: "+spaceID)
		return
	}

	// leave previous space/entity (re-join)
	if c.Entity != nil {
		if old := c.hub.space(c.spaceID); old != nil {
			old.RemoveEntity(c.Entity)
		}
		c.setEntity(nil)
	}

	x, y, hasXY := 0, 0, false
	if vx, ok := getInt(msg, "x"); ok {
		x, y, hasXY = vx, 0, true
	}
	if vy, ok := getInt(msg, "y"); ok {
		y = vy
	} else {
		hasXY = false
	}
	if !hasXY {
		x, y = sp.World.Spawn.X, sp.World.Spawn.Y
	}

	e := &Entity{
		ID: sess.ID, Name: name, X: x, Y: y, Dir: "down",
		UserID: sess.UserID, SessionID: sess.ID, Client: c,
	}
	if e.Name == "" {
		e.Name = "Guest-" + sess.ID[:4]
	}
	// Avatar: layered avatar_spec (validated + persisted per member) with
	// legacy color/icon fallback for old clients. Incoming spec wins; else the
	// member's stored spec; else a device-key default.
	if a, ok := msg["avatar"].(map[string]any); ok {
		if spec := parseAvatarSpec(a); spec != nil {
			s := resolveAvatarSpec(sess.UserID, spec)
			e.Avatar.Spec = &s
		}
		e.Avatar.Color = getString(a, "color")
		e.Avatar.Icon = getString(a, "icon")
	}
	if e.Avatar.Spec == nil {
		s := resolveAvatarSpec(sess.UserID, nil)
		e.Avatar.Spec = &s
	}
	if e.Avatar.Color == "" {
		e.Avatar.Color = defaultAvatarColor(e.Name)
	}
	if e.Avatar.Icon == "" {
		e.Avatar.Icon = "🙂"
	}
	sp.AddEntity(e)
	c.setEntity(e)
	c.setSpace(spaceID)
	c.setPos(e.X, e.Y)
	c.hub.register <- c

	if name != "" {
		c.hub.store.SetUserName(sess.UserID, name)
	}

	c.emit("welcome", map[string]any{
		"sessionId": sess.ID, "selfId": e.ID, "entityId": e.ID,
		"spaceId": spaceID, "name": e.Name, "x": e.X, "y": e.Y, "dir": e.Dir,
		"avatar": e.Avatar,
		"world":  sp.World.GeoJSON(),
	})
	// gravity/audit: presence event (Reach = unique visitors)
	c.hub.emitActivity(spaceID, sess.UserID, "member", "presence", "join", spaceID,
		diffJSON(map[string]any{"name": e.Name, "x": e.X, "y": e.Y}), sanitizeIP(c.conn.RemoteAddr().String()))
	log.Printf("join: %s (%s) -> %s @ %d,%d", e.Name, sess.ID[:8], spaceID, e.X, e.Y)
}

// defaultAvatarColor derives a stable accent from a name when the client did
// not send one (mirrors the client's per-name tinting).
func defaultAvatarColor(name string) string {
	cols := []string{"#8b5cf6", "#22d3ee", "#f472b6", "#4ade80", "#fb923c", "#e879f9", "#60a5fa", "#facc15"}
	h := 0
	for i := 0; i < len(name); i++ {
		h = (h*31 + int(name[i])) & 0x7fffffff
	}
	return cols[h%len(cols)]
}

func (c *Client) handleMove(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	x, okX := getInt(msg, "x")
	y, okY := getInt(msg, "y")
	if !okX || !okY {
		c.sendError("bad_move", "move requires x and y")
		return
	}
	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	sp.MoveEntity(c.Entity, x, y)
	if d := getString(msg, "dir"); d != "" {
		c.Entity.Dir = d
	}
	c.setPos(c.Entity.X, c.Entity.Y)
}

func (c *Client) handlePortal(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	portalID := getString(msg, "portalId")
	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	p := sp.World.FindPortal(portalID)
	if p == nil {
		c.sendError("portal_not_found", "no such portal: "+portalID)
		return
	}
	dx := c.Entity.X - p.X
	dy := c.Entity.Y - p.Y
	if dx*dx+dy*dy > 9 { // within ~3 tiles
		c.sendError("portal_too_far", "move to the portal tile first")
		return
	}
	target := c.hub.space(p.TargetSpace)
	if target == nil {
		c.sendError("portal_broken", "portal target space missing: "+p.TargetSpace)
		return
	}
	// Portal routing by world id: only published worlds (or town-square hub)
	// are reachable destinations. Draft owners may still enter their own
	// unpublished world to test it. Emits a nav event for gravity/audit.
	if meta, err := c.hub.store.worldMeta(p.TargetSpace); err == nil && !meta.IsPublished {
		ownerOK := c.Session != nil && meta.OwnerID == c.Session.UserID
		if !ownerOK {
			c.sendError("portal_broken", "portal target world not published: "+p.TargetSpace)
			return
		}
	}
	oldSpace := c.spaceID
	sp.RemoveEntity(c.Entity)
	target.AddEntity(c.Entity)
	c.setSpace(p.TargetSpace)
	c.setEntity(c.Entity)
	c.setPos(p.TargetX, p.TargetY)
	c.emit("portal", map[string]any{
		"portalId": p.ID,
		"spaceId":  p.TargetSpace, "x": p.TargetX, "y": p.TargetY,
	})
	actor := ""
	if c.Session != nil {
		actor = c.Session.UserID
	}
	c.hub.emitActivity(p.TargetSpace, actor, "member", "nav", "portal", p.TargetSpace,
		diffJSON(map[string]any{"portalId": p.ID, "from": oldSpace, "x": p.TargetX, "y": p.TargetY}),
		sanitizeIP(c.conn.RemoteAddr().String()))
	log.Printf("portal: %s used %s %s -> %s (%d,%d)", c.Entity.Name, oldSpace, p.ID, p.TargetSpace, p.TargetX, p.TargetY)
}

func (c *Client) handleSignal(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	to := getString(msg, "to")
	target := c.hub.findEntity(to)
	if target == nil || target.Client == nil {
		c.sendError("peer_not_found", "signal target offline: "+to)
		return
	}
	target.Client.emit("signal", map[string]any{"from": c.Entity.ID, "data": msg["data"]})
	log.Printf("signal relay: %s -> %s (type=%v)", c.Entity.ID[:8], to, msg["dataType"])
}

func (c *Client) handleMedia(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	to := getString(msg, "to")
	target := c.hub.findEntity(to)
	if target == nil || target.Client == nil {
		c.sendError("peer_not_found", "media target offline: "+to)
		return
	}
	// Pass-through relay for now — the media/ package integrates later.
	target.Client.emit("media", map[string]any{
		"from": c.Entity.ID,
		"action": getString(msg, "action"), "data": msg["data"],
	})
	log.Printf("media relay (pass-through, media/ integration pending): %s -> %s action=%s",
		c.Entity.ID[:8], to, getString(msg, "action"))
}

func (c *Client) handleBotMsg(msg map[string]any) {
	if c.Entity == nil {
		c.sendError("not_joined", "join a space first")
		return
	}
	text := getString(msg, "text")
	if text == "" {
		c.sendError("bad_bot_msg", "bot_msg requires text")
		return
	}
	if len(text) > chatMaxBytes {
		c.sendError("too_large", "bot_msg exceeds 2048 bytes")
		return
	}
	if !c.rate.Allow() {
		c.sendError("rate_limited", "slow down — burst 5/10s, sustained 1/2s")
		return
	}
	to := getString(msg, "to")
	sp := c.hub.space(c.spaceID)
	if sp == nil {
		return
	}
	if to != "" {
		if t := c.hub.findEntity(to); t != nil && t.Client != nil {
			t.Client.emit("bot_msg", map[string]any{"from": c.Entity.ID, "text": text})
		} else {
			c.sendError("peer_not_found", "bot_msg target offline: "+to)
			return
		}
	} else {
		sp.BroadcastEnvelope("bot_msg", map[string]any{"from": c.Entity.ID, "text": text})
	}
	c.hub.store.InsertMessage(c.spaceID, c.Session.ID, c.Session.UserID, c.Entity.Name, "bot", text)
	log.Printf("bot_msg relay: %s -> %s", c.Entity.Name, to)
}

// --- REST handlers ---

func (h *Hub) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.mu.RLock()
	nSpaces := len(h.spaces)
	nClients := len(h.clients)
	h.mu.RUnlock()
	totalEntities, totalBots := 0, 0
	for _, sp := range h.spaces {
		for _, e := range sp.EntitySnaps() {
			totalEntities++
			if e.Bot {
				totalBots++
			}
		}
	}
	sessions, _ := h.store.CountSessions()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "service": "hearth", "version": version,
		"uptime_s": int(time.Since(startTime).Seconds()),
		"spaces": nSpaces, "clients": nClients,
		"entities": totalEntities, "bots": totalBots,
		"sessions": sessions, "t": time.Now().UTC().Format(time.RFC3339),
	})
}

func (h *Hub) handleGuestAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		DeviceKey string `json:"deviceKey"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DeviceKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "deviceKey required"})
		return
	}
	u, err := h.store.UpsertUser(body.DeviceKey, body.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	s, err := h.store.CreateSession(u)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: s.ID, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: 30 * 24 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessionId": s.ID, "userId": u.ID, "name": u.Name})
}

func (h *Hub) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ck, err := r.Cookie(cookieName)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "not authenticated"})
		return
	}
	s, err := h.store.GetSession(ck.Value)
	if err != nil || s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid session"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessionId": s.ID, "userId": s.UserID, "name": s.User.Name})
}

// handleSpaces: POST create (GET lists — convenience).
func (h *Hub) handleSpaces(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad JSON"})
			return
		}
		world, err := h.store.CreateSpace(body.Name, body.Width, body.Height)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "db error"})
			return
		}
		h.mu.Lock()
		h.spaces[world.ID] = NewSpaceState(world)
		h.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "id": world.ID, "name": world.Name, "width": world.Width, "height": world.Height})
	case http.MethodGet:
		h.mu.RLock()
		list := make([]map[string]any, 0, len(h.spaces))
		for _, sp := range h.spaces {
			list = append(list, map[string]any{
				"id": sp.World.ID, "name": sp.World.Name,
				"width": sp.World.Width, "height": sp.World.Height,
				"entities": len(sp.EntitySnaps()),
			})
		}
		h.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "spaces": list})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSpaceGet: GET /api/spaces/{id} -> world JSON (tile map + zones + portals + live entities).
func (h *Hub) handleSpaceGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Path[len("/api/spaces/"):]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	world, err := h.store.LoadWorld(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "space not found"})
		return
	}
	var entities []*EntitySnap
	if sp := h.space(id); sp != nil {
		entities = sp.EntitySnaps()
	}
	writeJSON(w, http.StatusOK, world.WorldJSON(entities))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
