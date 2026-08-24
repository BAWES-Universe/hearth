package main

// "hearth-server bot build" — the headless bot builder CLI.
//
// Runs the same BotClient the REST API and tests use: authenticate with a
// deviceKey, join a live world over WS, and emit an op-sequence through the
// frozen edit envelope. Replay-safe: reuse -run-id to dedupe.
//
//	hearth-server bot build -demo                       # built-in demo (house + heart in garden)
//	hearth-server bot build -script script.json -world garden
//	hearth-server bot build -script script.json -url ws://51.75.74.214:8090/ws -run-id demo-1
//
// Exit code 0 = every op applied (or deduped); 1 = error; 2 = usage.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func runBotCLI(args []string) int {
	// optional subcommand word: "hearth-server bot build ..." == "hearth-server bot ..."
	if len(args) > 0 && args[0] == "build" {
		args = args[1:]
	}
	fs := flag.NewFlagSet("hearth bot", flag.ContinueOnError)
	world := fs.String("world", "garden", "target world id")
	scriptPath := fs.String("script", "", "path to a bot op-sequence JSON (or use -demo)")
	demo := fs.Bool("demo", false, "use the built-in demo script (5x5 house + heart in garden)")
	name := fs.String("name", "Bricky", "bot display name")
	deviceKey := fs.String("device-key", "", "bot deviceKey (default bot-<name>)")
	runID := fs.String("run-id", "", "stable idempotency run id (default bot-<name>-<rand>); reuse to replay-safe dedupe")
	url := fs.String("url", "ws://127.0.0.1:8090/ws", "server WS endpoint")
	interval := fs.Duration("interval", 120*time.Millisecond, "delay between ops")
	timeout := fs.Duration("timeout", 90*time.Second, "overall run timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*demo && *scriptPath == "" {
		fmt.Fprintln(os.Stderr, "hearth bot: need -demo or -script <file.json>")
		return 2
	}

	var script *BotScript
	if *demo {
		script = DemoBuildScript()
	} else {
		b, err := os.ReadFile(*scriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth bot: read script: %v\n", err)
			return 1
		}
		script, err = ParseBotScript(b)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hearth bot: %v\n", err)
			return 1
		}
	}
	if script.World == "" {
		script.World = *world
	}
	if script.Name == "" {
		script.Name = *name
	}
	if *deviceKey == "" {
		*deviceKey = "bot-" + slug(script.Name)
	}
	if *runID == "" {
		*runID = "bot-" + slug(script.Name) + "-" + randHex(4)
	}

	cfg := BotConfig{
		Name: script.Name, DeviceKey: *deviceKey, World: script.World,
		URL: *url, RunID: *runID, Script: script,
		Interval: *interval, Timeout: *timeout,
	}
	res := NewBotClient(cfg).Run()

	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	if res.FirstErr != "" {
		return 1
	}
	return 0
}
