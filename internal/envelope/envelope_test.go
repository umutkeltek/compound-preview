package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func hdr(sv, id string) Header {
	return Header{
		SchemaVersion: sv,
		ID:            id,
		CreatedAt:     "2026-06-28T00:00:00Z",
		Producer:      Producer{Kind: "cli", Name: "compound"},
		Authority:     Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier0",
		EvidenceRefs:  []string{},
		Uncertainty:   Uncertainty{Level: "known"},
	}
}

func withWork(h Header, wid string) Header {
	h.IDs.WorkID = wid
	return h
}

func TestReceiptEffectiveChanged(t *testing.T) {
	base := func(moveType, outcome string, changed bool) Receipt {
		return Receipt{
			Header:     hdr(SVReceipt, "rec_effective"),
			Outcome:    outcome,
			MoveType:   moveType,
			StateDelta: StateDelta{Changed: changed, Summary: "summary", FilesChanged: []string{}, Artifacts: []string{}, Verification: []string{}},
			Cost:       ReceiptCost{ModelCalls: []string{}},
			Skills:     ReceiptSkills{Used: []string{}, Missed: []string{}, Candidate: []string{}},
			Memory:     ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
			Next:       ReceiptNext{SuggestedWork: []string{}},
		}
	}

	tests := []struct {
		name string
		rec  Receipt
		want bool
	}{
		{name: "explicit changed", rec: base("audit_only", "completed", true), want: true},
		{name: "completed audit unchanged", rec: base("audit_only", "completed", false), want: false},
		{name: "failed proof unchanged", rec: func() Receipt {
			rec := base("control_plane_repair", "failed", false)
			rec.ProofRefs = []string{"proof:failed"}
			return rec
		}(), want: false},
		{name: "control repair proof", rec: func() Receipt {
			rec := base("control_plane_repair", "completed", false)
			rec.ProofRefs = []string{"proof:repair"}
			return rec
		}(), want: true},
		{name: "rule proof", rec: func() Receipt {
			rec := base("rule_delta", "completed", false)
			rec.ProofRefs = []string{"guard:repeat-failure"}
			return rec
		}(), want: true},
		{name: "memory proof", rec: func() Receipt {
			rec := base("memory_delta", "completed", false)
			rec.Memory.Promoted = []string{"claim_1"}
			return rec
		}(), want: true},
		{name: "outward proof", rec: func() Receipt {
			rec := base("outward_delta", "completed", false)
			rec.ProofRefs = []string{"artifact:handoff"}
			return rec
		}(), want: true},
		{name: "outward proofless unchanged", rec: base("outward_delta", "completed", false), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.EffectiveChanged(); got != tt.want {
				t.Fatalf("EffectiveChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllKindsValidateAndRoundTrip(t *testing.T) {
	envs := []Envelope{
		&EffectiveContract{Header: hdr(SVEffectiveContractV01, "contract_legacy"), Harness: "claude_code"},
		&EffectiveContract{Header: hdr(SVEffectiveContract, "contract_1"), Harness: "claude_code"},
		&ContextRequest{Header: hdr(SVContextRequest, "ctxreq_1"), Harness: "claude_code", RequestedMode: "execute"},
		&ContextPack{
			Header: hdr(SVContextPack, "ctx_1"),
			Included: []IncludedRef{{
				Ref:         "claim_1",
				SourceClass: "evidence",
				Status:      "current",
				Confidence:  0.9,
			}},
			Excluded: []ExcludedRef{},
		},
		&WorkEnvelope{Header: hdr(SVWork, "work_1"), Owner: "compound", State: "ready", SuccessPredicate: "a state delta exists"},
		func() Envelope {
			source := NewSourceEnvelope(
				hdr(SVSourceEnvelope, "src_1"),
				SourceOrigin{Kind: "web", Locator: "https://docs.example.invalid/fixture"},
				[]byte("fixture"),
				SourceTrustUntrusted,
				ConfidentialityPublic,
				[]string{"reader"},
			)
			return &source
		}(),
		&ActionIntent{
			Header:        withWork(hdr(SVActionIntent, "act_1"), "work_1"),
			Capability:    "filesystem.write",
			Action:        "write_file",
			Target:        ActionTarget{Kind: "file", Path: "docs/architecture/runtime.md"},
			MutationClass: "local_reversible",
			Reversibility: "easy",
		},
		&PolicyDecision{Header: hdr(SVPolicyDecision, "pol_1"), ActionIntentID: "act_1", Outcome: "allow"},
		&HookEvent{Header: hdr(SVHookEvent, "hook_1"), Harness: "claude_code", HookName: "SessionStart", Status: "ok"},
		&TraceEnvelope{
			Header: hdr(SVTraceEnvelope, "trace_1"),
			Source: TraceSource{
				System:     "os-event",
				Stream:     "control-events",
				SourceID:   "evt_1",
				SourceKind: "snapshot.generated",
			},
			MutationClass:   "read",
			ToolsModelsUsed: []string{},
			FilesTouched:    []string{},
			Outputs:         []string{},
			Cost:            ReceiptCost{ModelCalls: []string{}},
		},
		&Receipt{Header: hdr(SVReceipt, "rec_1"), Outcome: "completed", MoveType: "memory_delta", Memory: ReceiptMemory{Promoted: []string{"claim_1"}}},
		&Transition{Header: withWork(hdr(SVTransition, "tr_1"), "work_1")},
	}
	if len(envs) != len(registry) {
		t.Fatalf("covered %d kinds, registry has %d; kind is untested", len(envs), len(registry))
	}
	for _, e := range envs {
		if err := e.Validate(); err != nil {
			t.Errorf("%s: should be valid: %v", e.Kind(), err)
		}
		if err := RoundTrip(e); err != nil {
			t.Errorf("%s: round-trip: %v", e.Kind(), err)
		}
	}
}

func TestWorkEnvelopeGitDeliveryRoundTripsAndLegacyMissingFieldLoads(t *testing.T) {
	const baseSHA = "0123456789abcdef0123456789abcdef01234567"
	const repositoryFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	repoRoot := filepath.Join(string(filepath.Separator), "repo", "product")
	w := WorkEnvelope{
		Header:           hdr(SVWork, "work_expected_base"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "candidate lands from its frozen base",
		GitDelivery: &GitDeliveryContract{
			TargetWorktreeRoot:    repoRoot,
			RepositoryFingerprint: repositoryFingerprint,
			TargetLocalRef:        "refs/heads/main",
			ExpectedBaseSHA:       baseSHA,
			PublicationMode:       "required",
			PublicationRef:        "refs/remotes/origin/main",
		},
	}
	if err := RoundTrip(&w); err != nil {
		t.Fatalf("git-delivery work round-trip: %v", err)
	}

	legacyJSON := `{"schema_version":"compound.work.v0.1","id":"work_legacy_base","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound"},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"compound","state":"ready","success_predicate":"legacy remains valid","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	var legacy WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode legacy work: %v", err)
	}
	if legacy.GitDelivery != nil {
		t.Fatalf("legacy git_delivery = %+v, want nil", legacy.GitDelivery)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without git_delivery must remain valid: %v", err)
	}

	invalid := w
	invalid.ID = "work_invalid_base"
	invalid.GitDelivery = &GitDeliveryContract{
		TargetWorktreeRoot:    repoRoot,
		RepositoryFingerprint: repositoryFingerprint,
		TargetLocalRef:        "refs/heads/main",
		ExpectedBaseSHA:       "ABCDEF",
		PublicationMode:       "required",
		PublicationRef:        "refs/remotes/origin/main",
	}
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "expected_base_sha") {
		t.Fatalf("invalid expected_base_sha should fail validation, got %v", err)
	}

	invalidFingerprint := w
	invalidFingerprint.ID = "work_invalid_repository_fingerprint"
	invalidFingerprint.GitDelivery = &GitDeliveryContract{
		TargetWorktreeRoot:    repoRoot,
		RepositoryFingerprint: "ABCDEF",
		TargetLocalRef:        "refs/heads/main",
		ExpectedBaseSHA:       baseSHA,
		PublicationMode:       "required",
		PublicationRef:        "refs/remotes/origin/main",
	}
	if err := invalidFingerprint.Validate(); err == nil || !strings.Contains(err.Error(), "repository_fingerprint") {
		t.Fatalf("non-empty invalid repository fingerprint should fail validation, got %v", err)
	}

	partial := w
	partial.ID = "work_partial_delivery"
	partial.GitDelivery = &GitDeliveryContract{
		TargetWorktreeRoot:    repoRoot,
		RepositoryFingerprint: repositoryFingerprint,
		TargetLocalRef:        "refs/heads/main",
		ExpectedBaseSHA:       baseSHA,
		PublicationMode:       "required",
	}
	if err := partial.Validate(); err == nil || !strings.Contains(err.Error(), "publication_ref") {
		t.Fatalf("publication-required contract without remote-tracking ref should fail, got %v", err)
	}

	localOnly := w
	localOnly.ID = "work_local_only_delivery"
	localOnly.GitDelivery = &GitDeliveryContract{
		TargetWorktreeRoot:    repoRoot,
		RepositoryFingerprint: repositoryFingerprint,
		TargetLocalRef:        "refs/heads/main",
		ExpectedBaseSHA:       baseSHA,
		PublicationMode:       "local_only",
	}
	if err := localOnly.Validate(); err != nil {
		t.Fatalf("explicit local-only contract should validate: %v", err)
	}
}

// TestWorkPredictionRoundTripsAndLegacyMissingFieldLoads covers F2: a WorkEnvelope carrying a
// staked WorkPrediction round-trips byte-identical, and a hand-written legacy envelope JSON
// WITHOUT "prediction" loads, validates, and remains legacy-shaped (mirrors the GitDelivery
// legacy-missing-field precedent — additive optional fields need no schema_version bump).
func TestWorkPredictionRoundTripsAndLegacyMissingFieldLoads(t *testing.T) {
	w := WorkEnvelope{
		Header:           hdr(SVWork, "work_predicted"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "predicted work round-trips",
		Prediction: &WorkPrediction{
			Outcome:  "completed",
			MoveType: "rule_delta",
			Note:     "expect a clean rule-delta land",
		},
	}
	if err := w.Validate(); err != nil {
		t.Fatalf("predicted work should validate: %v", err)
	}
	if err := RoundTrip(&w); err != nil {
		t.Fatalf("predicted work round-trip: %v", err)
	}

	legacyJSON := `{"schema_version":"compound.work.v0.1","id":"work_legacy_shape","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound"},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"compound","state":"ready","success_predicate":"legacy remains valid","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`
	var legacy WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode legacy work: %v", err)
	}
	if legacy.Prediction != nil {
		t.Fatalf("legacy prediction = %+v, want nil", legacy.Prediction)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy work without prediction must remain valid: %v", err)
	}
	if strings.Contains(legacyJSON, "prediction") {
		t.Fatal("legacy fixture unexpectedly contains a prediction field; test setup error")
	}
}

// TestWorkPredictionValidation covers F3 (envelope layer): a staked prediction's outcome must be
// one of the CLOSE outcome enum (completed|failed|cancelled) -- NOT a Receipt.Outcome value like
// "no_op" or "blocked" -- and, when set, move_type must be one of envelope.MoveTypes.
func TestWorkPredictionValidation(t *testing.T) {
	base := WorkEnvelope{
		Header:           hdr(SVWork, "work_predicted_invalid"),
		Owner:            "compound",
		State:            "ready",
		SuccessPredicate: "predicate",
	}

	receiptOutcomeNotCloseOutcome := base
	receiptOutcomeNotCloseOutcome.Prediction = &WorkPrediction{Outcome: "no_op"}
	if err := receiptOutcomeNotCloseOutcome.Validate(); err == nil || !strings.Contains(err.Error(), "prediction.outcome") {
		t.Fatalf("prediction outcome=no_op (a Receipt outcome, not a close outcome) should fail validation, got %v", err)
	}

	badMoveType := base
	badMoveType.Prediction = &WorkPrediction{Outcome: "completed", MoveType: "banana"}
	if err := badMoveType.Validate(); err == nil || !strings.Contains(err.Error(), "prediction.move_type") {
		t.Fatalf("prediction move_type=banana should fail validation, got %v", err)
	}

	missingOutcome := base
	missingOutcome.Prediction = &WorkPrediction{MoveType: "rule_delta"}
	if err := missingOutcome.Validate(); err == nil || !strings.Contains(err.Error(), "prediction.outcome") {
		t.Fatalf("prediction without outcome should fail validation (required when struct present), got %v", err)
	}

	unstakedMoveType := base
	unstakedMoveType.Prediction = &WorkPrediction{Outcome: "completed"}
	if err := unstakedMoveType.Validate(); err != nil {
		t.Fatalf("prediction with only outcome staked (move_type left unstaked) should validate: %v", err)
	}
}

// TestReceiptSurpriseRoundTripsAndLegacyMissingFieldLoads covers F2 for the Receipt side: a
// Receipt carrying a Surprise round-trips byte-identical, and a legacy receipt JSON without
// "surprise" loads and validates.
func TestReceiptSurpriseRoundTripsAndLegacyMissingFieldLoads(t *testing.T) {
	rec := Receipt{
		Header:   withWork(hdr(SVReceipt, "rec_surprised"), "work_predicted"),
		Outcome:  "completed",
		MoveType: "control_plane_repair",
		StateDelta: StateDelta{
			Changed:      true,
			Artifacts:    []string{},
			Verification: []string{},
		},
		ProofRefs: []string{"check:proof"},
		Surprise: &SurpriseSignal{
			PredictedOutcome:  "completed",
			PredictedMoveType: "rule_delta",
			ActualOutcome:     "completed",
			ActualMoveType:    "control_plane_repair",
			Diverged:          true,
		},
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("receipt with surprise should validate: %v", err)
	}
	if err := RoundTrip(&rec); err != nil {
		t.Fatalf("receipt-with-surprise round-trip: %v", err)
	}

	legacyJSON := `{"schema_version":"compound.receipt.v0.1","id":"rec_legacy_shape","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound"},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"outcome":"completed","move_type":"outward_delta","state_delta":{"changed":true},"cost":{},"skills":{},"memory":{},"next":{},"proof_refs":["check:proof"]}`
	var legacy Receipt
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode legacy receipt: %v", err)
	}
	if legacy.Surprise != nil {
		t.Fatalf("legacy surprise = %+v, want nil", legacy.Surprise)
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy receipt without surprise must remain valid: %v", err)
	}
	if strings.Contains(legacyJSON, "surprise") {
		t.Fatal("legacy fixture unexpectedly contains a surprise field; test setup error")
	}
}

func TestWorkEnvelopePreFingerprintGitDeliveryIsReadableAndShapeStable(t *testing.T) {
	const legacyJSON = `{"schema_version":"compound.work.v0.1","id":"work_pre_fingerprint","created_at":"2026-07-13T22:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"codex","git_delivery":{"target_worktree_root":"/repo/product","target_local_ref":"refs/heads/main","expected_base_sha":"0123456789abcdef0123456789abcdef01234567","publication_mode":"local_only"},"state":"running","success_predicate":"legacy delivery remains readable without changing its hash","non_goals":[],"run_ids":[],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`

	var legacy WorkEnvelope
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("decode pre-fingerprint work: %v", err)
	}
	if legacy.GitDelivery == nil || legacy.GitDelivery.RepositoryFingerprint != "" {
		t.Fatalf("pre-fingerprint git_delivery = %+v", legacy.GitDelivery)
	}
	if err := legacy.Validate(); err != nil {
		t.Errorf("pre-fingerprint work must remain readable: %v", err)
	}
	remarshaled, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(remarshaled) != legacyJSON {
		t.Fatalf("pre-fingerprint work shape changed during re-marshal:\n got: %s\nwant: %s", remarshaled, legacyJSON)
	}
	if strings.Contains(string(remarshaled), "repository_fingerprint") {
		t.Fatalf("legacy re-marshal invented repository_fingerprint: %s", remarshaled)
	}
}

func TestWorkEnvelopeGitDeliveryPublicationRefMatchesWholeBranch(t *testing.T) {
	const baseSHA = "0123456789abcdef0123456789abcdef01234567"
	const repositoryFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	repoRoot := filepath.Join(string(filepath.Separator), "repo", "product")
	tests := []struct {
		name           string
		localRef       string
		publicationRef string
		wantErr        bool
	}{
		{
			name:           "wrong nested publication branch",
			localRef:       "refs/heads/main",
			publicationRef: "refs/remotes/origin/release/main",
			wantErr:        true,
		},
		{
			name:           "nested target branch",
			localRef:       "refs/heads/release/main",
			publicationRef: "refs/remotes/origin/release/main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := WorkEnvelope{
				Header:           hdr(SVWork, "work_publication_branch"),
				Owner:            "compound",
				State:            "ready",
				SuccessPredicate: "candidate reaches the exact publication branch",
				GitDelivery: &GitDeliveryContract{
					TargetWorktreeRoot:    repoRoot,
					RepositoryFingerprint: repositoryFingerprint,
					TargetLocalRef:        tt.localRef,
					ExpectedBaseSHA:       baseSHA,
					PublicationMode:       "required",
					PublicationRef:        tt.publicationRef,
				},
			}
			err := w.Validate()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "publication_ref") {
					t.Fatalf("wrong publication branch should fail validation, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("matching nested publication branch should validate: %v", err)
			}
		})
	}
}

func TestActionIntentTargetRequiredForMutatingActions(t *testing.T) {
	base := ActionIntent{
		Header:        withWork(hdr(SVActionIntent, "act_target"), "work_1"),
		Capability:    "filesystem.write",
		Action:        "write_file",
		MutationClass: "local_reversible",
		Reversibility: "easy",
	}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "target.kind") || !strings.Contains(err.Error(), "target.id or target.path") {
		t.Fatalf("mutating action without target should fail target identity, got %v", err)
	}
	withPath := base
	withPath.Target = ActionTarget{Kind: "file", Path: "docs/architecture/runtime.md"}
	if err := withPath.Validate(); err != nil {
		t.Fatalf("mutating action with path target should pass: %v", err)
	}
	withID := base
	withID.ID = "act_target2"
	withID.Target = ActionTarget{Kind: "work", ID: "work_1"}
	if err := withID.Validate(); err != nil {
		t.Fatalf("mutating action with id target should pass: %v", err)
	}
	read := base
	read.ID = "act_read"
	read.MutationClass = "read"
	read.Action = "inspect"
	read.Target = ActionTarget{}
	if err := read.Validate(); err != nil {
		t.Fatalf("read action without target should still pass: %v", err)
	}
}

func TestValidateBytesDetectsAndRejects(t *testing.T) {
	good := `{"schema_version":"compound.work.v0.1","id":"work_x","created_at":"2026-06-28T00:00:00Z","authority":{"source_class":"runtime"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"owner":"compound","state":"ready","success_predicate":"x"}`
	if kind, err := ValidateBytes([]byte(good)); err != nil || kind != "work" {
		t.Fatalf("good work: kind=%q err=%v", kind, err)
	}
	cases := map[string]string{
		"bad state enum":      `{"schema_version":"compound.work.v0.1","id":"work_x","created_at":"t","authority":{"source_class":"runtime"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"owner":"c","state":"frobnicate","success_predicate":"x"}`,
		"bad id prefix":       `{"schema_version":"compound.work.v0.1","id":"nope","created_at":"t","authority":{"source_class":"runtime"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"owner":"c","state":"ready","success_predicate":"x"}`,
		"missing required":    `{"schema_version":"compound.work.v0.1","id":"work_x","created_at":"t","authority":{"source_class":"runtime"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"state":"ready"}`,
		"bad source_class":    `{"schema_version":"compound.work.v0.1","id":"work_x","created_at":"t","authority":{"source_class":"made_up"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"owner":"c","state":"ready","success_predicate":"x"}`,
		"unknown schema":      `{"schema_version":"compound.nope.v9"}`,
		"action no work_id":   `{"schema_version":"compound.action_intent.v0.1","id":"act_x","created_at":"t","authority":{"source_class":"runtime"},"privacy":"private","risk":"tier2","uncertainty":{"level":"known"},"capability":"git.commit","mutation_class":"local_reversible","reversibility":"easy"}`,
		"obligations missing": `{"schema_version":"compound.policy_decision.v0.1","id":"pol_x","created_at":"t","authority":{"source_class":"runtime"},"privacy":"private","risk":"tier1","uncertainty":{"level":"known"},"action_intent_id":"act_x","outcome":"allow_with_obligations","obligations":[]}`,
	}
	for name, js := range cases {
		if _, err := ValidateBytes([]byte(js)); err == nil {
			t.Errorf("%s: expected rejection, got none", name)
		}
	}
}

// canonicalWorkSHA256 mirrors internal/work.workEnvelopeSHA256: the canonical work hash is taken
// over the MARSHALED struct, not the on-disk file bytes. Duplicated here (rather than imported)
// because internal/work depends on this package, not the reverse.
func canonicalWorkSHA256(t *testing.T, w WorkEnvelope) string {
	t.Helper()
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// legacyPreRunAttemptWorkJSON is a WorkEnvelope written BEFORE RunAttemptIDs existed. It is the
// exact shape whose canonical hash is already persisted in lifecycle events, leases, and terminal
// proof evidence across COMPOUND_HOME.
const legacyPreRunAttemptWorkJSON = `{"schema_version":"compound.work.v0.1","id":"work_pre_run_attempt","created_at":"2026-07-13T22:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier1","evidence_refs":[],"uncertainty":{"level":"known"},"owner":"codex","state":"running","success_predicate":"adding a reserved field must not move the canonical work hash","non_goals":[],"run_ids":["act_9f2c1d4e"],"artifact_ids":[],"verification_ids":[],"memory_candidate_ids":[]}`

// TestEmptyRunAttemptIDsIsHashNeutral is the migration guard for the RESERVED run slot. Declaring
// RunAttemptIDs must not move the canonical work hash of any envelope written before it existed,
// or every stored work_sha256 is invalidated at once. It proves neutrality by hashing the same
// envelope through the new struct and requiring a byte-exact match against the pre-field JSON --
// and then proves the guard is real by showing a POPULATED value does move the hash, so the test
// fails if `omitempty` is ever dropped from the tag.
func TestEmptyRunAttemptIDsIsHashNeutral(t *testing.T) {
	var w WorkEnvelope
	if err := json.Unmarshal([]byte(legacyPreRunAttemptWorkJSON), &w); err != nil {
		t.Fatalf("decode pre-RunAttempt work: %v", err)
	}
	if len(w.RunAttemptIDs) != 0 {
		t.Fatalf("pre-RunAttempt envelope must decode with an empty run slot, got %v", w.RunAttemptIDs)
	}
	if got, want := strings.Join(w.ActionIntentIDs, ","), "act_9f2c1d4e"; got != want {
		t.Fatalf("ActionIntentIDs = %q, want %q (legacy run_ids wire name must still bind)", got, want)
	}

	remarshaled, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if string(remarshaled) != legacyPreRunAttemptWorkJSON {
		t.Fatalf("declaring RunAttemptIDs moved the marshaled shape:\n got: %s\nwant: %s", remarshaled, legacyPreRunAttemptWorkJSON)
	}
	if strings.Contains(string(remarshaled), "run_attempt_ids") {
		t.Fatalf("empty RunAttemptIDs must stay off the wire, got: %s", remarshaled)
	}

	legacySum := sha256.Sum256([]byte(legacyPreRunAttemptWorkJSON))
	want := "sha256:" + hex.EncodeToString(legacySum[:])
	if got := canonicalWorkSHA256(t, w); got != want {
		t.Fatalf("canonical work hash moved: got %s want %s", got, want)
	}

	// The guard must be able to FAIL: a populated run slot is expected to change the hash. If this
	// does not diverge, `omitempty` is gone or the field is not marshaled at all, and the
	// neutrality assertion above proves nothing.
	populated := w
	populated.RunAttemptIDs = []string{"run_attempt_0001"}
	if got := canonicalWorkSHA256(t, populated); got == want {
		t.Fatal("populated RunAttemptIDs did not change the canonical hash; neutrality check is vacuous")
	}
}

// TestActionIntentIDsKeepsLegacyRunIDsWireName pins the deliberate misnomer. The Go field is
// ActionIntentIDs because it holds act_* ids, but the wire key stays `run_ids` until the
// compound.work.v0.2 bump: renaming it changes sha256(json.Marshal(WorkEnvelope)) and invalidates
// every work_sha256 already persisted in events, leases, and terminal proof evidence (measured:
// 120/121 real envelopes match today, 0/121 under a renamed key). This test fails on a premature
// rename.
func TestActionIntentIDsKeepsLegacyRunIDsWireName(t *testing.T) {
	data, err := json.Marshal(WorkEnvelope{ActionIntentIDs: []string{"act_abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"run_ids":["act_abc123"]`) {
		t.Fatalf("ActionIntentIDs must marshal under the legacy run_ids key, got: %s", data)
	}
	if strings.Contains(string(data), "action_intent_ids") {
		t.Fatalf("wire key renamed ahead of the v0.2 schema bump; this breaks stored work_sha256: %s", data)
	}
}

// TestRunAttemptIDsRejectEmptyEntries isolates the non-emptiness rule from the prefix rule
// (pinned separately by TestRunAttemptIDsRequireRattPrefix): the first entry is a valid ratt_
// id, so only the empty second entry fails.
func TestRunAttemptIDsRejectEmptyEntries(t *testing.T) {
	var w WorkEnvelope
	if err := json.Unmarshal([]byte(legacyPreRunAttemptWorkJSON), &w); err != nil {
		t.Fatal(err)
	}
	w.RunAttemptIDs = []string{"ratt_20260720T000000Z_aabbccdd", "   "}
	err := w.Validate()
	if err == nil || !strings.Contains(err.Error(), "run_attempt_ids[1]") {
		t.Fatalf("empty run attempt id must be rejected, got: %v", err)
	}
}

// TestRunAttemptIDsRequireRattPrefix pins the RunAttempt id shape settled in S1.
// RunAttempt ids minted by internal/runattempt follow `ratt_<UTC>_<8 hex>`. Any other value
// (legacy `prun_*` panel ids, `run_*` observation ids, free strings) must be rejected at the
// envelope boundary so the slot cannot quietly accumulate non-RunAttempt evidence again --
// that is exactly the bug the v0.1 ActionIntentIDs mix allowed.
//
// Falsifier: removing the prefix check from validateRunAttemptIDs turns this RED with
// "must be rejected" -- verified 2026-07-20.
func TestRunAttemptIDsRequireRattPrefix(t *testing.T) {
	mk := func(ids []string) *WorkEnvelope {
		return &WorkEnvelope{
			Header:           Header{SchemaVersion: SVWork, Producer: Producer{Kind: "cli", Name: "compound", Version: ""}, ID: "work_ratt_prefix_test", CreatedAt: "2026-07-20T00:00:00Z", Authority: Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1"},
			Owner:            "codex",
			SuccessPredicate: "RunAttempt id shape is enforced",
			State:            "ready",
			RunAttemptIDs:    ids,
		}
	}
	for _, badID := range []string{
		"prun_20260718T100000Z_aaaaaaaaaaaa", // legacy panel run id
		"run_packet",                         // legacy observation id
		"run_real",                           // legacy writeback id
		"free_string",                        // arbitrary
		"RATT_uppercase_is_wrong",            // prefix is lowercase
	} {
		err := mk([]string{badID}).Validate()
		if err == nil {
			t.Fatalf("RunAttemptIDs=[%q] must be rejected: validator accepted a non-ratt_ id", badID)
		}
		if !strings.Contains(err.Error(), "run_attempt_ids[0]") {
			t.Fatalf("RunAttemptIDs=[%q] rejection must name run_attempt_ids[0], got: %v", badID, err)
		}
	}
	// Happy path: a real ratt_ id round-trips.
	if err := mk([]string{"ratt_20260720T000000Z_aabbccdd"}).Validate(); err != nil {
		t.Fatalf("well-formed ratt_ id was rejected: %v", err)
	}
}

// validateHeader is a test helper: runs the private Header.validate() and returns the
// public-style error. Same-package access lets us touch the unexported method. Empty
// idPrefix means the ID-prefix check is skipped (these tests focus on HostBindings).
func validateHeader(h Header) error {
	var v verr
	h.validate(&v, SVWork, "")
	return v.result("Header")
}

// TestHeader_HostBindingsOptionalAndWellFormed: HostBindings is additive. A Header without
// Host field still validates (legacy v0.1 callers stay green). A Header with well-formed
// Host fields also validates. JSON round-trip preserves all three sub-fields.
func TestHeader_HostBindingsOptionalAndWellFormed(t *testing.T) {
	// Case 1: legacy Header without Host — must validate.
	legacy := hdr(SVWork, "work_legacy_no_host")
	if err := validateHeader(legacy); err != nil {
		t.Fatalf("legacy Header (no Host) must validate, got: %v", err)
	}
	if legacy.Host != nil {
		t.Fatalf("legacy Header Host must be nil, got: %+v", legacy.Host)
	}

	// Case 2: Header with all three Host fields populated — must validate.
	withHost := hdr(SVWork, "work_with_host")
	withHost.Host = &HostBindings{
		HostID:   "2e6424e0-e1d8-40b9-abf5-af5454502fb0",
		Hostname: "fixture-host.example",
		DeviceID: "aw-watcher-window",
	}
	if err := validateHeader(withHost); err != nil {
		t.Fatalf("well-formed HostBindings must validate, got: %v", err)
	}

	// Case 3: JSON round-trip preserves HostBindings fields.
	b, err := json.Marshal(withHost)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"host_bindings":{`) {
		t.Fatalf("marshaled JSON must contain host_bindings key, got: %s", b)
	}
	if !strings.Contains(string(b), `"host_id":"2e6424e0-e1d8-40b9-abf5-af5454502fb0"`) {
		t.Fatalf("marshaled JSON must contain host_id, got: %s", b)
	}

	var roundTripped Header
	if err := json.Unmarshal(b, &roundTripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTripped.Host == nil || *roundTripped.Host != *withHost.Host {
		t.Fatalf("round-trip mismatch:\n  sent:     %+v\n  received: %+v", withHost.Host, roundTripped.Host)
	}

	// Case 4: legacy Header (Host == nil) marshals WITHOUT host_bindings key — preserves
	// the byte-identity contract that existing wire-format consumers depend on.
	bLegacy, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if strings.Contains(string(bLegacy), `"host_bindings"`) {
		t.Fatalf("legacy Header (Host nil) must omit host_bindings, got: %s", bLegacy)
	}
}

// TestHeader_HostBindingsMalformedRejected: any non-empty HostBindings field must match
// the hostname-safe pattern. Slash, equals, semicolon, control bytes, whitespace, and
// over-length values are all rejected.
func TestHeader_HostBindingsMalformedRejected(t *testing.T) {
	cases := []struct {
		name  string
		host  HostBindings
		wants string // substring expected in error message
	}{
		{"slash_in_hostname", HostBindings{Hostname: "evil/host"}, "hostname"},
		{"equals_in_host_id", HostBindings{HostID: "x=y"}, "host_id"},
		{"semicolon_in_device_id", HostBindings{DeviceID: "a;b"}, "device_id"},
		{"space_in_hostname", HostBindings{Hostname: "bad host"}, "hostname"},
		{"control_byte_in_host_id", HostBindings{HostID: "abc\x00def"}, "host_id"},
		{"too_long_hostname", HostBindings{Hostname: strings.Repeat("a", 254)}, "hostname"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hdr(SVWork, "work_"+tc.name)
			h.Host = &tc.host
			err := validateHeader(h)
			if err == nil {
				t.Fatalf("malformed HostBindings %+v must reject, but validator accepted", tc.host)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("rejection must name %q field, got: %v", tc.wants, err)
			}
		})
	}
}

// TestHeader_HostBindingsBoundaryLength: exactly 253 chars is allowed; 254 is not.
// Pinning the boundary so a future refactor doesn't silently shift it.
func TestHeader_HostBindingsBoundaryLength(t *testing.T) {
	maxAllowed := strings.Repeat("a", 253)
	tooLong := strings.Repeat("a", 254)
	if err := (HostBindings{Hostname: maxAllowed}).Validate(); err != nil {
		t.Fatalf("253-char hostname must validate, got: %v", err)
	}
	if err := (HostBindings{Hostname: tooLong}).Validate(); err == nil {
		t.Fatalf("254-char hostname must reject")
	}
}

// The ephemeral work_live_ namespace is for unattributed live-hook mutations;
// a durable WorkEnvelope must never occupy it (reviewqueue, sourcecontrol, and
// decisions already refuse it as non-durable).
func TestWorkEnvelopeValidateRefusesLiveNamespace(t *testing.T) {
	w := WorkEnvelope{Header: hdr(SVWork, "work_live_abc123"), Owner: "compound", State: "ready", SuccessPredicate: "a state delta exists"}
	err := w.Validate()
	if err == nil || !strings.Contains(err.Error(), "work_live_") {
		t.Fatalf("work_live_ id must fail validation, got %v", err)
	}
	w.ID = "work_durable_abc123"
	if err := w.Validate(); err != nil {
		t.Fatalf("durable work id must validate: %v", err)
	}
}

// Present ContextPack sections are shape-checked: a hand-written pack carrying
// an empty-ref position, negative, lease, or body must not validate.
func TestContextPackValidateChecksPresentSections(t *testing.T) {
	pack := ContextPack{
		Header:          hdr(SVContextPack, "ctx_sections"),
		TaskContract:    TaskContract{Intent: "shape-check sections", SuccessPredicate: "sections shape-checked"},
		Included:        []IncludedRef{},
		Excluded:        []ExcludedRef{},
		ActiveDecisions: []string{},
		RequiredChecks:  []string{},
		Avoid:           []string{},
		ActivePositions: []ActivePosition{{Ref: "pos_1", Entity: "e", Stance: "holds"}},
		Negatives:       []NegativeCard{{ApproachKey: "a", Ref: "neg_1"}},
		Peers:           &PeersSection{ActiveLeases: []PeerLease{{WorkID: "work_1", Holder: "codex"}}, Lanes: []PeerLaneCard{}},
		BodyPack:        &BodyPackSection{Bodies: []BodyPackItem{{Ref: "claim_1", SHA256: "sha256:x", Status: "current"}}, Omitted: []BodyPackOmission{{Ref: "claim_2", Reason: "budget_exhausted"}}},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("a well-formed pack with all sections present must validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ContextPack)
		want   string
	}{
		{"empty position ref", func(p *ContextPack) { p.ActivePositions[0].Ref = "" }, "active_positions[0].ref"},
		{"empty negative approach key", func(p *ContextPack) { p.Negatives[0].ApproachKey = "" }, "negatives[0].approach_key"},
		{"empty lease work id", func(p *ContextPack) { p.Peers.ActiveLeases[0].WorkID = "" }, "peers.active_leases[0].work_id"},
		{"empty lease holder", func(p *ContextPack) { p.Peers.ActiveLeases[0].Holder = "" }, "peers.active_leases[0].holder"},
		{"empty body ref", func(p *ContextPack) { p.BodyPack.Bodies[0].Ref = "" }, "body_pack.bodies[0].ref"},
		{"empty body sha", func(p *ContextPack) { p.BodyPack.Bodies[0].SHA256 = "" }, "body_pack.bodies[0].sha256"},
		{"empty omission reason", func(p *ContextPack) { p.BodyPack.Omitted[0].Reason = "" }, "body_pack.omitted[0].reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := pack
			tc.mutate(&bad)
			err := bad.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}

func TestHookEventCapabilityUseValidation(t *testing.T) {
	valid := HookEvent{
		Header:      hdr(SVHookEvent, "hook_skill_use"),
		Harness:     "claude_code",
		HookName:    "PostToolUse",
		SideEffects: []string{"Skill"},
		Status:      "ok",
		CapabilityUse: &CapabilityUse{
			Ref:     "capability://skill:diagnosing-bugs",
			Skill:   "diagnosing-bugs",
			Trigger: "fix flaky regression",
			Lane:    "claude_code",
			Outcome: "used",
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid capability use rejected: %v", err)
	}

	wrongHook := valid
	wrongHook.HookName = "SessionStart"
	if err := wrongHook.Validate(); err == nil || !strings.Contains(err.Error(), "PostToolUse") {
		t.Fatalf("capability use on non-PostToolUse hook must reject, got %v", err)
	}
	mismatched := valid
	mismatchUse := *valid.CapabilityUse
	mismatchUse.Ref = "capability://skill:tdd"
	mismatched.CapabilityUse = &mismatchUse
	if err := mismatched.Validate(); err == nil || !strings.Contains(err.Error(), "capability_use.ref") {
		t.Fatalf("mismatched skill ref must reject, got %v", err)
	}
	malformed := valid
	malformedUse := *valid.CapabilityUse
	malformedUse.Skill = "../private"
	malformedUse.Ref = "capability://skill:../private"
	malformed.CapabilityUse = &malformedUse
	if err := malformed.Validate(); err == nil || !strings.Contains(err.Error(), "skill identifier") {
		t.Fatalf("path-like skill identity must reject, got %v", err)
	}
	oversized := valid
	oversizedUse := *valid.CapabilityUse
	oversizedUse.Trigger = strings.Repeat("x", 513)
	oversized.CapabilityUse = &oversizedUse
	if err := oversized.Validate(); err == nil || !strings.Contains(err.Error(), "512 bytes") {
		t.Fatalf("oversized trigger must reject, got %v", err)
	}
}
