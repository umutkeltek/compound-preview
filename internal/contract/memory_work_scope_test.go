package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/mem"
)

type addressedRecallSeed struct {
	name      string
	candidate mem.Candidate
}

func addressedSeed(name, entity, object, claimType string) addressedRecallSeed {
	return addressedRecallSeed{name, mem.Candidate{
		Entity: entity, Predicate: "fixture_" + name, Object: object,
		Confidence: 0.95, Derivation: "deterministic", ClaimType: claimType,
		Qualifiers:     map[string]string{"domain": "product"},
		PromotionBasis: candidates.BasisCuratedPage, Visibility: "operational",
	}}
}

func addressedFixturePack(t *testing.T, task, mode, semanticName string, seeds ...addressedRecallSeed) (envelope.ContextPack, map[string]mem.Claim) {
	t.Helper()
	home := t.TempDir()
	store, err := mem.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" && mode != "contested" {
		_, err := store.ImportTopics([]byte(`{"schema_version":"compound.memory_topics.v0.1","topics":[{"slug":"product","aliases":["product"]},{"slug":"standing-law"},{"slug":"trap"},{"slug":"opportunity"}]}`), fixedNow)
		if err != nil {
			t.Fatal(err)
		}
	}
	if mode == "default-contract" || mode == "product-probe" {
		writeProbeContracts(t, home, `{"schema_version":"compound.probe_contracts.v0.1","default_class":"general","task_classes":[{"class":"general","probes":[{"group":"product","budget":5}]}]}`)
	}
	claims := map[string]mem.Claim{}
	for _, seed := range seeds {
		evidence, err := store.RecordEvidence(mem.EvidenceItem{
			SourceKind: "note", SourceID: "fixture_" + seed.name,
			QuoteText: seed.candidate.Object, Actor: "test", Visibility: "contextual",
		}, fixedNow)
		if err != nil {
			t.Fatal(err)
		}
		candidate := seed.candidate
		if mode == "topic" {
			// Flat topic recall requires an explicit topic ref; folded positions
			// also support the legacy qualifier-derived topic axis.
			candidate.TopicRefs = []string{"product"}
		}
		candidate.SourceTraceRef = evidence.ID
		candidate.SnippetHash = "sha256:fixture_" + seed.name
		captured, err := store.Capture([]mem.Candidate{candidate}, fixedNow)
		if err != nil || len(captured) != 1 {
			t.Fatalf("capture %s: %v", seed.name, err)
		}
		promoted, err := store.Adjudicate(captured[0].ID, candidates.Promote, "test", fixedNow)
		if err != nil || promoted.Claim == nil {
			t.Fatalf("promote %s: %v", seed.name, err)
		}
		claims[seed.name] = *promoted.Claim
	}

	if mode == "contested" {
		// The public relation API intentionally refuses needs_review edges.
		// This fixture-owned append reaches the absent-active-claim branch;
		// it never writes an actual home or changes production review gates.
		file, err := os.OpenFile(filepath.Join(home, "ledger", "edges.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		encoder := json.NewEncoder(file)
		for i, seed := range seeds {
			if err := encoder.Encode(mem.Edge{
				Seq: int64(1000 + i), Kind: mem.EdgeKindNeedsReview,
				From: "ev_fixture_review", To: claims[seed.name].ID,
				Confidence: 1, CreatedAt: fixedNow.Format("2006-01-02T15:04:05Z07:00"),
			}); err != nil {
				file.Close()
				t.Fatal(err)
			}
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	var engine mem.Engine = unavailableTraceEngine{}
	if semanticName != "" {
		engine = &fakeBreadthEngine{hits: []mem.Hit{{ClaimID: claims[semanticName].ID, Score: 1}}}
	}
	previous := breadthEngineFactory
	breadthEngineFactory = func(string) (mem.Engine, func()) { return engine, func() {} }
	t.Cleanup(func() { breadthEngineFactory = previous })
	t.Setenv("COMPOUND_MEM_ENGINE_BIN", "fixture-unavailable")
	t.Setenv("COMPOUND_MEMORY_CLI_PROFILE", "fixture-profile")
	t.Setenv("COMPOUND_STORE", "")
	t.Setenv("COMPOUND_BREADTH_EMBED_PROVIDER", "")

	in := Input{
		Harness: "codex", Cwd: "/repo/fixture-workspace", Task: task,
		RepoKind: "control-plane", RepoKindVia: "flag", Mode: "execute",
	}
	if mode == "topic" {
		in.Topic = "product"
	}
	if mode == "product-probe" {
		in.Cwd = filepath.Join(t.TempDir(), "fixture-alpha")
		if err := os.MkdirAll(in.Cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		writeProductProjectContract(t, in.Cwd, "fixture-alpha")
		in.RepoKind = "product"
	}
	pack, _, err := PackWithCapabilitiesTraced(in, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	return pack, claims
}

func assertAddressedRecall(t *testing.T, pack envelope.ContextPack, claim mem.Claim, want bool, probe ...bool) {
	t.Helper()
	flat := contains(pack.ActiveMemory, "claim://"+claim.ID)
	position := false
	for _, stance := range pack.ActivePositions {
		if stance.Entity == claim.Entity && stance.Stance == claim.Object {
			position = true
			if stance.Ref == "" || stance.SinceSeq < 1 || !hasIncluded(pack, stance.Ref) {
				t.Fatalf("served position lost its source/time reference: %+v", stance)
			}
		}
	}
	if want != (flat || position) {
		t.Fatalf("claim %s (%s): flat=%t position=%t want served=%t; active=%v", claim.ID, claim.Entity, flat, position, want, pack.ActiveMemory)
	}
	if want && (!position || ((len(probe) == 0 || !probe[0]) && !flat)) {
		t.Fatalf("positive scope lost a consumer path for %s: flat=%t position=%t probe=%v", claim.ID, flat, position, probe)
	}
	if !want {
		for _, ref := range pack.Included {
			if ref.Ref == "claim://"+claim.ID {
				t.Fatalf("off-scope claim remained included: %+v", ref)
			}
		}
	}
}

func TestAddressedWorkRecallRetainsMeaningAndRejectsForeignStatus(t *testing.T) {
	for _, kind := range []string{"architecture", "constraint", "preference"} {
		t.Run(kind, func(t *testing.T) {
			approach := addressedSeed("approach", "RollbackPatterns", "Use source-bound rollback after a failed repair", kind)
			approach.candidate.EvidenceRefs = []string{"work_fixture_previous"}
			pack, claims := addressedFixturePack(t,
				"Continue prepared R7 delivery and maintain the private plan map under work_fixture_target using source-bound rollback", "", "",
				addressedSeed("foreign", "product:fixture_alpha", `{"stage":"past-delivery","fact":"prepared private delivery work repair review execution plan map"}`, "status"),
				addressedSeed("cwd", "repo:fixture-workspace", "Immutable revisions identify approved builds", "status"),
				approach,
				addressedSeed("unknown", "unknown-subject", "private delivery repair work", "status"),
			)
			assertAddressedRecall(t, pack, claims["foreign"], false)
			assertAddressedRecall(t, pack, claims["unknown"], false)
			assertAddressedRecall(t, pack, claims["cwd"], true)
			// Preferences retain the existing explicit-personal-memory policy.
			// Architecture/constraint lessons still prove beyond-entity recall.
			assertAddressedRecall(t, pack, claims["approach"], kind != "preference")
			if pack.Retrieval == nil || pack.Retrieval.Level != envelope.RetrievalLevelDegraded {
				t.Fatalf("fixture did not exercise degraded retrieval: %+v", pack.Retrieval)
			}
			foundScope := false
			for _, omitted := range pack.Excluded {
				if omitted.Ref == "claim://"+claims["foreign"].ID {
					foundScope = strings.Contains(omitted.Reason, "scope")
					if strings.Contains(omitted.Reason, "false") || strings.Contains(omitted.Reason, "stale") {
						t.Fatalf("scope exclusion adjudicated the claim: %+v", omitted)
					}
				}
			}
			if !foundScope {
				t.Fatal("off-scope status lacks observable eligibility reason")
			}
		})
	}
}

func TestAddressedWorkRecallRescuesExplicitScope(t *testing.T) {
	for _, task := range []string{
		"Compare fixture_alpha and fixture_beta",
		"Compare fixture_alpha and product:fixture_beta while continuing work_fixture_target",
	} {
		t.Run(task, func(t *testing.T) {
			pack, claims := addressedFixturePack(t, task, "", "",
				addressedSeed("alpha", "product:fixture_alpha", "First fixture release is available", "status"),
				addressedSeed("beta", "product:fixture_beta", "Second fixture release is available", "status"),
			)
			assertAddressedRecall(t, pack, claims["alpha"], true)
			assertAddressedRecall(t, pack, claims["beta"], true)
		})
	}
	for _, mode := range []string{"topic", "default-contract"} {
		t.Run(mode, func(t *testing.T) {
			pack, claims := addressedFixturePack(t, "repair work_fixture_target", mode, "",
				addressedSeed("foreign", "product:fixture_alpha", "A separately requested repair status", "status"))
			assertAddressedRecall(t, pack, claims["foreign"], true, mode == "default-contract")
		})
	}
	t.Run("semantic", func(t *testing.T) {
		pack, claims := addressedFixturePack(t, "repair work_fixture_target", "", "semantic",
			addressedSeed("semantic", "product:fixture_alpha", "A relevant state with no shared lexical words", "status"))
		assertAddressedRecall(t, pack, claims["semantic"], true)
	})
}

func TestAddressedWorkRecallUsesExactIdentityBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, meaning, provenance string
		want                      bool
	}{
		{"exact meaning", "Completed (work_fixture_target).", "", true},
		{"exact provenance", "Completed artifact.", "trace:work_fixture_target", true},
		{"longer suffix", "Review work_fixture_target_extra", "", false},
		{"hyphen suffix", "Review work_fixture_target-extra", "", false},
		{"prefixed", "Review prefixwork_fixture_target", "", false},
		{"foreign provenance", "Review artifact", "work_fixture_previous", false},
		{"namespace is not subject", "Review product repo", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := addressedSeed("status", "product:fixture_alpha", tc.meaning, "status")
			if tc.provenance != "" {
				seed.candidate.EvidenceRefs = []string{tc.provenance}
			}
			pack, claims := addressedFixturePack(t, "review work_fixture_target", "", "", seed)
			assertAddressedRecall(t, pack, claims["status"], tc.want)
		})
	}
}

func TestAddressedWorkRecallHonorsEffectiveClassAndOrdinaryQueries(t *testing.T) {
	for _, tc := range []struct {
		name, kind, canon string
		want              bool
	}{
		{"legacy status", "status", "", false},
		{"explicit status", "architecture", "status", false},
		{"explicit fact", "status", "research_fact", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := addressedSeed("record", "product:fixture_alpha", "delivery review", tc.kind)
			seed.candidate.CanonClass = tc.canon
			pack, claims := addressedFixturePack(t, "delivery review work_fixture_target", "", "", seed)
			assertAddressedRecall(t, pack, claims["record"], tc.want)
		})
	}
	t.Run("ordinary query", func(t *testing.T) {
		pack, claims := addressedFixturePack(t, "private delivery review", "", "",
			addressedSeed("record", "product:fixture_alpha", "private delivery review", "status"))
		assertAddressedRecall(t, pack, claims["record"], true)
	})
}

func TestAddressedWorkRecallCannotUseAutomaticProbeBaseAsScope(t *testing.T) {
	pack, claims := addressedFixturePack(t, "repair product work_fixture_target", "builtin", "",
		addressedSeed("foreign", "product:fixture_alpha", "private product delivery repair", "status"),
		addressedSeed("cwd", "repo:fixture-workspace", "Current workspace release is ready", "status"),
		addressedSeed("rule", "ReusableRule", "A reusable approach from an earlier activity", "constraint"),
	)
	assertAddressedRecall(t, pack, claims["foreign"], false)
	assertAddressedRecall(t, pack, claims["cwd"], true, true)
	assertAddressedRecall(t, pack, claims["rule"], true, true)

	// An address containing a topic word is not itself a topic request.
	pack, claims = addressedFixturePack(t, "repair work_product_fixture", "builtin", "",
		addressedSeed("fragment", "product:fixture_alpha", "product fixture repair", "status"))
	assertAddressedRecall(t, pack, claims["fragment"], false)
	for _, omitted := range pack.Excluded {
		if strings.HasPrefix(omitted.Ref, "probe://") {
			t.Fatalf("identifier fragments activated built-in probes: %+v", omitted)
		}
	}
}

func TestAddressedWorkRecallCannotOverrideExistingLedgerPolicy(t *testing.T) {
	for _, name := range []string{"private", "volatile"} {
		t.Run(name, func(t *testing.T) {
			seed := addressedSeed(name, "PrivateFixture", "review work_fixture_target", "constraint")
			if name == "private" {
				seed.candidate.Visibility = "private"
			} else {
				seed.candidate.Entity = "repo:fixture-workspace"
				seed.candidate.Predicate = "head"
				seed.candidate.ClaimType = "status"
			}
			pack, claims := addressedFixturePack(t, "review work_fixture_target", "", name, seed)
			assertAddressedRecall(t, pack, claims[name], false)
		})
	}
}

func TestAddressedWorkWithheldStatusKeepsOnlyRelevantOmission(t *testing.T) {
	pack, _ := addressedFixturePack(t, "review work_product_fixture", "contested", "",
		addressedSeed("foreign", "product:fixture_alpha", "private product delivery review", "status"),
		addressedSeed("cwd", "repo:fixture-workspace", "Current workspace release", "status"),
	)
	currentOmission := false
	for _, omitted := range pack.Excluded {
		if strings.HasPrefix(omitted.Ref, "position://product:fixture_alpha::") {
			t.Fatalf("foreign withheld status leaked as a generic-topic omission: %+v", omitted)
		}
		if strings.HasPrefix(omitted.Ref, "position://repo:fixture-workspace::") &&
			strings.Contains(omitted.Reason, "contested") {
			currentOmission = true
		}
	}
	if !currentOmission {
		t.Fatal("current-workspace dispute lost its useful omission explanation")
	}
}

func TestAddressedWorkProbeKeepsForeignProductIdentityOpaque(t *testing.T) {
	pack, claims := addressedFixturePack(t, "repair product work_fixture_target", "product-probe", "",
		addressedSeed("own", "product:fixture_alpha", "product repair status", "status"),
		addressedSeed("foreign", "product:fixture_beta", "product repair status", "status"),
	)
	assertAddressedRecall(t, pack, claims["own"], true, true)
	assertAddressedRecall(t, pack, claims["foreign"], false)
	raw, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{claims["foreign"].ID, claims["foreign"].Entity} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("product fence disclosed foreign identity %q", forbidden)
		}
	}
}

func TestAddressedWorkWithheldPositionsDoNotInventMissingTypeMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, visibility, canon string
	}{
		{"non-operational", "contextual", ""},
		{"explicit stable class", "operational", "research_fact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := addressedSeed("record", "product:fixture_alpha", "product review fact", "status")
			seed.candidate.Visibility = tc.visibility
			seed.candidate.CanonClass = tc.canon
			pack, _ := addressedFixturePack(t, "review product work_fixture_target", "contested", "", seed)
			for _, omitted := range pack.Excluded {
				if strings.HasPrefix(omitted.Ref, "position://product:fixture_alpha::") &&
					strings.Contains(omitted.Reason, "contested") {
					return
				}
			}
			t.Fatal("withheld position lost existing identity/topic omission handling despite unknown visibility/canon metadata")
		})
	}
}

func TestAddressedWorkRecallKeepsNonOperationalStatus(t *testing.T) {
	for _, visibility := range []string{"contextual", "public"} {
		t.Run(visibility, func(t *testing.T) {
			seed := addressedSeed("record", "product:fixture_alpha", "review delivery", "status")
			seed.candidate.Visibility = visibility
			pack, claims := addressedFixturePack(t, "review delivery work_fixture_target", "", "", seed)
			assertAddressedRecall(t, pack, claims["record"], true)
		})
	}
}

func TestAddressedWorkProbeRetainsEveryPositiveScopePath(t *testing.T) {
	for _, tc := range []struct {
		name, task, entity, object, provenance, semantic string
	}{
		{"cwd", "repair product work_fixture_target", "repo:fixture-workspace", "Available current build", "", ""},
		{"named", "repair product fixture_alpha work_fixture_target", "product:fixture_alpha", "Available release", "", ""},
		{"meaning", "repair product work_fixture_target", "product:fixture_alpha", "Result of work_fixture_target", "", ""},
		{"provenance", "repair product work_fixture_target", "product:fixture_alpha", "Available artifact", "trace:work_fixture_target", ""},
		{"semantic", "repair product work_fixture_target", "product:fixture_alpha", "Available release", "", "record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := addressedSeed("record", tc.entity, tc.object, "status")
			if tc.provenance != "" {
				seed.candidate.EvidenceRefs = []string{tc.provenance}
			}
			pack, claims := addressedFixturePack(t, tc.task, "builtin", tc.semantic, seed)
			assertAddressedRecall(t, pack, claims["record"], true, true)
		})
	}
	t.Run("named comparison", func(t *testing.T) {
		pack, claims := addressedFixturePack(t, "compare product fixture_alpha and fixture_beta under work_fixture_target", "builtin", "",
			addressedSeed("alpha", "product:fixture_alpha", "First release", "status"),
			addressedSeed("beta", "product:fixture_beta", "Second release", "status"),
		)
		assertAddressedRecall(t, pack, claims["alpha"], true, true)
		assertAddressedRecall(t, pack, claims["beta"], true, true)
	})
}
