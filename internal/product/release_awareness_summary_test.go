package product

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/sourceobs"
)

// ReleaseAwareness.Summary adapts an internal ReleaseAwareness into the
// orient-only JSON shape. It must:
//   - return nil for nil input (so orient omits the field)
//   - keep all fields intact in the orient-side projection
func TestReleaseAwarenessSummary_PassesThrough(t *testing.T) {
	aw := &ReleaseAwareness{
		SchemaVersion:    ReleaseAwarenessSchemaVersion,
		ProductID:        "product:alpha",
		Status:           "conflicting",
		ContractVersion:  "1.0.0",
		OutdatedContract: true,
		LatestObservedAt: "2026-09-06T11:30:00Z",
		LatestVersion:    "1.2.0",
		LatestChannel:    "app_store_connect",
		LatestPlatform:   "ios",
		LatestState:      "live",
		ObservationCount: 2,
		Conflict:         []string{"app_store_connect/ios:1.1.5|live", "app_store_connect/ios:1.2.0|live"},
		Boundary:         "evidence-only",
		NextAction:       "operator-reviewed reconcile required",
		ChannelPlatforms: []ReleaseChannelPlatformView{{
			Channel: "app_store_connect", Platform: "ios",
			Latest:         sourceobs.ReleaseObservation{ProductID: "product:alpha", Channel: "app_store_connect", Platform: "ios", ObservedVersion: "1.2.0", ObservedState: "live"},
			StateSet:       []string{"live"},
			VersionSet:     []string{"1.1.5", "1.2.0"},
			CurrentVersion: "1.2.0",
		}},
	}
	got := aw.Summary()
	if got == nil {
		t.Fatalf("expected non-nil orient view")
	}
	if got.Status != "conflicting" {
		t.Fatalf("status lost: %q", got.Status)
	}
	if !got.OutdatedContract {
		t.Fatalf("outdated_contract lost")
	}
	if got.LatestVersion != "1.2.0" {
		t.Fatalf("latest_version lost: %q", got.LatestVersion)
	}
	if len(got.ChannelPlatforms) != 1 || got.ChannelPlatforms[0].Channel != "app_store_connect" {
		t.Fatalf("channel_platforms lost: %+v", got.ChannelPlatforms)
	}
	// JSON shape must be stable for downstream consumers.
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"status":"conflicting"`) {
		t.Fatalf("orient JSON missing status=conflicting: %s", string(data))
	}
	if !strings.Contains(string(data), `"outdated_contract":true`) {
		t.Fatalf("orient JSON missing outdated_contract=true: %s", string(data))
	}
}

// nil must produce nil; orient uses this to omit the field for products
// without release observations.
func TestReleaseAwarenessSummary_NilReturnsNil(t *testing.T) {
	if got := (*ReleaseAwareness)(nil).Summary(); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// Sanity: ReleaseAwareness.Summary is deterministic across calls.
func TestReleaseAwarenessSummary_Deterministic(t *testing.T) {
	aw := &ReleaseAwareness{
		ProductID:        "product:alpha",
		Status:           "current",
		LatestObservedAt: time.Date(2026, 9, 6, 11, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	a := aw.Summary()
	b := aw.Summary()
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("orient projection must be deterministic: %s vs %s", ja, jb)
	}
}

func TestReleaseAwarenessSummaryJSONContract(t *testing.T) {
	aw := &ReleaseAwareness{
		SchemaVersion: ReleaseAwarenessSchemaVersion, ProductID: "product:alpha", Status: "conflicting",
		ContractVersion: "1.0.0", ContractBuild: "10", OutdatedContract: true,
		LatestObservedAt: "2026-09-06T11:30:00Z", LatestVersion: "1.2.0", LatestChannel: "app_store_connect",
		LatestPlatform: "ios", LatestState: "live", ObservationCount: 2,
		Conflict: []string{"fixture conflict"}, MissingEvidence: []string{"fixture missing"},
		Boundary: "evidence-only", NextAction: "review", EvidenceRefs: []string{"source-observation:synthetic"},
		ChannelPlatforms: []ReleaseChannelPlatformView{{
			Status: "current", CurrentVersion: "1.2.0", Channel: "app_store_connect", Platform: "ios",
			Latest:   sourceobs.ReleaseObservation{ObservedVersion: "1.2.0", ObservedState: "live", ObservedAt: "2026-09-06T11:30:00Z"},
			StateSet: []string{"live"}, VersionSet: []string{"1.2.0"}, EvidenceRefs: []string{"candidate:synthetic"},
		}},
	}
	body, err := json.Marshal(aw.Summary())
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `{"schema_version":"compound.product_release_awareness.v0.1","product_id":"product:alpha","status":"conflicting","contract_version":"1.0.0","contract_build":"10","outdated_contract":true,"latest_observed_at":"2026-09-06T11:30:00Z","latest_version":"1.2.0","latest_channel":"app_store_connect","latest_platform":"ios","latest_state":"live","observation_count":2,"conflict":["fixture conflict"],"missing_evidence":["fixture missing"],"boundary":"evidence-only","next_action":"review","channel_platforms":[{"status":"current","current_version":"1.2.0","channel":"app_store_connect","platform":"ios","latest_version":"1.2.0","latest_state":"live","latest_observed_at":"2026-09-06T11:30:00Z","state_set":["live"],"version_set":["1.2.0"],"evidence_refs":["candidate:synthetic"]}],"evidence_refs":["source-observation:synthetic"]}`
	var got, want map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compact release JSON changed:\ngot %s\nwant %s", body, wantJSON)
	}
}

func TestReleaseAwarenessSummaryCopyIsolation(t *testing.T) {
	aw := &ReleaseAwareness{
		Conflict: []string{"original conflict"}, MissingEvidence: []string{"original missing"},
		EvidenceRefs: []string{"original evidence"},
		ChannelPlatforms: []ReleaseChannelPlatformView{{
			Channel: "original channel", StateSet: []string{"original state"},
			VersionSet: []string{"original version"}, EvidenceRefs: []string{"original channel evidence"},
		}},
	}
	before, err := json.Marshal(aw)
	if err != nil {
		t.Fatal(err)
	}
	summary := aw.Summary()
	summary.Conflict[0] = "changed"
	summary.MissingEvidence[0] = "changed"
	summary.EvidenceRefs[0] = "changed"
	summary.ChannelPlatforms[0].Channel = "changed"
	summary.ChannelPlatforms[0].StateSet[0] = "changed"
	summary.ChannelPlatforms[0].VersionSet[0] = "changed"
	summary.ChannelPlatforms[0].EvidenceRefs[0] = "changed"
	after, err := json.Marshal(aw)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("summary mutated source evidence:\nbefore %s\nafter %s", before, after)
	}
}
