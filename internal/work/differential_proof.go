package work

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"compound/internal/authority/gitrepository"
	"compound/internal/envelope"
	"compound/internal/home"
)

const DifferentialProofVerificationPrefix = "proof-diff:"

// ProofFailureFacts seals deterministic unique facts from complete failing output.
// Known host-coupled failures are reduced to stable semantic facts; other failures
// retain exact normalized line hashes.
func ProofFailureFacts(gate string, output []byte, runErr error) []string {
	if runErr == nil {
		return nil
	}
	labels, fullyNormalized := semanticProofFailureLabels(gate, string(output))
	if !fullyNormalized {
		unique := make(map[string]struct{})
		for _, line := range strings.Split(string(output), "\n") {
			normalized := strings.Join(strings.Fields(line), " ")
			if normalized == "" {
				continue
			}
			if gate == "go-test" || gate == HomeFixtureGateName {
				if semantic, keep := semanticGoTestFallbackLine(normalized); !keep {
					continue
				} else if semantic != "" {
					unique[semantic] = struct{}{}
					continue
				}
			}
			if semantic := semanticBunCacheFailureLabel(gate, normalized); semantic != "" {
				unique[semantic] = struct{}{}
			} else {
				unique[normalized] = struct{}{}
			}
		}
		if len(unique) == 0 {
			if normalized := strings.Join(strings.Fields(runErr.Error()), " "); normalized != "" {
				unique[normalized] = struct{}{}
			}
		}
		for line := range unique {
			labels = append(labels, line)
		}
	}
	facts := make([]string, 0, len(labels))
	for _, label := range labels {
		sum := sha256.Sum256([]byte(label))
		facts = append(facts, "sha256:"+hex.EncodeToString(sum[:]))
	}
	sort.Strings(facts)
	return facts
}

var (
	goTimeoutHeaderPattern         = regexp.MustCompile(`^panic: test timed out after \S+$`)
	goTimeoutTestPattern           = regexp.MustCompile(`^[A-Za-z0-9_./-]+ \([^)]*\)$`)
	goGoroutinePattern             = regexp.MustCompile(`^goroutine [0-9]+(?: gp=.*)? \[([^]]+)\]:$`)
	goPassSummaryPattern           = regexp.MustCompile(`^ok\s+\S+\s+[0-9.]+s$`)
	goNoTestsPattern               = regexp.MustCompile(`^\?\s+\S+\s+\[no test files\]$`)
	goFailPackagePattern           = regexp.MustCompile(`^FAIL	(\S+)	[0-9.]+s$`)
	goNormalizedFailPackagePattern = regexp.MustCompile(`^FAIL\s+(\S+)\s+[0-9.]+s$`)
	goFailTestPattern              = regexp.MustCompile(`^--- FAIL: (\S+) \([^)]*\)$`)
	bunMissingPattern              = regexp.MustCompile(`Cannot find (?:package|module) '([^']+)'`)
)

func semanticGoTestFallbackLine(line string) (string, bool) {
	if goPassSummaryPattern.MatchString(line) || goNoTestsPattern.MatchString(line) {
		return "", false
	}
	if match := goFailTestPattern.FindStringSubmatch(line); len(match) == 2 {
		return "go-test:failed-test:" + match[1], true
	}
	if match := goNormalizedFailPackagePattern.FindStringSubmatch(line); len(match) == 2 {
		return "go-test:failed-package:" + match[1], true
	}
	return "", true
}

func semanticProofFailureLabels(gate, output string) ([]string, bool) {
	if (gate != "go-test" && gate != HomeFixtureGateName) || !strings.Contains(output, "panic: test timed out after") {
		return nil, false
	}
	labels := map[string]bool{}
	timeoutTests := map[string]bool{}
	phase := "before-timeout"
	expectSource := false
	sawGoroutine := false
	currentWaitState := ""
	currentFrames := []string{}
	pendingFunction := ""
	finalizeGoroutine := func() {
		for _, testName := range sortedTrueKeys(timeoutTests) {
			for i, frame := range currentFrames {
				if strings.HasSuffix(frame, "."+testName) {
					root := currentFrames[:i+1]
					labels["go-test:timeout-root:"+testName+":"+currentWaitState+":"+strings.Join(root, "|")] = true
					break
				}
			}
		}
		currentWaitState = ""
		currentFrames = nil
		pendingFunction = ""
	}
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch phase {
		case "before-timeout":
			if goTimeoutHeaderPattern.MatchString(line) {
				labels["go-test:timeout"] = true
				phase = "running-tests-header"
				continue
			}
			if goPassSummaryPattern.MatchString(line) || goNoTestsPattern.MatchString(line) {
				continue
			}
			return nil, false
		case "running-tests-header":
			if line != "running tests:" {
				return nil, false
			}
			phase = "running-tests"
			continue
		case "running-tests":
			if strings.HasPrefix(raw, "		") && goTimeoutTestPattern.MatchString(line) {
				testName := strings.Fields(line)[0]
				labels["go-test:timeout-test:"+testName] = true
				timeoutTests[testName] = true
				continue
			}
			if match := goGoroutinePattern.FindStringSubmatch(line); len(match) == 2 {
				sawGoroutine = true
				currentWaitState = normalizeGoWaitState(match[1])
				phase = "stack"
				continue
			}
			return nil, false
		case "stack":
			if match := goFailPackagePattern.FindStringSubmatch(raw); len(match) == 2 {
				if expectSource {
					return nil, false
				}
				finalizeGoroutine()
				labels["go-test:failed-package:"+match[1]] = true
				phase = "after-failure"
				continue
			}
			if match := goGoroutinePattern.FindStringSubmatch(line); len(match) == 2 {
				if expectSource {
					return nil, false
				}
				finalizeGoroutine()
				sawGoroutine = true
				currentWaitState = normalizeGoWaitState(match[1])
				continue
			}
			if expectSource {
				if !strings.HasPrefix(raw, "	") || (!strings.Contains(line, ".go:") && !strings.Contains(line, "_testmain.go:")) {
					return nil, false
				}
				expectSource = false
				currentFrames = append(currentFrames, pendingFunction)
				pendingFunction = ""
				continue
			}
			if strings.HasPrefix(line, "created by ") || (strings.Contains(line, "(") && (strings.HasSuffix(line, ")") || strings.HasSuffix(line, "(...)"))) {
				pendingFunction = normalizeGoFunction(line)
				expectSource = true
				continue
			}
			return nil, false
		case "after-failure":
			if goPassSummaryPattern.MatchString(line) || goNoTestsPattern.MatchString(line) || line == "FAIL" {
				continue
			}
			return nil, false
		}
	}
	if phase != "after-failure" || !sawGoroutine {
		return nil, false
	}
	timeoutTestCount := 0
	timeoutRoots := 0
	for label := range labels {
		if strings.HasPrefix(label, "go-test:timeout-test:") {
			timeoutTestCount++
		}
		if strings.HasPrefix(label, "go-test:timeout-root:") {
			timeoutRoots++
		}
	}
	if timeoutTestCount == 0 || timeoutRoots != timeoutTestCount {
		return nil, false
	}
	out := make([]string, 0, len(labels))
	for label := range labels {
		out = append(out, label)
	}
	sort.Strings(out)
	return out, true
}

func normalizeGoWaitState(state string) string {
	return strings.TrimSpace(strings.SplitN(state, ",", 2)[0])
}

func normalizeGoFunction(line string) string {
	if strings.HasPrefix(line, "created by ") {
		line = strings.TrimPrefix(line, "created by ")
		if in := strings.LastIndex(line, " in goroutine "); in >= 0 {
			line = line[:in]
		}
		return strings.TrimSpace(line)
	}
	if in := strings.LastIndexByte(line, '('); in >= 0 {
		line = line[:in]
	}
	return strings.TrimSpace(line)
}

func sortedTrueKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key, present := range values {
		if present {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func semanticBunCacheFailureLabel(gate, line string) string {
	if !strings.HasPrefix(gate, "console-") || !strings.Contains(line, "/.bun/install/cache/") {
		return ""
	}
	identity := ""
	if match := bunMissingPattern.FindStringSubmatch(line); len(match) == 2 {
		identity = bunPackageRoot(match[1])
	} else if strings.Contains(line, "EISDIR reading") {
		cachePath := strings.SplitN(line, "/.bun/install/cache/", 2)
		if len(cachePath) == 2 {
			identity = bunCachePathPackage(cachePath[1])
		}
	}
	if identity == "" {
		return ""
	}
	return "environment:bun-module-cache-resolution:" + identity
}

func bunPackageRoot(name string) string {
	parts := strings.Split(strings.TrimSpace(name), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	if strings.HasPrefix(parts[0], "@") {
		if len(parts) < 2 || parts[1] == "" {
			return ""
		}
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func bunCachePathPackage(cachePath string) string {
	component := strings.Trim(cachePath, `"`)
	if slash := strings.IndexByte(component, '/'); slash >= 0 {
		component = component[:slash]
	}
	if strings.HasPrefix(component, "@") {
		if at := strings.Index(component[1:], "@"); at >= 0 {
			component = component[:at+1]
		}
	} else if at := strings.Index(component, "@"); at >= 0 {
		component = component[:at]
	}
	return bunPackageRoot(component)
}

func (s *Store) loadCommittedFailedProofResult(id string) (envelope.WorkEnvelope, ProofResourceResult, ownedProofReport, error) {
	if !strings.HasPrefix(id, "pfrs_") {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, fmt.Errorf("invalid result identity %q", id)
	}
	var result ProofResourceResult
	if err := s.readEventJSON(id, &result); err != nil {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, err
	}
	if result.ID != id {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, fmt.Errorf("result identity mismatch")
	}
	w, err := s.Get(result.WorkID)
	if err != nil {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, err
	}
	artifacts, err := home.NewArtifactStore(s.home)
	if err != nil {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, err
	}
	body, err := artifacts.Get(result.ReportArtifactRef)
	if err != nil {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, err
	}
	report, err := decodeOwnedProofReport(body)
	if err != nil {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, err
	}
	if err := s.validateCommittedFailedProofResourceChain(w, result, report); err != nil {
		return envelope.WorkEnvelope{}, ProofResourceResult{}, ownedProofReport{}, err
	}
	return w, result, report, nil
}

func validateDifferentialProofComparison(candidate ProofResourceResult, candidateReport ownedProofReport, baseline ProofResourceResult, baselineReport ownedProofReport) error {
	if candidate.Status != ProofResourceFailed || baseline.Status != ProofResourceFailed || candidateReport.Status != "failed" || baselineReport.Status != "failed" {
		return fmt.Errorf("differential proof requires canonical failed candidate and baseline results")
	}
	if candidate.SourceSnapshot == nil || baseline.SourceSnapshot == nil || candidateReport.SourceSnapshot == nil || baselineReport.SourceSnapshot == nil {
		return fmt.Errorf("differential proof requires sealed source snapshots")
	}
	for label, snapshot := range map[string]gitrepository.Snapshot{"candidate": *candidate.SourceSnapshot, "baseline": *baseline.SourceSnapshot} {
		if err := gitrepository.ValidateSnapshotShape(snapshot); err != nil {
			return fmt.Errorf("%s source snapshot: %w", label, err)
		}
		if !snapshot.Clean {
			return fmt.Errorf("%s source snapshot is not clean", label)
		}
	}
	if baseline.SourceSnapshot.RepositoryFingerprint != candidate.SourceSnapshot.RepositoryFingerprint {
		return fmt.Errorf("candidate and baseline repository fingerprints differ")
	}
	if baseline.SourceSnapshot.HeadSHA != candidate.SourceSnapshot.BaseSHA {
		return fmt.Errorf("baseline HEAD %s is not candidate base %s", baseline.SourceSnapshot.HeadSHA, candidate.SourceSnapshot.BaseSHA)
	}
	if *candidate.SourceSnapshot != *candidateReport.SourceSnapshot || *baseline.SourceSnapshot != *baselineReport.SourceSnapshot {
		return fmt.Errorf("proof result source does not match content-addressed report")
	}
	if err := compareDifferentialGateInventory(candidateReport, baselineReport); err != nil {
		return err
	}
	baselineSteps := make(map[string]struct {
		status string
		facts  map[string]bool
	}, len(baselineReport.Steps))
	for _, step := range baselineReport.Steps {
		facts := make(map[string]bool, len(step.FailureFacts))
		for _, fact := range step.FailureFacts {
			if !validDifferentialFailureFact(fact) {
				return fmt.Errorf("baseline failed gate %s has malformed failure_facts", step.Name)
			}
			facts[fact] = true
		}
		baselineSteps[stepIdentity(step.Name, step.ProofRef)] = struct {
			status string
			facts  map[string]bool
		}{step.Status, facts}
	}
	for _, step := range candidateReport.Steps {
		if step.Status != "failed" {
			continue
		}
		if len(step.FailureFacts) == 0 {
			return fmt.Errorf("candidate failed gate %s has no failure_facts", step.Name)
		}
		base := baselineSteps[stepIdentity(step.Name, step.ProofRef)]
		if base.status != "failed" || len(base.facts) == 0 {
			return fmt.Errorf("candidate failed gate %s is not an attributable baseline failure", step.Name)
		}
		for _, fact := range step.FailureFacts {
			if !validDifferentialFailureFact(fact) {
				return fmt.Errorf("candidate failed gate %s has malformed failure_facts", step.Name)
			}
			if !base.facts[fact] {
				return fmt.Errorf("candidate failed gate %s fact %s is not present on baseline", step.Name, fact)
			}
		}
	}
	return nil
}

func validDifferentialFailureFact(fact string) bool {
	if !strings.HasPrefix(fact, "sha256:") || len(fact) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(fact, "sha256:"))
	return err == nil
}

func compareDifferentialGateInventory(candidate, baseline ownedProofReport) error {
	if len(candidate.Steps) != len(baseline.Steps) {
		return fmt.Errorf("candidate and baseline gate inventory length differs")
	}
	for i := range candidate.Steps {
		c, b := candidate.Steps[i], baseline.Steps[i]
		if c.Name != b.Name || c.Dir != b.Dir || c.SkipIfMissing != b.SkipIfMissing || c.ProofRef != b.ProofRef ||
			!reflect.DeepEqual(normalizeDifferentialCommand(c.Command, *candidate.SourceSnapshot), normalizeDifferentialCommand(b.Command, *baseline.SourceSnapshot)) {
			return fmt.Errorf("candidate and baseline gate inventory differs at step %d", i)
		}
	}
	return nil
}

func normalizeDifferentialCommand(command []string, source gitrepository.Snapshot) []string {
	out := make([]string, len(command))
	for i, token := range command {
		token = strings.ReplaceAll(token, source.BaseSHA, "$BASE")
		token = strings.ReplaceAll(token, source.HeadSHA, "$HEAD")
		out[i] = token
	}
	return out
}

func stepIdentity(name, proofRef string) string { return name + "\x00" + proofRef }

func differentialProofVerificationRef(candidateID, baselineID string) string {
	return DifferentialProofVerificationPrefix + candidateID + ":" + baselineID
}

func parseDifferentialProofVerificationRef(ref string) (candidateID, baselineID string, ok bool) {
	parts := strings.Split(ref, ":")
	if len(parts) != 3 || parts[0] != strings.TrimSuffix(DifferentialProofVerificationPrefix, ":") || !strings.HasPrefix(parts[1], "pfrs_") || !strings.HasPrefix(parts[2], "pfrs_") {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func (s *Store) validateDifferentialProofRef(w envelope.WorkEnvelope, ref string) (ProofResourceResult, error) {
	candidateID, baselineID, ok := parseDifferentialProofVerificationRef(ref)
	if !ok {
		return ProofResourceResult{}, fmt.Errorf("differential proof verification %q is malformed", ref)
	}
	candidateWork, candidate, candidateReport, err := s.loadCommittedFailedProofResult(candidateID)
	if err != nil {
		return ProofResourceResult{}, fmt.Errorf("candidate proof chain: %w", err)
	}
	if candidateWork.ID != w.ID {
		return ProofResourceResult{}, fmt.Errorf("differential candidate belongs to work %s, not %s", candidateWork.ID, w.ID)
	}
	_, baseline, baselineReport, err := s.loadCommittedFailedProofResult(baselineID)
	if err != nil {
		return ProofResourceResult{}, fmt.Errorf("baseline proof chain: %w", err)
	}
	if err := validateDifferentialProofComparison(candidate, candidateReport, baseline, baselineReport); err != nil {
		return ProofResourceResult{}, err
	}
	want := differentialProofVerificationRef(candidate.ID, baseline.ID)
	if want != ref {
		return ProofResourceResult{}, fmt.Errorf("differential proof verification identity mismatch")
	}
	candidate.VerificationRef = ref
	return candidate, nil
}
