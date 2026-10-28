package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/attention"
	"compound/internal/capgraph"
	"compound/internal/home"
	"compound/internal/narrate"
	"compound/internal/routeevidence"
	"compound/internal/work"
)

const narrateUsage = "usage: compound narrate [--json|--text] [--mood decide|wander|worry] [--recent N] [--home P]"

// cmdNarrate emits the compound.narrate.v0.1 correspondent script. It mirrors
// attention_cli's loader wiring and reuses the orient operator cursor/changed
// helpers read-only. Narration is a pure projection: it never advances the
// orient cursor or persists an emphasis sidecar.
func cmdNarrate(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(narrateUsage)
		return
	}
	jsonOut := false
	textOut := false
	homeFlag := ""
	recent := 5
	mood := narrate.MoodDecide
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--text":
			textOut = true
		case "--mood":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, narrateUsage)
				os.Exit(2)
			}
			i++
			m, ok := parseNarrateMood(args[i])
			if !ok {
				fmt.Fprintln(os.Stderr, "error: --mood must be decide, wander, or worry")
				os.Exit(2)
			}
			mood = m
		case "--recent":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, narrateUsage)
				os.Exit(2)
			}
			i++
			recent = parsePositiveInt(args[i], "--recent")
		case "--home":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				fmt.Fprintln(os.Stderr, narrateUsage)
				os.Exit(2)
			}
			i++
			homeFlag = strings.TrimSpace(args[i])
		default:
			fmt.Fprintln(os.Stderr, narrateUsage)
			os.Exit(2)
		}
	}

	homePath, err := home.Resolve(homeFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err, "(run `compound home init`)")
		os.Exit(1)
	}
	res, err := buildNarrateScript(homePath, mood, recent, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "narrate:", err)
		os.Exit(1)
	}
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		if err := enc.Encode(res.Script); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	_ = textOut
	printNarrateText(res.Script)
}

func parseNarrateMood(value string) (narrate.Mood, bool) {
	switch strings.TrimSpace(value) {
	case string(narrate.MoodDecide):
		return narrate.MoodDecide, true
	case string(narrate.MoodWander):
		return narrate.MoodWander, true
	case string(narrate.MoodWorry):
		return narrate.MoodWorry, true
	default:
		return narrate.MoodDecide, false
	}
}

// buildNarrateScript is the ONE builder both `compound narrate` and
// `compound orient --operator` call, so the served script is identical across
// the two channels (spec §1 emission path). It is self-contained and READ-ONLY
// with respect to the orient cursor: it reads the cursor + changed-set to flag
// novelty but never advances them. The returned Result carries the next
// emphasis sidecar; only the emphasis-owning caller persists it.
func buildNarrateScript(homePath string, mood narrate.Mood, recent int, now time.Time) (narrate.Result, error) {
	if recent <= 0 {
		recent = 5
	}
	// Read-only by registry contract: narrate (and `orient --operator`, which
	// shares this builder) serves a script off the ledger and must not
	// materialize work/ or events/ just by looking (see work.OpenReadOnly).
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return narrate.Result{}, err
	}
	reviews, err := buildReviewQueueReport(homePath, "", false)
	if err != nil {
		return narrate.Result{}, err
	}
	workbench, err := store.Workbench(now, work.WorkbenchOptions{RecentLimit: recent})
	if err != nil {
		return narrate.Result{}, err
	}
	// Reuse this one Workbench build for the executor projection instead of
	// letting ExecutorsProjection walk the whole event store a second time
	// (mirrors attention_cli). narrate's only executor consumer is attention.Build,
	// which reads the lease-hygiene and burden fields and never the Recent-derived
	// LastSuccess/LastFailure, so reusing this RecentLimit=recent report does not
	// change the narrated output while dropping one full event-store walk.
	executors, err := store.ExecutorsProjection(now, work.ExecutorsProjectionOptions{Workbench: &workbench})
	if err != nil {
		return narrate.Result{}, err
	}
	report := attention.Build(attention.Inputs{Reviews: &reviews, Workbench: &workbench, Executors: &executors}, now)

	currentSeq, err := maxClaimSeq(homePath)
	if err != nil {
		return narrate.Result{}, err
	}
	cursor, cursorPath, firstRun, err := readOrientOperatorCursor(homePath)
	if err != nil {
		return narrate.Result{}, err
	}
	changed := buildOrientChanged(homePath, workbench, currentSeq, cursor.LastSeenSeq, firstRun, cursorPath)

	seen, err := readNarrateThreadSeen(homePath)
	if err != nil {
		return narrate.Result{}, err
	}
	traffic, err := loadCheapShare(homePath)
	if err != nil {
		return narrate.Result{}, err
	}
	injection := loadNarrateInjection(homePath)
	return narrate.Build(narrate.Inputs{
		Attention:  report,
		Changed:    narrate.ChangedSet{CurrentSeq: currentSeq, LastSeenSeq: cursor.LastSeenSeq, FirstRun: firstRun},
		Receipts:   recentReceiptSummaries(changed),
		Traffic:    traffic,
		ThreadSeen: seen,
		Injection:  injection,
		Now:        now,
	}, narrate.Options{Mood: mood})
}

// loadNarrateInjection reduces the latest ContextPack's injection meter to the
// narrate beat's input. Best-effort: no pack → nil (beat absent), never an error.
func loadNarrateInjection(homePath string) *narrate.InjectionLegibility {
	leg, err := capgraph.LatestInjectionLegibility(homePath)
	if err != nil || leg.PackID == "" {
		return nil
	}
	return &narrate.InjectionLegibility{
		Present:         true,
		PackID:          leg.PackID,
		ServedRefs:      leg.ServedRefs,
		ServedMemory:    leg.ServedMemory,
		ServedPositions: leg.ServedPositions,
		ServedOffers:    leg.ServedOffers,
		ExcludedRefs:    leg.ExcludedRefs,
		ExcludedNoTask:  leg.ExcludedNoTask,
		InjectionBytes:  leg.InjectionBytes,
	}
}

// loadCheapShare reads the imported route-evidence ledger and reduces it to the
// spend-based cheap-share split (routeevidence owns the computation; narrate only
// renders it). When no provider ledger has been imported the split reports
// Present=false, so narrate omits the beat entirely rather than fabricating a
// ratio (`compound route import-provider-ledger` writes the underlying ledger).
func loadCheapShare(homePath string) (narrate.TrafficSplit, error) {
	rows, _, err := routeevidence.LoadRouteEvidenceLedger(homePath)
	if err != nil {
		return narrate.TrafficSplit{}, err
	}
	share := routeevidence.ComputeCheapShare(rows)
	return narrate.TrafficSplit{
		Present:     share.Present,
		TotalCalls:  share.TotalCalls,
		CheapCalls:  share.CheapCalls,
		TotalTokens: share.TotalTokens,
		CheapTokens: share.CheapTokens,
	}, nil
}

// recentReceiptSummaries reduces the orient changed-set's recent items (already
// joined to their receipts by buildOrientChanged) into the acknowledge inputs.
// Applied is the receipt's EffectiveChanged verdict; the failure reason is the
// human-legible StateDelta summary — the honest split composes from receipts.
func recentReceiptSummaries(changed orientChanged) []narrate.ReceiptSummary {
	out := make([]narrate.ReceiptSummary, 0, len(changed.Recent))
	for _, item := range changed.Recent {
		if strings.TrimSpace(item.ReceiptID) == "" || strings.TrimSpace(item.MoveType) == "" {
			continue
		}
		reason := ""
		if !item.Changed {
			reason = item.Summary
		}
		out = append(out, narrate.ReceiptSummary{
			ReceiptID: item.ReceiptID,
			WorkID:    item.WorkID,
			MoveType:  item.MoveType,
			Applied:   item.Changed,
			Summary:   item.Summary,
			Reason:    reason,
			CreatedAt: item.CreatedAt,
		})
	}
	return out
}

func narrateThreadSeenPath(homePath string) string {
	return filepath.Join(homePath, "projections", "narrate-thread-seen.json")
}

func readNarrateThreadSeen(homePath string) (map[string]narrate.ThreadSeen, error) {
	out := map[string]narrate.ThreadSeen{}
	data, err := os.ReadFile(narrateThreadSeenPath(homePath))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		// sidecar loss degrades honestly: everything reads `new` once (spec §5).
		return map[string]narrate.ThreadSeen{}, nil
	}
	return out, nil
}

func writeNarrateThreadSeen(homePath string, seen map[string]narrate.ThreadSeen) error {
	path := narrateThreadSeenPath(homePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(seen, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// printNarrateText renders the script as terminal prose (spec §7): movement
// beats in scene order, the standing summary line, the quiet block when
// applicable, and a trailing conservation line.
func printNarrateText(script narrate.Script) {
	for _, beat := range script.Beats {
		line := fmt.Sprintf("[%s] %s", beat.Voice, beat.Body)
		if strings.TrimSpace(beat.Unlock) != "" {
			line += fmt.Sprintf(" (→ %s)", beat.Unlock)
		}
		fmt.Println(line)
	}
	if s := strings.TrimSpace(script.Standing.Summary); s != "" {
		fmt.Println(s)
	}
	if script.Quiet != nil {
		fmt.Printf("· %s (%d folded, %s)\n", script.Quiet.Reason, script.Quiet.FoldedCount, script.Quiet.Since)
	}
	fmt.Printf("narrated %d of %d · folded %d · residue %d\n",
		script.Counts.Narrated, script.Counts.TotalMembers, script.Counts.Folded, script.Counts.Residue)
}
