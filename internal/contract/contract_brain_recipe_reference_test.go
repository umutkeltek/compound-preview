package contract

import (
	"path/filepath"
	"testing"

	"compound/internal/capgraph"
	"compound/internal/sourceobs"
)

func TestBrainRecipeReferenceConstraintsReachRuntimeContext(t *testing.T) {
	home := t.TempDir()
	recipeDir, err := filepath.Abs(filepath.Join("..", "..", "fixtures", "skills", "brain-recipe-tree"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := capgraph.LoadBrainRecipeTree(recipeDir, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	report, err := capgraph.ApplySkillManifest(home, manifest, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	sourceReport, err := sourceobs.Apply(home, sourceobs.FromSkillManifest(manifest, fixedNow), fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := capgraph.RecordSourceAuthority(home, capgraph.SourceAuthorityInput{
		SourcePath:          manifest.SourceRef.Path,
		SourceKind:          manifest.SourceRef.Kind,
		SourceOwner:         manifest.SourceRef.Owner,
		SourceClass:         manifest.SourceRef.SourceClass,
		SourceRevision:      manifest.SourceRef.Revision,
		SourceObservedAt:    manifest.ObservedAt,
		ImportedVia:         "compound capability import-skills --brain-recipe-dir",
		NodeIDs:             report.NodeIDs,
		EdgeIDs:             report.EdgeIDs,
		SourceObservationID: sourceReport.ObservationID,
		NoPromoteIDs:        sourceReport.NoPromoteIDs,
		EvidenceRefs:        manifest.EvidenceRefs,
	}, fixedNow); err != nil {
		t.Fatal(err)
	}

	matching := Input{
		Harness:     "codex",
		Cwd:         t.TempDir(),
		Task:        "triage the incident regression in a broken production flow",
		RepoKind:    "control-plane",
		RepoKindVia: "fixture",
		Mode:        "execute",
	}
	pack, err := PackWithCapabilities(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}

	const offerRef = "capability://procedure:triage"
	wantRead := filepath.Join(recipeDir, "triage.md")
	const wantBoundary = "feature build without incident signal"
	offer := findCapabilityOffer(pack, offerRef)
	if offer == nil {
		t.Fatalf("matching ContextPack missing triage procedure: %+v", pack.CapabilityOffers)
	}
	if !hasOfferConstraint(offer.Required, "read", wantRead) {
		t.Fatalf("triage procedure required constraints = %+v, want read %q", offer.Required, wantRead)
	}
	if !offerConstraintsContainDetail(offer.Conflicts, wantBoundary) {
		t.Fatalf("triage procedure conflicts = %+v, want boundary %q", offer.Conflicts, wantBoundary)
	}
	wantRevision, err := capgraph.AuthorityRevision(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceRev(pack.SourceRevisions, capgraph.CapabilityGraphAuthorityRef); got != wantRevision {
		t.Fatalf("ContextPack capability graph revision = %q, want %q", got, wantRevision)
	}

	effective, err := CompileWithRuntime(matching, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := effective.Validate(); err != nil {
		t.Fatal(err)
	}
	effectiveOffer := findEffectiveContractCapabilityOffer(effective, offerRef)
	if effectiveOffer == nil || !hasOfferConstraint(effectiveOffer.Required, "read", wantRead) ||
		!offerConstraintsContainDetail(effectiveOffer.Conflicts, wantBoundary) {
		t.Fatalf("EffectiveContract missing triage read/boundary constraints: %+v", effectiveOffer)
	}
	if got := sourceRev(effective.SourceRevisions, capgraph.CapabilityGraphAuthorityRef); got != wantRevision {
		t.Fatalf("EffectiveContract capability graph revision = %q, want %q", got, wantRevision)
	}

	unrelated := matching
	unrelated.Task = "rotate encryption keys for the staging database"
	unrelatedPack, err := PackWithCapabilities(unrelated, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if findCapabilityOffer(unrelatedPack, offerRef) != nil {
		t.Fatalf("unrelated ContextPack inherited triage procedure: %+v", unrelatedPack.CapabilityOffers)
	}
	unrelatedContract, err := CompileWithRuntime(unrelated, DefaultPolicy(), home, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if findEffectiveContractCapabilityOffer(unrelatedContract, offerRef) != nil {
		t.Fatalf("unrelated EffectiveContract inherited triage procedure: %+v", unrelatedContract.CapabilityOffers)
	}
}
