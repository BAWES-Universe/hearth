package main

import (
	"bufio"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"hearth/mcp"
)

//go:embed all:dist
var embeddedDist embed.FS

const version = "0.1.0"

var startTime = time.Now()

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// "hearth-server bot build ..." — headless bot client (S7, docs/BOT-PROTOCOL.md).
	// Runs standalone: joins a live world over WS and emits an op sequence
	// through the same edit envelope humans use. Server mode below.
	if len(os.Args) > 1 && os.Args[1] == "bot" {
		os.Exit(runBotCLI(os.Args[2:]))
	}

	addr := envOr("HEARTH_ADDR", "0.0.0.0:8090")
	dbPath := envOr("HEARTH_DB", filepath.Join("data", "hearth.db"))

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		log.Fatalf("mkdir db dir: %v", err)
	}

	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.SeedDefaults(); err != nil {
		log.Fatalf("seed defaults: %v", err)
	}

	// S1 Core: activity/audit log + gravity tables + world flags (idempotent).
	// Runs after SeedDefaults so seed worlds exist before flags are applied.
	if err := store.MigrateS1(); err != nil {
		log.Fatalf("migrate s1: %v", err)
	}

	// S9 Admin: operators + service tokens tables, then bootstrap the first
	// operator from HEARTH_BOOTSTRAP_OPERATOR_KEY (first boot only).
	if err := store.MigrateS9(); err != nil {
		log.Fatalf("migrate s9: %v", err)
	}
	if err := store.BootstrapOperator(); err != nil {
		log.Fatalf("bootstrap operator: %v", err)
	}

	hub := NewHub(store)
	go hub.Run()
	go hub.gravityCron() // nightly gravity recompute (design: nightly cron)
	defer hub.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", hub.handleHealth)
	mux.HandleFunc("/api/auth/guest", hub.handleGuestAuth)
	mux.HandleFunc("/api/me", hub.handleMe)
	mux.HandleFunc("/api/spaces", hub.handleSpaces)
	mux.HandleFunc("/api/spaces/", hub.handleSpaceGet)
	mux.HandleFunc("/api/worlds", hub.handleWorlds)
	mux.HandleFunc("/api/worlds/", hub.handleWorldRoute)
	mux.HandleFunc("/api/bots", hub.handleBots)
	mux.HandleFunc("/api/bots/", hub.handleBotStatus)
	mux.HandleFunc("/api/friends", hub.handleFriends)
	mux.HandleFunc("/api/friends/", hub.handleFriendRoute)
	mux.HandleFunc("/api/users", hub.handleUsers)
	mux.HandleFunc("/api/reports", hub.handleReports)
	mux.HandleFunc("/api/blocks", hub.handleBlocks)
	mux.HandleFunc("/api/blocks/", hub.handleBlockRoute)
	mux.HandleFunc("/ws", hub.handleWS)
	hub.RegisterAdminRoutes(mux) // S9: /api/admin/* + embedded /admin console
	// T2: Model Context Protocol — streamable HTTP JSON-RPC endpoint for AI
	// agents (world read/edit/chat/bot tools; docs/MCP.md). Additive route;
	// PROTOCOL.md untouched. The backend reuses bot.go's op-log client.
	mux.Handle("/mcp", mcp.NewServer(newMCPBackend(hub, addr)))
	mux.HandleFunc("/", serveClient)

	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("hearth v%s listening on %s (db=%s)", version, addr, dbPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("shutdown signal received")
	hub.Close()
	srv.Close()
}

// serveClient prefers the live client build on disk (../client/dist), falling
// back to the embedded fallback page when the client bundle is missing.
func serveClient(w http.ResponseWriter, r *http.Request) {
	clientDir := filepath.Join("..", "client", "dist")
	var handler http.Handler
	if fi, err := os.Stat(filepath.Join(clientDir, "index.html")); err == nil && !fi.IsDir() {
		handler = http.FileServer(http.Dir(clientDir))
	} else {
		sub, err := fs.Sub(embeddedDist, "dist")
		if err != nil {
			http.Error(w, "static assets unavailable", http.StatusInternalServerError)
			return
		}
		handler = http.FileServer(http.FS(sub))
	}
	handler.ServeHTTP(w, r)
}

// statusRecorder captures the response status for access logs. It must
// forward Hijacker/Flusher/Pusher so WebSocket upgrades and streaming work.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := sr.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	return hj.Hijack()
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if p, ok := sr.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sr, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, sr.status, time.Since(start).Round(time.Millisecond))
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
