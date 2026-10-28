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

// cmdSourceChangeStreamObserve is the general change-stream / file-drift door. It is the
// non-harness-repo analogue of `source canon-observe`: the Hermes change-stream scorer
// (change_stream_candidates.py) and capture (change_stream_capture.py) emit through it so the
// general drift stream flows into COMPOUND_HOME instead of dead-ending in ~/brain/reviews. It
// content-addresses the raw capture body (keep-all-data original layer) and parks each derived
// candidate no-promote above it (product_hypothesis for repo/session drift, capability_observation
// for canon drift). It observes; it never promotes. With no candidates it is preserve-only: the
// body is content-addressed and nothing is parked (the capture-time call).
func cmdSourceChangeStreamObserve(args []string) {
	if code := runSourceChangeStreamObserve(args, os.Stdout, os.Stderr, time.Now()); code != 0 {
		os.Exit(code)
	}
}

// runSourceChangeStreamObserve is the testable body: it returns a process exit code (0 ok, 1
// runtime/fail-closed, 2 usage) and writes to the given writers so falsifiers assert exit codes and
// output in-process against a test home.
func runSourceChangeStreamObserve(args []string, stdout, stderr io.Writer, now time.Time) int {
	usage := "usage: compound source change-stream-observe --intake <intake.json> [--json]"
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
		fmt.Fprintln(stderr, "source change-stream-observe: read intake:", err)
		return 1
	}
	var intake sourceobs.ChangeStreamIntake
	if err := json.Unmarshal(raw, &intake); err != nil {
		fmt.Fprintln(stderr, "source change-stream-observe: parse intake:", err)
		return 1
	}
	if strings.TrimSpace(intake.SourceCapture) == "" {
		fmt.Fprintln(stderr, "source change-stream-observe: intake missing source_capture")
		return 1
	}

	homePath, err := home.Resolve("")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err, "(run `compound home init`)")
		return 1
	}

	// Preserve the raw capture body content-addressed: the keep-all-data original layer. A missing
	// or unreadable capture fails closed (exit 1) rather than parking candidates over a phantom body.
	captureBytes, err := os.ReadFile(intake.SourceCapture)
	if err != nil {
		fmt.Fprintln(stderr, "source change-stream-observe: read source_capture:", err)
		return 1
	}
	sum := sha256.Sum256(captureBytes)
	captureSHA := hex.EncodeToString(sum[:])[:12]
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "source change-stream-observe:", err)
		return 1
	}
	bodyRef, err := store.Put(captureBytes)
	if err != nil {
		fmt.Fprintln(stderr, "source change-stream-observe: preserve capture body:", err)
		return 1
	}

	// Preserve-only (capture-time) call: no candidates to derive. The body is content-addressed;
	// nothing is parked. This is a valid, real write (the original artifact now lives in COMPOUND_HOME).
	if len(intake.Candidates) == 0 {
		if jsonOut {
			b, _ := json.MarshalIndent(struct {
				SourceCapture string `json:"source_capture"`
				CaptureSHA    string `json:"capture_sha"`
				BodyRef       string `json:"body_ref"`
				Candidates    int    `json:"candidates"`
				Mode          string `json:"mode"`
			}{intake.SourceCapture, captureSHA, bodyRef, 0, "preserve_only"}, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return 0
		}
		fmt.Fprintf(stdout, "# change-stream-observe (preserve-only)\n\n")
		fmt.Fprintf(stdout, "- source_capture: %s\n", intake.SourceCapture)
		fmt.Fprintf(stdout, "- capture_sha: %s\n", captureSHA)
		fmt.Fprintf(stdout, "- body_ref: %s\n", bodyRef)
		fmt.Fprintf(stdout, "- candidates: 0 (body preserved, nothing parked)\n")
		return 0
	}

	obs, err := sourceobs.FromChangeStream(intake, captureSHA, bodyRef, now)
	if err != nil {
		fmt.Fprintln(stderr, "source change-stream-observe:", err)
		return 1
	}
	report, err := sourceobs.Apply(homePath, obs, now)
	if err != nil {
		fmt.Fprintln(stderr, "source change-stream-observe:", err)
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
			BodyRef       string           `json:"body_ref"`
			ObservationID string           `json:"observation_id"`
			Mode          string           `json:"mode"`
			Report        sourceobs.Report `json:"report"`
		}{intake.SourceCapture, captureSHA, bodyRef, report.ObservationID, "observe", report}, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, "source change-stream-observe:", err)
			return 1
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	fmt.Fprintf(stdout, "# change-stream-observe %s\n\n", report.ObservationID)
	fmt.Fprintf(stdout, "- source_capture: %s\n", intake.SourceCapture)
	fmt.Fprintf(stdout, "- capture_sha: %s\n", captureSHA)
	fmt.Fprintf(stdout, "- body_ref: %s\n", bodyRef)
	fmt.Fprintf(stdout, "- status: %s\n", report.Status)
	for _, id := range report.CandidateIDs {
		fmt.Fprintf(stdout, "- candidate: %s\n", id)
	}
	for _, id := range report.NoPromoteIDs {
		fmt.Fprintf(stdout, "- no_promote: %s\n", id)
	}
	return 0
}
