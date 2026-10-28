package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/intent"
	"compound/internal/retrievalparity"
)

const (
	intentUsage       = "usage: compound intent <plan|review|apply> ..."
	intentPlanUsage   = "usage: compound intent plan --source <label> [--text <utterance>] [--task <task>] (--from-proposal <proposal.json>|--model-stdin) [--harness <h>] [--cwd <dir>] [--repo-kind <k>] [--mode <m>] [--topic <slug>] [--body-budget <tokens>] [--by <actor>] [--owner <owner>] [--canonical-surface <uri>] [--json]"
	intentReviewUsage = "usage: compound intent review <create-plan.json|ambiguity-park.json> [--json]"
	intentApplyUsage  = "usage: compound intent apply <create-plan.json> --by <reviewer> [--json]"
)

var materializeRuntimeForOrient = contract.MaterializeRuntime
var materializeBodiesForOrient = func(homePath string, pack envelope.ContextPack, cwd, task string, bodyBudget int) (envelope.ContextPack, error) {
	if bodyBudget <= 0 {
		return pack, nil
	}
	resolver, err := newOrientBodyResolver(homePath, pack, cwd)
	if err != nil {
		return envelope.ContextPack{}, err
	}
	candidates, err := resolver.Resolve(task)
	if err != nil {
		return envelope.ContextPack{}, err
	}
	if resolver.Degraded() {
		// Surface a single generic config-unavailable candidate so the materializer
		// accounts for it in the omitted ledger rather than the resolver silently
		// dropping the optional body source. The text is empty, the status is
		// unresolvable, the materializer maps it to body_pack.omitted.reason=unresolvable.
		candidates = append(candidates, contract.BodyCandidate{
			Ref:    orientBodyConfigUnavailableRef,
			Source: "corpus:configuration",
			Rank:   0,
			Status: contract.BodyStatusUnresolvable,
		})
	}
	return contract.ApplyBodyPack(pack, contract.MaterializeBodyPack(candidates, pack, bodyBudget)), nil
}

// orientMaterializationResult is the result of materializeOrientContextPackDetailed.
// EngineDependency traces the exact rented breadth dependency the materializer
// used (or "not_attempted" if the engine is disabled), BodyPackUsedTokens is the
// body_pack.estimated_used_tokens of the returned pack (0 when no body pack),
// Degraded reports whether the optional configured body source emitted any
// degradation candidate during this materialization.
type orientMaterializationResult struct {
	Pack               envelope.ContextPack
	EngineDependency   contract.EngineDependencyTrace
	BodyPackUsedTokens int
	Degraded           bool
}

// materializeOrientContextPackDetailed is the one cmd-layer orientation path
// used by both `compound orient` and the parity replay. It performs the
// shared runtime materialization, optional body materialization, sealing, and
// atomic persistence. Parity replay consumes the result directly; orient uses
// the compatibility wrapper materializeOrientContextPack that returns just the
// pack.
func materializeOrientContextPackDetailed(
	homePath string,
	in contract.Input,
	bodyBudget int,
	now time.Time,
) (orientMaterializationResult, error) {
	restoreEngine, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		return orientMaterializationResult{}, err
	}
	defer restoreEngine()

	orientationPolicy, _, err := contract.LoadPolicy(homePath)
	if err != nil {
		return orientMaterializationResult{}, err
	}
	retrievalStarted := time.Now()
	runtime, materializeErr := materializeRuntimeForOrient(in, orientationPolicy, homePath, now)
	pack := runtime.ContextPack
	retrievalparity.Record(homePath, retrievalparity.NewOrientRow(in.Task, parityOrientInput(in), pack.ActiveMemory, time.Since(retrievalStarted), materializeErr == nil, time.Now()))
	if materializeErr != nil {
		return orientMaterializationResult{}, materializeErr
	}
	ec := runtime.EffectiveContract
	if pack.IDs.ContractID != ec.ID {
		return orientMaterializationResult{}, fmt.Errorf("context pack contract_id %q does not match effective contract %q", pack.IDs.ContractID, ec.ID)
	}
	degraded := false
	if bodyBudget > 0 {
		pack, err = materializeBodiesForOrient(homePath, pack, in.Cwd, in.Task, bodyBudget)
		if err != nil {
			return orientMaterializationResult{}, err
		}
		// After materialization, scan the returned pack's body_pack omission ledger
		// for any of the resolver's generic degradation refs. This is how the CLI
		// learns whether the resolver degraded without coupling the parity
		// evaluator to the resolver's ref shapes.
		degraded = packReportsOrientBodyDegradation(pack)
	}
	runtime.EffectiveContract = ec
	runtime.ContextPack = pack
	runtime, err = contract.SealRuntimeMaterialization(runtime)
	if err != nil {
		return orientMaterializationResult{}, err
	}
	ec = runtime.EffectiveContract
	pack = runtime.ContextPack
	if _, err := contract.RecordRuntimeMaterialization(homePath, runtime); err != nil {
		return orientMaterializationResult{}, err
	}
	var usedTokens int
	if pack.BodyPack != nil {
		usedTokens = pack.BodyPack.EstimatedUsedTokens
	}
	return orientMaterializationResult{
		Pack:               pack,
		EngineDependency:   runtime.EngineDependencyTrace,
		BodyPackUsedTokens: usedTokens,
		Degraded:           degraded,
	}, nil
}

// packReportsOrientBodyDegradation returns true if the body_pack omission
// ledger contains any of the resolver's generic degradation refs. Kept local
// to this file so the parity evaluator does not need to know the resolver's
// internal ref shapes.
func packReportsOrientBodyDegradation(pack envelope.ContextPack) bool {
	if pack.BodyPack == nil {
		return false
	}
	for _, o := range pack.BodyPack.Omitted {
		if isOrientBodyDegradationRef(o.Ref) {
			return true
		}
	}
	return false
}

// isOrientBodyDegradationRef returns true if ref is one of the resolver's
// generic degradation refs. Kept here as a single source of truth for the
// resolver's surface, used both by the live CLI and by tests asserting the
// exact omission string.
func isOrientBodyDegradationRef(ref string) bool {
	trimmed := strings.TrimSpace(ref)
	if trimmed == orientBodyConfigUnavailableRef {
		return true
	}
	return strings.HasPrefix(trimmed, "corpus://lane-unavailable/")
}

// materializeOrientContextPack is the one cmd-layer orientation path used by
// both `compound orient` and expressive intent. It materializes the shared
// EffectiveContract/ContextPack runtime pair, optionally adds task-scoped
// bodies to the pack, then atomically persists that exact pair. It is the
// compatibility wrapper around materializeOrientContextPackDetailed; the
// detailed variant carries EngineDependency/Degraded for the parity replay
// evidence path.
func materializeOrientContextPack(homePath string, in contract.Input, bodyBudget int, now time.Time) (envelope.ContextPack, error) {
	result, err := materializeOrientContextPackDetailed(homePath, in, bodyBudget, now)
	if err != nil {
		return envelope.ContextPack{}, err
	}
	return result.Pack, nil
}

func cmdIntent(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, intentUsage)
		os.Exit(2)
	}
	if hasUsageFlag(args) && len(args) == 1 {
		fmt.Println(intentUsage)
		return
	}
	switch args[0] {
	case "plan":
		cmdIntentPlan(args[1:])
	case "review":
		cmdIntentReview(args[1:])
	case "apply":
		cmdIntentApply(args[1:])
	default:
		fmt.Fprintln(os.Stderr, intentUsage)
		os.Exit(2)
	}
}

func cmdIntentPlan(args []string) {
	valueFlags := map[string]bool{
		"--source": true, "--text": true, "--task": true, "--from-proposal": true,
		"--harness": true, "--cwd": true, "--repo-kind": true, "--mode": true,
		"--topic": true, "--body-budget": true, "--by": true, "--actor": true,
		"--owner": true, "--canonical-surface": true,
	}
	boolFlags := map[string]bool{"--model-stdin": true, "--json": true}
	if hasUsageFlag(args) {
		fmt.Println(intentPlanUsage)
		return
	}
	if err := validateKnownFlags(args, valueFlags, boolFlags); err != nil {
		intentUsageError(err, intentPlanUsage)
	}
	source := strings.TrimSpace(valueFlag(args, "--source"))
	proposalPath := strings.TrimSpace(valueFlag(args, "--from-proposal"))
	modelStdin := hasFlag(args, "--model-stdin")
	if source == "" {
		intentUsageError(fmt.Errorf("--source is required"), intentPlanUsage)
	}
	if (proposalPath == "") == !modelStdin {
		intentUsageError(fmt.Errorf("choose exactly one of --from-proposal or --model-stdin"), intentPlanUsage)
	}

	var utterance []byte
	if hasFlag(args, "--text") {
		utterance = []byte(valueFlag(args, "--text"))
	} else {
		if modelStdin {
			intentUsageError(fmt.Errorf("--text is required when proposal JSON comes from --model-stdin"), intentPlanUsage)
		}
		var err error
		utterance, err = io.ReadAll(os.Stdin)
		die(err)
	}
	if len(utterance) == 0 {
		intentUsageError(fmt.Errorf("utterance bytes are empty"), intentPlanUsage)
	}

	var proposalBytes []byte
	var err error
	if modelStdin {
		proposalBytes, err = io.ReadAll(os.Stdin)
	} else {
		proposalBytes, err = os.ReadFile(proposalPath)
	}
	die(err)
	proposal, err := intent.DecodeProposal(proposalBytes)
	die(err)

	homePath, err := home.Resolve("")
	die(err)
	in, err := parseOrientArgsWithHome(args, homePath)
	die(err)
	in.Harness = firstNonEmpty(in.Harness, "codex")
	in.Task = firstNonEmpty(in.Task, string(utterance))
	in.Mode = firstNonEmpty(in.Mode, "plan")
	bodyBudget, err := parseBodyBudget(args)
	if err != nil {
		intentUsageError(err, intentPlanUsage)
	}
	now := time.Now()
	pack, err := materializeOrientContextPack(homePath, in, bodyBudget, now)
	die(err)

	recalledRefs := append([]string{}, pack.ActiveDecisions...)
	recalledRefs = append(recalledRefs, pack.ActiveMemory...)
	report, err := intent.Plan(homePath, intent.PlanInput{
		Source:           source,
		Utterance:        utterance,
		ContextPackID:    pack.ID,
		RecalledRefs:     recalledRefs,
		Actor:            firstNonEmpty(valueFlag(args, "--by"), valueFlag(args, "--actor"), in.Harness),
		Owner:            firstNonEmpty(valueFlag(args, "--owner"), "compound-core"),
		CanonicalSurface: valueFlag(args, "--canonical-surface"),
		Proposal:         proposal,
	}, now)
	die(err)
	if hasFlag(args, "--json") {
		printJSON(report)
		return
	}
	id := report.CaptureCID
	if report.Plan != nil {
		id = report.Plan.ID
	} else if report.Park != nil {
		id = report.Park.ID
	}
	fmt.Printf("plan %s status=%s capture=%s recall=%drefs\n", id, report.Status, report.CaptureCID, len(recalledRefs))
	if report.PlanPath != "" {
		fmt.Printf("plan_path: %s\n", report.PlanPath)
	}
	if report.ParkPath != "" {
		fmt.Printf("park_path: %s\n", report.ParkPath)
	}
}

func cmdIntentReview(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(intentReviewUsage)
		return
	}
	if err := validateKnownFlags(args, nil, map[string]bool{"--json": true}); err != nil {
		intentUsageError(err, intentReviewUsage)
	}
	path := firstIntentPositional(args)
	if path == "" {
		intentUsageError(fmt.Errorf("plan or park path is required"), intentReviewUsage)
	}
	report, err := intent.Review(path)
	die(err)
	if hasFlag(args, "--json") {
		printJSON(report)
		return
	}
	if report.Plan != nil {
		fmt.Printf("intent review plan=%s work=%s status=%s\n", report.Plan.ID, report.Plan.WorkID, report.Plan.Status)
		return
	}
	fmt.Printf("intent review park=%s status=%s refs=%s\n", report.Park.ID, report.Park.Status, strings.Join(report.Park.AmbiguousRefs, ","))
	for _, question := range report.Park.NamedQuestions {
		fmt.Printf("question: %s\n", question)
	}
}

func cmdIntentApply(args []string) {
	if hasUsageFlag(args) {
		fmt.Println(intentApplyUsage)
		return
	}
	if err := validateKnownFlags(args, map[string]bool{"--by": true}, map[string]bool{"--json": true}); err != nil {
		intentUsageError(err, intentApplyUsage)
	}
	path := firstIntentPositional(args)
	reviewer := strings.TrimSpace(valueFlag(args, "--by"))
	if path == "" || reviewer == "" {
		intentUsageError(fmt.Errorf("create-plan path and --by reviewer are required"), intentApplyUsage)
	}
	homePath, err := home.Resolve("")
	die(err)
	report, err := intent.Apply(homePath, path, reviewer, time.Now())
	die(err)
	if hasFlag(args, "--json") {
		printJSON(report)
		return
	}
	fmt.Printf("intent apply plan=%s status=%s work=%s state=%s\n", report.PlanID, report.Status, report.WorkID, report.WorkState)
}

func firstIntentPositional(args []string) string {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--by":
			i++
		case "--json":
		default:
			if !strings.HasPrefix(args[i], "-") {
				return args[i]
			}
		}
	}
	return ""
}

func intentUsageError(err error, usage string) {
	fmt.Fprintln(os.Stderr, "error:", err)
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}
