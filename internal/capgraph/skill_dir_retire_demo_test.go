package capgraph

import (
	"testing"
)

func TestRetireIntentDemo(t *testing.T) {
	_, store := newStore(t)
	for _, s := range []string{"alpha-x", "beta-x", "gamma-x"} {
		if err := store.Add(Node{ID: "skill:" + s, Kind: "skill", Name: s, Triggers: []string{s}}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := store.Retire("skill:alpha-x", "skill:gamma-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("retired = %v, want 2", out)
	}
	loaded, _ := store.List()
	byID := map[string]Node{}
	for _, n := range loaded {
		byID[n.ID] = n
	}
	if !byID["skill:alpha-x"].Retired {
		t.Fatal("alpha-x not retired")
	}
	if !byID["skill:gamma-x"].Retired {
		t.Fatal("gamma-x not retired")
	}
	if byID["skill:beta-x"].Retired {
		t.Fatal("beta-x retired by mistake")
	}
}
