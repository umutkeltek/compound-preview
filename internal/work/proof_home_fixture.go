package work

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
)

const HomeFixtureEvidenceSchema = "compound.proof_home_fixture.v0.1"
const HomeFixtureGateName = "go-test-home"
const HomeFixtureProofRef = "go-test-count-1-home"

// Inputs describe the restricted, source-only fixture, including the external
// directory observations its rooted filesystem tests require. These are input
// seals, not an independent cache authority.
type HomeFixtureInputs struct {
	StageDirectory     string `json:"stage_directory"`
	BinarySHA256       string `json:"binary_sha256"`
	FixtureSHA256      string `json:"fixture_sha256"`
	SandboxSHA256      string `json:"sandbox_sha256"`
	SystemPolicySHA256 string `json:"system_policy_sha256"`
	EnvironmentSHA256  string `json:"environment_sha256"`
	HostSHA256         string `json:"host_sha256"`
	AncestorsSHA256    string `json:"ancestors_sha256"`
}

type HomeFixtureEvidence struct {
	SchemaVersion         string            `json:"schema_version"`
	Inputs                HomeFixtureInputs `json:"inputs"`
	InputSHA256           string            `json:"input_sha256"`
	OriginVerificationRef string            `json:"origin_verification_ref,omitempty"`
	OriginGeneratedAt     string            `json:"origin_generated_at,omitempty"`
}

var homeFixtureDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var homeFixtureVerification = regexp.MustCompile(`^proof-check:pfrs_[0-9a-f]{12}$`)

func NewHomeFixtureEvidence(inputs HomeFixtureInputs) (*HomeFixtureEvidence, error) {
	body, err := json.Marshal(inputs)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(body)
	evidence := &HomeFixtureEvidence{SchemaVersion: HomeFixtureEvidenceSchema, Inputs: inputs, InputSHA256: hex.EncodeToString(hash[:])}
	return evidence, evidence.validate()
}

func (e *HomeFixtureEvidence) validate() error {
	if e == nil || e.SchemaVersion != HomeFixtureEvidenceSchema || !filepath.IsAbs(e.Inputs.StageDirectory) || filepath.Clean(e.Inputs.StageDirectory) != e.Inputs.StageDirectory {
		return fmt.Errorf("invalid home fixture evidence identity")
	}
	for _, digest := range []string{e.Inputs.BinarySHA256, e.Inputs.FixtureSHA256, e.Inputs.SandboxSHA256, e.Inputs.SystemPolicySHA256, e.Inputs.EnvironmentSHA256, e.Inputs.HostSHA256, e.Inputs.AncestorsSHA256} {
		if !homeFixtureDigest.MatchString(digest) {
			return fmt.Errorf("home fixture input seal is missing or malformed")
		}
	}
	body, _ := json.Marshal(e.Inputs)
	hash := sha256.Sum256(body)
	if e.InputSHA256 != hex.EncodeToString(hash[:]) {
		return fmt.Errorf("home fixture input digest mismatch")
	}
	if e.OriginVerificationRef == "" {
		if e.OriginGeneratedAt != "" {
			return fmt.Errorf("home fixture origin time without origin")
		}
	} else if !homeFixtureVerification.MatchString(e.OriginVerificationRef) {
		return fmt.Errorf("invalid home fixture origin reference")
	} else if _, err := time.Parse(time.RFC3339Nano, e.OriginGeneratedAt); err != nil {
		return fmt.Errorf("invalid home fixture origin time")
	}
	return nil
}

func validHomeFixtureGate(step ProofCheckStep, source *gitrepository.Snapshot) bool {
	return source != nil && step.Name == HomeFixtureGateName && step.ProofRef == HomeFixtureProofRef && step.Dir == "" && step.SkipIfMissing == "" &&
		reflect.DeepEqual(step.Command, []string{"go", "run", "./scripts/testbattery", "home", "--source-head", source.HeadSHA})
}

// ResolveHomeFixtureReuse considers only directly executed, committed successful
// reports already linked to this Work. Missing or incompatible history is a miss.
// Completion validates any selected origin again before it becomes authority.
func (s *Store) ResolveHomeFixtureReuse(workID string, source gitrepository.Snapshot, gate ProofCheckStep, inputs HomeFixtureInputs, now time.Time) (*HomeFixtureEvidence, error) {
	fresh, err := NewHomeFixtureEvidence(inputs)
	if err != nil {
		return nil, err
	}
	if !validHomeFixtureGate(gate, &source) {
		return nil, fmt.Errorf("home fixture gate identity mismatch")
	}
	w, err := s.Get(workID)
	if err != nil {
		return nil, err
	}
	for i := len(w.VerificationIDs) - 1; i >= 0; i-- {
		ref := w.VerificationIDs[i]
		origin, _, err := s.loadDirectHomeFixtureOrigin(w, ref, now)
		if err != nil || !reflect.DeepEqual(origin.SourceSnapshot, &source) {
			continue
		}
		for _, step := range origin.Steps {
			e := step.HomeFixtureEvidence
			if step.Name != gate.Name || e == nil || e.InputSHA256 != fresh.InputSHA256 || !reflect.DeepEqual(step.Command, gate.Command) || step.ProofRef != gate.ProofRef {
				continue
			}
			selected := *fresh
			selected.OriginVerificationRef = ref
			selected.OriginGeneratedAt = origin.GeneratedAt
			return &selected, nil
		}
	}
	return nil, nil
}

func (s *Store) loadDirectHomeFixtureOrigin(w envelope.WorkEnvelope, ref string, before time.Time) (ownedProofReport, ProofResourceResult, error) {
	var result ProofResourceResult
	if !homeFixtureVerification.MatchString(ref) || !oneOf(ref, w.VerificationIDs) {
		return ownedProofReport{}, result, fmt.Errorf("home fixture origin is not linked to this work")
	}
	if err := s.readEventJSON(strings.TrimPrefix(ref, "proof-check:"), &result); err != nil {
		return ownedProofReport{}, result, err
	}
	if result.WorkID != w.ID || result.VerificationRef != ref {
		return ownedProofReport{}, result, fmt.Errorf("home fixture origin work mismatch")
	}
	completed, err := time.Parse(time.RFC3339Nano, result.CreatedAt)
	if err != nil || !completed.Before(before) {
		return ownedProofReport{}, result, fmt.Errorf("home fixture origin is not prior completed evidence")
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return ownedProofReport{}, result, err
	}
	body, err := artifacts.Get(result.ReportArtifactRef)
	if err != nil {
		return ownedProofReport{}, result, err
	}
	report, err := decodeOwnedProofReport(body)
	if err != nil {
		return report, result, err
	}
	if report.SchemaVersion != ProofCheckSchemaVersion && report.SchemaVersion != RemoteProofReportSchemaVersion {
		return report, result, fmt.Errorf("legacy report cannot donate home fixture proof")
	}
	generated, err := time.Parse(time.RFC3339Nano, report.GeneratedAt)
	if err != nil || !generated.Before(before) {
		return report, result, fmt.Errorf("home fixture origin is not prior execution")
	}
	// Refuse a reused origin before traversing its chain: validation is one hop.
	for _, step := range report.Steps {
		if step.HomeFixtureEvidence != nil && step.HomeFixtureEvidence.OriginVerificationRef != "" {
			return report, result, fmt.Errorf("home fixture origin was itself reused")
		}
	}
	if err := s.validateCommittedProofResourceChain(w, result, report); err != nil {
		return report, result, err
	}
	return report, result, nil
}

func (s *Store) validateHomeFixtureReport(workID string, report ownedProofReport) error {
	for _, step := range report.Steps {
		evidence := step.HomeFixtureEvidence
		if evidence == nil {
			continue
		}
		gate := ProofCheckStep{Name: step.Name, Command: step.Command, Dir: step.Dir, SkipIfMissing: step.SkipIfMissing, ProofRef: step.ProofRef}
		if (report.SchemaVersion != ProofCheckSchemaVersion && report.SchemaVersion != RemoteProofReportSchemaVersion) || step.Status != "ok" || !validHomeFixtureGate(gate, report.SourceSnapshot) {
			return fmt.Errorf("home fixture evidence is not bound to its source gate")
		}
		if err := evidence.validate(); err != nil {
			return err
		}
		if evidence.OriginVerificationRef == "" {
			continue
		}
		generated, err := time.Parse(time.RFC3339Nano, report.GeneratedAt)
		if err != nil {
			return err
		}
		w, err := s.Get(workID)
		if err != nil {
			return err
		}
		origin, _, err := s.loadDirectHomeFixtureOrigin(w, evidence.OriginVerificationRef, generated)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(origin.SourceSnapshot, report.SourceSnapshot) || origin.GeneratedAt != evidence.OriginGeneratedAt {
			return fmt.Errorf("home fixture origin source/time mismatch")
		}
		matched := false
		for _, prior := range origin.Steps {
			if prior.Name == step.Name && prior.ProofRef == step.ProofRef && reflect.DeepEqual(prior.Command, step.Command) && prior.HomeFixtureEvidence != nil && prior.HomeFixtureEvidence.InputSHA256 == evidence.InputSHA256 {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("home fixture origin input/gate mismatch")
		}
	}
	return nil
}
