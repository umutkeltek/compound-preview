package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"compound/internal/home"
	"compound/internal/stream"
)

const streamUsage = "usage: compound stream (--once [--backlog N] | --follow [--cursor BYTE]) [--json] [--home P]"

// cmdStream is the governed read-only streaming surface behind the console's
// live envelope-spine ticker. It reads events/work.jsonl by BYTE CURSOR and the
// ledger/seq pulse, opening the log read-only so it can never take the
// work-event write lock. Two modes:
//
//	--once   a finite snapshot (last N events + end-of-log cursor + seq +
//	         generated_at) as ONE JSON object — the stream-snapshot door.
//	--follow a long-running JSONL tail from --cursor, one frame per line
//	         (work_event / collaboration_event / seq_pulse / heartbeat / warning), each carrying the
//	         new cursor so a reconnect resumes losslessly.
//
// Output is always JSON; --json is accepted (and used by the doors argv) to make
// the machine contract explicit at the call site.
func cmdStream(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(streamUsage)
		return
	}
	once, follow := false, false
	homeFlag := ""
	backlog := stream.DefaultBacklog
	var cursor int64
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--once":
			once = true
		case "--follow":
			follow = true
		case "--json":
			// output is always JSON; the flag documents intent + backs the doors argv
		case "--backlog":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, streamUsage)
				os.Exit(2)
			}
			i++
			backlog = parsePositiveInt(args[i], "--backlog")
		case "--cursor":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, streamUsage)
				os.Exit(2)
			}
			i++
			c, err := parseNonNegativeInt64("--cursor", args[i])
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(2)
			}
			cursor = c
		case "--home":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				fmt.Fprintln(os.Stderr, streamUsage)
				os.Exit(2)
			}
			i++
			homeFlag = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(os.Stderr, streamUsage)
			os.Exit(2)
		}
	}
	if once == follow { // exactly one mode is required
		fmt.Fprintln(os.Stderr, streamUsage)
		os.Exit(2)
	}

	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}

	if once {
		snap, err := stream.BuildSnapshot(homePath, backlog, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, "stream:", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		if err := enc.Encode(snap); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// --follow: run until the client disconnects. The console kills this child
	// on SSE disconnect (SIGTERM); NotifyContext turns that into a clean exit,
	// and a broken stdout pipe also ends the loop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := stream.Follow(ctx, homePath, stream.FollowOptions{Cursor: cursor}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "stream:", err)
		os.Exit(1)
	}
}
