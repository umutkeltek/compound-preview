package work

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"compound/internal/home"
)

const presenceUsage = "usage: compound presence [--json] [--ttl DURATION] [--include-stale] [--tail-bytes N]\n       compound presence announce --actor A --session S --harness H [--cwd D] [--json]"

// RunPresence answers "who is here right now" from evidence Compound already
// holds. It is deliberately NOT sourced from delegate bindings: an agent this
// runtime never launched is still present the moment it touches the ledger.
// cmd/compound keeps only a thin os.Exit shim over this organ surface.
func RunPresence(args []string) int {
	if len(args) > 0 && args[0] == "announce" {
		return RunPresenceAnnounce(args[1:])
	}
	jsonOut := false
	opts := PresenceOptions{}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--include-stale":
			opts.IncludeStale = true
		case "--ttl":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, presenceUsage)
				return 2
			}
			parsed, err := time.ParseDuration(strings.TrimSpace(args[i]))
			if err != nil || parsed <= 0 {
				fmt.Fprintln(os.Stderr, "error: --ttl must be a positive duration (e.g. 30m)")
				return 2
			}
			opts.TTL = parsed
		case "--tail-bytes":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, presenceUsage)
				return 2
			}
			parsed, err := strconv.ParseInt(strings.TrimSpace(args[i]), 10, 64)
			if err != nil || parsed <= 0 {
				fmt.Fprintln(os.Stderr, "error: --tail-bytes must be a positive integer")
				return 2
			}
			opts.TailBytes = parsed
		default:
			fmt.Fprintln(os.Stderr, presenceUsage)
			return 2
		}
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		return 1
	}
	// Read-only by registry contract: presence inspects the ledger and must
	// not materialize work/ or events/ just by looking (see work.OpenReadOnly).
	store, err := OpenReadOnly(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	projection, err := store.PresenceProjectionFor(time.Now(), opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(projection); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
	printPresence(projection)
	return 0
}

func printPresence(projection PresenceProjection) {
	fmt.Printf("# presence (ttl %ds)\n\n", projection.TTLSeconds)
	if len(projection.Present) == 0 {
		fmt.Println("no actor seen inside the ttl window")
	}
	for _, row := range projection.Present {
		marker := "*"
		if !row.Fresh {
			marker = "-"
		}
		fmt.Printf("%s %s\n", marker, row.Actor)
		fmt.Printf("    last seen %s (%s ago) · leases %d · events %d\n",
			row.LastSeen, presenceAge(row.AgeSeconds), row.Leases, row.Events)
		for _, observation := range row.Latest {
			detail := observation.Detail
			if detail != "" {
				detail = " " + detail
			}
			fmt.Printf("    %-5s %s%s\n", observation.Kind, observation.WorkID, detail)
		}
	}
	if len(projection.Sessions) > 0 {
		fmt.Printf("\n## declared sessions (claims — corroborated only by independent evidence)\n\n")
		for _, session := range projection.Sessions {
			marker := "!"
			if session.Corroborated {
				marker = "+"
			}
			fmt.Printf("%s %s claims %s\n", marker, session.Harness, session.Actor)
			fmt.Printf("    session %s · %s\n", session.SessionID, session.Basis)
		}
	}
	fmt.Printf("\nactors %d · fresh %d · stale %d · lease-only %d · event-only %d · both %d\n",
		projection.Counts.Actors, projection.Counts.Fresh, projection.Counts.Stale,
		projection.Counts.LeaseOnly, projection.Counts.EventOnly, projection.Counts.BothSources)
	if projection.Counts.Sessions > 0 {
		fmt.Printf("declared sessions %d · corroborated %d\n",
			projection.Counts.Sessions, projection.Counts.Corroborated)
	}
	if projection.TailTruncated {
		fmt.Printf("event evidence read from the last %d bytes of the work log\n", projection.TailBytes)
	}
	for _, note := range projection.Integrity {
		fmt.Println("integrity:", note)
	}
}

// RunPresenceAnnounce records a session's claimed actor. The declaration is
// observed-class evidence, never authority: it is only usable once presence
// independently shows that actor active, so announcing an identity you do not
// hold gains nothing.
func RunPresenceAnnounce(args []string) int {
	in := PresenceSessionInput{}
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			jsonOut = true
		case "--actor", "--session", "--harness", "--cwd":
			i++
			if i >= len(args) || strings.TrimSpace(args[i]) == "" {
				fmt.Fprintln(os.Stderr, presenceUsage)
				return 2
			}
			value := strings.TrimSpace(args[i])
			switch arg {
			case "--actor":
				in.Actor = value
			case "--session":
				in.SessionID = value
			case "--harness":
				in.Harness = value
			case "--cwd":
				in.Cwd = value
			}
		default:
			fmt.Fprintln(os.Stderr, presenceUsage)
			return 2
		}
	}
	if strings.TrimSpace(in.Cwd) == "" {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		in.Cwd = wd
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		return 1
	}
	store, err := Open(homePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	record, err := store.AnnouncePresenceSession(in, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if jsonOut {
		if err := json.NewEncoder(os.Stdout).Encode(record); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Printf("announced %s as %s (session %s, announce #%d)\n",
		record.Harness, record.Actor, record.SessionID, record.Announces)
	fmt.Println("note: a declaration is evidence, not authority — it is usable only while")
	fmt.Println("      `compound presence` independently shows that actor active.")
	return 0
}

func presenceAge(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", seconds)
	case d < time.Hour:
		return fmt.Sprintf("%dm", seconds/60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", seconds/3600, (seconds%3600)/60)
	default:
		return fmt.Sprintf("%dd%dh", seconds/86400, (seconds%86400)/3600)
	}
}
