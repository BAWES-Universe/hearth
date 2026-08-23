package main

// Test helpers that mirror the live dispatch/handlers. These are the single
// source of truth for what the server accepts — keep in sync with ws.go.

// serverHandlesType reports whether handleMessage's switch dispatches typ.
// Mirrors ws.go:212-233. Add new protocol types here AND in handleMessage
// in the same commit.
func serverHandlesType(typ string) bool {
	switch typ {
	case "join", "move", "chat", "edit", "portal", "signal", "media", "bot_msg", "ping":
		return true
	default:
		return false
	}
}

// validChannel mirrors the channel check in handleChat (chat.go:46-52).
func validChannel(c string) bool {
	switch c {
	case "proximity", "space", "dm":
		return true
	default:
		return false
	}
}
