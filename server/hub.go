package main

import (
	"math/rand"
	"sync"
	"time"

	"hearth/media"
)

const (
	cellSize       = 8                // 8x8 spatial hash cells
	aoiRadius      = 20               // area-of-interest radius in tiles
	tickInterval   = time.Second / 12 // 12Hz coalesced state broadcast
	stateHeartbeat = time.Second      // force a state at least 1/s even if unchanged
	botMoveEvery   = 2 * time.Second
	sendQueueSize  = 256
	botsPerSpace   = 2
)

// Avatar is the visual identity broadcast with join/welcome/state. Legacy
// color+icon remains for old clients; Spec carries the layered avatar_spec
// (T1) that all viewers render identically. Sent with join, echoed in welcome,
// broadcast in state/roster.
type Avatar struct {
	Color string      `json:"color,omitempty"`
	Icon  string      `json:"icon,omitempty"`
	Spec  *AvatarSpec `json:"spec,omitempty"`
}

// Entity is a live presence in a space (player or ambient bot). Positions are
// RAM-only — never persisted. Coordinate reads/writes are synchronized via the
// owning SpaceState/hash locks; snapshot copies are handed out to consumers.
type Entity struct {
	ID        string
	Name      string
	X, Y      int
	Dir       string
	Bot       bool
	Avatar    Avatar
	UserID    string
	SessionID string
	Client    *Client // set once at creation, never mutated afterwards
}

// EntitySnap is an immutable copy of an entity's public fields, built while
// holding the spatial-hash lock so consumers never race with movers.
type EntitySnap struct {
	ID     string
	Name   string
	X, Y   int
	Dir    string
	Bot    bool
	Avatar Avatar
	Client *Client
}

func (e *EntitySnap) PublicJSON() map[string]any {
	return map[string]any{
		"id":     e.ID,
		"name":   e.Name,
		"x":      e.X,
		"y":      e.Y,
		"dir":    e.Dir,
		"bot":    e.Bot,
		"avatar": e.Avatar,
	}
}

// RosterJSON is PublicJSON plus the owning userId — used ONLY for the
// welcome roster and the additive presence envelope (the client matches
// roster rows against its friend list to gate the DM button). The frozen
// 12Hz state stream keeps PublicJSON's shape.
func (e *EntitySnap) RosterJSON() map[string]any {
	m := e.PublicJSON()
	if e.Client != nil { // ambient bots have no client/user
		m["userId"] = e.Client.sessionUserID()
	}
	return m
}

// SpatialHash is a uniform 8x8-grid spatial hash for AOI queries.
type SpatialHash struct {
	cell int
	mu   sync.RWMutex
	m    map[string]map[string]*Entity
}

func NewSpatialHash(cell int) *SpatialHash {
	return &SpatialHash{cell: cell, m: map[string]map[string]*Entity{}}
}

func cellKey(cx, cy int) string { return itoa(cx) + "," + itoa(cy) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (sh *SpatialHash) Insert(e *Entity) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	k := cellKey(e.X/sh.cell, e.Y/sh.cell)
	if sh.m[k] == nil {
		sh.m[k] = map[string]*Entity{}
	}
	sh.m[k][e.ID] = e
}

func (sh *SpatialHash) Remove(e *Entity) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	k := cellKey(e.X/sh.cell, e.Y/sh.cell)
	if cell := sh.m[k]; cell != nil {
		delete(cell, e.ID)
		if len(cell) == 0 {
			delete(sh.m, k)
		}
	}
}

func (sh *SpatialHash) Move(e *Entity, nx, ny int) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	ok := cellKey(e.X/sh.cell, e.Y/sh.cell)
	nk := cellKey(nx/sh.cell, ny/sh.cell)
	if ok == nk {
		e.X, e.Y = nx, ny
		return
	}
	if cell := sh.m[ok]; cell != nil {
		delete(cell, e.ID)
		if len(cell) == 0 {
			delete(sh.m, ok)
		}
	}
	if sh.m[nk] == nil {
		sh.m[nk] = map[string]*Entity{}
	}
	sh.m[nk][e.ID] = e
	e.X, e.Y = nx, ny
}

// Nearby returns snapshot copies of entities within radius tiles of (x,y),
// excluding skip. Copies are made under the hash lock so positions are atomic.
func (sh *SpatialHash) Nearby(x, y, radius int, skip string) []*EntitySnap {
	cx0, cy0 := (x-radius)/sh.cell, (y-radius)/sh.cell
	cx1, cy1 := (x+radius)/sh.cell, (y+radius)/sh.cell
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	seen := map[string]bool{}
	var out []*EntitySnap
	for cx := cx0; cx <= cx1; cx++ {
		for cy := cy0; cy <= cy1; cy++ {
			for id, e := range sh.m[cellKey(cx, cy)] {
				if id == skip || seen[id] {
					continue
				}
				seen[id] = true
				dx, dy := e.X-x, e.Y-y
				if dx*dx+dy*dy <= radius*radius {
					out = append(out, &EntitySnap{
						ID: e.ID, Name: e.Name, X: e.X, Y: e.Y,
						Dir: e.Dir, Bot: e.Bot, Avatar: e.Avatar, Client: e.Client,
					})
				}
			}
		}
	}
	return out
}

// SpaceState is the live RAM state of one space.
type SpaceState struct {
	World    *World
	mu       sync.RWMutex
	entities map[string]*Entity
	hash     *SpatialHash
}

func NewSpaceState(w *World) *SpaceState {
	sp := &SpaceState{World: w, entities: map[string]*Entity{}, hash: NewSpatialHash(cellSize)}
	// ambient presence sim: a couple of wandering wisps per space
	names := []string{"Wisp", "Ember"}
	for i := 0; i < botsPerSpace; i++ {
		botSpec := robotAvatarSpec(i)
		botColor := "#fb923c"
		if i%2 == 0 {
			botColor = "#a78bfa"
		}
		b := &Entity{
			ID:   "bot-" + w.ID + "-" + itoa(i+1),
			Name: names[i%len(names)],
			X:    w.Spawn.X + (i-1)*3,
			Y:    w.Spawn.Y + i*2,
			Dir:  "down",
			Bot:  true,
			Avatar: Avatar{
				Color: botColor,
				Icon:  "✦",
				Spec:  &botSpec,
			},
		}
		sp.AddEntity(b)
	}
	return sp
}

func (sp *SpaceState) AddEntity(e *Entity) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if e.X == 0 && e.Y == 0 {
		e.X, e.Y = sp.World.Spawn.X, sp.World.Spawn.Y
	}
	// Spawn safety: never park an entity on an impassable tile. Polluted
	// maps (walls painted on/near the spawn point — town-square live-test
	// bug) otherwise freeze tap-to-move: the client A* refuses a blocked
	// start, so a player spawned inside a wall can never move.
	if nx, ny, ok := sp.World.NearestPassable(e.X, e.Y); ok {
		e.X, e.Y = nx, ny
	}
	sp.entities[e.ID] = e
	sp.hash.Insert(e)
}

func (sp *SpaceState) RemoveEntity(e *Entity) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if _, ok := sp.entities[e.ID]; ok {
		delete(sp.entities, e.ID)
		sp.hash.Remove(e)
	}
}

func (sp *SpaceState) GetEntity(id string) *Entity {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	return sp.entities[id]
}

// AOI returns snapshot copies within radius tiles of (x,y).
func (sp *SpaceState) AOI(x, y, radius int, skip string) []*EntitySnap {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	return sp.hash.Nearby(x, y, radius, skip)
}

// MoveEntity updates a player/bot position within the hash (clamped to bounds).
func (sp *SpaceState) MoveEntity(e *Entity, nx, ny int) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if nx < 0 {
		nx = 0
	}
	if ny < 0 {
		ny = 0
	}
	if nx > sp.World.Width-1 {
		nx = sp.World.Width - 1
	}
	if ny > sp.World.Height-1 {
		ny = sp.World.Height - 1
	}
	sp.hash.Move(e, nx, ny)
}

// EntitySnaps returns snapshot copies of all entities (stable).
func (sp *SpaceState) EntitySnaps() []*EntitySnap {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	out := make([]*EntitySnap, 0, len(sp.entities))
	for _, e := range sp.entities {
		out = append(out, &EntitySnap{
			ID: e.ID, Name: e.Name, X: e.X, Y: e.Y,
			Dir: e.Dir, Bot: e.Bot, Avatar: e.Avatar, Client: e.Client,
		})
	}
	return out
}

// Bots returns the live bot entities (only the bot tick goroutine mutates them).
func (sp *SpaceState) Bots() []*Entity {
	sp.mu.RLock()
	defer sp.mu.RUnlock()
	var out []*Entity
	for _, e := range sp.entities {
		if e.Bot {
			out = append(out, e)
		}
	}
	return out
}

// Hub owns all spaces, clients, the 12Hz broadcaster, the bot sim and the
// SFU media plane (media_bridge.go).
type Hub struct {
	mu         sync.RWMutex
	store      *Store
	spaces     map[string]*SpaceState
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	closed     chan struct{}
	once       sync.Once
	bots       *BotManager // S7 headless builder bot registry (bot.go)
	sfu        *media.Media
	bubbles    map[string]string // peerID (entity/session id) -> spaceID voice bubble
	// recentNonces: per-SESSION chat nonce set for server-side idempotency
	// (social clarity dup fix). Keyed by session id — not per-connection —
	// so a reconnect resend (or a retry) of the same nonce within the 30s
	// TTL cannot double-broadcast or double-insert. Pruned lazily on access.
	recentNonces map[string]map[string]time.Time
	avatarReg    *AvatarRegistry // T2 avatar governance snapshot (avatars_t2.go)
}

func NewHub(store *Store) *Hub {
	h := &Hub{
		store:        store,
		spaces:       map[string]*SpaceState{},
		clients:      map[*Client]bool{},
		register:     make(chan *Client, 64),
		unregister:   make(chan *Client, 64),
		closed:       make(chan struct{}),
		bots:         NewBotManager(),
		sfu:          newMediaSFU(),
		bubbles:      map[string]string{},
		recentNonces: map[string]map[string]time.Time{},
		avatarReg:    newAvatarRegistry(),
	}
	for _, w := range store.ListWorlds() {
		h.spaces[w.ID] = NewSpaceState(w)
	}
	go h.mediaRelay()
	return h
}

func (h *Hub) Run() {
	tick := time.NewTicker(tickInterval)
	botTick := time.NewTicker(botMoveEvery)
	defer tick.Stop()
	defer botTick.Stop()
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.clients[c] = true
			h.mu.Unlock()
		case c := <-h.unregister:
			h.removeClient(c)
		case <-tick.C:
			h.broadcastStates()
		case <-botTick.C:
			h.moveBots()
		case <-h.closed:
			return
		}
	}
}

func (h *Hub) Close() { h.once.Do(func() { close(h.closed) }) }

func (h *Hub) space(id string) *SpaceState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.spaces[id]
}

// findEntity searches all spaces for a live entity (used by dm/signal/media).
func (h *Hub) findEntity(id string) *Entity {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, sp := range h.spaces {
		if e := sp.GetEntity(id); e != nil {
			return e
		}
	}
	return nil
}

// chatNonceTTL is how long a chat nonce stays dedupe-relevant. Matches the
// client's pending-message timeout (4s) with wide margin: a reconnect resend
// or manual retry within 30s of the original send cannot double-broadcast.
const chatNonceTTL = 30 * time.Second

// chatNonceSeen reports whether sessionID already sent nonce (peek only —
// does NOT record). Callers record via recordChatNonce only once the message
// actually passes every gate and is about to be delivered, so a rejected
// message (rate-limited, DM gate) never burns its nonce.
func (h *Hub) chatNonceSeen(sessionID, nonce string) bool {
	if sessionID == "" || nonce == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	set := h.recentNonces[sessionID]
	if set == nil {
		return false
	}
	for n, at := range set {
		if now.Sub(at) > chatNonceTTL {
			delete(set, n)
		}
	}
	if len(set) == 0 {
		delete(h.recentNonces, sessionID)
		return false
	}
	_, seen := set[nonce]
	return seen
}

// recordChatNonce remembers that sessionID delivered a chat under nonce.
func (h *Hub) recordChatNonce(sessionID, nonce string) {
	if sessionID == "" || nonce == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	set := h.recentNonces[sessionID]
	if set == nil {
		set = map[string]time.Time{}
		h.recentNonces[sessionID] = set
	}
	for n, at := range set { // prune stale while we hold the lock
		if now.Sub(at) > chatNonceTTL {
			delete(set, n)
		}
	}
	set[nonce] = now
}

// broadcastPresence pushes an additive {t:'presence'} roster delta to a
// whole space (social clarity roster: welcome seed + these join/leave deltas
// are the ONLY membership sources — the 12Hz AOI state stream never adds or
// removes roster rows). Hooks: join (handleJoin), portal (handlePortal:
// leave old space + join new), disconnect (removeClient).
func (h *Hub) broadcastPresence(sp *SpaceState, event string, ent *Entity) {
	if sp == nil || ent == nil {
		return
	}
	sp.BroadcastEnvelope("presence", map[string]any{
		"event":   event,
		"id":      ent.ID,
		"userId":  ent.UserID,
		"name":    ent.Name,
		"bot":     ent.Bot,
		"avatar":  ent.Avatar,
		"spaceId": sp.World.ID,
	})
}

func (h *Hub) removeClient(c *Client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
	}
	h.mu.Unlock()
	c.mu.Lock()
	ent := c.Entity
	spaceID := c.spaceID
	c.mu.Unlock()
	if ent != nil {
		if sp := h.space(spaceID); sp != nil {
			sp.RemoveEntity(ent)
			// social clarity roster: announce the leave to the space
			h.broadcastPresence(sp, "leave", ent)
		}
		h.dropBubble(ent.ID)
		c.setEntity(nil)
		c.setSpace("")
		// T2 social: friends learn the user went offline
		h.notifyFriendPresence(ent.UserID, map[string]any{
			"event": "offline", "userId": ent.UserID, "name": ent.Name,
			"online": false, "spaceId": "",
		})
	}
}

// broadcastStates pushes coalesced AOI state to each client at 12Hz.
func (h *Hub) broadcastStates() {
	now := time.Now()
	h.mu.RLock()
	spaces := make([]*SpaceState, 0, len(h.spaces))
	for _, sp := range h.spaces {
		spaces = append(spaces, sp)
	}
	clients := make([]*Client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, sp := range spaces {
		for _, c := range clients {
			c.mu.Lock()
			ent := c.Entity
			lx, ly := c.lastX, c.lastY
			eid := ""
			spaceID := c.spaceID
			if ent != nil {
				eid = ent.ID
			}
			c.mu.Unlock()
			if ent == nil {
				continue
			}
			if spaceID != sp.World.ID {
				continue
			}
			near := sp.AOI(lx, ly, aoiRadius, eid)
			if c.shouldSendState(sp.World.ID, near, now) {
				// FROZEN contract (PROTOCOL.md): state's d is the entity
				// ARRAY itself. The old object wrapper {spaceId,entities,t}
				// made the client's Array.isArray(d) check fail and remotes
				// silently never rendered (live-test: friend invisible).
				c.emit("state", entityListJSON(near))
			}
		}
	}
}

func entityListJSON(ents []*EntitySnap) []map[string]any {
	out := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.PublicJSON())
	}
	return out
}

// rosterListJSON is the welcome-roster seed (full space, with userId for
// friend gating) — the ONLY roster membership source alongside the additive
// presence deltas.
func rosterListJSON(ents []*EntitySnap) []map[string]any {
	out := make([]map[string]any, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.RosterJSON())
	}
	return out
}

// moveBots is the presence sim: ambient wisps wander their space every 2s.
func (h *Hub) moveBots() {
	h.mu.RLock()
	spaces := make([]*SpaceState, 0, len(h.spaces))
	for _, sp := range h.spaces {
		spaces = append(spaces, sp)
	}
	h.mu.RUnlock()
	dirs := [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}}
	for _, sp := range spaces {
		for _, e := range sp.Bots() {
			d := dirs[rand.Intn(len(dirs))]
			sp.MoveEntity(e, e.X+d[0], e.Y+d[1])
		}
	}
}

// --- client-side state coalescing ---

type entityPos struct {
	Name   string
	X, Y   int
	Dir    string
	Bot    bool
	Avatar Avatar
}

// shouldSendState coalesces: send only when the AOI entity set changed or the
// 1s heartbeat elapsed.
func (c *Client) shouldSendState(spaceID string, near []*EntitySnap, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastSpace != spaceID {
		c.lastSpace = spaceID
		c.lastEntities = map[string]entityPos{}
		c.lastStateSent = time.Time{}
	}
	snap := map[string]entityPos{}
	changed := false
	for _, e := range near {
		p := entityPos{Name: e.Name, X: e.X, Y: e.Y, Dir: e.Dir, Bot: e.Bot, Avatar: e.Avatar}
		snap[e.ID] = p
		if old, ok := c.lastEntities[e.ID]; !ok || old != p {
			changed = true
		}
	}
	if !changed && len(c.lastEntities) != len(snap) {
		changed = true
	}
	if !changed && now.Sub(c.lastStateSent) < stateHeartbeat {
		return false
	}
	c.lastEntities = snap
	c.lastStateSent = now
	return true
}
