package mem

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"compound/internal/candidates"
)

const currentViewTestBoundaryTimeout = 2 * time.Second

func awaitCurrentViewTestValue[T any](t *testing.T, name string, values <-chan T) T {
	t.Helper()
	timer := time.NewTimer(currentViewTestBoundaryTimeout)
	defer timer.Stop()
	select {
	case value, ok := <-values:
		if !ok {
			t.Fatalf("%s closed before delivering a value", name)
		}
		return value
	case <-timer.C:
		t.Fatalf("timed out after %s waiting for %s", currentViewTestBoundaryTimeout, name)
		var zero T
		return zero
	}
}

func awaitCurrentViewTestSignal(t *testing.T, name string, signal <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(currentViewTestBoundaryTimeout)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out after %s waiting for %s", currentViewTestBoundaryTimeout, name)
	}
}

func awaitCurrentViewTestGroup(t *testing.T, name string, group *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	awaitCurrentViewTestSignal(t, name, done)
}

func closeCurrentViewTestSignalOnce(signal chan struct{}) func() {
	var once sync.Once
	return func() {
		once.Do(func() { close(signal) })
	}
}

func waitForCurrentViewTestPeer(signal <-chan struct{}) bool {
	timer := time.NewTimer(2 * currentViewTestBoundaryTimeout)
	defer timer.Stop()
	select {
	case <-signal:
		return true
	case <-timer.C:
		return false
	}
}

type engineMustNotRun struct{}

func (engineMustNotRun) Capabilities() EngineCapabilities {
	panic("mismatched expansion reached rented engine capabilities")
}
func (engineMustNotRun) Index([]Claim, int64) error {
	panic("mismatched expansion reached rented engine index")
}
func (engineMustNotRun) Query(string, string, []string, int) ([]Hit, error) {
	panic("mismatched expansion reached rented engine query")
}

func TestExpandedResultRejectsMismatchedCaptureBeforeEngineWork(t *testing.T) {
	store := newStore(t)
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	const query = "memory not updating"
	mismatched := ExpandedQuery{
		Original: "different query", Text: "different query caller_expansion",
		Added: []string{"caller_expansion"}, Active: true,
	}
	result, err := store.QueryBreadthReadOnlyFromViewExpandedResult(query, 10, engineMustNotRun{}, view, mismatched)
	if err == nil || !strings.Contains(err.Error(), "expansion") {
		t.Fatalf("mismatched captured expansion error = %v, want explicit expansion mismatch", err)
	}
	if result.ConsumedExpansion != nil || len(result.Claims) != 0 {
		t.Fatalf("mismatched expansion returned consumed meaning: %+v", result)
	}
}

type expansionConsumptionEngine struct {
	breadth         bool
	corpusGen       int64
	indexErr        error
	queryErrByText  map[string]error
	capabilityCalls int
	indexCalls      int
	queries         []string
}

func (e *expansionConsumptionEngine) Capabilities() EngineCapabilities {
	e.capabilityCalls++
	return EngineCapabilities{Name: "expansion-consumption-test", Breadth: e.breadth, CorpusGen: e.corpusGen}
}

func (e *expansionConsumptionEngine) Index([]Claim, int64) error {
	e.indexCalls++
	return e.indexErr
}

func (e *expansionConsumptionEngine) Query(text, _ string, _ []string, _ int) ([]Hit, error) {
	e.queries = append(e.queries, text)
	if err := e.queryErrByText[text]; err != nil {
		return nil, err
	}
	return nil, nil
}

func TestExpandedResultOmitsConsumptionOnEarlyCapabilityAndIndexErrors(t *testing.T) {
	const query = "memory not updating"
	expanded := ExpandedQuery{Original: query, Text: query + " extract_facts", Added: []string{"extract_facts"}, Active: true}

	t.Run("unsupported capability", func(t *testing.T) {
		store := newStore(t)
		view, err := store.CurrentView()
		if err != nil {
			t.Fatal(err)
		}
		engine := &expansionConsumptionEngine{}
		result, err := store.QueryBreadthReadOnlyFromViewExpandedResult(query, 10, engine, view, expanded)
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("unsupported error = %v, want %v", err, ErrUnsupported)
		}
		if result.ConsumedExpansion != nil || len(result.Claims) != 0 {
			t.Fatalf("unsupported capability reported consumption: %+v", result)
		}
		if engine.capabilityCalls != 1 || engine.indexCalls != 0 || len(engine.queries) != 0 {
			t.Fatalf("unsupported capability engine work: capabilities=%d index=%d queries=%v", engine.capabilityCalls, engine.indexCalls, engine.queries)
		}
	})

	t.Run("index error", func(t *testing.T) {
		store := storeWith(t, "project:focus", "positioning", "owned capture")
		view, err := store.CurrentView()
		if err != nil {
			t.Fatal(err)
		}
		wantErr := errors.New("index failed")
		engine := &expansionConsumptionEngine{breadth: true, indexErr: wantErr}
		result, err := store.QueryBreadthReadOnlyFromViewExpandedResult(query, 10, engine, view, expanded)
		if !errors.Is(err, wantErr) {
			t.Fatalf("index error = %v, want %v", err, wantErr)
		}
		if result.ConsumedExpansion != nil || len(result.Claims) != 0 {
			t.Fatalf("index error reported consumption: %+v", result)
		}
		if engine.capabilityCalls != 1 || engine.indexCalls != 1 || len(engine.queries) != 0 {
			t.Fatalf("index failure engine work: capabilities=%d index=%d queries=%v", engine.capabilityCalls, engine.indexCalls, engine.queries)
		}
	})
}

func TestExpandedResultOmitsConsumptionWhenExpansionQueryFails(t *testing.T) {
	store := newStore(t)
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	const query = "memory not updating"
	const added = "extract_facts"
	wantErr := errors.New("expansion query failed")
	engine := &expansionConsumptionEngine{
		breadth: true, queryErrByText: map[string]error{added: wantErr},
	}
	expanded := ExpandedQuery{Original: query, Text: query + " " + added, Added: []string{added}, Active: true}
	result, err := store.QueryBreadthReadOnlyFromViewExpandedResult(query, 10, engine, view, expanded)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expansion query error = %v, want %v", err, wantErr)
	}
	if result.ConsumedExpansion != nil || len(result.Claims) != 0 {
		t.Fatalf("failed expansion query reported consumption: %+v", result)
	}
	if !reflect.DeepEqual(engine.queries, []string{query, added}) {
		t.Fatalf("engine queries = %v, want original then expansion", engine.queries)
	}
}

func TestExpandedResultReportsExactSuccessfullyConsumedExpansion(t *testing.T) {
	store := newStore(t)
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	const query = "memory not updating"
	expanded := ExpandedQuery{
		Original: query, Text: query + " extract_facts starvation",
		Added: []string{"extract_facts", "starvation"}, Active: true,
		Matched: []string{"memory not updating"},
	}
	engine := &expansionConsumptionEngine{breadth: true}
	result, err := store.QueryBreadthReadOnlyFromViewExpandedResult(query, 10, engine, view, expanded)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConsumedExpansion == nil || !reflect.DeepEqual(*result.ConsumedExpansion, expanded) {
		t.Fatalf("consumed expansion = %+v, want exact selected expansion %+v", result.ConsumedExpansion, expanded)
	}
	if !reflect.DeepEqual(engine.queries, []string{query, "extract_facts starvation"}) {
		t.Fatalf("engine queries = %v, want exact original and expansion terms", engine.queries)
	}
	expanded.Added[0] = "caller mutation"
	expanded.Matched[0] = "caller mutation"
	if result.ConsumedExpansion.Added[0] != "extract_facts" {
		t.Fatalf("reported consumed expansion aliases caller memory: %+v", result.ConsumedExpansion)
	}
	if result.ConsumedExpansion.Matched[0] != "memory not updating" {
		t.Fatalf("reported matched aliases alias caller memory: %+v", result.ConsumedExpansion)
	}
}

func TestCurrentViewTopiclessNeverLoadsEntityRegistry(t *testing.T) {
	store := storeWith(t, "project:focus", "positioning", "owned capture")
	loads := 0
	store.entityLoader = func() ([]Entity, error) {
		loads++
		return nil, errors.New("topicless CurrentView must not load entities")
	}

	view, err := store.CurrentView()
	if err != nil {
		t.Fatalf("topicless CurrentView consulted entity registry: %v", err)
	}
	if loads != 0 {
		t.Fatalf("topicless CurrentView entity loads = %d, want 0", loads)
	}
	if got, ok := view.TopicResolution("delivery"); ok || got.Status != "unresolved" {
		t.Fatalf("topicless CurrentView fabricated a topic resolution: got=%+v ok=%v", got, ok)
	}

	blank, err := store.CurrentViewForTopic("   ")
	if err != nil {
		t.Fatalf("blank-topic CurrentView consulted entity registry: %v", err)
	}
	if loads != 0 {
		t.Fatalf("blank-topic CurrentView entity loads = %d, want 0", loads)
	}
	if got, ok := blank.TopicResolution("delivery"); ok || got.Status != "unresolved" {
		t.Fatalf("blank-topic CurrentView fabricated a topic resolution: got=%+v ok=%v", got, ok)
	}
}

func TestCurrentViewForTopicCapturesOneImmutableResolutionAcrossDuplicateHeavyRegistry(t *testing.T) {
	store := storeWith(t, "project:focus", "positioning", "owned capture")
	const historyRows = 50_000
	entities := make([]Entity, historyRows)
	for i := range entities {
		entities[i] = Entity{Slug: "topic:shipping", Kind: "topic", Aliases: []string{"delivery"}}
	}
	loads := 0
	store.entityLoader = func() ([]Entity, error) {
		loads++
		return entities, nil
	}

	view, err := store.CurrentViewForTopic("delivery")
	if err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("duplicate-heavy topic capture entity loads = %d, want exactly 1 for %d rows", loads, historyRows)
	}
	want := EntityResolution{Slug: "topic:shipping", Status: "resolved", Via: "alias"}
	if got, ok := view.TopicResolution("delivery"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("captured topic resolution = %+v ok=%v, want %+v true", got, ok, want)
	}
	if got, ok := view.TopicResolution("shipping"); ok || got.Status != "unresolved" {
		t.Fatalf("uncaptured token fabricated a resolution: got=%+v ok=%v", got, ok)
	}

	// Mutating the loader's source after capture must not mutate the value held by
	// the view, and repeated concurrent reads must not trigger another registry load.
	entities[0].Aliases[0] = "mutated-after-capture"
	const readers = 64
	var wg sync.WaitGroup
	errs := make(chan string, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 100; attempt++ {
				if got, ok := view.TopicResolution("delivery"); !ok || !reflect.DeepEqual(got, want) {
					errs <- "captured token changed"
					return
				}
				if got, ok := view.TopicResolution("not-captured"); ok || got.Status != "unresolved" {
					errs <- "uncaptured token resolved"
					return
				}
			}
		}()
	}
	awaitCurrentViewTestGroup(t, "immutable current-view readers", &wg)
	close(errs)
	for message := range errs {
		t.Fatal(message)
	}
	if loads != 1 {
		t.Fatalf("immutable topic reads reloaded entity registry: loads=%d want=1", loads)
	}
}

func TestCurrentViewIsImmutableAndRevisionTracksOnlyConsumedMeaning(t *testing.T) {
	store := newStore(t)
	candidate := cand("project:focus", "positioning", "owned capture", "")
	candidate.Qualifiers = map[string]string{"scope": "owned"}
	captured, err := store.Capture([]Candidate{candidate}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, store, captured[0].ID, "", t0); err != nil {
		t.Fatal(err)
	}
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	claims := view.Claims()
	legacyClaims, err := store.AllCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claims, legacyClaims) {
		t.Fatalf("current view changed ordinary fold output:\nview=%+v\nlegacy=%+v", claims, legacyClaims)
	}
	if len(claims) != 1 {
		t.Fatalf("view claims = %d, want 1", len(claims))
	}
	want, ok := view.Claim(claims[0].ID)
	if !ok {
		t.Fatalf("view lost current claim %s", claims[0].ID)
	}
	if want.Qualifiers["scope"] != "owned" {
		t.Fatalf("defensive copy lost qualifiers: %+v", want.Qualifiers)
	}
	if view.Generation() < want.Seq {
		t.Fatalf("view generation = %d, want >= claim seq %d", view.Generation(), want.Seq)
	}
	if view.Revision() == "" {
		t.Fatal("view revision is empty")
	}
	claims[0].Object = "caller mutation"
	claims[0].Qualifiers["scope"] = "caller mutation"
	got, ok := view.Claim(want.ID)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("caller mutated immutable view: got=%+v ok=%v want=%+v", got, ok, want)
	}
	got.Object = "second caller mutation"
	got.Qualifiers["scope"] = "second caller mutation"
	if again, ok := view.Claim(want.ID); !ok || !reflect.DeepEqual(again, want) {
		t.Fatalf("by-id result aliases immutable view: got=%+v ok=%v want=%+v", again, ok, want)
	}

	beforeRevision := view.Revision()
	if _, err := store.RegisterEntity(Entity{Slug: "unrelated-registry-row", Kind: "concept"}, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	afterUnrelated, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	if afterUnrelated.Revision() != beforeRevision {
		t.Fatalf("unconsumed entity registry changed current-view revision: before=%s after=%s", beforeRevision, afterUnrelated.Revision())
	}

	if _, err := store.Relate(want.Entity, "relates", "new-neighbor", 0.9, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	afterEdge, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	if afterEdge.Revision() == beforeRevision {
		t.Fatalf("consumed edge append did not change current-view revision %s", beforeRevision)
	}
	if afterEdge.Generation() <= view.Generation() {
		t.Fatalf("edge append did not advance view generation: before=%d after=%d", view.Generation(), afterEdge.Generation())
	}
}

func TestCurrentViewUsesSharedMemoryLock(t *testing.T) {
	store := storeWith(t, "project:focus", "positioning", "owned capture")
	lockPath := filepath.Join(store.home, "lock", "mem.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockFile.Close() })
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var unlockOnce sync.Once
	var unlockErr error
	unlock := func() error {
		unlockOnce.Do(func() {
			unlockErr = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		})
		return unlockErr
	}
	t.Cleanup(func() { _ = unlock() })
	attempted := make(chan struct{})
	store.beforeLockObserver = closeCurrentViewTestSignalOnce(attempted)
	type viewResult struct {
		view *CurrentView
		err  error
	}
	done := make(chan viewResult, 1)
	go func() {
		view, err := store.CurrentView()
		done <- viewResult{view: view, err: err}
	}()
	awaitCurrentViewTestSignal(t, "CurrentView memory-lock attempt", attempted)
	select {
	case result := <-done:
		t.Fatalf("CurrentView bypassed held exclusive memory lock: %+v", result)
	default:
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	result := awaitCurrentViewTestValue(t, "CurrentView after memory-lock release", done)
	if result.err != nil || result.view == nil {
		t.Fatalf("CurrentView after lock release: view=%v err=%v", result.view, result.err)
	}
}

func TestQueryBreadthReadOnlyFromViewPreservesAuthorityAndParity(t *testing.T) {
	store := newStore(t)
	candidate := cand("project:focus", "positioning", "owned capture", "")
	candidate.Qualifiers = map[string]string{"scope": "owned"}
	captured, err := store.Capture([]Candidate{candidate}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := promoteForTest(t, store, captured[0].ID, "", t0); err != nil {
		t.Fatal(err)
	}
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	claims := view.Claims()
	if len(claims) != 1 {
		t.Fatalf("view claims = %d, want 1", len(claims))
	}
	hits := []Hit{{Kind: KindIndexHit, ClaimID: claims[0].ID, Score: 1}}
	legacy, err := store.QueryBreadthReadOnly("owned", 10, &cannedHitEngine{name: "legacy", hits: hits})
	if err != nil {
		t.Fatal(err)
	}
	fromView, err := store.QueryBreadthReadOnlyFromView("owned", 10, &cannedHitEngine{name: "shared-view", hits: hits}, view)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromView, legacy) {
		t.Fatalf("view-backed breadth changed query output:\nlegacy=%+v\nfrom_view=%+v", legacy, fromView)
	}
	fromView[0].Qualifiers["scope"] = "caller mutation"
	if current, ok := view.Claim(claims[0].ID); !ok || current.Qualifiers["scope"] != "owned" {
		t.Fatalf("breadth result mutated immutable view: claim=%+v ok=%v", current, ok)
	}

	malformed := &cannedHitEngine{name: "shared-view", hits: []Hit{{ClaimID: "claim_not_in_view", Score: 1}}}
	if _, err := store.QueryBreadthReadOnlyFromView("owned", 10, malformed, view); !errors.Is(err, ErrEngineServedUnownedClaim) {
		t.Fatalf("view-backed breadth weakened unowned-hit refusal: %v", err)
	}
}

func TestCurrentViewPositionsMatchOwnerFoldSemantics(t *testing.T) {
	store := storeWith(t, "project:focus", "positioning", "old stance", "current stance")
	persisted, err := store.FoldPositions(PositionFoldOptions{}, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	got := view.Positions()
	want := append([]Position(nil), persisted.Positions...)
	for i := range want {
		want[i].FoldedAt = ""
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CurrentView position semantics drifted from owner fold:\nview=%+v\nowner=%+v", got, want)
	}
}

func TestCurrentViewRejectsFabricatedAndCrossStoreAuthority(t *testing.T) {
	store := storeWith(t, "project:focus", "positioning", "owned capture")
	view, err := store.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	claims := view.Claims()
	engine := &cannedHitEngine{name: "owned", hits: []Hit{{ClaimID: claims[0].ID, Score: 1}}}
	if _, err := store.QueryBreadthReadOnlyFromView("owned", 10, engine, new(CurrentView)); err == nil {
		t.Fatal("fabricated zero CurrentView was accepted as memory authority")
	}
	other, err := Open(store.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.QueryBreadthReadOnlyFromView("owned", 10, engine, view); err == nil {
		t.Fatal("CurrentView minted by a different Store was accepted as memory authority")
	}
}

func TestCurrentViewIsOneLockConsistentGenerationAgainstGovernedWriter(t *testing.T) {
	reader := storeWith(t, "project:focus", "positioning", "old stance")
	successor := cand("project:focus", "positioning", "new stance", "")
	successor.PromotionBasis = candidates.BasisCuratedPage
	captured, err := reader.Capture([]Candidate{successor}, t0.Add(time.Minute))
	if err != nil || len(captured) != 1 {
		t.Fatalf("capture successor: got=%d err=%v", len(captured), err)
	}
	writer, err := Open(reader.home)
	if err != nil {
		t.Fatal(err)
	}
	claimsRead := make(chan struct{})
	releaseReader := make(chan struct{})
	signalClaimsRead := closeCurrentViewTestSignalOnce(claimsRead)
	releaseCurrentView := closeCurrentViewTestSignalOnce(releaseReader)
	t.Cleanup(releaseCurrentView)
	reader.currentViewAfterClaimsRead = func() {
		signalClaimsRead()
		_ = waitForCurrentViewTestPeer(releaseReader)
	}
	writerAttempted := make(chan struct{})
	writer.beforeLockObserver = closeCurrentViewTestSignalOnce(writerAttempted)

	type viewResult struct {
		view *CurrentView
		err  error
	}
	viewDone := make(chan viewResult, 1)
	go func() {
		view, err := reader.CurrentView()
		viewDone <- viewResult{view: view, err: err}
	}()
	awaitCurrentViewTestSignal(t, "CurrentView claims read", claimsRead)
	type promoteResult struct {
		result AdjudicateResult
		err    error
	}
	writerDone := make(chan promoteResult, 1)
	go func() {
		result, err := writer.Adjudicate(captured[0].ID, "promote", "test", t0.Add(time.Minute))
		writerDone <- promoteResult{result: result, err: err}
	}()
	awaitCurrentViewTestSignal(t, "governed writer lock attempt", writerAttempted)
	select {
	case result := <-writerDone:
		t.Fatalf("governed writer interleaved while CurrentView held shared lock: %+v", result)
	default:
	}
	releaseCurrentView()
	before := awaitCurrentViewTestValue(t, "CurrentView after reader release", viewDone)
	if before.err != nil {
		t.Fatal(before.err)
	}
	beforeClaims := before.view.Claims()
	if len(beforeClaims) != 1 || beforeClaims[0].Object != "old stance" {
		t.Fatalf("lock snapshot mixed writer generation: %+v", beforeClaims)
	}
	promoted := awaitCurrentViewTestValue(t, "governed writer after reader release", writerDone)
	if promoted.err != nil || promoted.result.Claim == nil {
		t.Fatalf("governed writer after release: result=%+v err=%v", promoted.result, promoted.err)
	}
	reader.currentViewAfterClaimsRead = nil
	after, err := reader.CurrentView()
	if err != nil {
		t.Fatal(err)
	}
	afterClaims := after.Claims()
	if len(afterClaims) != 1 || afterClaims[0].Object != "new stance" || after.Generation() <= before.view.Generation() {
		t.Fatalf("fresh snapshot did not observe complete writer generation: claims=%+v before_gen=%d after_gen=%d", afterClaims, before.view.Generation(), after.Generation())
	}
}

func TestCurrentViewCapturesTopicResolutionAgainstConcurrentRegistryWriter(t *testing.T) {
	reader := storeWith(t, "project:focus", "positioning", "owned capture")
	if _, err := reader.RegisterEntity(Entity{Slug: "topic:shipping", Kind: "topic", Aliases: []string{"delivery"}}, t0); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(reader.home)
	if err != nil {
		t.Fatal(err)
	}
	claimsRead := make(chan struct{})
	releaseReader := make(chan struct{})
	signalClaimsRead := closeCurrentViewTestSignalOnce(claimsRead)
	releaseCurrentView := closeCurrentViewTestSignalOnce(releaseReader)
	t.Cleanup(releaseCurrentView)
	reader.currentViewAfterClaimsRead = func() {
		signalClaimsRead()
		_ = waitForCurrentViewTestPeer(releaseReader)
	}
	writerAttempted := make(chan struct{})
	writer.beforeLockObserver = closeCurrentViewTestSignalOnce(writerAttempted)

	type viewResult struct {
		view *CurrentView
		err  error
	}
	viewDone := make(chan viewResult, 1)
	go func() {
		view, err := reader.CurrentViewForTopic("delivery")
		viewDone <- viewResult{view: view, err: err}
	}()
	awaitCurrentViewTestSignal(t, "topic CurrentView claims read", claimsRead)
	writerDone := make(chan error, 1)
	go func() {
		_, err := writer.RegisterEntity(Entity{Slug: "topic:collision", Kind: "topic", Aliases: []string{"delivery"}}, t0.Add(time.Minute))
		writerDone <- err
	}()
	awaitCurrentViewTestSignal(t, "entity writer lock attempt", writerAttempted)
	select {
	case err := <-writerDone:
		t.Fatalf("entity writer bypassed the CurrentView shared lock: %v", err)
	default:
	}
	releaseCurrentView()
	captured := awaitCurrentViewTestValue(t, "topic CurrentView after reader release", viewDone)
	if captured.err != nil {
		t.Fatal(captured.err)
	}
	if resolved, ok := captured.view.TopicResolution("delivery"); !ok || resolved.Status != "resolved" || resolved.Slug != "topic:shipping" {
		t.Fatalf("captured topic resolution mixed writer generation: resolved=%+v ok=%v", resolved, ok)
	}
	if err := awaitCurrentViewTestValue(t, "entity writer after reader release", writerDone); err != nil {
		t.Fatal(err)
	}
	reader.currentViewAfterClaimsRead = nil
	fresh, err := reader.CurrentViewForTopic("delivery")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, ok := fresh.TopicResolution("delivery"); !ok || resolved.Status != "multi" {
		t.Fatalf("fresh view did not observe registry collision: resolved=%+v ok=%v", resolved, ok)
	}
}
