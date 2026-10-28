package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"compound/internal/persist"
)

// testDSN returns the test database DSN or skips the test.
// Set COMPOUND_TEST_DSN to run integration tests:
//
//	COMPOUND_TEST_DSN="postgres://<user>:<password>@<host>:<port>/<database>?sslmode=disable" go test ./internal/persist/pg/ -v
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("COMPOUND_TEST_DSN")
	if dsn == "" {
		t.Skip("COMPOUND_TEST_DSN not set; skipping integration test")
	}
	return dsn
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := persist.Config{
		Backend:              persist.BackendPostgres,
		DSN:                  dsn,
		ExperimentalPostgres: true,
		PostgresProfile:      "integration-test",
		VectorDimensions:     8, // small dims for fast tests
	}
	store, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.EnsureSchema(ctx); err != nil {
		store.Close()
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// ─── Connection + Schema ─────────────────────────────────────────────────────────

func TestOpenAndEnsureSchema(t *testing.T) {
	store := testStore(t)
	// EnsureSchema is idempotent — calling twice should not error
	ctx := context.Background()
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema (2nd call): %v", err)
	}
}

func TestTableCounts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	counts := store.TableCounts(ctx)
	// All expected tables should be present
	expected := []string{"claims", "edges", "candidates", "events", "work_log", "work_envelopes", "vectors"}
	for _, name := range expected {
		if _, ok := counts[name]; !ok {
			t.Errorf("TableCounts missing table %q", name)
		}
	}
}

// ─── Claims ──────────────────────────────────────────────────────────────────────

func TestAppendClaimAndFold(t *testing.T) {
	store := testStore(t)
	now := time.Now()

	claim := persist.Claim{
		ID:         fmt.Sprintf("test-claim-%d", now.UnixNano()),
		Entity:     "test::entity",
		Predicate:  "color",
		Object:     "blue",
		Confidence: 0.95,
		Derivation: "extracted",
		Visibility: "operational",
		State:      "promoted",
	}

	seq, err := store.AppendClaim(claim, now)
	if err != nil {
		t.Fatalf("AppendClaim: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("AppendClaim returned non-positive seq: %d", seq)
	}

	// Fold back
	claims, err := store.FoldClaims(persist.ClaimFilter{
		Entity: "test::entity",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("FoldClaims: %v", err)
	}
	if len(claims) == 0 {
		t.Fatal("FoldClaims returned 0 claims")
	}

	found := false
	for _, c := range claims {
		if c.ID == claim.ID && c.Object == "blue" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("FoldClaims did not return the appended claim (id=%s)", claim.ID)
	}
}

func TestFoldClaimsFilters(t *testing.T) {
	store := testStore(t)
	now := time.Now()

	// Append two claims with different topics (Topic maps to predicate in fold)
	c1 := persist.Claim{
		ID: fmt.Sprintf("filter-a-%d", now.UnixNano()), Entity: "test::filters",
		Predicate: "size", Object: "large", Confidence: 0.8,
		Derivation: "extracted", Visibility: "operational", State: "promoted",
	}
	c2 := persist.Claim{
		ID: fmt.Sprintf("filter-b-%d", now.UnixNano()), Entity: "test::filters",
		Predicate: "weight", Object: "heavy", Confidence: 0.7,
		Derivation: "inferred", Visibility: "operational", State: "promoted",
	}
	if _, err := store.AppendClaim(c1, now); err != nil {
		t.Fatalf("AppendClaim c1: %v", err)
	}
	if _, err := store.AppendClaim(c2, now); err != nil {
		t.Fatalf("AppendClaim c2: %v", err)
	}

	// Filter by topic (predicate)
	claims, err := store.FoldClaims(persist.ClaimFilter{
		Entity: "test::filters",
		Topic:  "size",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("FoldClaims with topic filter: %v", err)
	}
	for _, c := range claims {
		if c.Predicate != "size" {
			t.Errorf("expected predicate 'size', got %q", c.Predicate)
		}
	}
}

func TestNextSeq(t *testing.T) {
	store := testStore(t)
	seq1, err := store.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq (1st): %v", err)
	}
	seq2, err := store.NextSeq()
	if err != nil {
		t.Fatalf("NextSeq (2nd): %v", err)
	}
	if seq2 <= seq1 {
		t.Errorf("NextSeq not monotonic: %d then %d", seq1, seq2)
	}
}

// ─── Edges ───────────────────────────────────────────────────────────────────────

func TestAppendEdge(t *testing.T) {
	store := testStore(t)
	now := time.Now()

	edge := persist.Edge{
		From:    "claim-a",
		To:      "claim-b",
		Kind:    "supersedes",
		ClaimID: "claim-a",
	}
	if err := store.AppendEdge(edge, now); err != nil {
		t.Fatalf("AppendEdge: %v", err)
	}
}

func TestAppendClaimRejectsDivergentContent(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	claim := persist.Claim{
		ID:         fmt.Sprintf("divergent-%d", now.UnixNano()),
		Entity:     "test::divergent",
		Predicate:  "color",
		Object:     "blue",
		Confidence: 0.9,
		Derivation: "extracted",
		Visibility: "operational",
		State:      "promoted",
	}

	seq1, err := store.AppendClaim(claim, now)
	if err != nil {
		t.Fatalf("AppendClaim 1st: %v", err)
	}
	if seq1 <= 0 {
		t.Fatalf("expected positive seq, got %d", seq1)
	}

	// Identical re-insert must be idempotent and return the existing seq.
	seq2, err := store.AppendClaim(claim, now)
	if err != nil {
		t.Fatalf("AppendClaim identical: %v", err)
	}
	if seq2 != seq1 {
		t.Fatalf("idempotent insert returned different seq: %d != %d", seq2, seq1)
	}

	// Divergent content with the same id must be rejected, not silently dropped.
	claim.Object = "red"
	_, err = store.AppendClaim(claim, now)
	if err == nil {
		t.Fatal("expected error for divergent claim id, got nil")
	}
	if !strings.Contains(err.Error(), "different content") {
		t.Fatalf("expected different-content error, got: %v", err)
	}

	// Fold must still expose the original (immutable) claim.
	claims, err := store.FoldClaims(persist.ClaimFilter{Entity: "test::divergent"})
	if err != nil {
		t.Fatalf("FoldClaims: %v", err)
	}
	for _, c := range claims {
		if c.ID == claim.ID && c.Object != "blue" {
			t.Fatalf("divergent insert corrupted original claim: object=%q", c.Object)
		}
	}
}

func TestBulkAppendClaimsRollsBackOnConflict(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Now()
	sharedID := fmt.Sprintf("bulk-conflict-%d", now.UnixNano())

	// Seed the first claim so the second bulk insert conflicts.
	first := persist.Claim{
		ID: sharedID, Entity: "test::bulk", Predicate: "p", Object: "first",
		Confidence: 0.5, Derivation: "extracted", Visibility: "operational", State: "promoted",
	}
	if _, err := store.AppendClaim(first, now); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	second := persist.Claim{
		ID: fmt.Sprintf("bulk-ok-%d", now.UnixNano()), Entity: "test::bulk", Predicate: "p", Object: "second",
		Confidence: 0.5, Derivation: "extracted", Visibility: "operational", State: "promoted",
	}
	conflict := persist.Claim{
		ID: sharedID, Entity: "test::bulk", Predicate: "p", Object: "second",
		Confidence: 0.5, Derivation: "extracted", Visibility: "operational", State: "promoted",
	}

	_, err := store.BulkAppendClaims(ctx, []persist.Claim{second, conflict})
	if err == nil {
		t.Fatal("expected bulk append to fail on divergent id conflict")
	}

	// The successful second claim must NOT be visible: the whole phase rolled back.
	claims, err := store.FoldClaims(persist.ClaimFilter{Entity: "test::bulk"})
	if err != nil {
		t.Fatalf("FoldClaims: %v", err)
	}
	for _, c := range claims {
		if c.ID == second.ID {
			t.Fatalf("bulk phase was not rolled back; found %s after conflict", second.ID)
		}
	}
}

func TestCandidateFullSchemaRoundTrip(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	cand := persist.Candidate{
		ID:              fmt.Sprintf("cand-full-%d", now.UnixNano()),
		Entity:          "test::full-candidate",
		Predicate:       "hypothesis",
		Object:          "postgres-carries-all-fields",
		ObservationDate: "2026-08-10",
		Confidence:      0.8,
		Derivation:      "extracted",
		ClaimType:       "architecture",
		CanonClass:      "system",
		Qualifiers:      map[string]string{"domain": "storage"},
		Visibility:      "candidate",
		State:           "proposed",
		SourceTraceRef:  "trace://abc",
		EvidenceRefs:    []string{"ev://1", "ev://2"},
		Reason:          "initial extraction",
		AdjudicatedBy:   "operator-1",
		AdjudicatedAt:   now.UTC().Format(time.RFC3339),
		CreatedAt:       now.UTC().Format(time.RFC3339),
	}

	if err := store.AppendCandidate(cand, now); err != nil {
		t.Fatalf("AppendCandidate: %v", err)
	}

	got, err := store.GetCandidate(cand.ID)
	if err != nil {
		t.Fatalf("GetCandidate: %v", err)
	}
	if got == nil {
		t.Fatal("GetCandidate returned nil")
	}
	if got.ClaimType != cand.ClaimType {
		t.Errorf("claim_type lost: got %q want %q", got.ClaimType, cand.ClaimType)
	}
	if got.CanonClass != cand.CanonClass {
		t.Errorf("canon_class lost: got %q want %q", got.CanonClass, cand.CanonClass)
	}
	if len(got.Qualifiers) != 1 || got.Qualifiers["domain"] != "storage" {
		t.Errorf("qualifiers lost: got %v", got.Qualifiers)
	}
	if got.Reason != cand.Reason {
		t.Errorf("reason lost: got %q want %q", got.Reason, cand.Reason)
	}
	if got.AdjudicatedBy != cand.AdjudicatedBy {
		t.Errorf("adjudicated_by lost: got %q want %q", got.AdjudicatedBy, cand.AdjudicatedBy)
	}
	if got.AdjudicatedAt != cand.AdjudicatedAt {
		t.Errorf("adjudicated_at lost: got %q want %q", got.AdjudicatedAt, cand.AdjudicatedAt)
	}
	if len(got.EvidenceRefs) != 2 || got.EvidenceRefs[0] != "ev://1" {
		t.Errorf("evidence_refs lost: got %v", got.EvidenceRefs)
	}

	// UpdateCandidate must preserve the full schema when the mutate callback changes
	// fields beyond visibility/state.
	if err := store.UpdateCandidate(cand.ID, func(c *persist.Candidate) error {
		c.State = "parked"
		c.Reason = "operator reviewed"
		c.AdjudicatedBy = "operator-2"
		c.AdjudicatedAt = now.UTC().Format(time.RFC3339)
		c.ClaimType = "status"
		c.Qualifiers["domain"] = "governance"
		return nil
	}); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	updated, err := store.GetCandidate(cand.ID)
	if err != nil {
		t.Fatalf("GetCandidate after update: %v", err)
	}
	if updated.State != "parked" {
		t.Errorf("state update lost: got %q", updated.State)
	}
	if updated.Reason != "operator reviewed" {
		t.Errorf("reason update lost: got %q", updated.Reason)
	}
	if updated.AdjudicatedBy != "operator-2" {
		t.Errorf("adjudicated_by update lost: got %q", updated.AdjudicatedBy)
	}
	if updated.ClaimType != "status" {
		t.Errorf("claim_type update lost: got %q", updated.ClaimType)
	}
	if updated.Qualifiers["domain"] != "governance" {
		t.Errorf("qualifiers update lost: got %v", updated.Qualifiers)
	}

	list, err := store.ListCandidates(persist.CandidateFilter{Entity: cand.Entity})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("ListCandidates returned no rows")
	}
	if list[0].Reason != "operator reviewed" {
		t.Errorf("ListCandidates dropped reason: got %q", list[0].Reason)
	}
}

func TestCandidateLifecycle(t *testing.T) {
	store := testStore(t)
	now := time.Now()

	cand := persist.Candidate{
		ID:         fmt.Sprintf("cand-%d", now.UnixNano()),
		Entity:     "test::candidates",
		Predicate:  "hypothesis",
		Object:     "postgres-is-fast",
		Confidence: 0.7,
		Derivation: "extracted",
		Visibility: "candidate",
		State:      "proposed",
	}

	// Append
	if err := store.AppendCandidate(cand, now); err != nil {
		t.Fatalf("AppendCandidate: %v", err)
	}

	// Get
	got, err := store.GetCandidate(cand.ID)
	if err != nil {
		t.Fatalf("GetCandidate: %v", err)
	}
	if got.State != "proposed" {
		t.Errorf("expected state 'proposed', got %q", got.State)
	}
	if got.Object != "postgres-is-fast" {
		t.Errorf("expected object 'postgres-is-fast', got %q", got.Object)
	}

	// Update (adjudication: proposed → parked)
	err = store.UpdateCandidate(cand.ID, func(c *persist.Candidate) error {
		c.State = "parked"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}

	// Verify update
	updated, err := store.GetCandidate(cand.ID)
	if err != nil {
		t.Fatalf("GetCandidate after update: %v", err)
	}
	if updated.State != "parked" {
		t.Errorf("expected state 'parked' after update, got %q", updated.State)
	}

	// List by state
	list, err := store.ListCandidates(persist.CandidateFilter{
		Entity: "test::candidates",
		State:  "parked",
	})
	if err != nil {
		t.Fatalf("ListCandidates: %v", err)
	}
	found := false
	for _, c := range list {
		if c.ID == cand.ID {
			found = true
		}
	}
	if !found {
		t.Error("ListCandidates did not find the parked candidate")
	}
}

// ─── Events ──────────────────────────────────────────────────────────────────────

func TestEventPutAndRead(t *testing.T) {
	store := testStore(t)

	id := fmt.Sprintf("event-%d", time.Now().UnixNano())
	payload := map[string]any{
		"type":  "test_event",
		"count": 42,
		"tags":  []string{"a", "b"},
	}

	if err := store.PutJSON(id, payload); err != nil {
		t.Fatalf("PutJSON: %v", err)
	}

	rec, err := store.Read(id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if rec == nil {
		t.Fatal("Read returned nil")
	}
	if rec.ID != id {
		t.Errorf("expected ID %q, got %q", id, rec.ID)
	}

	// Verify payload round-trips via Bytes
	var m map[string]any
	if err := json.Unmarshal(rec.Bytes, &m); err != nil {
		t.Fatalf("Unmarshal rec.Bytes: %v", err)
	}
	if m["type"] != "test_event" {
		t.Errorf("expected type 'test_event', got %v", m["type"])
	}
}

func TestEventPutJSONIfAbsent(t *testing.T) {
	store := testStore(t)

	id := fmt.Sprintf("absent-%d", time.Now().UnixNano())

	// First write should create
	created, err := store.PutJSONIfAbsent(id, map[string]string{"v": "1"})
	if err != nil {
		t.Fatalf("PutJSONIfAbsent (1st): %v", err)
	}
	if !created {
		t.Error("PutJSONIfAbsent should return created=true on first write")
	}

	// Second write should NOT overwrite
	created, err = store.PutJSONIfAbsent(id, map[string]string{"v": "2"})
	if err != nil {
		t.Fatalf("PutJSONIfAbsent (2nd): %v", err)
	}
	if created {
		t.Error("PutJSONIfAbsent should return created=false on duplicate")
	}

	// Verify original value preserved
	rec, err := store.Read(id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var m map[string]string
	json.Unmarshal(rec.Bytes, &m)
	if m["v"] != "1" {
		t.Errorf("expected original value '1', got %q", m["v"])
	}
}

func TestEventScan(t *testing.T) {
	store := testStore(t)

	id := fmt.Sprintf("scan-%d", time.Now().UnixNano())
	if err := store.PutJSON(id, map[string]string{"scan": "test"}); err != nil {
		t.Fatalf("PutJSON: %v", err)
	}

	events, err := store.Scan(persist.EventScanOptions{Limit: 100})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	found := false
	for _, e := range events {
		if e.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("Scan did not return the stored event")
	}
}

// ─── Vectors ─────────────────────────────────────────────────────────────────────

func TestVectorUpsertSearchDelete(t *testing.T) {
	store := testStore(t)

	ref := fmt.Sprintf("vec-%d", time.Now().UnixNano())
	embedding := []float32{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
	meta := map[string]string{"topic": "testing", "source": "unit-test"}

	// Upsert
	if err := store.Upsert(ref, embedding, meta); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Search with similar vector — should return high similarity
	query := []float32{0.9, 0.1, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
	hits, err := store.Search(query, 5, map[string]string{"topic": "testing"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Search returned 0 hits")
	}
	if hits[0].Ref != ref {
		t.Errorf("expected first hit ref %q, got %q", ref, hits[0].Ref)
	}
	if hits[0].Score < 0.9 {
		t.Errorf("expected high similarity score, got %f", hits[0].Score)
	}

	// Count
	count, err := store.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count < 1 {
		t.Error("Count should be >= 1 after upsert")
	}

	// Delete
	if err := store.Delete(ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Search after delete — should not find it
	hits2, err := store.Search(query, 5, map[string]string{"topic": "testing"})
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	for _, h := range hits2 {
		if h.Ref == ref {
			t.Error("Search returned deleted vector")
		}
	}
}

func TestVectorUpsertOverwrites(t *testing.T) {
	store := testStore(t)

	ref := fmt.Sprintf("vec-ow-%d", time.Now().UnixNano())
	v1 := []float32{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
	v2 := []float32{0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}

	if err := store.Upsert(ref, v1, nil); err != nil {
		t.Fatalf("Upsert v1: %v", err)
	}

	count1, err := store.Count()
	if err != nil {
		t.Fatalf("Count after v1: %v", err)
	}
	if count1 != 1 {
		t.Fatalf("count after v1 = %d, want 1", count1)
	}

	if err := store.Upsert(ref, v2, nil); err != nil {
		t.Fatalf("Upsert v2 (overwrite): %v", err)
	}

	count2, err := store.Count()
	if err != nil {
		t.Fatalf("Count after v2: %v", err)
	}
	if count2 != 1 {
		t.Fatalf("upsert same ref changed count from %d to %d, want 1 (overwrite)", count1, count2)
	}

	// Search with v2 direction — should find the updated vector
	hits, err := store.Search(v2, 1, nil)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Ref != ref {
		t.Error("Upsert did not overwrite — search with v2 did not find ref")
	}

	// The old vector direction must NOT still match: if Upsert created a
	// duplicate or left the old embedding, v1 would also score near 1.
	hitsOld, err := store.Search(v1, 1, nil)
	if err != nil {
		t.Fatalf("Search old direction: %v", err)
	}
	if len(hitsOld) == 0 {
		t.Fatal("search with old direction returned 0 hits")
	}
	if hitsOld[0].Ref != ref {
		t.Fatalf("old direction returned different ref %q, want %q", hitsOld[0].Ref, ref)
	}
	if hitsOld[0].Score > 0.1 {
		t.Fatalf("old direction still scores %.3f after overwrite; old vector not replaced", hitsOld[0].Score)
	}
}

// ─── Interface compliance ────────────────────────────────────────────────────────

func TestInterfaceCompliance(t *testing.T) {
	// Compile-time check that *Store satisfies all interfaces
	var _ persist.LedgerStore = (*Store)(nil)
	var _ persist.EventStore = (*Store)(nil)
	var _ persist.VectorStore = (*Store)(nil)
}
