package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"compound/internal/home"
	"compound/internal/sourceobs"
)

// cmdSourceInventoryObserve is the project-miner inventory door: the standing-census sibling of
// `source change-stream-observe`. The project-miner inventory writer (project_miner.py
// command_inventory) emits through it so the full ~/Projects repo census flows into COMPOUND_HOME
// instead of dead-ending in ~/brain/reviews/project-miner. It content-addresses the raw
// inventory.json body (keep-all-data original layer) and parks each repo row no-promote as a
// product_hypothesis candidate above it. It observes; it never promotes. With no rows it is
// preserve-only: the body is content-addressed and nothing is parked.
func cmdSourceInventoryObserve(args []string) {
	if code := runSourceInventoryObserve(args, os.Stdout, os.Stderr, time.Now()); code != 0 {
		os.Exit(code)
	}
}

// runSourceInventoryObserve is the testable body: it returns a process exit code (0 ok, 1
// runtime/fail-closed, 2 usage) and writes to the given writers so falsifiers assert exit codes and
// output in-process against a test home.
func runSourceInventoryObserve(args []string, stdout, stderr io.Writer, now time.Time) int {
	usage := "usage: compound source inventory-observe --intake <intake.json> [--json]"
	intakePath := ""
	jsonOut := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--intake":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, usage)
				return 2
			}
			intakePath = args[i+1]
			i++
		default:
			fmt.Fprintln(stderr, usage)
			return 2
		}
	}
	if strings.TrimSpace(intakePath) == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}

	// Read + parse the intake. Malformed input fails closed (exit 1) BEFORE the home is touched.
	raw, err := os.ReadFile(intakePath)
	if err != nil {
		fmt.Fprintln(stderr, "source inventory-observe: read intake:", err)
		return 1
	}
	var intake sourceobs.InventoryIntake
	if err := json.Unmarshal(raw, &intake); err != nil {
		fmt.Fprintln(stderr, "source inventory-observe: parse intake:", err)
		return 1
	}
	if strings.TrimSpace(intake.SourceCapture) == "" {
		fmt.Fprintln(stderr, "source inventory-observe: intake missing source_capture")
		return 1
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err, "(run `compound home init`)")
		return 1
	}

	// Preserve the raw inventory.json body content-addressed: the keep-all-data original layer. A
	// missing or unreadable capture fails closed (exit 1) rather than parking candidates over a
	// phantom body. The byte hash (captureSHA) addresses the exact original; the stable content hash
	// below (contentSHA) addresses the MEANING and drives idempotency.
	captureBytes, err := os.ReadFile(intake.SourceCapture)
	if err != nil {
		fmt.Fprintln(stderr, "source inventory-observe: read source_capture:", err)
		return 1
	}
	sum := sha256.Sum256(captureBytes)
	captureSHA := hex.EncodeToString(sum[:])[:12]
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "source inventory-observe:", err)
		return 1
	}
	bodyRef, err := store.Put(captureBytes)
	if err != nil {
		fmt.Fprintln(stderr, "source inventory-observe: preserve inventory body:", err)
		return 1
	}

	contentSHA := sourceobs.InventoryContentSHA(intake)

	// Preserve-only (capture-time) call: no rows to derive. The body is content-addressed; nothing is
	// parked. This is a valid, real write (the original artifact now lives in COMPOUND_HOME).
	if len(intake.Projects) == 0 {
		if jsonOut {
			b, _ := json.MarshalIndent(struct {
				SourceCapture string `json:"source_capture"`
				CaptureSHA    string `json:"capture_sha"`
				ContentSHA    string `json:"content_sha"`
				BodyRef       string `json:"body_ref"`
				Projects      int    `json:"projects"`
				Mode          string `json:"mode"`
			}{intake.SourceCapture, captureSHA, contentSHA, bodyRef, 0, "preserve_only"}, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		fmt.Fprintf(stdout, "# inventory-observe (preserve-only)\n\n")
		fmt.Fprintf(stdout, "- source_capture: %s\n", intake.SourceCapture)
		fmt.Fprintf(stdout, "- capture_sha: %s\n", captureSHA)
		fmt.Fprintf(stdout, "- content_sha: %s\n", contentSHA)
		fmt.Fprintf(stdout, "- body_ref: %s\n", bodyRef)
		fmt.Fprintf(stdout, "- projects: 0 (body preserved, nothing parked)\n")
		return 0
	}

	obs, err := sourceobs.FromInventory(intake, contentSHA, bodyRef, now)
	if err != nil {
		fmt.Fprintln(stderr, "source inventory-observe:", err)
		return 1
	}
	report, err := sourceobs.Apply(homePath, obs, now)
	if err != nil {
		fmt.Fprintln(stderr, "source inventory-observe:", err)
		for _, check := range report.Checks {
			fmt.Fprintf(stderr, "- %s: %s", check.Name, check.Status)
			if check.Detail != "" {
				fmt.Fprintf(stderr, " (%s)", check.Detail)
			}
			fmt.Fprintln(stderr)
		}
		return 1
	}

	if jsonOut {
		b, err := json.MarshalIndent(struct {
			SourceCapture string           `json:"source_capture"`
			CaptureSHA    string           `json:"capture_sha"`
			ContentSHA    string           `json:"content_sha"`
			BodyRef       string           `json:"body_ref"`
			ObservationID string           `json:"observation_id"`
			Projects      int              `json:"projects"`
			Mode          string           `json:"mode"`
			Report        sourceobs.Report `json:"report"`
		}{intake.SourceCapture, captureSHA, contentSHA, bodyRef, report.ObservationID, len(intake.Projects), "observe", report}, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "source inventory-observe:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	fmt.Fprintf(stdout, "# inventory-observe %s\n\n", report.ObservationID)
	fmt.Fprintf(stdout, "- source_capture: %s\n", intake.SourceCapture)
	fmt.Fprintf(stdout, "- capture_sha: %s\n", captureSHA)
	fmt.Fprintf(stdout, "- content_sha: %s\n", contentSHA)
	fmt.Fprintf(stdout, "- body_ref: %s\n", bodyRef)
	fmt.Fprintf(stdout, "- projects: %d\n", len(intake.Projects))
	fmt.Fprintf(stdout, "- status: %s\n", report.Status)
	for _, id := range report.CandidateIDs {
		fmt.Fprintf(stdout, "- candidate: %s\n", id)
	}
	for _, id := range report.NoPromoteIDs {
		fmt.Fprintf(stdout, "- no_promote: %s\n", id)
	}
	return 0
}
