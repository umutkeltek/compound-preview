package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/product"
	"compound/internal/work"
)

func seedReceiptCardWork(t *testing.T, homePath, workID string) envelope.Receipt {
	t.Helper()
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("init home: %v", err)
	}
	w := work.BuildWork(work.CreateInput{
		WorkID: workID, Owner: "fixture-owner", State: "completed",
		SuccessPredicate: "the governed post reaches its configured transport",
	}, cliTestNow)
	if err := w.Validate(); err != nil {
		t.Fatalf("closed work fixture invalid: %v", err)
	}
	wBody, err := json.MarshalIndent(w, "", " ")
	if err != nil {
		t.Fatalf("marshal closed work fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "work", "work", workID+".json"), append(wBody, '\n'), 0o644); err != nil {
		t.Fatalf("write closed work fixture: %v", err)
	}
	receipt := envelope.Receipt{
		Header: envelope.Header{
			SchemaVersion: envelope.SVReceipt,
			ID:            "rec_fixture_card",
			CreatedAt:     cliTestNow.UTC().Format("2006-01-02T15:04:05Z07:00"),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: workID},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Outcome:    "completed",
		MoveType:   "outward_delta",
		StateDelta: envelope.StateDelta{Changed: true, Summary: "fixture changed", FilesChanged: []string{}, Artifacts: []string{}, Verification: []string{}},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: []string{}, Missed: []string{}, Candidate: []string{}},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  []string{"check:F1", "check:F2", "live:fixture", "commit:abc123", "check:omitted"},
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("fixture receipt invalid: %v", err)
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	if _, err := events.PutJSON(receipt.ID, &receipt, " "); err != nil {
		t.Fatalf("record fixture receipt: %v", err)
	}
	terminal := work.Event{
		TS: cliTestNow.UTC().Format("20060102T150405.000000000Z"), WorkID: workID,
		From: "verifying", To: "completed", Note: "closed: outward_delta (policy allow_with_obligations)",
		Attached: []string{"act_fixture_card", "pol_fixture_card", receipt.ID, "tr_fixture_card"},
	}
	line, err := json.Marshal(terminal)
	if err != nil {
		t.Fatalf("marshal terminal event: %v", err)
	}
	line = append(line, '\n')
	if err := events.AppendWorkLog(line, func(w io.Writer, p []byte) (int, error) { return w.Write(p) }); err != nil {
		t.Fatalf("append terminal event: %v", err)
	}
	return receipt
}

func writePostSpyTransport(t *testing.T, homePath, name string) string {
	t.Helper()
	marker := filepath.Join(homePath, "post-marker")
	script := filepath.Join(homePath, "post-spy.sh")
	scriptBody := "#!/bin/sh\ncount_file=\"$1.count\"\ncount=0\n[ ! -f \"$count_file\" ] || count=$(cat \"$count_file\")\ncount=$((count + 1))\nprintf '%s' \"$count\" > \"$count_file\"\ncat > \"$1\"\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatalf("write post spy: %v", err)
	}
	dir := filepath.Join(homePath, "actuator", "transports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create transport config dir: %v", err)
	}
	config, err := json.Marshal(map[string]any{"argv": []string{script, marker}, "timeout_s": 30})
	if err != nil {
		t.Fatalf("marshal transport config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), config, 0o644); err != nil {
		t.Fatalf("write transport config: %v", err)
	}
	return marker
}

func seedAuthorizedPostDraft(t *testing.T, homePath, accountStatus, integrationID string) product.ContentDraft {
	t.Helper()
	if err := home.Init(homePath, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	draft, err := product.RecordContentDraft(homePath, product.ContentDraftInput{
		ProductID:    "product:prod_synth_a",
		Platform:     "x",
		Body:         "grounded synthetic post",
		EvidenceRefs: []string{"finding:prod-synth"},
		ExpiresAt:    time.Now().UTC().Add(72 * time.Hour),
	}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := product.ContentPackageDigest(homePath, draft)
	if err != nil {
		t.Fatal(err)
	}
	review := product.ContentQueueReview{
		SchemaVersion: product.ContentQueueReviewSchemaVersion,
		ID:            "cqr_act_post",
		Drafts:        []product.ContentQueueReviewRow{{DraftID: draft.ID, PackageDigest: digest, Verdict: "approve"}},
	}
	if _, err := product.ApplyContentQueueReview(homePath, review, "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	registry, err := json.Marshal(map[string]any{
		"schema_version": product.SocialAccountRegistrySchemaVersion,
		"accounts": []map[string]string{{
			"id": "prod-synth-x", "product_id": draft.ProductID, "platform": draft.Platform,
			"transport": "fixture", "postiz_integration_id": integrationID, "status": accountStatus,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homePath, "registries", "social-accounts.json"), registry, 0o644); err != nil {
		t.Fatal(err)
	}
	return draft
}

func writeVerifiedPostTransport(t *testing.T, homePath, name string) string {
	t.Helper()
	if name != "fixture" {
		t.Fatal("synthetic transport is fixture")
	}
	return filepath.Join(b2Transport(t, homePath), "request")
}

// F5: a closed envelope composes deterministically; a non-terminal envelope is refused.
func TestComposeReceiptCardClosedAndNonTerminal(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	workID := "work_receipt_card"
	seedReceiptCardWork(t, homePath, workID)

	card, err := composeReceiptCard(homePath, workID)
	if err != nil {
		t.Fatalf("compose closed work card: %v", err)
	}
	want := strings.Join([]string{
		"🧾 [The ledger] work_receipt_card — completed",
		"the governed post reaches its configured transport",
		"- move: outward_delta · receipt: rec_fixture_card · transition: tr_fixture_card",
		"- proofs: check:F1, check:F2, live:fixture, commit:abc123",
	}, "\n")
	if card != want {
		t.Fatalf("receipt card is not deterministic:\ngot:\n%s\nwant:\n%s", card, want)
	}

	readyHome := t.TempDir()
	if err := home.Init(readyHome, "test", cliTestNow); err != nil {
		t.Fatalf("init ready home: %v", err)
	}
	readyStore, err := work.Open(readyHome)
	if err != nil {
		t.Fatalf("open ready work store: %v", err)
	}
	if _, err := readyStore.Create(work.CreateInput{
		WorkID: "work_receipt_card_ready", Owner: "fixture-owner", State: "ready",
		SuccessPredicate: "non-terminal work cannot be posted as a receipt card",
	}, cliTestNow); err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	if _, err := composeReceiptCard(readyHome, "work_receipt_card_ready"); err == nil || !strings.Contains(err.Error(), "not terminal") {
		t.Fatalf("non-terminal compose error = %v, want terminal-state refusal", err)
	}
}

// F2: without --confirm, act post prints the card and policy outcome but never execs.
func TestActPostDryRunPreviewsWithoutExecuting(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	workID := "work_post_dry_run"
	seedReceiptCardWork(t, homePath, workID)
	marker := writePostSpyTransport(t, homePath, "fixture")

	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post",
		"--from-work", workID,
		"--work", workID,
		"--transport", "fixture",
		"--home", homePath,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("dry-run exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "🧾 [The ledger] "+workID+" — completed") || !strings.Contains(stdout.String(), "policy: hold") {
		t.Fatalf("dry-run preview missing card or policy outcome:\n%s", stdout.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("dry-run executed transport; marker stat error = %v", err)
	}
}

// F7: a confirmed governed post belongs to an explicit work envelope.
func TestActPostConfirmRequiresWork(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--content", "-", "--transport", "fixture", "--confirm", "--home", homePath,
	}, strings.NewReader("raw card"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("confirmed post without --work exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "--work") || !strings.Contains(stderr.String(), "required") {
		t.Fatalf("confirmed post without --work missing user error: %q", stderr.String())
	}
}

// F3: an imported exact grant lets --confirm execute once and records the governed result.
func TestActPostConfirmWithGrantExecutesOnceAndRecordsReceipt(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	workID := "work_post_granted"
	seedReceiptCardWork(t, homePath, workID)
	marker := writePostSpyTransport(t, homePath, "fixture")
	wantCard, err := composeReceiptCard(homePath, workID)
	if err != nil {
		t.Fatalf("compose expected card: %v", err)
	}
	grantSource := filepath.Join(homePath, "standing-grants.yaml")
	grantBody := "generated_at: \"2026-07-01T12:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:test-fixture-post\"\n" +
		"  reviewed_by: \"operator:test\"\n" +
		"grants:\n" +
		"  - grant_id: grant_fixture_post\n" +
		"    capabilities: [\"" + product.ContentPostCapability + "\"]\n" +
		"    target_scope: \"actuator/transports/fixture.json\"\n" +
		"    mutation_class: external_side_effect\n" +
		"    reversibility_ceiling: irreversible\n" +
		"    blast_radius: local\n"
	if err := os.WriteFile(grantSource, []byte(grantBody), 0o644); err != nil {
		t.Fatalf("write grant source: %v", err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatalf("import standing grants: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post",
		"--from-work", workID,
		"--work", workID,
		"--transport", "fixture",
		"--home", homePath,
		"--confirm",
		"--json",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("granted post exit = %d, stderr=%q", code, stderr.String())
	}
	gotCard, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read posted card: %v", err)
	}
	if string(gotCard) != wantCard {
		t.Fatalf("transport body mismatch:\ngot:\n%s\nwant:\n%s", gotCard, wantCard)
	}
	count, err := os.ReadFile(marker + ".count")
	if err != nil {
		t.Fatalf("read transport execution count: %v", err)
	}
	if string(count) != "1" {
		t.Fatalf("transport execution count = %q, want 1", count)
	}
	var result struct {
		ReceiptID    string `json:"receipt_id"`
		TransitionID string `json:"transition_id"`
		PreImageRef  string `json:"pre_image_ref"`
		PostImageRef string `json:"post_image_ref"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode post result: %v\n%s", err, stdout.String())
	}
	if result.ReceiptID == "" || result.TransitionID == "" || result.PreImageRef == "" || result.PostImageRef == "" {
		t.Fatalf("post result missing governed refs: %+v", result)
	}
	if !testEventExists(t, homePath, result.ReceiptID) {
		t.Fatalf("receipt %s does not resolve through event store", result.ReceiptID)
	}
	var receipt envelope.Receipt
	readTestEventJSON(t, homePath, result.ReceiptID, &receipt)
	if len(receipt.ProofRefs) != 2 || receipt.ProofRefs[0] != result.PreImageRef || receipt.ProofRefs[1] != result.PostImageRef {
		t.Fatalf("receipt artifact refs = %v, want [%s %s]", receipt.ProofRefs, result.PreImageRef, result.PostImageRef)
	}
}

func TestActPostConfirmWithoutGrantHoldsWithoutExecuting(t *testing.T) {
	t.Parallel()
	homePath := t.TempDir()
	workID := "work_post_held"
	seedReceiptCardWork(t, homePath, workID)
	marker := writePostSpyTransport(t, homePath, "fixture")
	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--from-work", workID, "--work", workID,
		"--transport", "fixture", "--home", homePath, "--confirm",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("held post exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "held — nothing sent") || !strings.Contains(stdout.String(), "policy: hold") {
		t.Fatalf("held preview missing outcome:\n%s", stdout.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("held post executed transport; marker stat error = %v", err)
	}
}

func TestActPostDraftRefusesPendingEnrollmentBeforeTransport(t *testing.T) {
	// No t.Parallel: Go forbids it alongside t.Setenv, and the explicit
	// experimental opt-in below is what this test needs more than parallelism.
	// This test exercises the EXPERIMENTAL distribution loop deliberately, so it
	// opts in explicitly. The gate exists to stop the surface being reached by
	// accident, not to stop it being tested.
	t.Setenv(product.DistributionExperimentEnv, "1")
	homePath := t.TempDir()
	draft := seedAuthorizedPostDraft(t, homePath, "pending_enrollment", "")
	marker := writeVerifiedPostTransport(t, homePath, "fixture")
	scheduleAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--draft", draft.ID, "--at", scheduleAt,
		"--transport", "fixture", "--home", homePath,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "human enrollment") {
		t.Fatalf("pending account exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pending account executed transport: %v", err)
	}
}

func TestActPostDraftWithGrantSendsCompleteRequestAndCapturesRemoteProof(t *testing.T) {
	// No t.Parallel: Go forbids it alongside t.Setenv, and the explicit
	// experimental opt-in below is what this test needs more than parallelism.
	// This test exercises the EXPERIMENTAL distribution loop deliberately, so it
	// opts in explicitly. The gate exists to stop the surface being reached by
	// accident, not to stop it being tested.
	t.Setenv(product.DistributionExperimentEnv, "1")
	homePath := t.TempDir()
	draft := seedAuthorizedPostDraft(t, homePath, "active", "integration-synth")
	marker := writeVerifiedPostTransport(t, homePath, "fixture")
	grantSource := filepath.Join(homePath, "standing-grants.yaml")
	grantBody := "generated_at: \"2026-07-01T12:00:00Z\"\n" +
		"authority:\n" +
		"  decision_ref: \"grant:test-content-draft-post\"\n" +
		"  reviewed_by: \"operator:test\"\n" +
		"grants:\n" +
		"  - grant_id: grant_content_draft_post\n" +
		"    capabilities: [\"" + product.ContentPostCapability + "\"]\n" +
		"    target_scope: \"actuator/transports/fixture.json\"\n" +
		"    mutation_class: external_side_effect\n" +
		"    reversibility_ceiling: irreversible\n" +
		"    blast_radius: local\n"
	if err := os.WriteFile(grantSource, []byte(grantBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, grantSource, cliTestNow); err != nil {
		t.Fatal(err)
	}
	scheduleAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)

	var stdout, stderr bytes.Buffer
	code := runActWithInput([]string{
		"post", "--draft", draft.ID, "--at", scheduleAt,
		"--work", "work_publish_synth", "--transport", "fixture",
		"--home", homePath, "--confirm", "--json",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("draft post exit=%d stderr=%q", code, stderr.String())
	}
	requestBody, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var request product.ContentPostTransportRequest
	if err := json.Unmarshal(requestBody, &request); err != nil {
		t.Fatalf("transport did not receive request JSON: %v\n%s", err, requestBody)
	}
	if request.DraftID != draft.ID || request.ProductID != draft.ProductID ||
		request.Platform != draft.Platform || request.IntegrationID != "integration-synth" ||
		request.ScheduleAt != scheduleAt || request.Body != "grounded synthetic post" {
		t.Fatalf("transport request incomplete: %+v", request)
	}
	var result struct {
		ReceiptID    string `json:"receipt_id"`
		PostImageRef string `json:"post_image_ref"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v\n%s", err, stdout.String())
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		t.Fatal(err)
	}
	postImage, err := store.Get(result.PostImageRef)
	if err != nil {
		t.Fatal(err)
	}
	var observation product.ContentTransportObservation
	if err := json.Unmarshal(postImage, &observation); err != nil {
		t.Fatal(err)
	}
	if observation.Status != "scheduled" || observation.ScheduleID != "schedule-synth" || observation.RemoteID != "" || observation.AttemptID != request.AttemptID {
		t.Fatalf("schedule proof = %+v", observation)
	}
	stored, err := product.ReadContentDraft(homePath, draft.ID)
	if err != nil || stored.State != product.ContentDraftStateScheduled || stored.Post != nil {
		t.Fatalf("schedule became publication: %+v %v", stored, err)
	}
}
