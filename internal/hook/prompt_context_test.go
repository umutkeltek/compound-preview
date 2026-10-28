package hook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"compound/internal/candidates"
	"compound/internal/capgraph"
	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/mem"
	"compound/internal/work"
)

func TestLiveUserPromptIsWorkNeutralAndPersistsOpaqueExactContext(t *testing.T) {
	homePath := t.TempDir()
	manifest, err := capgraph.LoadSkillManifest(filepath.Join("..", "..", "fixtures", "skills", "harness-split.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.ApplySkillManifest(homePath, manifest, fixedNow); err != nil {
		t.Fatal(err)
	}
	seedPromptContextMemory(t, homePath)
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.List()
	if err != nil {
		t.Fatal(err)
	}

	raw := "PROMPT_PRIVACY_SENTINEL_7c1f pricing prompt context should prefer concise progress updates"
	deps := baseDeps()
	deps.HomePath = homePath
	deps.Store = store
	var last Result
	for i := 0; i < 3; i++ {
		last, err = Run(Fixture{
			Hook:     "user-prompt-submit",
			Harness:  "codex",
			Live:     true,
			Cwd:      "/repo/product",
			Task:     raw,
			Prompt:   raw,
			RepoKind: "product",
		}, deps, fixedNow.Add(timeOffset(i)))
		if err != nil {
			t.Fatalf("prompt %d: %v", i, err)
		}
		if got := kinds(last.Produced); len(got) != 3 || got[0] != "context_request" || got[1] != "effective_contract" || got[2] != "context_pack" {
			t.Fatalf("prompt %d produced=%v, want context spine only", i, got)
		}
		if last.Event.IDs.WorkID != "" {
			t.Fatalf("prompt %d hook event work_id=%q, want empty", i, last.Event.IDs.WorkID)
		}
	}
	after, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("three live prompts changed work count %d -> %d", len(before), len(after))
	}
	if last.Injection == nil {
		t.Fatal("live prompt did not persist an injection")
	}
	persisted, err := os.ReadFile(last.Injection.Path)
	if err != nil {
		t.Fatal(err)
	}
	if last.ContextOutput != string(persisted) {
		t.Fatalf("returned context differs from persisted injection\nreturned=%q\npersisted=%q", last.ContextOutput, string(persisted))
	}
	for _, want := range []string{
		"pricing prompt context should prefer concise progress updates",
		"capability://skill:codex-pricing",
	} {
		if !strings.Contains(last.ContextOutput, want) {
			t.Fatalf("returned context missing relevant meaning %q:\n%s", want, last.ContextOutput)
		}
	}
	for _, forbidden := range []string{raw, "UNRELATED_DATABASE_MARKER_951d", "candidate work"} {
		if strings.Contains(last.ContextOutput, forbidden) {
			t.Fatalf("returned context contains forbidden %q:\n%s", forbidden, last.ContextOutput)
		}
	}
	if utf8.RuneCountInString(last.ContextOutput) > MaxContextChars {
		t.Fatalf("returned context chars=%d, max=%d", utf8.RuneCountInString(last.ContextOutput), MaxContextChars)
	}
	for _, want := range []string{last.Event.Header.ID, last.Context.Pack.Header.ID, "chars=", "sha256:"} {
		if !strings.Contains(last.Summary, want) {
			t.Fatalf("prompt diagnostic missing %q: %q", want, last.Summary)
		}
	}
	for _, forbidden := range []string{"# Compound Context Injection", "pricing prompt context should prefer concise progress updates"} {
		if strings.Contains(last.Summary, forbidden) {
			t.Fatalf("prompt diagnostic leaked context %q: %q", forbidden, last.Summary)
		}
	}

	ref := "sha256:" + hashStr(raw)
	req := last.Produced[0].(*envelope.ContextRequest)
	ec := last.Produced[1].(*envelope.EffectiveContract)
	pack := last.Produced[2].(*envelope.ContextPack)
	if req.IntentTextRef != ref || req.TaskGuess != ref || ec.TaskContract.Intent != ref || pack.TaskContract.Intent != ref {
		t.Fatalf("persisted prompt identity not opaque: request=%q/%q contract=%q pack=%q want=%q",
			req.IntentTextRef, req.TaskGuess, ec.TaskContract.Intent, pack.TaskContract.Intent, ref)
	}
	if last.Context == nil || last.Context.Input.Task != ref || last.Context.Input.TaskContract.Intent != ref {
		t.Fatalf("retrieval observation retained prompt: %+v", last.Context)
	}
	resealed, err := contract.SealRuntimeMaterialization(contract.RuntimeMaterialization{
		EffectiveContract: *ec,
		ContextPack:       *pack,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ec.Header.ID != resealed.EffectiveContract.Header.ID ||
		pack.Header.ID != resealed.ContextPack.Header.ID ||
		pack.Header.IDs.ContractID != resealed.ContextPack.Header.IDs.ContractID {
		t.Fatalf("persisted prompt ids are not stable content-bound proof refs: contract=%q/%q pack=%q/%q pack_contract=%q/%q",
			ec.Header.ID, resealed.EffectiveContract.Header.ID,
			pack.Header.ID, resealed.ContextPack.Header.ID,
			pack.Header.IDs.ContractID, resealed.ContextPack.Header.IDs.ContractID)
	}
	assertPromptSentinelAbsentFromTree(t, homePath, raw)
}

func TestRenderInjectionPrioritizesMeaningWithinWholeContextBound(t *testing.T) {
	const (
		positionRef  = "position://project:compound::prompt-context-authority"
		positionLine = "project:compound::prompt-context-authority [predicate=authority_status, claim_type=architecture] → PROMPT_POSITION_HIGH_SIGNAL (since-seq 7, chain-length 1)"
		decisionRef  = "decision://PROMPT_DECISION_HIGH_SIGNAL"
	)
	included := []envelope.IncludedRef{{
		Ref:         positionRef,
		SourceClass: "runtime",
		Reason:      "Compound memory folded position; source stores and indexes are evidence, not authority: " + positionLine,
		Confidence:  1,
		Status:      "current",
	}}
	for i := 0; i < maxInjectionIncludedRefs; i++ {
		included = append(included, envelope.IncludedRef{
			Ref:         fmt.Sprintf("generic://provenance-%02d", i),
			SourceClass: "generated",
			Reason:      strings.Repeat("generic provenance detail ", 40) + fmt.Sprintf("GENERIC_TAIL_SENTINEL_%02d", i),
			Confidence:  1,
			Status:      "current",
		})
	}
	pack := envelope.ContextPack{
		SummaryMD:       "bounded prompt context",
		Included:        included,
		ActiveMemory:    []string{positionRef},
		ActivePositions: []envelope.ActivePosition{{Ref: positionRef, Line: positionLine}},
		ActiveDecisions: []string{decisionRef},
		RequiredChecks:  []string{"PROMPT_REQUIRED_HIGH_SIGNAL"},
		Avoid:           []string{"PROMPT_AVOID_HIGH_SIGNAL"},
		Negatives: []envelope.NegativeCard{{
			Source: "failure_pattern", ApproachKey: "PROMPT_NEGATIVE_HIGH_SIGNAL",
			WhyItFailed: "prior lane burned this path", Ref: "candidate://failure_pattern/cand_prompt",
		}},
		CapabilityOffers: []envelope.CapabilityOffer{{
			Ref: "capability://skill:prompt-high-signal", Kind: "skill", Name: "prompt-high-signal", Score: 2,
			Why: "PROMPT_CAPABILITY_HIGH_SIGNAL",
		}},
	}
	pack.Header.ID = "ctx_prompt_bound"
	text := renderInjection(injectionRender{HookID: "hook_prompt_bound", Pack: pack, Surface: injectionSurfaceTask})
	if got := utf8.RuneCountInString(text); got > MaxContextChars {
		t.Fatalf("rendered injection chars=%d, max=%d", got, MaxContextChars)
	}
	// Decisions are deliberately rendered as a count + resolution pointer, never as
	// bare opaque ids — the budget they used to consume funds the negatives section.
	if strings.Contains(text, decisionRef) {
		t.Fatalf("bounded injection must not list bare decision ids:\n%s", text)
	}
	if !strings.Contains(text, "1 decision ref carried by this ContextPack") {
		t.Fatalf("bounded injection dropped the decisions count line:\n%s", text)
	}
	for _, want := range []string{positionLine, "PROMPT_REQUIRED_HIGH_SIGNAL", "PROMPT_AVOID_HIGH_SIGNAL", "PROMPT_NEGATIVE_HIGH_SIGNAL", "PROMPT_CAPABILITY_HIGH_SIGNAL"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bounded injection dropped high-signal meaning %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "GENERIC_TAIL_SENTINEL_23") {
		t.Fatalf("bounded injection retained late generic provenance:\n%s", text)
	}
	// High-signal meaning must reach the per-prompt surface and generic provenance
	// must not. This was an ordering assertion while both shared one budget; the
	// surface split makes it a partition, so high-signal content can no longer be
	// pushed out by provenance no matter how much provenance there is.
	if !strings.Contains(text, positionLine) {
		t.Fatalf("per-prompt surface dropped high-signal memory:\n%s", text)
	}
	if strings.Contains(text, "## Included") {
		t.Fatalf("per-prompt surface must not carry generic provenance:\n%s", text)
	}
}

func TestRenderInjectionReservesBudgetAcrossPathologicalHighSignalSections(t *testing.T) {
	const positionRef = "position://prompt-context::reserved-budget"
	huge := strings.Repeat("界", MaxContextChars*2)
	pack := envelope.ContextPack{
		SummaryMD:       "SUMMARY_REPRESENTATIVE " + huge,
		ActiveMemory:    []string{positionRef},
		ActivePositions: []envelope.ActivePosition{{Ref: positionRef, Line: "MEMORY_POSITION_REPRESENTATIVE " + huge}},
		ActiveDecisions: []string{"decision://DECISION_REPRESENTATIVE_" + huge},
		RequiredChecks:  []string{"REQUIRED_REPRESENTATIVE_" + huge},
		Avoid:           []string{"AVOID_REPRESENTATIVE_" + huge},
		Negatives: []envelope.NegativeCard{{
			Source: "failure_pattern", ApproachKey: "NEGATIVE_REPRESENTATIVE",
			WhyItFailed: huge, Ref: "candidate://failure_pattern/cand_pathological",
		}},
		ActiveInstructions: []string{"instruction://INSTRUCTION_REPRESENTATIVE_" + huge},
		CapabilityOffers: []envelope.CapabilityOffer{{
			Ref:   "capability://CAPABILITY_REPRESENTATIVE",
			Kind:  "skill",
			Name:  "oversized-capability",
			Score: 1,
			Why:   huge,
		}},
		Included: []envelope.IncludedRef{{
			Ref:         positionRef,
			SourceClass: "runtime",
			Reason:      "Compound memory folded position; source stores and indexes are evidence, not authority: MEMORY_POSITION_REPRESENTATIVE " + huge,
			Confidence:  1,
			Status:      "current",
		}},
	}
	pack.Header.ID = "ctx_pathological_budget"

	// Both surfaces are exercised: a per-section reservation only means something if
	// it holds on the surface that renders the section, and an oversized section on
	// one surface must not be able to starve anything on either.
	text := renderInjection(injectionRender{HookID: "hook_pathological_budget", Pack: pack, Surface: injectionSurfaceTask})
	orientation := renderInjection(injectionRender{HookID: "hook_pathological_orientation", Pack: pack, Surface: injectionSurfaceOrientation})

	for name, rendered := range map[string]string{"task": text, "orientation": orientation} {
		if got := utf8.RuneCountInString(rendered); got > MaxContextChars {
			t.Fatalf("pathological %s injection chars=%d, max=%d", name, got, MaxContextChars)
		}
	}

	for _, want := range []string{
		"MEMORY_POSITION_REPRESENTATIVE",
		"REQUIRED_REPRESENTATIVE",
		"AVOID_REPRESENTATIVE",
		"NEGATIVE_REPRESENTATIVE",
		"CAPABILITY_REPRESENTATIVE",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("pathological first section starved later high-signal marker %q on the task surface:\n%s", want, text)
		}
	}
	// Orientation sections reserve against each other the same way. SUMMARY is the
	// first section on that surface and is deliberately the largest, so it is the one
	// most able to starve what follows it — which is exactly what an oversized
	// constitution would do if the caps below it were not reserved.
	for _, want := range []string{
		"SUMMARY_REPRESENTATIVE",
		"INSTRUCTION_REPRESENTATIVE",
	} {
		if !strings.Contains(orientation, want) {
			t.Fatalf("pathological summary starved later orientation marker %q:\n%s", want, orientation)
		}
	}
	// The decisions section renders a count, not ref content, so its reservation is
	// proven by the count line surviving rather than by a passthrough marker.
	if !strings.Contains(text, "1 decision ref carried by this ContextPack") {
		t.Fatalf("pathological injection starved the decisions count line:\n%s", text)
	}
}

func TestLiveUserPromptBreadthEchoCannotPersistRawQuery(t *testing.T) {
	homePath := t.TempDir()
	t.Setenv("COMPOUND_TEST_FAKE_BREADTH_SIDECAR", "1")
	t.Setenv("COMPOUND_TEST_FAKE_BREADTH_ECHO_OBSERVED", "1")
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", os.Args[0]+" -test.run=TestFakeBreadthSidecar")
	raw := "BREADTH_ECHO_PRIVACY_SENTINEL_4f5c adapt prompt meaning"
	deps := baseDeps()
	deps.HomePath = homePath
	res, err := Run(Fixture{
		Hook: "user-prompt-submit", Harness: "codex", Live: true,
		Cwd: "/repo/product", RepoKind: "product", Task: raw, Prompt: raw,
	}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.Context == nil || res.Context.Input.Task != "sha256:"+hashStr(raw) {
		t.Fatalf("prompt context observation not sanitized: %+v", res.Context)
	}
	assertPromptSentinelAbsentFromTree(t, homePath, raw)
}

func TestLiveUserPromptMaterializesRuntimeOnce(t *testing.T) {
	homePath := t.TempDir()
	seedPromptContextMemory(t, homePath)
	graph, err := capgraph.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Add(capgraph.Node{
		ID: "skill:runtime-snapshot", Kind: "skill", Name: "Runtime Snapshot",
		Triggers: []string{"task scoped materialization"}, Weight: 1,
	}); err != nil {
		t.Fatal(err)
	}
	const task = "one runtime snapshot"
	taskEmbeds := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, 0, len(request.Input))
		for i, text := range request.Input {
			if text == task {
				taskEmbeds++
			}
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "openai")
	t.Setenv("COMPOUND_BREADTH_EMBED_MODEL", "runtime-test")
	t.Setenv("COMPOUND_BREADTH_EMBED_BASE_URL", server.URL)
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "")

	deps := baseDeps()
	deps.HomePath = homePath
	res, err := Run(Fixture{
		Hook: "user-prompt-submit", Harness: "codex", Live: true,
		Cwd: "/repo/product", RepoKind: "product", Task: task, Prompt: task,
	}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if res.BreakerTripped {
		t.Fatalf("prompt context breaker tripped: %+v", res)
	}
	if taskEmbeds != 1 {
		t.Fatalf("prompt runtime semantic task embeddings = %d, want 1", taskEmbeds)
	}
}

func TestLiveUserPromptBreakerCreatesOnlyFailOpenTelemetry(t *testing.T) {
	homePath := t.TempDir()
	store, err := work.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	deps := baseDeps()
	deps.HomePath = homePath
	deps.Store = store
	deps.After = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- fixedNow
		return ch
	}
	deps.BeforeContextBuild = func() { select {} }

	res, err := Run(Fixture{
		Hook: "user-prompt-submit", Harness: "codex", Live: true,
		Cwd: "/repo/product", Task: "breaker prompt", Prompt: "breaker prompt",
	}, deps, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BreakerTripped || res.Event.Status != "fail_open" || res.Event.IDs.WorkID != "" ||
		res.Summary != "" || res.Injection != nil || res.Context != nil || len(res.Produced) != 0 {
		t.Fatalf("breaker returned partial prompt state: %+v", res)
	}
	works, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 0 {
		t.Fatalf("breaker created work: %+v", works)
	}
	for _, dir := range []string{"context/requests", "context/packs", "context/injections", "contracts/effective"} {
		if entries, err := os.ReadDir(filepath.Join(homePath, filepath.FromSlash(dir))); err == nil && len(entries) > 0 {
			t.Fatalf("breaker persisted partial artifacts in %s: %+v", dir, entries)
		}
	}
}

func seedPromptContextMemory(t *testing.T, homePath string) {
	t.Helper()
	store, err := mem.Open(homePath)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := store.Capture([]mem.Candidate{
		{
			Entity: "prompt-context", Predicate: "authority_status",
			Object:     "pricing prompt context should prefer concise progress updates",
			Confidence: 0.99, Derivation: "deterministic", ClaimType: "architecture",
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "trace_prompt_context_relevant",
			SnippetHash: "sha256:prompt-context-relevant", Visibility: "operational",
		},
		{
			Entity: "database-maintenance", Predicate: "status",
			Object:     "UNRELATED_DATABASE_MARKER_951d database vacuum status",
			Confidence: 0.99, Derivation: "deterministic", ClaimType: "status",
			PromotionBasis: candidates.BasisRuntimeProof, SourceTraceRef: "trace_prompt_context_unrelated",
			SnippetHash: "sha256:prompt-context-unrelated", Visibility: "operational",
		},
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range captured {
		if _, err := store.Adjudicate(candidate.ID, candidates.Promote, "test", fixedNow); err != nil {
			t.Fatal(err)
		}
	}
}

func assertPromptSentinelAbsentFromTree(t *testing.T, root, sentinel string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), sentinel) {
			return fmt.Errorf("raw prompt sentinel found in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func timeOffset(i int) time.Duration {
	return time.Duration(i) * time.Second
}

func assertJSONOmitsPrompt(t *testing.T, value any, raw string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), raw) {
		t.Fatalf("JSON retained raw prompt: %s", data)
	}
}
