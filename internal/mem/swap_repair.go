package mem

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	routecodec "compound/internal/routeevidence/codec"
)

// Route-evidence publication swaps (the CAS temporaries named
// .execution-route-evidence.jsonl.<pid>.<preimage12>.<digest12>.swap) are
// left behind when a writer dies between swap-write and commit. The live
// cause observed 2026-08-24: the hermes route-projections refresh is killed
// at its 900s script cap mid-publication of the ~111 MB ledger, twice a day,
// and each kill strands a full-size swap that the doctor then (correctly)
// fails the sync/embedding hop on — permanently, because nothing owned the
// recovery. This repair is that recovery: classify every stranded swap
// against the CURRENT ledger bytes and quarantine the provable residue with
// a receipt. It never touches a swap that could still validly commit.

const (
	// RouteEvidenceSwapQuarantineDir is where provable residue is moved
	// (same-volume rename: atomic, reversible, retention-reapable).
	RouteEvidenceSwapQuarantineDir = ".swap-resolved"
	// RouteEvidenceSwapRepairLog is the append-only receipt journal for
	// quarantined swaps (one JSON line per artifact).
	RouteEvidenceSwapRepairLog = "route-evidence-swap-repairs.jsonl"
)

// Swap classifications:
//
//	committable           current ledger is exactly the swap minus one canonical
//	                      row — the publication could still be completed by its
//	                      owning writer. REPORTED, never touched.
//	residue_already_applied  the swap's bytes are fully contained in the current
//	                      ledger — the row (or a successor of it) is already
//	                      published. Provably safe to quarantine.
//	residue_race_lost     the swap was built on a ledger state that is no longer
//	                      a prefix of current — a concurrent writer won the CAS.
//	                      Provably safe to quarantine.
const (
	SwapClassCommittable           = "committable"
	SwapClassResidueAlreadyApplied = "residue_already_applied"
	SwapClassResidueRaceLost       = "residue_race_lost"
)

// SwapRepairItem is the classification and (under apply) the action taken
// for one stranded swap artifact.
type SwapRepairItem struct {
	Artifact       string `json:"artifact"`
	Classification string `json:"classification"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
	Action         string `json:"action"` // dry-run: "reported"; apply: "quarantined" | "reported"
}

// SwapRepairReport is the full result of one repair pass.
type SwapRepairReport struct {
	DryRun      bool             `json:"dry_run"`
	GeneratedAt string           `json:"generated_at"`
	LedgerPath  string           `json:"ledger_path"`
	Items       []SwapRepairItem `json:"items"`
	Quarantined int              `json:"quarantined"`
}

// swapTailIsOneCanonicalRow reports whether the bytes after a known prefix
// form exactly one canonical routecodec row.
func swapTailIsOneCanonicalRow(tail []byte) bool {
	if len(tail) < 2 || tail[len(tail)-1] != '\n' || bytes.Count(tail, []byte{'\n'}) != 1 {
		return false
	}
	row := tail[:len(tail)-1]
	ev, err := routecodec.Decode(row)
	if err != nil {
		return false
	}
	canonical, err := routecodec.Encode(ev)
	return err == nil && bytes.Equal(canonical, row)
}

// classifySwap classifies one swap against the current ledger bytes.
func classifySwap(current, swap []byte) string {
	switch {
	case len(swap) > len(current) && bytes.Equal(swap[:len(current)], current) && swapTailIsOneCanonicalRow(swap[len(current):]):
		return SwapClassCommittable
	case len(swap) <= len(current) && bytes.Equal(current[:len(swap)], swap):
		return SwapClassResidueAlreadyApplied
	default:
		return SwapClassResidueRaceLost
	}
}

// RouteEvidenceSwapRepair scans the ledger directory for stranded
// publication swaps, classifies each against the current ledger, and — under
// apply — quarantines the provable residue (rename into .swap-resolved/) and
// appends one receipt line per quarantine to route-evidence-swap-repairs.jsonl.
// Committable swaps are reported and left untouched. Dry-run by default.
//
// Receipt ordering: the repair log is opened for append BEFORE the first
// rename, and each receipt line is written (and synced) immediately after the
// rename it describes. Mutating first and batching receipts at the end meant
// a failed log write — most plausibly under the same disk pressure that
// strands these swaps — could leave quarantined artifacts with no durable
// record of the repair.
func RouteEvidenceSwapRepair(homePath string, apply bool, now time.Time) (SwapRepairReport, error) {
	report := SwapRepairReport{
		DryRun:      !apply,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		LedgerPath:  filepath.Join(homePath, "ledger"),
	}
	entries, err := os.ReadDir(report.LedgerPath)
	if err != nil {
		return report, fmt.Errorf("read ledger dir: %w", err)
	}
	var swapNames []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, _, ok := parseRouteEvidenceSwapLeaf(e.Name()); ok {
			swapNames = append(swapNames, e.Name())
		}
	}
	sort.Strings(swapNames)
	if len(swapNames) == 0 {
		return report, nil
	}
	current, err := os.ReadFile(filepath.Join(report.LedgerPath, routeEvidenceLedgerFile))
	if err != nil {
		return report, fmt.Errorf("read current ledger: %w", err)
	}
	quarantineDir := filepath.Join(report.LedgerPath, RouteEvidenceSwapQuarantineDir)
	var repairLog *os.File
	if apply {
		if err := os.MkdirAll(quarantineDir, 0o755); err != nil {
			return report, fmt.Errorf("quarantine dir: %w", err)
		}
		// Preflight: if the receipt journal cannot be opened, no rename may
		// happen. This is the write-side twin of the audit rule that
		// mutation without a durable receipt is a proof hole, not a repair.
		repairLog, err = os.OpenFile(filepath.Join(report.LedgerPath, RouteEvidenceSwapRepairLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return report, fmt.Errorf("repair log preflight: %w", err)
		}
		defer repairLog.Close()
	}
	for _, name := range swapNames {
		swap, err := os.ReadFile(filepath.Join(report.LedgerPath, name))
		if err != nil {
			return report, fmt.Errorf("read swap %s: %w", name, err)
		}
		class := classifySwap(current, swap)
		sum := sha256.Sum256(swap)
		item := SwapRepairItem{
			Artifact:       name,
			Classification: class,
			SizeBytes:      int64(len(swap)),
			SHA256:         hex.EncodeToString(sum[:]),
			Action:         "reported",
		}
		if apply && class != SwapClassCommittable {
			stamped := fmt.Sprintf("%s__%s", now.UTC().Format("20060102T150405Z"), name)
			if err := os.Rename(filepath.Join(report.LedgerPath, name), filepath.Join(quarantineDir, stamped)); err != nil {
				return report, fmt.Errorf("quarantine %s: %w", name, err)
			}
			item.Action = "quarantined"
			report.Quarantined++
			line, err := json.Marshal(map[string]any{
				"ts":             now.UTC().Format(time.RFC3339),
				"artifact":       name,
				"classification": class,
				"size_bytes":     item.SizeBytes,
				"sha256":         item.SHA256,
				"action":         "quarantined",
				"quarantined_as": stamped,
				"reason":         "route evidence publication swap residue; classified against current ledger bytes",
			})
			if err != nil {
				return report, err
			}
			// Receipt line goes durable NOW, not at the end of the batch.
			// If it fails after the rename, the artifact is already moved:
			// leave a pending-repair marker next to the quarantine so a
			// later run or an operator can reconcile the receiptless
			// quarantine instead of discovering it by diffing directories.
			if _, err := repairLog.Write(append(line, '\n')); err != nil {
				markerErr := os.WriteFile(
					filepath.Join(quarantineDir, stamped+".pending-receipt"),
					[]byte(line), 0o644)
				return report, fmt.Errorf(
					"quarantined %s as %s but repair receipt write failed: %w (pending-receipt marker: %v)",
					name, stamped, err, markerErr)
			}
			if err := repairLog.Sync(); err != nil {
				return report, fmt.Errorf("repair log sync after quarantining %s: %w", name, err)
			}
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

// RouteEvidenceSwapRepairCLI is the budget-extracted CLI body for
// `compound home route-swap-repair` (same pattern as policy.RetentionCLI):
// parse the flags, run the repair, print, return the exit code.
//
// Unknown flags are a usage error (exit 2), never a silent default: a typo'd
// `--aply` silently dry-running a command whose real form mutates the ledger
// trains operators to trust an exit 0 that did not do what they asked.
func RouteEvidenceSwapRepairCLI(homePath string, args []string, stdout, stderr io.Writer) int {
	apply := false
	jsonOut := false
	usage := "usage: compound home route-swap-repair [--apply] [--dry-run] [--json] [--home <path>] [--local|--global]"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--apply":
			apply = true
		case "--json":
			jsonOut = true
		case "--dry-run":
			// Default; accepted explicitly.
		case "--home":
			// Consumed by the caller's home resolver; value is skipped here.
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "error: --home requires a value")
				fmt.Fprintln(stderr, usage)
				return 2
			}
			i++
		case "--local", "--global":
			// Consumed by the caller's home resolver.
		case "-h", "--help":
			fmt.Fprintln(stdout, usage)
			fmt.Fprintln(stdout, "dry-run is the default; --apply quarantines provable residue with a per-rename receipt")
			return 0
		default:
			fmt.Fprintf(stderr, "error: unknown flag %s\n", args[i])
			fmt.Fprintln(stderr, usage)
			return 2
		}
	}
	report, err := RouteEvidenceSwapRepair(homePath, apply, time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return 0
	}
	mode := "dry-run"
	if !report.DryRun {
		mode = "apply"
	}
	fmt.Fprintf(stdout, "route-swap-repair %s: %d swap(s) scanned, %d quarantined\n", mode, len(report.Items), report.Quarantined)
	for _, it := range report.Items {
		fmt.Fprintf(stdout, "- %-70s %-26s %10d bytes  %s\n", it.Artifact, it.Classification, it.SizeBytes, it.Action)
	}
	if report.Quarantined > 0 {
		fmt.Fprintf(stdout, "receipts: ledger/%s; quarantine: ledger/%s/\n", RouteEvidenceSwapRepairLog, RouteEvidenceSwapQuarantineDir)
	}
	return 0
}
