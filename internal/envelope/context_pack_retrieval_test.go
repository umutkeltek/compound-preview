package envelope

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func retrievalPostureTestPack() ContextPack {
	return ContextPack{
		Header:             hdr(SVContextPack, "ctx_retrieval_posture"),
		SummaryMD:          "retrieval posture validation",
		Included:           []IncludedRef{},
		Excluded:           []ExcludedRef{},
		ActiveInstructions: []string{},
		ActiveHooks:        []string{},
		ActiveDecisions:    []string{},
		ActiveMemory:       []string{},
		SourceRevisions:    []SourceRev{},
		RequiredChecks:     []string{},
		Avoid:              []string{},
	}
}

func TestContextPackRetrievalPostureIsOptionalForLegacyJSON(t *testing.T) {
	legacyJSON := `{"schema_version":"compound.context_pack.v0.1","id":"ctx_legacy_retrieval","created_at":"2026-06-28T00:00:00Z","producer":{"kind":"cli","name":"compound","version":""},"ids":{},"scope":{},"authority":{"source_class":"runtime","status":"current"},"privacy":"private","risk":"tier0","evidence_refs":[],"uncertainty":{"level":"known"},"summary_md":"legacy","included":[],"excluded":[],"active_instructions":[],"active_hooks":[],"active_decisions":[],"required_checks":[],"avoid":[]}`
	var pack ContextPack
	if err := json.Unmarshal([]byte(legacyJSON), &pack); err != nil {
		t.Fatalf("decode legacy ContextPack: %v", err)
	}
	if pack.Retrieval != nil {
		t.Fatalf("legacy retrieval = %+v, want nil", pack.Retrieval)
	}
	if err := pack.Validate(); err != nil {
		t.Fatalf("legacy ContextPack without retrieval must validate: %v", err)
	}
	encoded, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != legacyJSON {
		t.Fatalf("optional retrieval field moved legacy ContextPack bytes:\n got: %s\nwant: %s", encoded, legacyJSON)
	}
	legacySum := sha256.Sum256([]byte(legacyJSON))
	if got, want := hex.EncodeToString(legacySum[:]), "b2a6d0d1eca5bc69ce47ce0b14801b56cdf4c7a6cbe710aa71369d0df5e58249"; got != want {
		t.Fatalf("frozen legacy ContextPack hash = %s, want %s", got, want)
	}
	if strings.Contains(string(encoded), `"retrieval":`) {
		t.Fatalf("legacy ContextPack gained retrieval key: %s", encoded)
	}
	populated := pack
	populated.Retrieval = &RetrievalPosture{
		Level: RetrievalLevelNotAttempted, Fallback: RetrievalFallbackLedgerRanker,
		Authority: RetrievalAuthorityCompoundLedger,
	}
	populatedJSON, err := json.Marshal(populated)
	if err != nil {
		t.Fatal(err)
	}
	populatedSum := sha256.Sum256(populatedJSON)
	if populatedSum == legacySum {
		t.Fatal("populated retrieval posture did not move the ContextPack hash; legacy neutrality guard is vacuous")
	}
}

func TestContextPackRetrievalPostureAcceptsCanonicalCombinations(t *testing.T) {
	tests := []struct {
		name      string
		retrieval RetrievalPosture
	}{
		{
			name: "used",
			retrieval: RetrievalPosture{
				Level: RetrievalLevelUsed, Authority: RetrievalAuthorityCompoundLedger,
				RequestedEngine: "protocol", ObservedEngine: "semantic-index",
			},
		},
		{
			name: "degraded unavailable",
			retrieval: RetrievalPosture{
				Level: RetrievalLevelDegraded, Fallback: RetrievalFallbackLedgerRanker,
				Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol",
				ErrorClass: RetrievalErrorUnavailable,
			},
		},
		{
			name: "refused integrity mismatch",
			retrieval: RetrievalPosture{
				Level: RetrievalLevelRefused, Fallback: RetrievalFallbackLedgerRanker,
				Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol",
				ObservedEngine: "semantic-index", ErrorClass: RetrievalErrorEmbedderMismatch,
			},
		},
		{
			name: "not attempted offline",
			retrieval: RetrievalPosture{
				Level: RetrievalLevelNotAttempted, Fallback: RetrievalFallbackLedgerRanker,
				Authority: RetrievalAuthorityCompoundLedger,
			},
		},
		{
			name: "not attempted after selecting an unavailable engine",
			retrieval: RetrievalPosture{
				Level: RetrievalLevelNotAttempted, Fallback: RetrievalFallbackLedgerRanker,
				Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "pgvector",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pack := retrievalPostureTestPack()
			pack.Retrieval = &tc.retrieval
			if err := pack.Validate(); err != nil {
				t.Fatalf("canonical retrieval posture invalid: %v", err)
			}
			if err := RoundTrip(&pack); err != nil {
				t.Fatalf("retrieval posture round-trip: %v", err)
			}
		})
	}
}

func TestContextPackRetrievalPostureRejectsContradictions(t *testing.T) {
	overlong := strings.Repeat("a", 65)
	tests := []struct {
		name      string
		retrieval RetrievalPosture
		want      string
	}{
		{name: "unknown level", retrieval: RetrievalPosture{Level: "healthy", Authority: RetrievalAuthorityCompoundLedger}, want: "retrieval.level"},
		{name: "used with error", retrieval: RetrievalPosture{Level: RetrievalLevelUsed, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", ObservedEngine: "semantic-index", ErrorClass: RetrievalErrorUnavailable}, want: "must not carry error_class"},
		{name: "used with fallback", retrieval: RetrievalPosture{Level: RetrievalLevelUsed, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", ObservedEngine: "semantic-index", Fallback: RetrievalFallbackLedgerRanker}, want: "must not carry fallback"},
		{name: "used without observed engine", retrieval: RetrievalPosture{Level: RetrievalLevelUsed, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol"}, want: "observed_engine is required"},
		{name: "degraded without fallback", retrieval: RetrievalPosture{Level: RetrievalLevelDegraded, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", ErrorClass: RetrievalErrorUnavailable}, want: "fallback"},
		{name: "degraded without error", retrieval: RetrievalPosture{Level: RetrievalLevelDegraded, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", Fallback: RetrievalFallbackLedgerRanker}, want: "degradable error_class"},
		{name: "degraded with refusal error", retrieval: RetrievalPosture{Level: RetrievalLevelDegraded, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", Fallback: RetrievalFallbackLedgerRanker, ErrorClass: RetrievalErrorProtocolVersion}, want: "degradable error_class"},
		{name: "refused without fallback", retrieval: RetrievalPosture{Level: RetrievalLevelRefused, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", ErrorClass: RetrievalErrorProtocolVersion}, want: "fallback"},
		{name: "refused with degraded error", retrieval: RetrievalPosture{Level: RetrievalLevelRefused, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", Fallback: RetrievalFallbackLedgerRanker, ErrorClass: RetrievalErrorUnavailable}, want: "integrity-refusal error_class"},
		{name: "not attempted with observed engine", retrieval: RetrievalPosture{Level: RetrievalLevelNotAttempted, Authority: RetrievalAuthorityCompoundLedger, Fallback: RetrievalFallbackLedgerRanker, ObservedEngine: "semantic-index"}, want: "must not carry observed_engine"},
		{name: "not attempted with error", retrieval: RetrievalPosture{Level: RetrievalLevelNotAttempted, Authority: RetrievalAuthorityCompoundLedger, Fallback: RetrievalFallbackLedgerRanker, ErrorClass: RetrievalErrorRetrieval}, want: "must not carry error_class"},
		{name: "wrong authority", retrieval: RetrievalPosture{Level: RetrievalLevelNotAttempted, Authority: "provider_index", Fallback: RetrievalFallbackLedgerRanker}, want: "authority"},
		{name: "wrong fallback", retrieval: RetrievalPosture{Level: RetrievalLevelDegraded, Authority: RetrievalAuthorityCompoundLedger, Fallback: "retry_provider", RequestedEngine: "protocol", ErrorClass: RetrievalErrorUnavailable}, want: "fallback"},
		{name: "punctuation invalid requested engine", retrieval: RetrievalPosture{Level: RetrievalLevelDegraded, Authority: RetrievalAuthorityCompoundLedger, Fallback: RetrievalFallbackLedgerRanker, RequestedEngine: "https://engine.example", ErrorClass: RetrievalErrorUnavailable}, want: "requested_engine"},
		{name: "overlong requested engine", retrieval: RetrievalPosture{Level: RetrievalLevelDegraded, Authority: RetrievalAuthorityCompoundLedger, Fallback: RetrievalFallbackLedgerRanker, RequestedEngine: overlong, ErrorClass: RetrievalErrorUnavailable}, want: "requested_engine"},
		{name: "punctuation invalid observed engine", retrieval: RetrievalPosture{Level: RetrievalLevelUsed, Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol", ObservedEngine: "engine/request?id=secret"}, want: "observed_engine"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pack := retrievalPostureTestPack()
			pack.Retrieval = &tc.retrieval
			err := pack.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestContextPackRetrievalPostureJSONIsBoundedMetadataOnly(t *testing.T) {
	pack := retrievalPostureTestPack()
	pack.Retrieval = &RetrievalPosture{
		Level: RetrievalLevelDegraded, Fallback: RetrievalFallbackLedgerRanker,
		Authority: RetrievalAuthorityCompoundLedger, RequestedEngine: "protocol",
		ObservedEngine: "semantic-index", ErrorClass: RetrievalErrorUnavailable,
	}
	body, err := json.Marshal(pack)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	var posture map[string]json.RawMessage
	if err := json.Unmarshal(raw["retrieval"], &posture); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"level": true, "fallback": true, "authority": true,
		"requested_engine": true, "observed_engine": true, "error_class": true,
	}
	for field := range posture {
		if !allowed[field] {
			t.Fatalf("retrieval posture serialized unbounded field %q: %s", field, body)
		}
	}
	for _, forbidden := range []string{"query", "prompt", "body", "path", "url", "endpoint", "credential", "request_id", "retry"} {
		if _, exists := posture[forbidden]; exists {
			t.Fatalf("retrieval posture serialized forbidden field %q: %s", forbidden, body)
		}
	}
}
