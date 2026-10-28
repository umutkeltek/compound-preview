package work

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"compound/internal/authority/gitrepository"
)

const (
	CanonicalProofPlanCompoundV1      = "compound_v1"
	CanonicalProofPlanRustWorkspaceV1 = "rust_workspace_v1"
)

// ProofCheckStep is one executable proof gate plus its eventual result. The same type crosses
// canonical plan selection, admission, execution, report serialization, and differential proof.
type ProofCheckStep struct {
	Name          string   `json:"name"`
	Command       []string `json:"command"`
	Dir           string   `json:"dir,omitempty"`
	SkipIfMissing string   `json:"skip_if_missing,omitempty"`
	SkipReason    string   `json:"skip_reason,omitempty"`
	ProofRef      string   `json:"proof_ref"`
	Status        string   `json:"status"`
	ExitCode      int      `json:"exit_code,omitempty"`
	DurationMS    int64    `json:"duration_ms,omitempty"`
	OutputTail    string   `json:"output_tail,omitempty"`
	// OutputPath is the absolute path to the FULL (untruncated) gate output when a failed gate
	// was persisted next to the --out report (<out>.gates/<gate>.log). Empty for ok/skipped gates
	// or when no --out report is in flight. Recorded alongside OutputTail so a triage reader can
	// recover the real failure context, not only its 4000-char tail.
	OutputPath string `json:"output_path,omitempty"`
	// FailureLines is the bounded set of canonical failure markers extracted from the gate's
	// full output (--- FAIL:, FAIL\t, panic:, timed out). Populated for go-test gates today;
	// other gates leave it nil because their failure markers are gate-specific and out of scope
	// for a generic on-the-wire field. Capped to keep report size predictable under a red
	// battery that fans out dozens of FAIL lines per package.
	FailureLines          []string                   `json:"failure_lines,omitempty"`
	FailureFacts          []string                   `json:"failure_facts,omitempty"`
	ConsumerEvidence      *CanonicalConsumerEvidence `json:"consumer_evidence,omitempty"`
	RedactionEvidence     *PublicRedactionEvidence   `json:"redaction_evidence,omitempty"`
	PrivateSourceEvidence *PrivateSourceEvidence     `json:"private_source_evidence,omitempty"`
	RemoteEvidence        *RemoteProofEvidence       `json:"remote_evidence,omitempty"`
	HomeFixtureEvidence   *HomeFixtureEvidence       `json:"home_fixture_evidence,omitempty"`
	// CIPortable marks a fresh-clone CI gate: no external non-Go binary, installed binary,
	// or COMPOUND_HOME body. It is runtime inventory metadata, not serialized proof output.
	CIPortable bool `json:"-"`
}

// CanonicalProofInventory supplies Compound's command-owned gate inventory. The work owner keeps
// repository classification and Rust policy deep while the command adapter retains its existing
// home-owning gate runners and executable substitutions.
type CanonicalProofInventory func(baseRef string, headRefs ...string) ([]ProofCheckStep, error)

// CanonicalProofPlan is the immutable, source-bound inventory shared by proof admission and
// execution. Its ID is descriptive; ProofResourceProfile's digest remains the authority because
// it seals the repository, source snapshot, and every ordered gate identity.
type CanonicalProofPlan struct {
	ID     string
	Repo   string
	Source gitrepository.Snapshot
	Steps  []ProofCheckStep
}

type canonicalProofPlanContextKey struct{}

func ResolveCanonicalProofPlan(
	repo string,
	source gitrepository.Snapshot,
	compoundInventory CanonicalProofInventory,
) (CanonicalProofPlan, error) {
	return ResolveCanonicalProofPlanContext(context.Background(), repo, source, compoundInventory)
}

// ResolveCanonicalProofPlanContext cancels committed inventory reads with their owner.
func ResolveCanonicalProofPlanContext(ctx context.Context, repo string, source gitrepository.Snapshot, inventory CanonicalProofInventory) (CanonicalProofPlan, error) {
	return resolveCanonicalProofPlanContext(ctx, repo, source, inventory, false)
}

// ValidateCanonicalProofProfile checks the real committed profile declaration
// without running gates or manufacturing an inventory or proof certificate.
func ValidateCanonicalProofProfile(repo string, source gitrepository.Snapshot) error {
	_, err := resolveCanonicalProofPlanContext(context.Background(), repo, source, nil, true)
	return err
}

func resolveCanonicalProofPlan(repo string, source gitrepository.Snapshot, inventory CanonicalProofInventory, profileOnly bool) (CanonicalProofPlan, error) {
	return resolveCanonicalProofPlanContext(context.Background(), repo, source, inventory, profileOnly)
}

func resolveCanonicalProofPlanContext(
	ctx context.Context,
	repo string,
	source gitrepository.Snapshot,
	compoundInventory CanonicalProofInventory,
	profileOnly bool,
) (CanonicalProofPlan, error) {
	resolvedRepo, err := resolveCanonicalProofRepo(repo)
	if err != nil {
		return CanonicalProofPlan{}, err
	}
	if err := gitrepository.ValidateSnapshotShape(source); err != nil {
		return CanonicalProofPlan{}, fmt.Errorf("canonical proof source: %w", err)
	}
	if !source.Clean {
		return CanonicalProofPlan{}, fmt.Errorf("canonical proof source must be clean")
	}

	markers, err := canonicalProofRootMarkersContext(ctx,
		resolvedRepo,
		source.HeadSHA,
		[]string{"Cargo.lock", "Cargo.toml", "go.mod", "package.json", "bun.lock", "pyproject.toml", "uv.lock", CanonicalConsumerContractPath},
	)
	if err != nil {
		return CanonicalProofPlan{}, fmt.Errorf("inspect canonical proof root markers at %s: %w", source.HeadSHA, err)
	}
	hasCargoManifest := markers["Cargo.toml"]
	hasCargoLock := markers["Cargo.lock"]
	hasGoModule := markers["go.mod"]
	hasPackageJSON := markers["package.json"]
	hasBunLock := markers["bun.lock"]
	hasPyproject := markers["pyproject.toml"]
	hasUVLock := markers["uv.lock"]
	contractPath, err := canonicalConsumerDeclarationPathContext(ctx, resolvedRepo, source.HeadSHA)
	if err != nil {
		return CanonicalProofPlan{}, err
	}

	plan := CanonicalProofPlan{Repo: resolvedRepo, Source: source}
	switch {
	case contractPath != "":
		if hasCargoManifest || hasCargoLock {
			return CanonicalProofPlan{}, fmt.Errorf("external consumer contract cannot replace Rust canonical inventory")
		}
		body, blobErr := canonicalProofGitBlobContext(ctx, resolvedRepo, source.HeadSHA, contractPath)
		if blobErr != nil {
			return CanonicalProofPlan{}, blobErr
		}
		contract, contractErr := parseCanonicalConsumerContract(body)
		if contractErr != nil {
			return CanonicalProofPlan{}, contractErr
		}
		if contractPath != CanonicalConsumerContractPath && contract.Profile != "node_npm_consumers_v1" {
			return CanonicalProofPlan{}, fmt.Errorf("nested consumer declaration requires Node/npm profile")
		}
		if contract.Profile == "python_uv_consumers_v1" && (!hasPyproject || !hasUVLock) {
			return CanonicalProofPlan{}, fmt.Errorf("Python consumers require tracked root pyproject.toml and uv.lock")
		}
		if contract.Profile != "python_uv_consumers_v1" && (hasPyproject || hasUVLock) {
			return CanonicalProofPlan{}, fmt.Errorf("consumer profile does not match tracked root Python markers")
		}
		if (contract.Profile == "go_consumers_v1") != hasGoModule {
			return CanonicalProofPlan{}, fmt.Errorf("consumer profile does not match tracked root Go module")
		}
		if contract.Profile == "bun_consumers_v1" && (!hasPackageJSON || !hasBunLock) {
			return CanonicalProofPlan{}, fmt.Errorf("Bun consumer profile requires tracked root package.json and bun.lock")
		}
		if contract.Profile != "bun_consumers_v1" && hasBunLock {
			return CanonicalProofPlan{}, fmt.Errorf("consumer profile does not match tracked root Bun package markers")
		}
		if hasGoModule {
			moduleBody, blobErr := canonicalProofGitBlobContext(ctx, resolvedRepo, source.HeadSHA, "go.mod")
			if blobErr != nil {
				return CanonicalProofPlan{}, blobErr
			}
			if isCanonicalCompoundModule(moduleBody) {
				return CanonicalProofPlan{}, fmt.Errorf("external consumer contract cannot replace Compound canonical inventory")
			}
		}
		plan.ID = contract.Profile
		for _, consumer := range contract.Consumers {
			if contract.Profile == "node_npm_consumers_v1" {
				paths := []string{path.Join(consumer.Dir, "package.json"), path.Join(consumer.Dir, "package-lock.json")}
				packageMarkers, err := canonicalProofRootMarkersContext(ctx, resolvedRepo, source.HeadSHA, paths)
				if err != nil {
					return CanonicalProofPlan{}, err
				}
				if !packageMarkers[paths[0]] || !packageMarkers[paths[1]] {
					return CanonicalProofPlan{}, fmt.Errorf("Node consumer %s requires tracked regular package.json and package-lock.json in its directory", consumer.ID)
				}
			}
			plan.Steps = append(plan.Steps, canonicalConsumerStepAtPath(consumer, body, contractPath))
		}
		diffRange := source.BaseSHA + "..." + source.HeadSHA
		plan.Steps = append(plan.Steps,
			ProofCheckStep{Name: "diff-check", Command: []string{"git", "diff", "--check", diffRange}, ProofRef: "diff-check", CIPortable: true},
			ProofCheckStep{Name: "redact-public", Command: []string{"redact", "--repo-visibility", "public", "<committed blobs changed in " + diffRange + ">"}, ProofRef: "redact-public-clean"},
		)
	case hasCargoManifest && hasGoModule:
		return CanonicalProofPlan{}, fmt.Errorf("mixed root Cargo.toml and go.mod are not a canonical proof profile")
	case hasCargoManifest && !hasCargoLock:
		return CanonicalProofPlan{}, fmt.Errorf("Rust canonical proof requires a tracked root Cargo.lock")
	case hasCargoManifest:
		plan.ID = CanonicalProofPlanRustWorkspaceV1
		plan.Steps = rustWorkspaceProofCheckSteps(source.BaseSHA, source.HeadSHA)
	case hasCargoLock:
		return CanonicalProofPlan{}, fmt.Errorf("tracked root Cargo.lock without Cargo.toml is not a canonical proof profile")
	case hasGoModule:
		moduleBody, blobErr := canonicalProofGitBlobContext(ctx, resolvedRepo, source.HeadSHA, "go.mod")
		if blobErr != nil {
			return CanonicalProofPlan{}, blobErr
		}
		if !isCanonicalCompoundModule(moduleBody) {
			return CanonicalProofPlan{}, fmt.Errorf("canonical Compound proof requires tracked root go.mod declaring `module compound`")
		}
		plan.ID = CanonicalProofPlanCompoundV1
		if profileOnly {
			return cloneCanonicalProofPlan(plan), nil
		}
		if compoundInventory == nil {
			return CanonicalProofPlan{}, fmt.Errorf("canonical Compound proof inventory is unavailable")
		}
		plan.Steps, err = compoundInventory(source.BaseSHA, source.HeadSHA)
		if err != nil {
			return CanonicalProofPlan{}, err
		}
		if len(plan.Steps) == 0 {
			return CanonicalProofPlan{}, fmt.Errorf("canonical Compound proof inventory is empty")
		}
	default:
		return CanonicalProofPlan{}, fmt.Errorf("unsupported canonical proof repository: tracked root go.mod or Cargo.toml plus Cargo.lock required")
	}
	return cloneCanonicalProofPlan(plan), nil
}

func validConsumerContractPath(value string) bool {
	return value != "" && len(value) <= 4096 && !path.IsAbs(value) && path.Clean(value) == value && value != ".." &&
		!strings.HasPrefix(value, "../") && !strings.ContainsAny(value, "\\\x00\r\n") && path.Base(value) == CanonicalConsumerContractPath
}

// Existing root inventories always win. Only otherwise unsupported repositories
// can discover one nested declaration; a runtime flag never selects a weaker one.
func canonicalConsumerDeclarationPath(repo, head string) (string, error) {
	return canonicalConsumerDeclarationPathContext(context.Background(), repo, head)
}
func canonicalConsumerDeclarationPathContext(ctx context.Context, repo, head string) (string, error) {
	markers, err := canonicalProofRootMarkersContext(ctx, repo, head, []string{CanonicalConsumerContractPath, "go.mod", "Cargo.toml", "Cargo.lock", "package.json", "bun.lock", "pyproject.toml", "uv.lock"})
	if err != nil {
		return "", err
	}
	if markers[CanonicalConsumerContractPath] {
		return CanonicalConsumerContractPath, nil
	}
	if len(markers) != 0 {
		return "", nil
	}
	packageLock, err := canonicalProofRootMarkersContext(ctx, repo, head, []string{"package-lock.json"})
	if err != nil {
		return "", err
	}
	if len(packageLock) != 0 {
		return "", nil
	}
	// Include trees: a directory or submodule named like a declaration must not
	// disappear from recursive discovery and leave a different declaration chosen.
	cmd := gitrepository.CommandContext(ctx, "git", "ls-tree", "-r", "-t", "-z", "--full-tree", head)
	cmd.Dir = repo
	var output consumerTreeOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", context.Cause(ctx)
		}
		return "", fmt.Errorf("discover consumer declarations: %w", err)
	}
	selected := ""
	for _, record := range bytes.Split(output.Bytes(), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := strings.SplitN(string(record), "\t", 2)
		if len(parts) != 2 {
			return "", fmt.Errorf("malformed consumer Git tree")
		}
		if path.Base(parts[1]) != CanonicalConsumerContractPath {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) != 3 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || !validConsumerContractPath(parts[1]) {
			return "", fmt.Errorf("consumer declaration must be a clean regular Git blob")
		}
		if selected != "" {
			return "", fmt.Errorf("ambiguous nested consumer declarations")
		}
		selected = parts[1]
	}
	return selected, nil
}

type consumerTreeOutput struct{ bytes.Buffer }

func (b *consumerTreeOutput) Write(p []byte) (int, error) {
	if len(p) > (16<<20)-b.Len() {
		return 0, fmt.Errorf("consumer declaration Git tree exceeds 16 MiB discovery limit")
	}
	return b.Buffer.Write(p)
}

func canonicalProofRootMarkers(repo, headSHA string, paths []string) (map[string]bool, error) {
	return canonicalProofRootMarkersContext(context.Background(), repo, headSHA, paths)
}
func canonicalProofRootMarkersContext(ctx context.Context, repo, headSHA string, paths []string) (map[string]bool, error) {
	args := []string{"ls-tree", "-z", headSHA, "--"}
	args = append(args, paths...)
	records, err := canonicalProofGitNULListContext(ctx, repo, args...)
	if err != nil {
		return nil, err
	}
	markers := make(map[string]bool, len(records))
	for _, record := range records {
		metadataAndPath := strings.SplitN(record, "\t", 2)
		if len(metadataAndPath) != 2 {
			return nil, fmt.Errorf("malformed Git tree marker %q", record)
		}
		metadata := strings.Fields(metadataAndPath[0])
		path := strings.TrimSpace(metadataAndPath[1])
		if len(metadata) != 3 || path == "" {
			return nil, fmt.Errorf("malformed Git tree marker %q", record)
		}
		mode, objectType := metadata[0], metadata[1]
		if objectType != "blob" || (mode != "100644" && mode != "100755") {
			return nil, fmt.Errorf("canonical proof root marker %s must be a regular blob, got mode=%s type=%s", path, mode, objectType)
		}
		markers[path] = true
	}
	return markers, nil
}

// RefuseRustInCompoundOnlyProofMode keeps development, CI, and legacy dry-run inventories from
// silently presenting Compound's Go gates for a Rust repository.
func RefuseRustInCompoundOnlyProofMode(repo, mode string) error {
	// Keep the historical function name for callers; external declared consumers must
	// likewise never borrow Compound's non-certifying development/CI inventory.
	if _, err := os.Lstat(filepath.Join(repo, CanonicalConsumerContractPath)); err == nil {
		return fmt.Errorf("%s is Compound-specific; declared consumers require governed canonical proof", mode)
	} else if !os.IsNotExist(err) {
		return err
	}
	if committed, err := canonicalProofGitNULList(repo, "ls-tree", "-z", "--name-only", "HEAD", "--", CanonicalConsumerContractPath); err == nil && len(committed) > 0 {
		return fmt.Errorf("%s is Compound-specific; declared consumers require governed canonical proof", mode)
	}
	if _, err := canonicalProofGitNULList(repo, "rev-parse", "--verify", "HEAD"); err == nil {
		contractPath, err := canonicalConsumerDeclarationPath(repo, "HEAD")
		if err != nil {
			return err
		}
		if contractPath != "" {
			return fmt.Errorf("%s is Compound-specific; declared consumers require governed canonical proof", mode)
		}
	}
	refusal := func() error {
		return fmt.Errorf("%s is Compound-specific; a Rust workspace requires governed canonical proof with explicit --base, --work, --actor, and --out", mode)
	}
	for _, marker := range []string{"Cargo.toml", "Cargo.lock"} {
		if _, err := os.Lstat(filepath.Join(repo, marker)); err == nil {
			return refusal()
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect %s root marker %s: %w", mode, marker, err)
		}
	}
	committed, err := canonicalProofGitNULList(
		repo,
		"ls-tree", "-z", "--name-only", "HEAD", "--", "Cargo.lock", "Cargo.toml",
	)
	if err == nil && len(committed) > 0 {
		return refusal()
	}
	return nil
}

var rustfmtDiffHeaderPattern = regexp.MustCompile(`^Diff in (.+):([0-9]+):$`)

// CanonicalProofFailureFacts retains generic normalization for every gate except rustfmt. Rustfmt
// failures are block-structured whitespace evidence, so only the execution-root prefix is
// normalized and each otherwise byte-exact diff block becomes one fact.
func CanonicalProofFailureFacts(repo, gate string, output []byte, runErr error) []string {
	if gate == "rustfmt" && runErr != nil {
		if blocks, ok := rustfmtFailureBlocks(repo, output); ok {
			facts := make([]string, 0, len(blocks))
			for _, block := range blocks {
				sum := sha256.Sum256([]byte("rustfmt:block:v1\x00" + block))
				facts = append(facts, "sha256:"+hex.EncodeToString(sum[:]))
			}
			sort.Strings(facts)
			return facts
		}
	}
	return ProofFailureFacts(gate, output, runErr)
}

func rustfmtFailureBlocks(repo string, output []byte) ([]string, bool) {
	absRepo, err := filepath.Abs(repo)
	if err != nil || strings.ContainsAny(absRepo, "\r\n") {
		return nil, false
	}
	roots := []string{filepath.Clean(absRepo)}
	if physicalRepo, evalErr := filepath.EvalSymlinks(absRepo); evalErr == nil {
		physicalRepo = filepath.Clean(physicalRepo)
		if physicalRepo != roots[0] {
			roots = append(roots, physicalRepo)
		}
	}

	var blocks []*strings.Builder
	var sawChange []bool
	for _, rawLine := range strings.SplitAfter(string(output), "\n") {
		if rawLine == "" {
			continue
		}
		line, lineEnding := rustfmtLineAndEnding(rawLine)
		if !strings.HasPrefix(line, "Diff in ") {
			if len(blocks) == 0 || !validRustfmtDiffBodyLine(line) {
				return nil, false
			}
			if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
				sawChange[len(sawChange)-1] = true
			}
			blocks[len(blocks)-1].WriteString(rawLine)
			continue
		}
		if len(blocks) > 0 && !sawChange[len(sawChange)-1] {
			return nil, false
		}
		match := rustfmtDiffHeaderPattern.FindStringSubmatch(line)
		if len(match) != 3 || !filepath.IsAbs(match[1]) {
			return nil, false
		}
		relative, inside := rustfmtPathRelativeToRepository(roots, match[1])
		if !inside {
			return nil, false
		}
		blocks = append(blocks, &strings.Builder{})
		sawChange = append(sawChange, false)
		blocks[len(blocks)-1].WriteString("Diff in $REPO/" + filepath.ToSlash(relative) + ":" + match[2] + ":" + lineEnding)
	}
	if len(blocks) == 0 || !sawChange[len(sawChange)-1] {
		return nil, false
	}
	labels := make([]string, len(blocks))
	for i := range blocks {
		labels[i] = blocks[i].String()
	}
	return labels, true
}

func rustfmtLineAndEnding(raw string) (line, ending string) {
	line = raw
	if strings.HasSuffix(line, "\n") {
		line = strings.TrimSuffix(line, "\n")
		ending = "\n"
	}
	if strings.HasSuffix(line, "\r") {
		line = strings.TrimSuffix(line, "\r")
		ending = "\r" + ending
	}
	return line, ending
}

func validRustfmtDiffBodyLine(line string) bool {
	if line == "" {
		return true
	}
	return line[0] == ' ' || line[0] == '+' || line[0] == '-'
}

func rustfmtPathRelativeToRepository(roots []string, path string) (string, bool) {
	path = filepath.Clean(path)
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			continue
		}
		return relative, true
	}
	return "", false
}

func canonicalProofGitBlob(repo, headSHA, path string) ([]byte, error) {
	return canonicalProofGitBlobContext(context.Background(), repo, headSHA, path)
}
func canonicalProofGitBlobContext(ctx context.Context, repo, headSHA, path string) ([]byte, error) {
	cmd := gitrepository.CommandContext(ctx, "git", "show", headSHA+":"+path)
	cmd.Dir = repo
	body, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("read canonical proof marker %s: %w", path, err)
	}
	return body, nil
}

func isCanonicalCompoundModule(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		// The module directive may follow other directives, use a quoted path,
		// or carry a trailing comment. Spelling must not select a weaker plan.
		module := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "module"))
		if quoted, err := strconv.QuotedPrefix(module); err == nil {
			if rest := strings.TrimSpace(module[len(quoted):]); rest != "" && !strings.HasPrefix(rest, "//") {
				continue
			}
			module, _ = strconv.Unquote(quoted)
		} else {
			module, _, _ = strings.Cut(module, "//")
			module = strings.TrimSpace(module)
		}
		if module == "compound" {
			return true
		}
	}
	return false
}

func rustWorkspaceProofCheckSteps(baseRef, headRef string) []ProofCheckStep {
	diffRange := strings.TrimSpace(baseRef) + "..." + strings.TrimSpace(headRef)
	redactCommand := []string{
		"redact", "--repo-visibility", "public",
		"<committed blobs changed in " + diffRange + ">",
	}
	return []ProofCheckStep{
		{Name: "rustfmt", Command: []string{"cargo", "fmt", "--all", "--", "--check"}, ProofRef: "cargo-fmt-all-check", CIPortable: true},
		{Name: "cargo-clippy", Command: []string{"cargo", "clippy", "--locked", "--workspace", "--all-targets", "--", "-D", "warnings"}, ProofRef: "cargo-clippy-locked-workspace-all-targets-deny-warnings", CIPortable: true},
		{Name: "cargo-test", Command: []string{"cargo", "test", "--locked", "--workspace"}, ProofRef: "cargo-test-locked-workspace", CIPortable: true},
		{Name: "cargo-build", Command: []string{"cargo", "build", "--locked", "--workspace"}, ProofRef: "cargo-build-locked-workspace", CIPortable: true},
		{Name: "diff-check", Command: []string{"git", "diff", "--check", diffRange}, ProofRef: "diff-check", CIPortable: true},
		{Name: "redact-public", Command: redactCommand, ProofRef: "redact-public-clean"},
	}
}

func ProofResourceProfileForCanonicalPlan(plan CanonicalProofPlan) (ProofResourceProfile, error) {
	if strings.TrimSpace(plan.ID) == "" || strings.TrimSpace(plan.Repo) == "" || len(plan.Steps) == 0 {
		return ProofResourceProfile{}, fmt.Errorf("canonical proof plan is incomplete")
	}
	gates := make([]ProofResourceGateProfile, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		gates = append(gates, ProofResourceGateProfile{
			Name:          step.Name,
			Command:       append([]string(nil), step.Command...),
			Dir:           step.Dir,
			SkipIfMissing: step.SkipIfMissing,
			ProofRef:      step.ProofRef,
		})
	}
	return NewProofResourceProfileWithSource(
		ProofResourceProfileCanonicalFull,
		plan.Repo,
		plan.Source,
		gates,
	)
}

func CanonicalProofProfilePublishesCompoundRuntimeArtifact(
	plan CanonicalProofPlan,
	profile ProofResourceProfile,
	compoundInventory CanonicalProofInventory,
) (bool, error) {
	if plan.ID != CanonicalProofPlanCompoundV1 {
		return false, nil
	}
	if compoundInventory == nil {
		return false, fmt.Errorf("canonical Compound proof inventory is unavailable")
	}
	expectedSteps, err := compoundInventory(plan.Source.BaseSHA, plan.Source.HeadSHA)
	if err != nil {
		return false, err
	}
	expectedPlan := cloneCanonicalProofPlan(plan)
	expectedPlan.Steps = expectedSteps
	expectedProfile, err := ProofResourceProfileForCanonicalPlan(expectedPlan)
	if err != nil {
		return false, err
	}
	if profile.RemoteGoTest != nil {
		expectedProfile.RemoteGoTest = profile.RemoteGoTest
		expectedProfile, err = normalizeProofResourceProfile(expectedProfile)
		if err != nil {
			return false, err
		}
	}
	return reflect.DeepEqual(profile, expectedProfile), nil
}

func ContextWithCanonicalProofPlan(ctx context.Context, plan CanonicalProofPlan) context.Context {
	return context.WithValue(ctx, canonicalProofPlanContextKey{}, cloneCanonicalProofPlan(plan))
}

func CanonicalProofPlanForExecution(
	ctx context.Context,
	repo string,
	source gitrepository.Snapshot,
	compoundInventory CanonicalProofInventory,
) (CanonicalProofPlan, error) {
	if stored, ok := ctx.Value(canonicalProofPlanContextKey{}).(CanonicalProofPlan); ok {
		resolvedRepo, err := resolveCanonicalProofRepo(repo)
		if err != nil {
			return CanonicalProofPlan{}, err
		}
		if !reflect.DeepEqual(stored.Source, source) {
			return CanonicalProofPlan{}, fmt.Errorf("canonical proof plan does not match execution source")
		}
		executionSource, err := gitrepository.CaptureCleanSnapshotContext(ctx, resolvedRepo, source.BaseSHA)
		if err != nil {
			return CanonicalProofPlan{}, fmt.Errorf("canonical proof execution checkout: %w", err)
		}
		if executionSource.BaseSHA != source.BaseSHA ||
			executionSource.HeadSHA != source.HeadSHA ||
			executionSource.TreeSHA != source.TreeSHA {
			return CanonicalProofPlan{}, fmt.Errorf("canonical proof execution checkout does not match sealed source")
		}
		return cloneCanonicalProofPlan(stored), nil
	}
	return ResolveCanonicalProofPlanContext(ctx, repo, source, compoundInventory)
}

func cloneCanonicalProofPlan(in CanonicalProofPlan) CanonicalProofPlan {
	out := in
	out.Steps = make([]ProofCheckStep, len(in.Steps))
	for i, step := range in.Steps {
		out.Steps[i] = step
		out.Steps[i].Command = append([]string(nil), step.Command...)
		out.Steps[i].FailureFacts = append([]string(nil), step.FailureFacts...)
		if step.ConsumerEvidence != nil {
			evidence := *step.ConsumerEvidence
			evidence.Contract = append([]byte(nil), evidence.Contract...)
			evidence.Report = append([]byte(nil), evidence.Report...)
			evidence.Tests = append([]byte(nil), evidence.Tests...)
			out.Steps[i].ConsumerEvidence = &evidence
		}
		if step.RedactionEvidence != nil {
			evidence := *step.RedactionEvidence
			evidence.Review = append([]byte(nil), evidence.Review...)
			evidence.Scans = append([]PublicRedactionScan(nil), evidence.Scans...)
			for j := range evidence.Scans {
				evidence.Scans[j].Report = append([]byte(nil), evidence.Scans[j].Report...)
			}
			out.Steps[i].RedactionEvidence = &evidence
		}
	}
	return out
}

func resolveCanonicalProofRepo(repoPath string) (string, error) {
	if strings.TrimSpace(repoPath) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		repoPath = cwd
	}
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("--repo must be a directory, got %s", abs)
	}
	return abs, nil
}

func canonicalProofGitNULList(repo string, args ...string) ([]string, error) {
	return canonicalProofGitNULListContext(context.Background(), repo, args...)
}
func canonicalProofGitNULListContext(ctx context.Context, repo string, args ...string) ([]string, error) {
	cmd := gitrepository.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, err
	}
	raw := bytes.Split(out, []byte{0})
	values := make([]string, 0, len(raw))
	for _, part := range raw {
		if len(part) == 0 {
			continue
		}
		values = append(values, string(part))
	}
	return values, nil
}
