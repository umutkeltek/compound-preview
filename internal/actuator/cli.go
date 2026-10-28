package actuator

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"compound/internal/envelope"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/product"
)

var actuatorSwitchbackFn = Switchback

func actuatorUsage() string {
	return "usage:\n" +
		"  compound actuator plan --work W --class C --reversibility R [--channel ch] [--product p] [--home P] [--json]\n" +
		"      Dry-run the routing brain: policy + budget + Route -> auto | hold | deny (executes nothing).\n" +
		"  compound actuator budget [--product p] [--home P] [--json]\n" +
		"      Show the effective per-channel budget + current rolling headroom for a product.\n" +
		"  compound actuator budget-set --channel ch [--max-actions N] [--max-spend-usd X] [--max-tokens T] --window-hours H [--product p] [--home P]\n" +
		"      Install/replace one channel's hard limit (0 = unlimited). Enforced BEFORE dispatch.\n" +
		"  compound actuator switchback --profile FILE --work W [--home P] [--json]\n" +
		"      Apply a ProjectProfile: materialize AGENTS.md/CLAUDE.md into the workspace and repoint\n" +
		"      harness routing — smoothly, each step policy-gated + receipted; settings surfaced as commands.\n" +
		"  compound actuator run --work W --harness H [--channel ch] [--product p] [--workspace path] [--prompt P | -- cmd args...] [--permission-mode m] [--home P] [--json]\n" +
		"      Execute one move of known work on a harness, within limits. Auto only when the verdict is\n" +
		"      auto (local_reversible + easy + policy-allowed + budget headroom); otherwise holds with reasons.\n" +
		"  compound actuator held [--product p] [--home P]\n" +
		"      List the queue of held moves waiting on a human — each with its release gate, required\n" +
		"      inputs, and stop conditions. This is the operator's to-review list.\n" +
		"  compound actuator observe --product p [--full] [--home P] [--json]\n" +
		"      Did our moves land? Joins open signal checks against ingested signals (OPEN / CLOSED /\n" +
		"      EXPIRED). Fast by default; --full adds the heavy product loop model (open hypotheses,\n" +
		"      missing-signal groups, action cards)."
}

// RunCLI is the organ's CLI face, called from cmd/compound's command
// registry. Mirrors internal/executorlens: the organ owns the exit code so
// cmd/compound stays a thin dispatch shell.
func RunCLI(args []string) {
	if code := runCLI(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || hasUsageFlag(args) {
		fmt.Fprintln(stderr, actuatorUsage())
		return 2
	}
	switch args[0] {
	case "plan":
		return runActuatorPlan(args[1:], stdout, stderr)
	case "budget":
		return runActuatorBudget(args[1:], stdout, stderr)
	case "budget-set":
		return runActuatorBudgetSet(args[1:], stdout, stderr)
	case "switchback":
		return runActuatorSwitchback(args[1:], stdout, stderr)
	case "run":
		return runActuatorRun(args[1:], stdout, stderr)
	case "post":
		return runActuatorPost(args[1:], stdout, stderr)
	case "observe":
		return runActuatorObserve(args[1:], stdout, stderr)
	case "held":
		return runActuatorHeld(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, actuatorUsage())
		return 2
	}
}

func actuatorHome(args []string, stderr io.Writer) (string, bool) {
	homePath, err := home.Resolve(flagValue(args, "--home"))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return "", false
	}
	return homePath, true
}

// planIntent builds a synthetic ActionIntent from plan flags so the routing brain
// can be exercised without a live work move. mutation_class + reversibility are
// the two fields Route reads; everything else is defaulted to a local, known move.
func planIntent(args []string, now time.Time) envelope.ActionIntent {
	work := firstNonEmptyStr(flagValue(args, "--work"), "work_plan")
	class := firstNonEmptyStr(flagValue(args, "--class"), "local_reversible")
	rev := firstNonEmptyStr(flagValue(args, "--reversibility"), "easy")
	return envelope.ActionIntent{
		Header: envelope.Header{
			SchemaVersion: envelope.SVActionIntent,
			ID:            "act_plan_" + now.UTC().Format("20060102T150405Z"),
			CreatedAt:     now.UTC().Format(time.RFC3339),
			Producer:      envelope.Producer{Kind: "cli", Name: "compound"},
			IDs:           envelope.IDs{WorkID: work},
			Authority:     envelope.Authority{SourceClass: "runtime", Status: "current"},
			Privacy:       "private",
			Risk:          "tier1",
			EvidenceRefs:  []string{},
			Uncertainty:   envelope.Uncertainty{Level: "known"},
		},
		Actor:         "operator",
		Capability:    firstNonEmptyStr(flagValue(args, "--capability"), "plan_probe"),
		Action:        "plan",
		Target:        envelope.ActionTarget{Kind: "plan", ID: "plan"},
		MutationClass: class,
		Reversibility: rev,
		BlastRadius:   "local",
	}
}

func runActuatorPlan(args []string, stdout, stderr io.Writer) int {
	if err := validateKnownFlags(args, map[string]bool{
		"--work": true, "--class": true, "--reversibility": true, "--channel": true,
		"--product": true, "--capability": true, "--home": true,
	}, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, actuatorUsage())
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	now := time.Now()
	channel := firstNonEmptyStr(flagValue(args, "--channel"), WildcardChannel)
	product := flagValue(args, "--product")
	plan, err := EvaluatePlan(homePath, planIntent(args, now), channel, product, cfg, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, plan); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# actuator plan (dry-run — nothing executed)\n")
	fmt.Fprintf(stdout, "- work:        %s\n", plan.Intent.IDs.WorkID)
	fmt.Fprintf(stdout, "- channel:     %s  product: %s\n", plan.Channel, firstNonEmptyStr(plan.ProductID, "(default)"))
	fmt.Fprintf(stdout, "- class:       %s  reversibility: %s\n", plan.Intent.MutationClass, plan.Intent.Reversibility)
	fmt.Fprintf(stdout, "- policy:      %s\n", plan.PolicyDecision.Outcome)
	fmt.Fprintf(stdout, "- budget:      allowed=%v actions_left=%s spend_left=%s\n",
		plan.Headroom.Allowed, headroomNum(plan.Headroom.ActionsRemaining, plan.Headroom.UnlimitedActions), spendNum(plan.Headroom.SpendRemainingUSD, plan.Headroom.UnlimitedSpend))
	fmt.Fprintf(stdout, "- decision:    %s\n", strings.ToUpper(string(plan.Decision.Mode)))
	for _, r := range plan.Decision.Reasons {
		fmt.Fprintf(stdout, "    · %s\n", r)
	}
	// A non-auto plan is not a dead end: show the human-gated action card inline so
	// the operator sees exactly what releases the move. `plan` stays pure (dry-run),
	// so the card is shown here but only persisted by the real execution paths.
	if plan.Decision.Mode != ModeAuto {
		card := ReleaseGateFor(plan, now)
		printHeldMoveCard(stdout, card)
	}
	return 0
}

// printHeldMoveCard renders a held-move action card: the release gate, the inputs
// required to proceed, and the conditions that must hold to stop.
func printHeldMoveCard(stdout io.Writer, card HeldMoveCard) {
	fmt.Fprintf(stdout, "\n## held-move action card (%s)\n", card.ID)
	fmt.Fprintf(stdout, "- release gate: %s\n", card.ReleaseGate)
	if len(card.RequiredInputs) > 0 {
		fmt.Fprintf(stdout, "- required to proceed:\n")
		for _, r := range card.RequiredInputs {
			fmt.Fprintf(stdout, "    · %s\n", r)
		}
	}
	if len(card.StopConditions) > 0 {
		fmt.Fprintf(stdout, "- stop conditions:\n")
		for _, s := range card.StopConditions {
			fmt.Fprintf(stdout, "    · %s\n", s)
		}
	}
}

func runActuatorBudget(args []string, stdout, stderr io.Writer) int {
	if err := validateKnownFlags(args, map[string]bool{
		"--product": true, "--home": true,
	}, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, actuatorUsage())
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	now := time.Now()
	product := flagValue(args, "--product")
	ledger := OpenLedger(homePath)
	budget, err := ledger.LoadBudget(product, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, budget); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# actuator budget — product %s (%s)\n", firstNonEmptyStr(product, "(default)"), budget.ID)
	for _, ch := range budget.Channels {
		h, err := ledger.Check(budget, product, ch.Channel, now)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		fmt.Fprintf(stdout, "- channel %-18s window=%2dh  actions=%s  spend=%s  -> %s\n",
			ch.Channel, ch.WindowHours,
			limitNum(ch.MaxActions), spendLimit(ch.MaxSpendUSD),
			verdictWord(h.Allowed))
	}
	return 0
}

func runActuatorBudgetSet(args []string, stdout, stderr io.Writer) int {
	usage := "usage: compound actuator budget-set --channel ch --window-hours H [--max-actions N] [--max-spend-usd X] [--max-tokens T] [--product p] [--home P]"
	if err := validateKnownFlags(args, map[string]bool{
		"--channel": true, "--window-hours": true, "--max-actions": true,
		"--max-spend-usd": true, "--max-tokens": true, "--product": true, "--home": true,
	}, nil); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	channel := strings.TrimSpace(flagValue(args, "--channel"))
	window, err := actuatorInt(args, "--window-hours", 24)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if channel == "" || window <= 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	maxActions, err := actuatorInt(args, "--max-actions", 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	maxSpendUSD, err := actuatorFloat(args, "--max-spend-usd", 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	maxTokens, err := actuatorInt(args, "--max-tokens", 0)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	now := time.Now()
	product := flagValue(args, "--product")
	ledger := OpenLedger(homePath)
	budget, err := ledger.LoadBudget(product, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if budget.ID == DefaultBudgetID {
		budget.ID = "budget_" + firstNonEmptyStr(product, "default")
		budget.ProductID = product
		budget.CreatedAt = now.UTC().Format(time.RFC3339)
	}
	lim := ChannelLimits{
		Channel:     channel,
		WindowHours: window,
		MaxActions:  maxActions,
		MaxSpendUSD: maxSpendUSD,
		MaxTokens:   maxTokens,
	}
	replaced := false
	for i, ch := range budget.Channels {
		if ch.Channel == channel {
			budget.Channels[i] = lim
			replaced = true
		}
	}
	if !replaced {
		// drop a pure-wildcard default before adding a real channel set
		budget.Channels = append(budget.Channels, lim)
	}
	if err := ledger.SaveBudget(budget); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "budget saved: %s\n", ledger.BudgetPath(product))
	fmt.Fprintf(stdout, "- channel %s window=%dh actions=%s spend=%s tokens=%s\n",
		channel, window, limitNum(lim.MaxActions), spendLimit(lim.MaxSpendUSD), limitNum(lim.MaxTokens))
	return 0
}

func runActuatorSwitchback(args []string, stdout, stderr io.Writer) int {
	usage := "usage: compound actuator switchback --profile FILE --work W [--home P] [--json]"
	if err := validateKnownFlags(args, map[string]bool{
		"--profile": true, "--work": true, "--home": true,
	}, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	profilePath := strings.TrimSpace(flagValue(args, "--profile"))
	work := strings.TrimSpace(flagValue(args, "--work"))
	if profilePath == "" || work == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	var profile ProjectProfile
	data, err := os.ReadFile(profilePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := yaml.Unmarshal(data, &profile); err != nil {
		fmt.Fprintln(stderr, "error: parse profile:", err)
		return 1
	}
	result, err := actuatorSwitchbackFn(homePath, profile, work, cfg, time.Now())
	hasResult := result.ProfileID != "" || result.WorkID != "" || len(result.Steps) > 0
	if err != nil && !hasResult {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if hasFlag(args, "--json") {
		if encodeErr := encodeJSON(stdout, result); encodeErr != nil {
			fmt.Fprintln(stderr, "error:", encodeErr)
			return 1
		}
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "# switchback %s for work %s\n", result.ProfileID, result.WorkID)
	for _, s := range result.Steps {
		status := "HELD"
		if s.Applied {
			status = "APPLIED"
		}
		fmt.Fprintf(stdout, "- [%-7s] %-12s %s\n", status, s.Kind, s.Description)
		for _, r := range s.Reasons {
			fmt.Fprintf(stdout, "      · %s\n", r)
		}
		if s.ReceiptRef != "" {
			fmt.Fprintf(stdout, "      receipt: %s  transition: %s\n", s.ReceiptRef, s.TransitionRef)
		}
		if s.ApplyHint != "" {
			fmt.Fprintf(stdout, "      apply: %s\n", s.ApplyHint)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func runActuatorRun(args []string, stdout, stderr io.Writer) int {
	usage := "usage: compound actuator run --work W --harness H [--channel ch] [--product p] [--workspace path] [--prompt P | -- cmd args...] [--permission-mode m] [--home P] [--json]"
	// Everything from `--` onward is the subprocess argv, not our flags — validate
	// only the slice before the separator so passthrough args aren't misread as ours.
	validateArgs := args
	if idx := indexOf(args, "--"); idx >= 0 {
		validateArgs = args[:idx]
	}
	if err := validateKnownFlags(validateArgs, map[string]bool{
		"--work": true, "--harness": true, "--channel": true, "--product": true,
		"--workspace": true, "--prompt": true, "--permission-mode": true, "--home": true,
	}, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	work := strings.TrimSpace(flagValue(args, "--work"))
	harnessName := strings.TrimSpace(flagValue(args, "--harness"))
	if work == "" || harnessName == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// Everything after `--` is the subprocess argv (program + args).
	var command string
	var cmdArgs []string
	if idx := indexOf(args, "--"); idx >= 0 && idx+1 < len(args) {
		command = args[idx+1]
		cmdArgs = args[idx+2:]
	}
	in := RunInput{
		WorkID:         work,
		ProductID:      flagValue(args, "--product"),
		Channel:        firstNonEmptyStr(flagValue(args, "--channel"), "execution"),
		Harness:        harnessName,
		Workspace:      flagValue(args, "--workspace"),
		Command:        command,
		Args:           cmdArgs,
		Prompt:         flagValue(args, "--prompt"),
		PermissionMode: flagValue(args, "--permission-mode"),
	}
	result, runErr := Run(homePath, in, cfg, time.Now())
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, result); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if runErr != nil {
			fmt.Fprintln(stderr, "run error:", runErr)
			return 1
		}
		return 0
	}
	if runErr != nil && !result.Executed {
		// Pre-dispatch failure: no RunAttempt exists, so printing BLOCKED with an
		// empty attempt and exit_code 0 would manufacture a misleading run summary.
		fmt.Fprintln(stderr, "run error:", runErr)
		return 1
	}
	if result.Held {
		fmt.Fprintf(stdout, "# actuator run — HELD (nothing dispatched)\n")
		for _, r := range result.Plan.Decision.Reasons {
			fmt.Fprintf(stdout, "    · %s\n", r)
		}
		if result.HeldMoveID != "" {
			fmt.Fprintf(stdout, "- held-move card: %s\n", result.HeldMovePath)
			printHeldMoveCard(stdout, ReleaseGateFor(result.Plan, time.Now()))
		}
		return 0
	}
	fmt.Fprintf(stdout, "# actuator run — %s on %s\n", verdictWord(result.Executed), harnessName)
	fmt.Fprintf(stdout, "- work: %s  run_attempt: %s\n", work, result.RunAttemptID)
	if result.TranscriptPath != "" {
		fmt.Fprintf(stdout, "- transcript: %s\n", result.TranscriptPath)
	}
	fmt.Fprintf(stdout, "- exit_code: %d  timed_out: %v\n", result.ExitCode, result.TimedOut)
	if runErr != nil {
		fmt.Fprintln(stderr, "run error:", runErr)
		return 1
	}
	return 0
}

// --- small rendering / flag helpers local to the actuator CLI ---

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func verdictWord(allowed bool) string {
	if allowed {
		return "ok"
	}
	return "BLOCKED"
}

func limitNum(n int) string {
	if n <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}

func spendLimit(x float64) string {
	if x <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("$%.2f", x)
}

func headroomNum(n int, unlimited bool) string {
	if unlimited {
		return "unlimited"
	}
	return fmt.Sprintf("%d", n)
}

func spendNum(x float64, unlimited bool) string {
	if unlimited {
		return "unlimited"
	}
	return fmt.Sprintf("$%.2f", x)
}

// actuatorInt parses an integer flag. An absent/empty value keeps def; a
// present-but-unparseable value is an error naming the flag and the bad
// value (never silently falls back to def — that silently disabled limits
// like --max-actions ten -> 0 == unlimited).
func actuatorInt(args []string, name string, def int) (int, error) {
	v := strings.TrimSpace(flagValue(args, name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid integer value %q", name, v)
	}
	return n, nil
}

// actuatorFloat is actuatorInt's float64 counterpart; same empty-stays-default,
// non-empty-non-parseable-is-error contract.
func actuatorFloat(args []string, name string, def float64) (float64, error) {
	v := strings.TrimSpace(flagValue(args, name))
	if v == "" {
		return def, nil
	}
	x, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid numeric value %q", name, v)
	}
	return x, nil
}

func runActuatorPost(args []string, stdout, stderr io.Writer) int {
	usage := "usage: compound actuator post --work W --content \"...\" [--product p] [--channel distribution] [--surface x] [--metric M] [--window 7d] [--home P] [--json]"
	if err := validateKnownFlags(args, map[string]bool{
		"--work": true, "--content": true, "--product": true, "--channel": true,
		"--surface": true, "--metric": true, "--expected-effect": true, "--window": true, "--home": true,
	}, map[string]bool{"--json": true}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}
	work := strings.TrimSpace(flagValue(args, "--work"))
	content := flagValue(args, "--content")
	if work == "" || strings.TrimSpace(content) == "" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	in := PostInput{
		WorkID:         work,
		ProductID:      flagValue(args, "--product"),
		Channel:        firstNonEmptyStr(flagValue(args, "--channel"), "distribution"),
		Surface:        firstNonEmptyStr(flagValue(args, "--surface"), "generic"),
		Content:        content,
		Metric:         flagValue(args, "--metric"),
		ExpectedEffect: flagValue(args, "--expected-effect"),
		Window:         firstNonEmptyStr(flagValue(args, "--window"), "7d"),
	}
	// Post can return a POPULATED result (Executed=true, receipt/transition refs)
	// TOGETHER with a non-nil error — F9.1: "the post landed but budget accounting
	// failed". Mirror runActuatorRun: surface the result, then fail loudly. Dropping
	// the result on error would lose the receipt refs the operator needs.
	result, postErr := Post(homePath, in, cfg, time.Now())
	if hasFlag(args, "--json") {
		if err := encodeJSON(stdout, result); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if postErr != nil {
			fmt.Fprintln(stderr, "error:", postErr)
			return 1
		}
		return 0
	}
	if result.Held {
		fmt.Fprintf(stdout, "# actuator post — HELD (nothing staged)\n")
		for _, r := range result.Plan.Decision.Reasons {
			fmt.Fprintf(stdout, "    · %s\n", r)
		}
		if result.HeldMoveID != "" {
			fmt.Fprintf(stdout, "- held-move card: %s\n", result.HeldMovePath)
			printHeldMoveCard(stdout, ReleaseGateFor(result.Plan, time.Now()))
		}
		return 0
	}
	if postErr != nil && !result.Executed {
		// Pre-staging failure: nothing landed, so there is no EXECUTED block to
		// print — just the loud error and a non-zero exit (today's behavior).
		fmt.Fprintln(stderr, "error:", postErr)
		return 1
	}
	fmt.Fprintf(stdout, "# actuator post — EXECUTED (staged within limits, loop opened)\n")
	fmt.Fprintf(stdout, "- channel:    %s  surface: %s\n", in.Channel, in.Surface)
	fmt.Fprintf(stdout, "- post:       %s\n", result.PostPath)
	fmt.Fprintf(stdout, "- receipt:    %s  transition: %s\n", result.ReceiptRef, result.TransitionRef)
	fmt.Fprintf(stdout, "- hypothesis: %s  (open, waiting for a signal on %s)\n", result.HypothesisID, firstNonEmptyStr(in.Metric, actuatorDefaultMetric(in.Channel)))
	fmt.Fprintf(stdout, "- signal check: %s\n", result.SignalCheckID)
	fmt.Fprintf(stdout, "\nnext: ingest the world's response, then watch the loop close:\n")
	fmt.Fprintf(stdout, "  compound product signal <signal-export.yaml>\n")
	fmt.Fprintf(stdout, "  compound actuator observe --product %s\n", firstNonEmptyStr(in.ProductID, "(default)"))
	if postErr != nil {
		// The post staged + was receipted (refs printed above), but budget accounting
		// failed (F9.1). The operator gets the refs AND the loud failure + exit 1.
		fmt.Fprintln(stderr, "error:", postErr)
		return 1
	}
	return 0
}

func actuatorDefaultMetric(channel string) string {
	switch channel {
	case "aso":
		return "app_store_connect_impressions"
	case "paid_marketing":
		return "ga4_installs"
	default:
		return "posthog_signups"
	}
}

func runActuatorObserve(args []string, stdout, stderr io.Writer) int {
	if err := validateKnownFlags(args, map[string]bool{
		"--product": true, "--home": true,
	}, map[string]bool{"--full": true, "--json": true}); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, actuatorUsage())
		return 2
	}
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	productID := flagValue(args, "--product")
	now := time.Now()

	// The loop model is internal/product's own projection — the actuator reads it,
	// never rebuilds it. It is HEAVY (opens the mem store + authority over the whole
	// home), so it is opt-in via --full; the default observe stays fast by answering
	// closure + checks only. --json keeps its contract and always includes the model.
	needModel := hasFlag(args, "--full") || hasFlag(args, "--json")
	var model product.LoopModel
	if needModel {
		m, err := product.BuildLoopModel(homePath, product.LoopOptions{ProductID: productID, Now: now})
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		model = m
	}
	checks, err := ListPendingSignalChecks(homePath, productID)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	closure, err := ObserveClosure(homePath, productID, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if hasFlag(args, "--json") {
		out := map[string]any{"loop_model": model, "pending_signal_checks": checks, "closure": closure}
		if err := encodeJSON(stdout, out); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "# actuator observe — feedback-loop status%s\n", productSuffix(productID))

	// The headline: did the moves we made get confirmed by the world's signal?
	fmt.Fprintf(stdout, "\n## loop closure (did our moves land their expected effect?)\n")
	if len(closure) == 0 {
		fmt.Fprintf(stdout, "- no actuator moves have been observed yet (run `compound actuator post` / `run` first)\n")
	}
	for _, st := range closure {
		switch st.State {
		case "signal_received":
			fmt.Fprintf(stdout, "- CLOSED  %s  metric=%s  signal=%s  before=%.3f -> after=%.3f\n",
				st.HypothesisID, st.Metric, st.MatchedSignalID, st.Before, st.After)
		case "expired":
			fmt.Fprintf(stdout, "- EXPIRED %s  metric=%s  no signal by %s — move never confirmed; review or re-run it\n",
				st.HypothesisID, st.Metric, st.ObservationUntil)
		default:
			fmt.Fprintf(stdout, "- OPEN    %s  waiting for a signal on %s (by %s)\n",
				st.HypothesisID, st.Metric, st.ObservationUntil)
		}
	}
	if hasFlag(args, "--full") {
		for _, loop := range model.Products {
			fmt.Fprintf(stdout, "\nproduct %s (%s)\n", loop.ProductID, loop.Name)
			fmt.Fprintf(stdout, "- open hypotheses:            %d\n", len(loop.OpenHypotheses))
			fmt.Fprintf(stdout, "- hypotheses MISSING signal:  %d groups\n", len(loop.OpenHypothesesByMissingSignalSource))
			for _, g := range loop.OpenHypothesesByMissingSignalSource {
				fmt.Fprintf(stdout, "      · source %s waiting on: %s\n", g.Source, strings.Join(g.HypothesisIDs, ", "))
			}
			fmt.Fprintf(stdout, "- signals WITHOUT hypothesis: %d groups (world moved, we didn't predict it)\n", len(loop.SignalsWithoutHypothesis))
			for _, g := range loop.SignalsWithoutHypothesis {
				fmt.Fprintf(stdout, "      · %s (signals: %s)\n", g.ChangeEvent, strings.Join(g.SignalIDs, ", "))
			}
			fmt.Fprintf(stdout, "- action cards ready:         %d\n", len(loop.ActionCards))
			if loop.Freshness.Stale {
				fmt.Fprintf(stdout, "- freshness: STALE (latest signal %s)\n", firstNonEmptyStr(loop.Freshness.LatestSignalAt, "none"))
			}
		}
	} else {
		fmt.Fprintf(stdout, "\n(full product loop model omitted for speed — add --full to include it)\n")
	}
	fmt.Fprintf(stdout, "\npending signal checks: %d\n", len(checks))
	for _, c := range checks {
		state := "watching"
		if SignalCheckExpired(c, now) {
			state = "OVERDUE — no signal arrived in time"
		}
		fmt.Fprintf(stdout, "- %s: %s %s by %s (hypothesis %s)\n", c.Channel, state, c.Metric, c.ObservationUntil, c.HypothesisID)
	}
	return 0
}

func productSuffix(productID string) string {
	if strings.TrimSpace(productID) == "" {
		return ""
	}
	return " — product " + productID
}

func runActuatorHeld(args []string, stdout, stderr io.Writer) int {
	if err := validateKnownFlags(args, map[string]bool{
		"--product": true, "--home": true,
	}, nil); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, actuatorUsage())
		return 2
	}
	productID := flagValue(args, "--product")
	homePath, ok := actuatorHome(args, stderr)
	if !ok {
		return 1
	}
	cards, err := ListHeldMoves(homePath, productID)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "# actuator held — moves waiting on a human%s\n\n", productSuffix(productID))
	if len(cards) == 0 {
		fmt.Fprintf(stdout, "- none — every recent move was auto-executed or there is no held-move queue yet.\n")
		return 0
	}
	fmt.Fprintf(stdout, "held moves: %d\n\n", len(cards))
	for _, c := range cards {
		fmt.Fprintf(stdout, "## %s  [%s / %s]  channel=%s\n", c.ID, strings.ToUpper(c.Decision), c.MutationClass, c.Channel)
		if c.ProductID != "" {
			fmt.Fprintf(stdout, "- product:    %s\n", c.ProductID)
		}
		if c.Capability != "" {
			fmt.Fprintf(stdout, "- capability: %s\n", c.Capability)
		}
		fmt.Fprintf(stdout, "- release gate: %s\n", c.ReleaseGate)
		for _, r := range c.RequiredInputs {
			fmt.Fprintf(stdout, "    required: %s\n", r)
		}
		for _, s := range c.StopConditions {
			fmt.Fprintf(stdout, "    stop-if:  %s\n", s)
		}
		fmt.Fprintln(stdout)
	}
	return 0
}
