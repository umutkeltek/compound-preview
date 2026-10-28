package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/candidates"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/mem"
)

// seedInjectionClaim promotes one fictional claim whose entity+object share tokens
// with the prompt task so the live retrieval path selects it. Returns "claim://<id>".
func seedInjectionClaim(t *testing.T, home, entity, object string) string {
	t.Helper()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]mem.Candidate{{
		Entity:         entity,
		Predicate:      "authority_status",
		Object:         object,
		Confidence:     0.99,
		Derivation:     "deterministic",
		ClaimType:      "architecture",
		PromotionBasis: candidates.BasisRuntimeProof,
		SourceTraceRef: "trace_" + entity,
		SnippetHash:    "sha256:" + entity,
		Visibility:     "operational",
	}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 1 {
		t.Fatalf("seed %s: captured %d candidates, want 1", entity, len(captured))
	}
	res, err := store.Adjudicate(captured[0].ID, candidates.Promote, "test", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Claim == nil {
		t.Fatalf("seed %s: claim not promoted", entity)
	}
	return "claim://" + res.Claim.ID
}

func livePromptPack(t *testing.T, fx Fixture, deps Deps, now time.Time) *envelope.ContextPack {
	t.Helper()
	res, err := Run(fx, deps, now)
	if err != nil {
		t.Fatalf("run live prompt: %v", err)
	}
	if len(res.Produced) < 3 {
		t.Fatalf("produced %v, want a context_pack", kinds(res.Produced))
	}
	pack, ok := res.Produced[2].(*envelope.ContextPack)
	if !ok {
		t.Fatalf("produced[2] = %T, want *ContextPack", res.Produced[2])
	}
	return pack
}

func injExcludedReasonFor(pack *envelope.ContextPack, ref string) (string, bool) {
	for _, e := range pack.Excluded {
		if e.Ref == ref {
			return e.Reason, true
		}
	}
	return "", false
}

func injIncludedHas(pack *envelope.ContextPack, ref string) bool {
	for _, inc := range pack.Included {
		if inc.Ref == ref {
			return true
		}
	}
	return false
}

// TestLiveInjectionDeltaSuppressesRepeatWithExcludedRecord: the same prompt twice in
// one session injects the memory ref once, then suppresses it with an honest Excluded
// entry — the whole point of the delta.
func TestLiveInjectionDeltaSuppressesRepeatWithExcludedRecord(t *testing.T) {
	home := t.TempDir()
	ref := seedInjectionClaim(t, home, "alpha-widget", "alpha widget context should prefer concise progress updates")
	deps := baseDeps()
	deps.HomePath = home
	fx := Fixture{
		Hook: "user-prompt-submit", Harness: "codex", Live: true,
		Cwd: "/repo/alpha", RepoKind: "control-plane", SessionID: "sess_delta_repeat",
		Task:   "alpha widget context should prefer concise progress updates",
		Prompt: "alpha widget context should prefer concise progress updates",
	}

	first := livePromptPack(t, fx, deps, fixedNow)
	if !containsString(first.ActiveMemory, ref) {
		t.Fatalf("first prompt should inject %s; active=%v", ref, first.ActiveMemory)
	}
	if _, excluded := injExcludedReasonFor(first, ref); excluded {
		t.Fatalf("first prompt must not suppress %s; excluded=%v", ref, first.Excluded)
	}

	second := livePromptPack(t, fx, deps, fixedNow.Add(time.Minute))
	if containsString(second.ActiveMemory, ref) {
		t.Fatalf("second prompt should suppress %s from active memory; active=%v", ref, second.ActiveMemory)
	}
	if injIncludedHas(second, ref) {
		t.Fatalf("second prompt should drop %s from included; included=%v", ref, second.Included)
	}
	reason, excluded := injExcludedReasonFor(second, ref)
	if !excluded || reason != injectionSuppressReason {
		t.Fatalf("second prompt should record excluded %q for %s; excluded=%v", injectionSuppressReason, ref, second.Excluded)
	}
}

// TestLiveInjectionDeltaInjectsNewEntityMidSession: a ref that first appears
// mid-session injects, while the earlier ref stays suppressed — proving the delta
// tracks a set, not a single high-water mark.
func TestLiveInjectionDeltaInjectsNewEntityMidSession(t *testing.T) {
	home := t.TempDir()
	alphaRef := seedInjectionClaim(t, home, "alpha-widget", "alpha widget authority is fixed")
	deps := baseDeps()
	deps.HomePath = home
	base := Fixture{
		Hook: "user-prompt-submit", Harness: "codex", Live: true,
		Cwd: "/repo/alpha", RepoKind: "control-plane", SessionID: "sess_delta_new",
	}

	firstFX := base
	firstFX.Task = "alpha widget authority"
	firstFX.Prompt = firstFX.Task
	first := livePromptPack(t, firstFX, deps, fixedNow)
	if !containsString(first.ActiveMemory, alphaRef) {
		t.Fatalf("first prompt should inject %s; active=%v", alphaRef, first.ActiveMemory)
	}

	// A new entity becomes promotable mid-session.
	bravoRef := seedInjectionClaim(t, home, "bravo-gadget", "bravo gadget authority is fixed")

	secondFX := base
	secondFX.Task = "alpha widget authority and bravo gadget authority"
	secondFX.Prompt = secondFX.Task
	second := livePromptPack(t, secondFX, deps, fixedNow.Add(time.Minute))

	if !containsString(second.ActiveMemory, bravoRef) {
		t.Fatalf("second prompt should inject NEW ref %s; active=%v", bravoRef, second.ActiveMemory)
	}
	if containsString(second.ActiveMemory, alphaRef) {
		t.Fatalf("second prompt should still suppress already-injected %s; active=%v", alphaRef, second.ActiveMemory)
	}
	if reason, excluded := injExcludedReasonFor(second, alphaRef); !excluded || reason != injectionSuppressReason {
		t.Fatalf("second prompt should record excluded suppression for %s; excluded=%v", alphaRef, second.Excluded)
	}
}

// TestLiveInjectionDeltaCorruptStateFailsOpenByteForByte pins requirement #4: a
// corrupt state file yields the full-injection pack byte-for-byte (mirrors
// TestPackWithCapabilitiesWithoutProbeTriggerMatchesFlatPackByteForByte's approach).
func TestLiveInjectionDeltaCorruptStateFailsOpenByteForByte(t *testing.T) {
	const object = "alpha widget context should prefer concise progress updates"
	base := Fixture{
		Hook: "user-prompt-submit", Harness: "codex", Live: true,
		Cwd: "/repo/alpha", RepoKind: "control-plane",
		Task:   object,
		Prompt: object,
	}

	// Two homes seeded identically. Claim ids are content-derived (idFor over
	// entity/predicate/object/seq), so both homes mint the same claim:// ref and the
	// packs are byte-for-byte comparable — without a hook-event id collision from two
	// runs sharing one home (the event id hash omits session_id).
	baseHome := t.TempDir()
	seedInjectionClaim(t, baseHome, "alpha-widget", object)
	baselineDeps := baseDeps()
	baselineDeps.HomePath = baseHome
	// Baseline: no session id → delta disabled → raw full-injection pack.
	baseline := livePromptPack(t, base, baselineDeps, fixedNow)

	corruptHome := t.TempDir()
	seedInjectionClaim(t, corruptHome, "alpha-widget", object)
	corruptDeps := baseDeps()
	corruptDeps.HomePath = corruptHome
	sess := "sess_delta_corrupt"
	if err := os.MkdirAll(filepath.Join(corruptHome, "observed", "injection-sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(injectionSessionStatePath(corruptHome, sess), []byte("{ this is not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	corruptFX := base
	corruptFX.SessionID = sess
	corrupt := livePromptPack(t, corruptFX, corruptDeps, fixedNow)

	gotJSON, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("corrupt-state pack diverged from full-injection pack\ngot:  %s\nwant: %s", gotJSON, wantJSON)
	}

	// The corrupt file self-heals to a valid record so the next prompt can suppress.
	if _, status := readInjectionSessionState(corruptHome, sess); status != injectionStateOK {
		t.Fatalf("corrupt state should self-heal to OK, got status %d", status)
	}
}

// TestApplyInjectionDeltaReopensOnContestedFlip drives requirement #3 trigger (1)
// directly: a position injected while calm re-opens once it turns contested, while a
// sibling calm claim stays suppressed.
func TestApplyInjectionDeltaReopensOnContestedFlip(t *testing.T) {
	home := t.TempDir()
	sess := "sess_flip"
	posRef := "position://project:demo::stance"
	claimRef := "claim://claim_calm"

	in := contract.Input{RepoKind: "control-plane"}
	// Current pack: position is NOW contested; claim unchanged.
	pack := envelope.ContextPack{
		Included: []envelope.IncludedRef{
			{Ref: claimRef, SourceClass: "runtime", Reason: "claim", Confidence: 1, Status: "current"},
			{Ref: posRef, SourceClass: "runtime", Reason: "position", Confidence: 1, Status: "current"},
		},
		Excluded:        []envelope.ExcludedRef{},
		ActiveMemory:    []string{claimRef, posRef},
		ActivePositions: []envelope.ActivePosition{{Ref: posRef, Entity: "project:demo", Topic: "topic:demo", Stance: "contested-now", Contested: true, Line: "demo"}},
	}

	// Prior state: both injected, position recorded as NOT contested, classification
	// pinned to the current pack so suppression is eligible.
	writeInjectionSessionState(home, sess, injectionSessionState{
		Schema:         injectionSessionSchema,
		SessionKey:     hashStr(sess),
		Classification: injectionClassificationKey(pack, in),
		Injected: map[string]injectedRefRecord{
			posRef:   {Kind: "position", Contested: false},
			claimRef: {Kind: "claim"},
		},
		UpdatedAt: fixedNow.Format(time.RFC3339Nano),
	})

	out := applyInjectionDelta(pack, in, home, sess, fixedNow)

	if !containsString(out.ActiveMemory, posRef) {
		t.Fatalf("contested-flip position should re-open; active=%v", out.ActiveMemory)
	}
	if _, excluded := injExcludedReasonFor(&out, posRef); excluded {
		t.Fatalf("re-opened position must not be excluded; excluded=%v", out.Excluded)
	}
	if containsString(out.ActiveMemory, claimRef) {
		t.Fatalf("calm claim should stay suppressed; active=%v", out.ActiveMemory)
	}
	if reason, excluded := injExcludedReasonFor(&out, claimRef); !excluded || reason != injectionSuppressReason {
		t.Fatalf("calm claim should record excluded suppression; excluded=%v", out.Excluded)
	}

	// The re-opened position now records contested=true so it does not re-open forever.
	healed, status := readInjectionSessionState(home, sess)
	if status != injectionStateOK || !healed.Injected[posRef].Contested {
		t.Fatalf("re-opened position should record contested=true; state=%+v status=%d", healed.Injected[posRef], status)
	}
}

// TestApplyInjectionDeltaReopensOnClassificationChange drives requirement #3 trigger
// (2): when the classification changed since the last prompt, previously-injected refs
// re-open (full injection) rather than being suppressed.
func TestApplyInjectionDeltaReopensOnClassificationChange(t *testing.T) {
	home := t.TempDir()
	sess := "sess_reclassify"
	claimRef := "claim://claim_reclassify"

	in := contract.Input{RepoKind: "control-plane", Topic: "topic:new-seed"}
	pack := envelope.ContextPack{
		Included:     []envelope.IncludedRef{{Ref: claimRef, SourceClass: "runtime", Reason: "claim", Confidence: 1, Status: "current"}},
		Excluded:     []envelope.ExcludedRef{},
		ActiveMemory: []string{claimRef},
	}

	// Prior state injected the ref under a DIFFERENT (stale) classification.
	writeInjectionSessionState(home, sess, injectionSessionState{
		Schema:         injectionSessionSchema,
		SessionKey:     hashStr(sess),
		Classification: "sha256:stale-classification",
		Injected:       map[string]injectedRefRecord{claimRef: {Kind: "claim"}},
		UpdatedAt:      fixedNow.Format(time.RFC3339Nano),
	})

	out := applyInjectionDelta(pack, in, home, sess, fixedNow)

	if !containsString(out.ActiveMemory, claimRef) {
		t.Fatalf("classification change should re-open %s; active=%v", claimRef, out.ActiveMemory)
	}
	if _, excluded := injExcludedReasonFor(&out, claimRef); excluded {
		t.Fatalf("re-opened ref must not be excluded on classification change; excluded=%v", out.Excluded)
	}
	// The new classification is recorded so the NEXT prompt can suppress again.
	healed, status := readInjectionSessionState(home, sess)
	if status != injectionStateOK || healed.Classification != injectionClassificationKey(pack, in) {
		t.Fatalf("classification should be refreshed; got %q status %d", healed.Classification, status)
	}
}

// TestApplyInjectionDeltaFirstPromptIsFullInjection: an absent state file (first
// prompt of a session) injects everything and leaves the pack untouched.
func TestApplyInjectionDeltaFirstPromptIsFullInjection(t *testing.T) {
	home := t.TempDir()
	sess := "sess_first"
	claimRef := "claim://claim_first"
	in := contract.Input{RepoKind: "control-plane"}
	pack := envelope.ContextPack{
		Included:     []envelope.IncludedRef{{Ref: claimRef, SourceClass: "runtime", Reason: "claim", Confidence: 1, Status: "current"}},
		Excluded:     []envelope.ExcludedRef{},
		ActiveMemory: []string{claimRef},
	}

	out := applyInjectionDelta(pack, in, home, sess, fixedNow)

	if !containsString(out.ActiveMemory, claimRef) || len(out.Excluded) != 0 {
		t.Fatalf("first prompt should fully inject with no suppression; active=%v excluded=%v", out.ActiveMemory, out.Excluded)
	}
	if _, status := readInjectionSessionState(home, sess); status != injectionStateOK {
		t.Fatalf("first prompt should persist a fresh session state; status=%d", status)
	}
}
