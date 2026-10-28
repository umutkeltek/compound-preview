package capgraph

import "testing"

// TestServiceNodeKindIsAccepted proves the one-line NodeKinds addition
// (work_c8178a5ab583, F7): a node minted with Kind "service" is accepted by
// Store.Add now that the infra-services enrollment door can use it, while
// Store.Add still rejects an unknown kind exactly as before.
func TestServiceNodeKindIsAccepted(t *testing.T) {
	_, s := newStore(t)
	node := Node{ID: "service:example-svc", Kind: "service", Name: "example-svc.internal", Triggers: []string{"example-svc", "service", "deploy", "infra", "dns"}}
	if err := s.Add(node); err != nil {
		t.Fatalf("Store.Add should accept kind \"service\": %v", err)
	}
	nodes, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.ID == node.ID && n.Kind == "service" {
			found = true
		}
	}
	if !found {
		t.Fatalf("service node not present after Add: %+v", nodes)
	}
}

func TestServiceNodeKindDoesNotOpenUnknownKinds(t *testing.T) {
	_, s := newStore(t)
	if err := s.Add(Node{ID: "plugin:not-a-real-kind", Kind: "plugin", Name: "bogus"}); err == nil {
		t.Fatal("Store.Add should still reject an unknown node kind")
	}
}
