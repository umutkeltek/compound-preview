package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
)

func homeFixtureTestInputs() HomeFixtureInputs {
	digest := strings.Repeat("a", 64)
	return HomeFixtureInputs{StageDirectory: "/synthetic/home-fixture", BinarySHA256: digest, FixtureSHA256: digest, SandboxSHA256: digest, SystemPolicySHA256: digest, EnvironmentSHA256: digest, HostSHA256: digest, AncestorsSHA256: digest}
}

type homeProofFixture struct {
	store        *Store
	workID, repo string
	source       gitrepository.Snapshot
}

func newHomeProofFixture(t *testing.T) homeProofFixture {
	t.Helper()
	s := newStore(t)
	repo := initProofSourceRepo(t, "https://example.invalid/compound/home-proof.git")
	source, err := gitrepository.CaptureCleanSnapshot(repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Create(CreateInput{Owner: proofActorAlpha, Actor: proofActorAlpha, SuccessPredicate: "home proof with fresh runtime acceptance", GitDelivery: &envelope.GitDeliveryContract{TargetWorktreeRoot: repo, RepositoryFingerprint: source.RepositoryFingerprint, TargetLocalRef: "refs/heads/main", ExpectedBaseSHA: source.BaseSHA, PublicationMode: "local_only"}}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(w.ID, proofActorAlpha, fixedNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return homeProofFixture{s, w.ID, repo, source}
}
func homeProofTestGate(source gitrepository.Snapshot) ProofCheckStep {
	return ProofCheckStep{Name: HomeFixtureGateName, Command: []string{"go", "run", "./scripts/testbattery", "home", "--source-head", source.HeadSHA}, ProofRef: HomeFixtureProofRef, Status: "ok"}
}
func (f homeProofFixture) complete(t *testing.T, repo string, evidence *HomeFixtureEvidence, runtimeOK bool, at time.Time, versions ...string) (ProofResourceCompleteResult, error) {
	t.Helper()
	gate := homeProofTestGate(f.source)
	gate.HomeFixtureEvidence = evidence
	runtime := ProofCheckStep{Name: "synthetic-runtime", Command: []string{"synthetic-runtime"}, ProofRef: "synthetic-runtime", Status: "ok"}
	gates := []ProofResourceGateProfile{{Name: gate.Name, Command: gate.Command, ProofRef: gate.ProofRef}, {Name: runtime.Name, Command: runtime.Command, ProofRef: runtime.ProofRef}}
	profile, err := NewProofResourceProfileWithSource(ProofResourceProfileCanonicalFull, repo, f.source, gates)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := f.store.ReserveProofResource(f.workID, ProofResourceReserveInput{Actor: proofActorAlpha, TTL: time.Minute, Workspace: repo, Command: "synthetic home proof", Profile: profile}, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := closeProofFixtureExecutionBarrier(reserved.ExecutionBarrier); err != nil {
		t.Fatal(err)
	}
	status, exit := "ok", 0
	refs := []string{gate.ProofRef, runtime.ProofRef}
	if !runtimeOK {
		status, exit = "failed", 1
		runtime.Status = "failed"
		refs = refs[:1]
	}
	schema := ProofCheckSchemaVersion
	if len(versions) > 0 {
		schema = versions[0]
	}
	report := map[string]any{"schema_version": schema, "profile_kind": ProofResourceProfileCanonicalFull, "status": status, "generated_at": at.Add(time.Second).Format(time.RFC3339Nano), "repo": repo, "base_ref": f.source.BaseSHA, "source_snapshot": f.source, "dry_run": false, "proof_refs": refs, "steps": []ProofCheckStep{gate, runtime}}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := home.NewArtifactStore(f.store.home)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := artifacts.Put(body)
	if err != nil {
		t.Fatal(err)
	}
	return f.store.CompleteProofResource(f.workID, ProofResourceCompleteInput{Actor: proofActorAlpha, ReservationID: reserved.Reservation.ID, ReportArtifactRef: ref, ProducerExitKnown: true, ProducerExitCode: exit}, at.Add(2*time.Second))
}

func TestHomeFixtureReuseOwnsOriginAndFreshRuntimeFailure(t *testing.T) {
	f := newHomeProofFixture(t)
	direct, err := NewHomeFixtureEvidence(homeFixtureTestInputs())
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.complete(t, f.repo, direct, true, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := f.store.ResolveHomeFixtureReuse(f.workID, f.source, homeProofTestGate(f.source), direct.Inputs, fixedNow.Add(10*time.Second))
	if err != nil || selected == nil || selected.OriginVerificationRef != first.VerificationRef {
		t.Fatalf("reuse=%+v err=%v", selected, err)
	}
	second, err := f.complete(t, f.repo, selected, false, fixedNow.Add(11*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.Result.Status != ProofResourceFailed || second.VerificationRef != "" {
		t.Fatalf("home reuse hid fresh runtime failure: %+v", second)
	}
	if second.Result.RunAttemptID == first.Result.RunAttemptID || second.Result.ReservationID == first.Result.ReservationID {
		t.Fatal("reuse reused execution authority")
	}
}

func TestHomeFixtureReuseAllowsDifferentCheckoutPathButNotSourceOrInputs(t *testing.T) {
	f := newHomeProofFixture(t)
	direct, _ := NewHomeFixtureEvidence(homeFixtureTestInputs())
	first, err := f.complete(t, f.repo, direct, true, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	gate := homeProofTestGate(f.source)
	for _, mutate := range []func(*HomeFixtureInputs){
		func(i *HomeFixtureInputs) { i.BinarySHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.FixtureSHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.SandboxSHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.SystemPolicySHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.EnvironmentSHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.HostSHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.AncestorsSHA256 = strings.Repeat("b", 64) },
		func(i *HomeFixtureInputs) { i.StageDirectory = "/synthetic/other-fixture" },
	} {
		inputs := direct.Inputs
		mutate(&inputs)
		hit, err := f.store.ResolveHomeFixtureReuse(f.workID, f.source, gate, inputs, fixedNow.Add(10*time.Second))
		if err != nil || hit != nil {
			t.Fatalf("changed input reused: hit=%+v err=%v", hit, err)
		}
	}
	source := f.source
	source.RepositoryFingerprint = strings.Repeat("b", 64)
	hit, err := f.store.ResolveHomeFixtureReuse(f.workID, source, gate, direct.Inputs, fixedNow.Add(10*time.Second))
	if err != nil || hit != nil {
		t.Fatalf("different repository reused: %+v %v", hit, err)
	}
	checkout := filepath.Join(t.TempDir(), "second-checkout")
	landingGit(t, f.repo, "worktree", "add", "--detach", checkout, f.source.HeadSHA)
	selected, err := f.store.ResolveHomeFixtureReuse(f.workID, f.source, gate, direct.Inputs, fixedNow.Add(10*time.Second))
	if err != nil || selected == nil {
		t.Fatalf("select: %+v %v", selected, err)
	}
	second, err := f.complete(t, checkout, selected, true, fixedNow.Add(11*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.VerificationRef == "" || second.VerificationRef == first.VerificationRef {
		t.Fatal("new checkout lacks new committed proof")
	}
	// The second report is reused, so only the first directly executed origin
	// can donate to a third invocation.
	third, err := f.store.ResolveHomeFixtureReuse(f.workID, f.source, gate, direct.Inputs, fixedNow.Add(20*time.Second))
	if err != nil || third == nil || third.OriginVerificationRef != first.VerificationRef {
		t.Fatalf("transitive origin selected: %+v %v", third, err)
	}
}

func TestHomeFixtureReuseRejectsOriginTamperAtConsumption(t *testing.T) {
	f := newHomeProofFixture(t)
	direct, _ := NewHomeFixtureEvidence(homeFixtureTestInputs())
	first, err := f.complete(t, f.repo, direct, true, fixedNow.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := f.store.ResolveHomeFixtureReuse(f.workID, f.source, homeProofTestGate(f.source), direct.Inputs, fixedNow.Add(10*time.Second))
	if err != nil || selected == nil {
		t.Fatal(err)
	}
	digest, err := home.ArtifactHex(first.Result.ReportArtifactRef)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately bypass the immutable write door to model disk corruption.
	if err := os.WriteFile(filepath.Join(f.store.home, "artifacts", "sha256", digest), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = f.complete(t, f.repo, selected, true, fixedNow.Add(11*time.Second))
	if err == nil {
		t.Fatal("origin corruption after selection was accepted")
	}
}

func TestHomeFixtureEvidenceRefusesUnsealedInputsAndUnboundOrigin(t *testing.T) {
	inputs := homeFixtureTestInputs()
	direct, err := NewHomeFixtureEvidence(inputs)
	if err != nil {
		t.Fatal(err)
	}
	inputBody, _ := json.Marshal(inputs)
	if len(inputBody) == 0 || !reflect.DeepEqual(direct.Inputs, inputs) {
		t.Fatal("input identity lost")
	}
	direct.Inputs.HostSHA256 = strings.Repeat("b", 64)
	if direct.validate() == nil {
		t.Fatal("changed inputs accepted under old digest")
	}
	inputs.StageDirectory = "relative"
	if _, err := NewHomeFixtureEvidence(inputs); err == nil {
		t.Fatal("relative fixture accepted")
	}
	e, _ := NewHomeFixtureEvidence(homeFixtureTestInputs())
	e.OriginGeneratedAt = fixedNow.Format(time.RFC3339)
	if e.validate() == nil {
		t.Fatal("unbound origin timestamp accepted")
	}
}

func TestHomeFixtureLegacyV05ProofReadableButCannotDonate(t *testing.T) {
	f := newHomeProofFixture(t)
	completed, err := f.complete(t, f.repo, nil, true, fixedNow.Add(3*time.Second), "compound.proof_check_report.v0.5")
	if err != nil || completed.VerificationRef == "" {
		t.Fatalf("legacy native proof refused: %+v %v", completed, err)
	}
	selected, err := f.store.ResolveHomeFixtureReuse(f.workID, f.source, homeProofTestGate(f.source), homeFixtureTestInputs(), fixedNow.Add(20*time.Second))
	if err != nil || selected != nil {
		t.Fatalf("legacy proof donated fixture reuse: %+v %v", selected, err)
	}
}
