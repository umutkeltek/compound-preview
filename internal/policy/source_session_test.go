package policy

import (
	"strings"
	"testing"
	"time"

	"compound/internal/envelope"
	"compound/internal/home"
)

func TestSessionSourceStoreCarriesLiveIngressWithoutHarnessSuppliedLineage(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	store := NewSessionSourceStore(homePath)
	sessionID := "native-session-fixture"

	operator := envelope.NewSourceEnvelope(
		sourceSessionHeader("src_operator_turn_one", fixedNow),
		envelope.SourceOrigin{
			Kind:          "operator",
			Principal:     "operator:local-harness",
			Authenticated: true,
		},
		[]byte("inspect the page and update the local report"),
		envelope.SourceTrustAuthenticatedOperator,
		envelope.ConfidentialityInternal,
		[]string{"reader", "filesystem", "network", "browser", "control"},
	)
	operatorRef, err := store.Record(sessionID, operator, SourceRecordReset, fixedNow)
	if err != nil {
		t.Fatalf("record operator ingress: %v", err)
	}
	if !strings.Contains(operatorRef, operator.ID) {
		t.Fatalf("operator source ref = %q, want source id %q", operatorRef, operator.ID)
	}

	web := envelope.NewSourceEnvelope(
		sourceSessionHeader("src_web_result", fixedNow.Add(time.Second)),
		envelope.SourceOrigin{
			Kind:    "web",
			Locator: "https://docs.example.invalid/fixture",
		},
		[]byte("untrusted page output"),
		envelope.SourceTrustUntrusted,
		envelope.ConfidentialityPublic,
		[]string{"reader", "filesystem", "network", "browser", "control"},
	)
	if _, err := store.Record(sessionID, web, SourceRecordAppend, fixedNow.Add(time.Second)); err != nil {
		t.Fatalf("record web ingress: %v", err)
	}

	active, err := store.Active(sessionID)
	if err != nil {
		t.Fatalf("load active sources: %v", err)
	}
	if len(active) != 2 || active[0].ID != operator.ID || active[1].ID != web.ID {
		t.Fatalf("active lineage = %+v, want operator then web", active)
	}

	nextTurn := envelope.NewSourceEnvelope(
		sourceSessionHeader("src_operator_turn_two", fixedNow.Add(2*time.Second)),
		envelope.SourceOrigin{
			Kind:          "operator",
			Principal:     "operator:local-harness",
			Authenticated: true,
		},
		[]byte("continue"),
		envelope.SourceTrustAuthenticatedOperator,
		envelope.ConfidentialityInternal,
		[]string{"reader", "filesystem", "network", "browser", "control"},
	)
	if _, err := store.Record(sessionID, nextTurn, SourceRecordReset, fixedNow.Add(2*time.Second)); err != nil {
		t.Fatalf("reset next turn: %v", err)
	}
	active, err = store.Active(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != nextTurn.ID {
		t.Fatalf("turn reset retained stale lineage: %+v", active)
	}
}

func sourceSessionHeader(id string, now time.Time) envelope.Header {
	return envelope.Header{
		SchemaVersion: envelope.SVSourceEnvelope,
		ID:            id,
		CreatedAt:     now.UTC().Format(time.RFC3339Nano),
		Producer:      envelope.Producer{Kind: "test", Name: "compound"},
		Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
		Privacy:       "private",
		Risk:          "tier1",
		EvidenceRefs:  []string{},
		Uncertainty:   envelope.Uncertainty{Level: "known"},
	}
}
