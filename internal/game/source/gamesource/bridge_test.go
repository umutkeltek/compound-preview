package gamesource

import (
	"testing"
	"time"
)

func TestBuildObservationAlwaysPromoteFalse(t *testing.T) {
	cases := []struct {
		adapter   string
		system    string
		kind      string
		sourceKey string
		subject   string
	}{
		{AdapterIDSteamWeb, SourceSystemSteamWeb, "app", "440", "Team Fortress 2"},
		{AdapterIDSteamWeb, SourceSystemSteamWeb, "genre", "action", "Action"},
		{AdapterIDSteamWeb, SourceSystemSteamWeb, "company", "valve", "Valve"},
		{AdapterIDSteamSpy, SourceSystemSteamSpy, "spy_estimate", "440", "Team Fortress 2"},
		{AdapterIDSteamSpy, SourceSystemSteamSpy, "company", "valve", "Valve"},
	}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for _, c := range cases {
		it := RawObservation{
			Kind: c.kind, SourceKey: c.sourceKey, Subject: c.subject,
			Statement: "test", Confidence: 1.0,
			Evidence: []string{"https://example.test/api", "revision:abc123"},
		}
		obs, err := BuildObservation(c.adapter, c.system, "abc123", it, now)
		if err != nil {
			t.Errorf("%s/%s: %v", c.adapter, c.kind, err)
			continue
		}
		if len(obs.MapsTo) == 0 {
			t.Errorf("%s/%s: MapsTo empty", c.adapter, c.kind)
			continue
		}
		for i, m := range obs.MapsTo {
			if m.PromoteAllowed {
				t.Errorf("%s/%s MapsTo[%d].PromoteAllowed = true; MUST stay false", c.adapter, c.kind, i)
			}
		}
	}
}

func TestBuildObservationSchemaVersion(t *testing.T) {
	it := RawObservation{
		Kind: "app", SourceKey: "440", Subject: "X",
		Evidence: []string{"https://example.test/api", "revision:abc"},
	}
	obs, err := BuildObservation(AdapterIDSteamWeb, SourceSystemSteamWeb, "abc", it, time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if obs.SchemaVersion == "" {
		t.Errorf("SchemaVersion empty")
	}
}

func TestBuildObservationIDStableAcrossReobservations(t *testing.T) {
	it := RawObservation{
		Kind: "genre", SourceKey: "action", Subject: "Action",
		Evidence: []string{"https://example.test/api", "revision:a"},
	}
	t1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	obs1, _ := BuildObservation(AdapterIDSteamWeb, SourceSystemSteamWeb, "rev-aaa", it, t1)
	obs2, _ := BuildObservation(AdapterIDSteamWeb, SourceSystemSteamWeb, "rev-bbb", it, t2)
	if obs1.ID != obs2.ID {
		t.Errorf("ID differs: %q vs %q", obs1.ID, obs2.ID)
	}
}

func TestBuildObservationRequiresEvidence(t *testing.T) {
	it := RawObservation{Kind: "app", SourceKey: "440", Subject: "x"}
	if _, err := BuildObservation(AdapterIDSteamWeb, SourceSystemSteamWeb, "rev", it, time.Now()); err == nil {
		t.Fatalf("expected error on missing evidence")
	}
}

func TestBuildObservationRejectsEmptyFields(t *testing.T) {
	good := RawObservation{Kind: "app", SourceKey: "440", Subject: "x", Evidence: []string{"https://e.test"}}
	now := time.Now()
	cases := []struct {
		name    string
		adapter string
		system  string
		it      RawObservation
	}{
		{"empty adapter", "", SourceSystemSteamWeb, good},
		{"empty system", AdapterIDSteamWeb, "", good},
		{"empty kind", AdapterIDSteamWeb, SourceSystemSteamWeb, RawObservation{SourceKey: "440", Subject: "x", Evidence: good.Evidence}},
		{"empty source key", AdapterIDSteamWeb, SourceSystemSteamWeb, RawObservation{Kind: "app", Subject: "x", Evidence: good.Evidence}},
	}
	for _, c := range cases {
		if _, err := BuildObservation(c.adapter, c.system, "rev", c.it, now); err == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

func TestBuildObservationEvidenceRefsIsCopy(t *testing.T) {
	ev := []string{"https://e.test/api", "revision:abc"}
	it := RawObservation{Kind: "app", SourceKey: "440", Subject: "x", Evidence: ev}
	obs, err := BuildObservation(AdapterIDSteamWeb, SourceSystemSteamWeb, "abc", it, time.Now())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	ev[0] = "https://malicious.test/api"
	if obs.EvidenceRefs[0] != "https://e.test/api" {
		t.Errorf("EvidenceRefs aliased input")
	}
}

func TestUniqueObservationRevision(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	r1 := uniqueObservationRevision("bodyhash", now, 0)
	r2 := uniqueObservationRevision("bodyhash", now, 1)
	if r1 == r2 {
		t.Errorf("expected distinct revisions for index 0 vs 1")
	}
	r3 := uniqueObservationRevision("", now, 0)
	if r3 == "" {
		t.Errorf("empty body rev must produce non-empty")
	}
}
