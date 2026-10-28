package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/work"
)

const paddleSandboxApprovalTTL = 10 * time.Minute

var paddleSandboxNow = time.Now

type paddleSandboxOptions struct {
	setting        string
	toURL          string
	workID         string
	actor          string
	expectedDigest string
	homePath       string
	plan           bool
	confirm        bool
	json           bool
}

type paddleSandboxOutput struct {
	Mode             string                                 `json:"mode"`
	NoOp             bool                                   `json:"no_op,omitempty"`
	SettingID        string                                 `json:"setting_id"`
	CurrentSHA256    string                                 `json:"current_sha256"`
	Before           act.PaddleSandboxNotificationSnapshot  `json:"before"`
	ProposedURL      string                                 `json:"proposed_url"`
	ProposedEvents   []string                               `json:"proposed_events"`
	ActionIntentID   string                                 `json:"action_intent_id,omitempty"`
	PolicyDecisionID string                                 `json:"policy_decision_id,omitempty"`
	PolicyOutcome    string                                 `json:"policy_outcome,omitempty"`
	ApprovalRequired bool                                   `json:"approval_required,omitempty"`
	ApprovalPlanID   string                                 `json:"approval_plan_id,omitempty"`
	ApprovalExpires  string                                 `json:"approval_expires_at,omitempty"`
	RerunCommand     string                                 `json:"rerun_command,omitempty"`
	ReceiptID        string                                 `json:"receipt_id,omitempty"`
	TransitionID     string                                 `json:"transition_id,omitempty"`
	MoveType         string                                 `json:"move_type,omitempty"`
	PreImageRef      string                                 `json:"pre_image_ref,omitempty"`
	PostImageRef     string                                 `json:"post_image_ref,omitempty"`
	After            *act.PaddleSandboxNotificationSnapshot `json:"after,omitempty"`
}

// RunPaddleSandboxNotification is the CLI assembly around the typed actuator.
// It owns parsing, work/lease admission, exact-approval orchestration, and
// redacted rendering. Provider I/O and mutation scope remain in internal/act.
func RunPaddleSandboxNotification(args []string, stdout, stderr io.Writer) int {
	opts, err := parsePaddleSandboxOptions(args)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		fmt.Fprintln(stderr, PaddleSandboxUsage())
		return 2
	}
	apiKey := os.Getenv("PADDLE_SANDBOX_API_KEY")
	if strings.TrimSpace(apiKey) == "" {
		fmt.Fprintln(stderr, "error: PADDLE_SANDBOX_API_KEY is required")
		return 1
	}
	homePath, err := home.Resolve(opts.homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	now := paddleSandboxNow().UTC()
	if opts.confirm {
		if err := requirePaddleSandboxWorkLease(homePath, opts.workID, opts.actor); err != nil {
			fmt.Fprintln(stderr, "refused:", err)
			return 1
		}
	}

	adapter := act.NewPaddleSandboxNotificationAdapter(apiKey)
	plan, err := adapter.Plan(opts.setting, opts.toURL)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ai, err := act.BuildPaddleSandboxNotificationIntent(plan, opts.workID, opts.actor, now)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	baseOutput := paddleSandboxOutput{
		SettingID:      opts.setting,
		CurrentSHA256:  plan.BeforeDigest(),
		Before:         plan.Before,
		ProposedURL:    opts.toURL,
		ProposedEvents: append([]string(nil), plan.ToEvents...),
		ActionIntentID: ai.ID,
	}

	if !opts.plan && !opts.confirm {
		baseOutput.Mode = "preview"
		return printPaddleSandboxOutput(stdout, baseOutput, opts.json)
	}
	if opts.plan {
		return planPaddleSandboxAction(homePath, opts, plan, ai, baseOutput, stdout, stderr, now)
	}
	return confirmPaddleSandboxAction(homePath, opts, adapter, plan, ai, baseOutput, stdout, stderr, now)
}

func planPaddleSandboxAction(
	homePath string,
	opts paddleSandboxOptions,
	plan act.PaddleSandboxNotificationPlan,
	ai envelope.ActionIntent,
	out paddleSandboxOutput,
	stdout, stderr io.Writer,
	now time.Time,
) int {
	events, err := eventstore.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := events.PutJSON(ai.ID, ai, " "); err != nil {
		fmt.Fprintln(stderr, "error: persist ActionIntent:", err)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	pd := policy.Evaluate(ai, cfg, now)
	out.Mode = "plan"
	out.PolicyDecisionID = pd.ID
	out.PolicyOutcome = pd.Outcome
	out.RerunCommand = paddleSandboxRerun(opts, plan.BeforeDigest())
	if policy.Allowed(pd) {
		return printPaddleSandboxOutput(stdout, out, opts.json)
	}
	if pd.Outcome != "hold" || strings.TrimSpace(ai.SecurityFloorOutcome) != "" {
		fmt.Fprintf(stderr, "refused: policy outcome %s is not exactly approvable\n", pd.Outcome)
		return 1
	}
	approvalPlan, _, _, err := policy.NewExactActionApprovalStore(homePath).EnsurePlanForIntent(
		ai,
		cfg,
		paddleSandboxApprovalTTL,
		now,
	)
	if err != nil {
		fmt.Fprintln(stderr, "error: create exact approval plan:", err)
		return 1
	}
	out.ApprovalRequired = true
	out.ApprovalPlanID = approvalPlan.ID
	out.ApprovalExpires = approvalPlan.ExpiresAt
	return printPaddleSandboxOutput(stdout, out, opts.json)
}

func confirmPaddleSandboxAction(
	homePath string,
	opts paddleSandboxOptions,
	adapter *act.PaddleSandboxNotificationAdapter,
	plan act.PaddleSandboxNotificationPlan,
	ai envelope.ActionIntent,
	out paddleSandboxOutput,
	stdout, stderr io.Writer,
	now time.Time,
) int {
	if opts.expectedDigest != plan.BeforeDigest() {
		fmt.Fprintf(stderr, "refused: current notification fingerprint is %s; expected fingerprint is stale\n", plan.BeforeDigest())
		return 1
	}
	if plan.IsNoOp() {
		out.Mode = "confirmed"
		out.NoOp = true
		return printPaddleSandboxOutput(stdout, out, opts.json)
	}

	// The provider read above can overlap a work or kill-switch transition.
	// Re-read both authorities immediately before one-use approval consumption.
	if err := requirePaddleSandboxWorkLease(homePath, opts.workID, opts.actor); err != nil {
		fmt.Fprintln(stderr, "refused:", err)
		return 1
	}
	halted, reason, err := policy.AutonomyHalted(homePath, ai.MutationClass)
	if err != nil {
		fmt.Fprintln(stderr, "error: load autonomy halt:", err)
		return 1
	}
	if halted {
		fmt.Fprintf(stderr, "refused: autonomy halt blocks this action (%s)\n", reason)
		return 1
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	now = paddleSandboxNow().UTC()
	pd := policy.Evaluate(ai, cfg, now)
	artifacts, err := preflightPaddleSandboxStores(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "refused: persistence preflight:", err)
		return 1
	}
	if !policy.Allowed(pd) {
		if pd.Outcome != "hold" || strings.TrimSpace(ai.SecurityFloorOutcome) != "" {
			fmt.Fprintf(stderr, "refused: policy outcome %s cannot be overridden by exact approval\n", pd.Outcome)
			return 1
		}
		approved, _, applyErr := policy.NewExactActionApprovalStore(homePath).ApplyMatching(ai, cfg, pd, now)
		if applyErr != nil {
			fmt.Fprintln(stderr, "refused: exact approval:", applyErr)
			return 1
		}
		pd = approved
		if !policy.Allowed(pd) {
			fmt.Fprintln(stderr, "refused: exact approval required; run --plan and approve its exact plan ID")
			return 1
		}
	}

	workStore, err := work.Open(homePath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := workStore.RecordAction(opts.workID, ai, pd, now); err != nil {
		fmt.Fprintln(stderr, "refused: record governed action:", err)
		return 1
	}
	adapter.SetAdmissionCheck(func() error {
		if err := requirePaddleSandboxReservation(homePath, opts.workID, opts.actor, ai.ID, pd.ID); err != nil {
			return err
		}
		halted, reason, err := policy.AutonomyHalted(homePath, ai.MutationClass)
		if err != nil {
			return fmt.Errorf("autonomy halt authority unavailable")
		}
		if halted {
			return fmt.Errorf("autonomy halt blocks this action (%s)", reason)
		}
		return nil
	})
	result, err := act.Execute(artifacts, adapter, ai, pd, plan.ForwardValue(), now)
	if err != nil {
		fmt.Fprintln(stderr, "refused:", err)
		return 1
	}
	var after act.PaddleSandboxNotificationSnapshot
	if err := json.Unmarshal(result.PostImage, &after); err != nil {
		fmt.Fprintln(stderr, "error: recorded post-image is invalid")
		return 1
	}
	out.Mode = "confirmed"
	out.PolicyDecisionID = pd.ID
	out.PolicyOutcome = pd.Outcome
	out.ReceiptID = result.Receipt.ID
	out.TransitionID = result.Transition.ID
	out.MoveType = result.Receipt.MoveType
	out.PreImageRef = result.PreImageRef
	out.PostImageRef = result.PostImageRef
	out.After = &after
	return printPaddleSandboxOutput(stdout, out, opts.json)
}

func preflightPaddleSandboxStores(homePath string) (*act.ArtifactStore, error) {
	artifacts, err := act.NewArtifactStore(homePath)
	if err != nil {
		return nil, err
	}
	events, err := eventstore.Open(homePath)
	if err != nil {
		return nil, err
	}
	if err := events.Preflight(); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func requirePaddleSandboxWorkLease(homePath, workID, actor string) error {
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return fmt.Errorf("work registry unavailable")
	}
	w, err := store.Get(workID)
	if err != nil {
		return fmt.Errorf("unknown work %q", workID)
	}
	if w.State != "claimed" && w.State != "running" && w.State != "verifying" {
		return fmt.Errorf("work %s is not executable (%s); expected claimed, running, or verifying", workID, w.State)
	}
	leases, err := store.ListLeases()
	if err != nil {
		return fmt.Errorf("work lease authority unavailable")
	}
	for _, lease := range leases {
		if lease.WorkID != workID {
			continue
		}
		if lease.By != actor {
			return fmt.Errorf("work %s is held by %q, not actor %q", workID, lease.By, actor)
		}
		return nil
	}
	return fmt.Errorf("work %s has no active lease for actor %q", workID, actor)
}

func requirePaddleSandboxReservation(homePath, workID, actor, actionIntentID, policyDecisionID string) error {
	if err := requirePaddleSandboxWorkLease(homePath, workID, actor); err != nil {
		return err
	}
	store, err := work.OpenReadOnly(homePath)
	if err != nil {
		return fmt.Errorf("work registry unavailable")
	}
	w, err := store.Get(workID)
	if err != nil {
		return fmt.Errorf("unknown work %q", workID)
	}
	if w.PolicyDecisionID != policyDecisionID || len(w.ActionIntentIDs) == 0 || w.ActionIntentIDs[len(w.ActionIntentIDs)-1] != actionIntentID {
		return fmt.Errorf("work %s no longer reserves the exact governed action generation", workID)
	}
	return nil
}

func parsePaddleSandboxOptions(args []string) (paddleSandboxOptions, error) {
	var opts paddleSandboxOptions
	seen := map[string]bool{}
	valueFlags := map[string]*string{
		"--setting":                 &opts.setting,
		"--to-url":                  &opts.toURL,
		"--work":                    &opts.workID,
		"--actor":                   &opts.actor,
		"--expected-current-sha256": &opts.expectedDigest,
		"--home":                    &opts.homePath,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if seen[arg] {
			return opts, fmt.Errorf("duplicate flag %s", arg)
		}
		if dst, ok := valueFlags[arg]; ok {
			seen[arg] = true
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return opts, fmt.Errorf("%s requires a value", arg)
			}
			i++
			*dst = strings.TrimSpace(args[i])
			continue
		}
		switch arg {
		case "--plan":
			seen[arg], opts.plan = true, true
		case "--confirm":
			seen[arg], opts.confirm = true, true
		case "--json":
			seen[arg], opts.json = true, true
		case "--help", "-h":
			return opts, fmt.Errorf("help requested")
		default:
			return opts, fmt.Errorf("unknown argument %s", arg)
		}
	}
	missing := make([]string, 0, 4)
	for flag, value := range map[string]string{
		"--setting": opts.setting,
		"--to-url":  opts.toURL,
		"--work":    opts.workID,
		"--actor":   opts.actor,
	} {
		if value == "" {
			missing = append(missing, flag)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return opts, fmt.Errorf("required flags missing: %s", strings.Join(missing, ", "))
	}
	if opts.plan && opts.confirm {
		return opts, fmt.Errorf("--plan and --confirm are mutually exclusive")
	}
	if opts.confirm && opts.expectedDigest == "" {
		return opts, fmt.Errorf("--confirm requires --expected-current-sha256")
	}
	if !opts.confirm && opts.expectedDigest != "" {
		return opts, fmt.Errorf("--expected-current-sha256 is only valid with --confirm")
	}
	return opts, nil
}

func paddleSandboxRerun(opts paddleSandboxOptions, digest string) string {
	parts := []string{
		"compound", "act", "paddle-sandbox-notification",
		"--setting", opts.setting,
		"--to-url", opts.toURL,
		"--work", opts.workID,
		"--actor", opts.actor,
		"--expected-current-sha256", digest,
		"--confirm",
	}
	if opts.homePath != "" {
		parts = append(parts, "--home", opts.homePath)
	}
	if opts.json {
		parts = append(parts, "--json")
	}
	for i := range parts {
		parts[i] = shellQuote(parts[i])
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func printPaddleSandboxOutput(stdout io.Writer, out paddleSandboxOutput, asJSON bool) int {
	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(out); err != nil {
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "paddle sandbox notification %s\n", out.Mode)
	fmt.Fprintf(stdout, "setting: %s\ncurrent_sha256: %s\nproposed_url: %s\n", out.SettingID, out.CurrentSHA256, out.ProposedURL)
	fmt.Fprintf(stdout, "proposed_events: %s\n", strings.Join(out.ProposedEvents, ","))
	if out.NoOp {
		fmt.Fprintln(stdout, "no_op: true")
	}
	if out.PolicyOutcome != "" {
		fmt.Fprintf(stdout, "policy: %s\n", out.PolicyOutcome)
	}
	if out.ApprovalPlanID != "" {
		fmt.Fprintf(stdout, "approval_plan_id: %s\nrerun: %s\n", out.ApprovalPlanID, out.RerunCommand)
	}
	if out.ReceiptID != "" {
		fmt.Fprintf(stdout, "receipt: %s\ntransition: %s\n", out.ReceiptID, out.TransitionID)
	}
	return 0
}

func PaddleSandboxUsage() string {
	return "usage: compound act paddle-sandbox-notification --setting ntfset_* --to-url https://.../webhooks/paddle --work W --actor A [--expected-current-sha256 sha256:*] [--plan|--confirm] [--json] [--home P]"
}
