package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/work"
)

// consumptionEnv is a home + work store wired for the session-end consumption detector.
func consumptionEnv(t *testing.T) (Deps, string, *work.Store) {
	t.Helper()
	home := t.TempDir()
	store, err := work.Open(home)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	deps := baseDeps()
	deps.HomePath = home
	deps.Store = store
	return deps, home, store
}

// writeConsumePack writes a valid ContextPack whose included[] carries the given claim:// refs.
func writeConsumePack(t *testing.T, home, packID string, claimRefs ...string) {
	t.Helper()
	included := make([]envelope.IncludedRef, 0, len(claimRefs))
	for _, ref := range claimRefs {
		included = append(included, envelope.IncludedRef{Ref: ref, SourceClass: "source", Reason: "recency", Confidence: 1, Status: "current"})
	}
	pack := envelope.ContextPack{
		Header: envelope.Header{
			SchemaVersion: envelope.SVContextPack,
			ID:            packID,
			CreatedAt:     fixedNow.Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "test", Name: "hook"},
			Authority:     envelope.Authority{SourceClass: "generated", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		SummaryMD:          "consume pack",
		Included:           included,
		Excluded:           []envelope.ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("context pack fixture invalid: %v", err)
	}
	path := filepath.Join(home, "context", "packs", packID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(pack, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// closeableWork creates a work bound to packID and advances it to a closeable state.
func closeableWork(t *testing.T, store *work.Store, packID string) string {
	t.Helper()
	w, err := store.Create(work.CreateInput{Owner: "o", SuccessPredicate: "prove consumption " + packID, ContextPackID: packID}, fixedNow)
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if _, err := store.Claim(w.ID, "claude_code", fixedNow); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Advance(w.ID, work.AdvanceInput{To: "running", Actor: "claude_code"}, fixedNow); err != nil {
		t.Fatalf("advance running: %v", err)
	}
	return w.ID
}

func writeTranscript(t *testing.T, home, body string) string {
	t.Helper()
	path := filepath.Join(home, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Falsifier 1: a transcript that references an injected claim id as a bare token AFTER its
// scheme-qualified injection echo lands that exact claim:// ref in the receipt's Skills.Used,
// with used_declared=true. Without the detector this reds (Used carries no claim ref).
func TestSessionEndDetectsConsumedInjectedClaim(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const claimRef = "claim://claim_consumed01"
	writeConsumePack(t, home, "ctx_consumed", claimRef)
	wid := closeableWork(t, store, "ctx_consumed")

	// Injection echo (scheme-qualified) THEN a genuine bare reference by the assistant.
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claimRef + "`" + ` recency"}` + "\n" +
		`{"role":"assistant","content":"I applied claim_consumed01 to the pricing change."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("detector ran => used_declared must be true, got %+v", rec.Skills)
	}
	if !containsString(rec.Skills.Referenced, claimRef) {
		t.Fatalf("consumed claim ref missing from receipt used: %v", rec.Skills.Referenced)
	}
}

// Falsifier 2: a claim id appearing ONLY in its scheme-qualified injection form (self-echo) is
// NOT consumed — the injection block is delimited by FORM (claim://<id>), so echo earns no credit.
// used_declared is still true (the detector ran and honestly found nothing).
func TestSessionEndInjectionEchoDoesNotCountAsConsumed(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const claimRef = "claim://claim_echoonly01"
	writeConsumePack(t, home, "ctx_echo", claimRef)
	wid := closeableWork(t, store, "ctx_echo")

	// The id appears ONLY inside the scheme-qualified injected ref — never bare.
	transcript := "" +
		`{"role":"system","content":"# Compound Context Injection\n- ` + "`" + claimRef + "`" + ` recency\n- other line"}` + "\n" +
		`{"role":"assistant","content":"I worked on the pricing change with no memory reference."}` + "\n"
	path := writeTranscript(t, home, transcript)

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.ObservationQuality != "readable" {
		t.Fatalf("detector ran => used_declared must be true even when nothing consumed, got %+v", rec.Skills)
	}
	if containsString(rec.Skills.Referenced, claimRef) {
		t.Fatalf("injection self-echo must not count as consumed: %v", rec.Skills.Referenced)
	}
}

// Falsifier 3: an absent/unreadable transcript is fail-open — the receipt still lands with
// used_declared=false and zero consumption refs, and the session close is NEVER blocked.
func TestSessionEndAbsentTranscriptFailsOpen(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const claimRef = "claim://claim_absent01"
	writeConsumePack(t, home, "ctx_absent", claimRef)
	wid := closeableWork(t, store, "ctx_absent")

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: filepath.Join(home, "does-not-exist.jsonl"),
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("absent transcript must not block session end: %v", err)
	}
	if got := kinds(res.Produced); len(got) != 2 || got[0] != "receipt" {
		t.Fatalf("produced = %v, want a receipt to still land", got)
	}
	rec := res.Produced[0].(*envelope.Receipt)
	if rec.Skills.UsedDeclared {
		t.Fatalf("unreadable transcript => used_declared must be false (fail-open), got %+v", rec.Skills)
	}
	if containsString(rec.Skills.Referenced, claimRef) {
		t.Fatalf("fail-open close must carry no consumption refs: %v", rec.Skills.Referenced)
	}
	closed, err := store.Get(wid)
	if err != nil {
		t.Fatalf("get closed work: %v", err)
	}
	if closed.State != "completed" {
		t.Fatalf("work must still close on fail-open, got state %q", closed.State)
	}
}

// Falsifier 6 (event-door depth): the session-end receipt reaches COMPOUND_HOME/events/ (and is
// therefore visible to the capgraph receipt sweep), and NO receipt is written to any other sink.
// A regression that stopped routing the close receipt through the event door reds this.
func TestSessionEndReceiptLandsOnlyInEventDoor(t *testing.T) {
	deps, home, store := consumptionEnv(t)
	const claimRef = "claim://claim_eventdoor01"
	writeConsumePack(t, home, "ctx_eventdoor", claimRef)
	wid := closeableWork(t, store, "ctx_eventdoor")
	path := writeTranscript(t, home, `{"role":"assistant","content":"used claim_eventdoor01 here"}`+"\n")

	res, err := Run(Fixture{
		Hook: "session-end", Harness: "claude_code", WorkID: wid,
		MoveType: "outward_delta", Outcome: "completed", Summary: "shipped",
		TranscriptPath: path,
	}, deps, fixedNow)
	if err != nil {
		t.Fatalf("session-end run: %v", err)
	}
	rec := res.Produced[0].(*envelope.Receipt)

	// The receipt landed in the event door.
	if !hookEventRecordExists(t, home, rec.ID) {
		t.Fatal("receipt did not reach the event door")
	}
	// No receipt-kind file exists anywhere OUTSIDE events/ (no legacy/double-write sink).
	if err := filepath.Walk(home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if strings.HasPrefix(filepath.Base(p), "rec_") {
			rel, _ := filepath.Rel(home, p)
			if dir := filepath.Dir(rel); dir != "events" {
				t.Fatalf("receipt %s written outside the event door (dir=%s)", filepath.Base(p), dir)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk home: %v", err)
	}
	// Proof it reached the door the projection reads: the capgraph sweep sees it and scores
	// the consumed claim ref.
	proj, err := capgraph.BuildCapabilityUsage(home, capgraph.UsageBuildOptions{GeneratedAt: fixedNow})
	if err != nil {
		t.Fatalf("build usage: %v", err)
	}
	if proj.ReceiptsScanned < 1 {
		t.Fatalf("event-door receipt not swept by usage projection: scanned=%d", proj.ReceiptsScanned)
	}
	var found *capgraph.InjectedRefUsage
	for i := range proj.InjectedRefs {
		if proj.InjectedRefs[i].Ref == claimRef {
			found = &proj.InjectedRefs[i]
		}
	}
	if found == nil || found.ReferencedCount != 1 || found.ConsumedCount != 0 {
		t.Fatalf("usage projection did not score consumed via the event door: %+v", found)
	}
}
