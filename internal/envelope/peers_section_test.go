package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

// The peers section is additive and optional: a pack WITHOUT it stays valid and never
// serializes a "peers" key (the additive precedent — like task_contract/active_positions),
// and a pack WITH it validates and round-trips losslessly through the typed spine.
func TestContextPackPeersSectionIsAdditiveAndRoundTrips(t *testing.T) {
	base := func() ContextPack {
		return ContextPack{
			Header:             hdr(SVContextPack, "ctx_peers_roundtrip"),
			SummaryMD:          "additive-field check",
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

	// Absent peers: valid, and no "peers" key in the JSON (omitempty on the pointer).
	without := base()
	if err := without.Validate(); err != nil {
		t.Fatalf("pack without peers invalid: %v", err)
	}
	data, err := json.Marshal(&without)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"peers":`) {
		t.Fatalf("pack without peers unexpectedly serialized a peers key: %s", data)
	}

	// Present peers: validates and round-trips (all peers fields survive).
	with := base()
	with.Peers = &PeersSection{
		ActiveLeases: []PeerLease{{WorkID: "work_live", Holder: "lane-a", Age: "1h", Stale: false},
			{WorkID: "work_dead", Holder: "lane-b", Age: "2d", Stale: true}},
		Lanes: []PeerLaneCard{{Lane: "codex / auto/build / gpt / balanced", Harness: "codex",
			TopCapabilities: []string{"build-implement"}, Tendencies: []string{"falsifier-first"}}},
	}
	if err := with.Validate(); err != nil {
		t.Fatalf("pack with peers invalid: %v", err)
	}
	if err := RoundTrip(&with); err != nil {
		t.Fatalf("pack with peers round-trip: %v", err)
	}
	roundData, err := json.Marshal(&with)
	if err != nil {
		t.Fatal(err)
	}
	var back ContextPack
	if err := json.Unmarshal(roundData, &back); err != nil {
		t.Fatal(err)
	}
	if back.Peers == nil || len(back.Peers.ActiveLeases) != 2 || len(back.Peers.Lanes) != 1 {
		t.Fatalf("peers did not survive round-trip: %+v", back.Peers)
	}
	if !back.Peers.ActiveLeases[1].Stale || back.Peers.ActiveLeases[0].Stale {
		t.Fatalf("stale flags lost in round-trip: %+v", back.Peers.ActiveLeases)
	}
	if back.Peers.Lanes[0].Harness != "codex" || len(back.Peers.Lanes[0].TopCapabilities) != 1 {
		t.Fatalf("lane card lost in round-trip: %+v", back.Peers.Lanes[0])
	}
}
