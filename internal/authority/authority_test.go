package authority

import "testing"

func TestDefaultRegistryHasNoPrivateFallback(t *testing.T) {
	t.Setenv("COMPOUND_AUTHORITY_REGISTRY", "")

	if got := DefaultRegistry(); got != "" {
		t.Fatalf("expected no public-core authority default, got %q", got)
	}
}

func TestDefaultRegistryUsesConfiguredOverlay(t *testing.T) {
	t.Setenv("COMPOUND_AUTHORITY_REGISTRY", "/tmp/synthetic-registry.yaml")

	if got := DefaultRegistry(); got != "/tmp/synthetic-registry.yaml" {
		t.Fatalf("expected configured registry, got %q", got)
	}
}
