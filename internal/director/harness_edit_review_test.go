package director

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/policy"
	"compound/internal/reviewartifact"
)

// seedHarnessEditCard writes a realistic propose-only harness-edit card (the
// same struct + writer the director scanner emits) under
// reviews/harness-edit-proposals/ and returns its absolute JSON path.
func seedHarnessEditCard(t *testing.T, home, id, surface string) string {
	t.Helper()
	card := harnessEditProposalCard{
		SchemaVersion:      harnessEditProposalCardSchema,
		ID:                 id,
		CreatedAt:          directorTestNow.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Status:             "review_required",
		TriggerRef:         "director:harness-edit:" + surface + ":" + id,
		TargetSurface:      surface,
		SurfaceLabel:       reviewartifact.PlainLabel(surface),
		Title:              "Trim the never-fired example helper offer",
		EditAction:         "trim the never-fired example helper offer",
		EditSignal:         "it was offered 12 times and never fired",
		Consequence:        "the " + reviewartifact.PlainLabel(surface) + " surface drops an offer nothing uses",
		SuccessPredicate:   "edit the surface only after a green before/after bench run and operator admission",
		SurfaceWhitelisted: false,
		AdmissionStatus:    "blocked_until_operator_whitelists_surface",
		EvidenceRefs:       []string{"capability_usage_never_fired:capability://skill:example-helper"},
	}
	paths, err := reviewartifact.Write(home, harnessEditReviewArea, id, card, "  ", renderHarnessEditCard(card))
	if err != nil {
		t.Fatalf("seed harness-edit card: %v", err)
	}
	return paths.PlanPath
}

// queueHarnessEdit runs the production read pipeline for the harness-edit area:
// Queue -> AnnotateHarnessEditProposalQueue -> Recount, so a test sees exactly
// what the workbench review queue projects.
func queueHarnessEdit(t *testing.T, home string) reviewartifact.QueueReport {
	t.Helper()
	report, err := reviewartifact.Queue(home, reviewartifact.QueueOptions{Area: harnessEditReviewArea}, directorTestNow)
	if err != nil {
		t.Fatalf("queue harness-edit: %v", err)
	}
	AnnotateHarnessEditProposalQueue(&report)
	reviewartifact.Recount(&report)
	return report
}

func findHarnessEditItem(t *testing.T, report reviewartifact.QueueReport, id string) reviewartifact.QueueItem {
	t.Helper()
	for _, item := range report.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("harness-edit item %q not found in queue (items=%d)", id, len(report.Items))
	return reviewartifact.QueueItem{}
}

func groupsReferenceItem(report reviewartifact.QueueReport, id string) bool {
	for _, g := range report.Groups {
		for _, memberID := range g.ItemIDs {
			if memberID == id {
				return true
			}
		}
	}
	return false
}

func harnessEditSidecarFiles(t *testing.T, home string) []string {
	t.Helper()
	dir := filepath.Join(home, "reviews", harnessEditReviewArea)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), harnessEditReviewedSuffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// --- B1: whitelist writes the registry and bands the card out; re-run is
// idempotent (no duplicate registry entry, no second sidecar). ---
func TestHarnessEditReviewWhitelistWritesRegistryAndBandsOut(t *testing.T) {
	home := t.TempDir()
	surface := "capability_edges"
	cardPath := seedHarnessEditCard(t, home, "dirprop_b1fixture", surface)

	// Precondition: registry empty; the open card already carries the one action.
	open := findHarnessEditItem(t, queueHarnessEdit(t, home), "dirprop_b1fixture")
	if len(open.Actions) != 1 {
		t.Fatalf("open card must carry exactly one whitelist action pre-decision, got %d", len(open.Actions))
	}

	rep, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:   cardPath,
		Decision:   HarnessEditDecisionWhitelist,
		Rationale:  "agents may trim dead offers on this surface",
		ReviewedBy: "operator:test",
	}, directorTestNow)
	if err != nil {
		t.Fatalf("whitelist: %v", err)
	}
	if rep.Decision != HarnessEditDecisionWhitelist || rep.TargetSurface != surface {
		t.Fatalf("report should record whitelist decision on %q, got %+v", surface, rep)
	}

	// Registry now contains the surface, active.
	reg, _, err := policy.LoadEditableSurfaces(home)
	if err != nil {
		t.Fatal(err)
	}
	if allowed, reason := reg.Allowed(surface); !allowed {
		t.Fatalf("whitelist must make the surface active, got denied: %s", reason)
	}
	if reg.ActiveCount() != 1 {
		t.Fatalf("exactly one surface should be active, got %d", reg.ActiveCount())
	}

	// The card is banded OUT of needs-you: no action, filed band, no needs-you group.
	report := queueHarnessEdit(t, home)
	resolved := findHarnessEditItem(t, report, "dirprop_b1fixture")
	if len(resolved.Actions) != 0 {
		t.Fatalf("resolved card must carry no action, got %+v", resolved.Actions)
	}
	if resolved.Band != reviewartifact.BandFiled {
		t.Fatalf("resolved card must be filed, got band %q", resolved.Band)
	}
	if groupsReferenceItem(report, "dirprop_b1fixture") {
		t.Fatalf("resolved card must not appear in any review group (banded out of needs-you)")
	}

	// Idempotent re-run: exit 0, byte-identical registry, one active entry, one sidecar.
	regPath := policy.EditableSurfacesPath(home)
	before, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:   cardPath,
		Decision:   HarnessEditDecisionWhitelist,
		Rationale:  "agents may trim dead offers on this surface",
		ReviewedBy: "operator:test",
	}, directorTestNow); err != nil {
		t.Fatalf("idempotent re-run must not error: %v", err)
	}
	after, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("idempotent re-run rewrote the registry:\nbefore=%s\nafter=%s", before, after)
	}
	reg2, _, _ := policy.LoadEditableSurfaces(home)
	if reg2.ActiveCount() != 1 {
		t.Fatalf("re-run duplicated a registry entry; active count = %d", reg2.ActiveCount())
	}
	if got := harnessEditSidecarFiles(t, home); len(got) != 1 {
		t.Fatalf("re-run must not double-record; sidecar count = %d (%v)", len(got), got)
	}
}

// --- B2: reject records the decline + rationale but writes NOTHING to the
// registry; the card still leaves needs-you. ---
func TestHarnessEditReviewRejectLeavesRegistryUnchanged(t *testing.T) {
	home := t.TempDir()
	cardPath := seedHarnessEditCard(t, home, "dirprop_b2fixture", "capability_edges")
	regPath := policy.EditableSurfacesPath(home)
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatalf("registry must be absent before reject, stat err=%v", err)
	}

	rep, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:  cardPath,
		Decision:  HarnessEditDecisionReject,
		Rationale: "not opening this surface to edits right now",
	}, directorTestNow)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if rep.Decision != HarnessEditDecisionReject {
		t.Fatalf("report should record reject, got %+v", rep)
	}

	// Registry STILL absent — reject writes no registry byte.
	if _, err := os.Stat(regPath); !os.IsNotExist(err) {
		t.Fatalf("reject must not create the editable-surfaces registry; stat err=%v", err)
	}

	// Sidecar recorded with the decline + rationale.
	sidecars := harnessEditSidecarFiles(t, home)
	if len(sidecars) != 1 {
		t.Fatalf("reject must record exactly one sidecar, got %d", len(sidecars))
	}
	loaded, err := LoadHarnessEditReviewReport(sidecars[0])
	if err != nil {
		t.Fatalf("load recorded sidecar: %v", err)
	}
	if loaded.Decision != HarnessEditDecisionReject || loaded.Rationale != "not opening this surface to edits right now" {
		t.Fatalf("sidecar must carry the decline + rationale, got %+v", loaded)
	}

	// Card leaves needs-you.
	report := queueHarnessEdit(t, home)
	item := findHarnessEditItem(t, report, "dirprop_b2fixture")
	if len(item.Actions) != 0 {
		t.Fatalf("declined card must carry no action, got %+v", item.Actions)
	}
	if item.Band != reviewartifact.BandFiled {
		t.Fatalf("declined card must be filed, got band %q", item.Band)
	}
}

// --- B2b: whitelist opens the editable-surfaces registry, so it refuses
// without explicit operator provenance on the reviewer (the operator-console
// default can never open a surface); reject keeps the default reviewer. ---
func TestHarnessEditReviewWhitelistRequiresOperatorProvenance(t *testing.T) {
	// No reviewer at all: the default must not satisfy a whitelist.
	home := t.TempDir()
	cardPath := seedHarnessEditCard(t, home, "dirprop_b2b_default", "capability_edges")
	if _, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:  cardPath,
		Decision:  HarnessEditDecisionWhitelist,
		Rationale: "open it",
	}, directorTestNow); err == nil {
		t.Fatal("whitelist with the default reviewer must be refused")
	}
	if _, err := os.Stat(policy.EditableSurfacesPath(home)); !os.IsNotExist(err) {
		t.Fatalf("refused whitelist must not write the registry; stat err=%v", err)
	}
	if got := harnessEditSidecarFiles(t, home); len(got) != 0 {
		t.Fatalf("refused whitelist must write no sidecar, got %v", got)
	}

	// A non-operator reviewer (an executor identity) is refused the same way.
	cardPath2 := seedHarnessEditCard(t, home, "dirprop_b2b_executor", "capability_edges")
	if _, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:   cardPath2,
		Decision:   HarnessEditDecisionWhitelist,
		Rationale:  "open it",
		ReviewedBy: "claude-code-executor",
	}, directorTestNow); err == nil {
		t.Fatal("whitelist by a non-operator reviewer must be refused")
	}
	if _, err := os.Stat(policy.EditableSurfacesPath(home)); !os.IsNotExist(err) {
		t.Fatalf("refused whitelist must not write the registry; stat err=%v", err)
	}

	// An explicit operator-provenance reviewer whitelists.
	rep, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:   cardPath2,
		Decision:   HarnessEditDecisionWhitelist,
		Rationale:  "open it",
		ReviewedBy: "operator:test",
	}, directorTestNow)
	if err != nil {
		t.Fatalf("whitelist with operator provenance: %v", err)
	}
	if !rep.SurfaceWhitelisted || rep.ReviewedBy != "operator:test" {
		t.Fatalf("whitelist report = %+v", rep)
	}

	// Reject still tolerates the default reviewer (it writes no registry byte).
	cardPath3 := seedHarnessEditCard(t, home, "dirprop_b2b_reject", "capability_edges")
	rep, err = RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:  cardPath3,
		Decision:  HarnessEditDecisionReject,
		Rationale: "not now",
	}, directorTestNow)
	if err != nil {
		t.Fatalf("reject with default reviewer: %v", err)
	}
	if rep.ReviewedBy != harnessEditDefaultReviewer {
		t.Fatalf("reject should keep the default reviewer, got %q", rep.ReviewedBy)
	}
}

// --- B3: every refusal path exits non-zero and writes NOTHING (registry stays
// absent, card byte-identical, no sidecar). ---
func TestHarnessEditReviewRefusesAndWritesNothing(t *testing.T) {
	validCardID := "dirprop_b3fixture"

	// A card carrying the wrong schema — used by the "schema-invalid" case.
	writeSchemaInvalidCard := func(t *testing.T, home string) string {
		t.Helper()
		dir := filepath.Join(home, "reviews", harnessEditReviewArea)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "dirprop_badschema.json")
		body := `{"schema_version":"compound.some_other_card.v9","id":"dirprop_badschema","status":"review_required","target_surface":"capability_edges"}` + "\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cases := []struct {
		name  string
		setup func(t *testing.T, home string) HarnessEditReviewInput
	}{
		{
			name: "missing_rationale",
			setup: func(t *testing.T, home string) HarnessEditReviewInput {
				card := seedHarnessEditCard(t, home, validCardID, "capability_edges")
				return HarnessEditReviewInput{CardPath: card, Decision: HarnessEditDecisionWhitelist, Rationale: "   "}
			},
		},
		{
			name: "unknown_decision",
			setup: func(t *testing.T, home string) HarnessEditReviewInput {
				card := seedHarnessEditCard(t, home, validCardID, "capability_edges")
				return HarnessEditReviewInput{CardPath: card, Decision: "approve", Rationale: "looks fine"}
			},
		},
		{
			name: "missing_card",
			setup: func(t *testing.T, home string) HarnessEditReviewInput {
				missing := filepath.Join(home, "reviews", harnessEditReviewArea, "dirprop_ghost.json")
				return HarnessEditReviewInput{CardPath: missing, Decision: HarnessEditDecisionWhitelist, Rationale: "open it"}
			},
		},
		{
			name: "schema_invalid_card",
			setup: func(t *testing.T, home string) HarnessEditReviewInput {
				card := writeSchemaInvalidCard(t, home)
				return HarnessEditReviewInput{CardPath: card, Decision: HarnessEditDecisionWhitelist, Rationale: "open it"}
			},
		},
		{
			name: "path_outside_reviews_root",
			setup: func(t *testing.T, home string) HarnessEditReviewInput {
				dir := filepath.Join(home, "outside")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "dirprop_outside.json")
				body := `{"schema_version":"` + harnessEditProposalCardSchema + `","id":"dirprop_outside","status":"review_required","target_surface":"capability_edges"}` + "\n"
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				return HarnessEditReviewInput{CardPath: path, Decision: HarnessEditDecisionWhitelist, Rationale: "open it"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			input := tc.setup(t, home)

			// Snapshot the card bytes (if a card exists under reviews) before the refusal.
			var cardBefore []byte
			if strings.HasPrefix(filepath.ToSlash(input.CardPath), filepath.ToSlash(filepath.Join(home, "reviews"))) {
				cardBefore, _ = os.ReadFile(input.CardPath)
			}

			if _, err := RecordHarnessEditReview(home, input, directorTestNow); err == nil {
				t.Fatalf("%s: expected a refusal, got nil error", tc.name)
			}

			// Registry never created.
			if _, err := os.Stat(policy.EditableSurfacesPath(home)); !os.IsNotExist(err) {
				t.Fatalf("%s: refusal must not write the registry; stat err=%v", tc.name, err)
			}
			// No sidecar written.
			if got := harnessEditSidecarFiles(t, home); len(got) != 0 {
				t.Fatalf("%s: refusal must write no sidecar, got %v", tc.name, got)
			}
			// The card (when present under reviews) is byte-identical.
			if cardBefore != nil {
				after, err := os.ReadFile(input.CardPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(cardBefore) != string(after) {
					t.Fatalf("%s: refusal mutated the card file", tc.name)
				}
			}
		})
	}
}

// --- B4: the annotator gives an open card EXACTLY one whitelist action (exact
// argv, never --rationale), and a resolved card none. ---
func TestHarnessEditAnnotatorActionShape(t *testing.T) {
	home := t.TempDir()
	cardPath := seedHarnessEditCard(t, home, "dirprop_b4fixture", "capability_edges")

	report := queueHarnessEdit(t, home)
	item := findHarnessEditItem(t, report, "dirprop_b4fixture")
	if len(item.Actions) != 1 {
		t.Fatalf("open card must carry exactly one action, got %d: %+v", len(item.Actions), item.Actions)
	}
	act := item.Actions[0]
	if act.ID != "harness-edit.whitelist" {
		t.Fatalf("action id = %q, want harness-edit.whitelist", act.ID)
	}
	if act.Label != "Allow edits to this surface" {
		t.Fatalf("action label = %q, want \"Allow edits to this surface\"", act.Label)
	}
	if !act.RequiresReview {
		t.Fatalf("whitelist action must require review")
	}
	wantArgv := []string{"compound", "authority", "harness-edit-review", item.JSONPath, "--decision", "whitelist", "--json"}
	if strings.Join(act.Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("action argv = %v, want %v", act.Argv, wantArgv)
	}
	for _, a := range act.Argv {
		if a == "--rationale" {
			t.Fatalf("projected argv must NEVER carry --rationale; the console appends it")
		}
	}
	wantCommand := "compound authority harness-edit-review " + item.JSONPath + " --decision whitelist"
	if act.Command != wantCommand {
		t.Fatalf("action command = %q, want %q", act.Command, wantCommand)
	}
	// The card path threaded into argv is the card we seeded.
	if item.JSONPath != cardPath {
		t.Fatalf("annotator argv should carry the card path %q, got %q", cardPath, item.JSONPath)
	}

	// A resolved card carries NO action.
	if _, err := RecordHarnessEditReview(home, HarnessEditReviewInput{
		CardPath:  cardPath,
		Decision:  HarnessEditDecisionReject,
		Rationale: "not now",
	}, directorTestNow); err != nil {
		t.Fatalf("reject to resolve the card: %v", err)
	}
	resolved := findHarnessEditItem(t, queueHarnessEdit(t, home), "dirprop_b4fixture")
	if len(resolved.Actions) != 0 {
		t.Fatalf("resolved card must carry no action, got %+v", resolved.Actions)
	}
}
