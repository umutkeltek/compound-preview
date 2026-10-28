package browser

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/actuator"
	"compound/internal/capgraph"
	"compound/internal/eventstore"
	"compound/internal/harness"
	"compound/internal/home"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
)

// runTask admits a saved, claimed browser job into the normal Work execution
// rail. The browser protocol remains an adapter action, never a second work
// lifecycle or a task-success decision.
func runTask(ctx context.Context, req TaskRequest, cfg policy.Config, launch bridgeFactory) (TaskResult, error) {
	result := TaskResult{ReceiptIDs: []string{}, TransitionIDs: []string{}}
	settings, err := LoadSettings(req.Home)
	if err != nil {
		return result, err
	}
	route := SelectRoute(settings, req.ProfileID)
	result.Backend = route.Backend
	if route.Backend != "jev" {
		result.Status, result.Reason = "routed", route.Reason
		return result, ErrRoutedToPlaywright
	}
	if !jevProcessSupported() {
		return result, errors.New("isolated Jev process supervision is unsupported on this OS")
	}
	if !strings.HasPrefix(req.WorkID, "work_") || req.Actor == "" || req.Goal == "" || req.ExpectText == "" || req.MaxSteps < 1 || req.MaxSteps > 6 || len(req.Goal) > 1000 || len(req.ExpectText) > 200 {
		return result, errors.New("browser goal requires work, actor, bounded goal, expected text, and 1-6 steps")
	}
	if _, err := publicOrigin(req.URL); err != nil {
		return result, err
	}
	store, err := work.OpenReadOnly(req.Home)
	if err != nil {
		return result, err
	}
	saved, err := store.Get(req.WorkID)
	if err != nil {
		return result, fmt.Errorf("browser work: %w", err)
	}
	if saved.State != "claimed" {
		return result, fmt.Errorf("browser work %s must be claimed, got %s", req.WorkID, saved.State)
	}
	if saved.ExecutionSelection != nil {
		return result, fmt.Errorf("browser work %s has saved execution selection %q; Jev has no compatible saved launch profile", req.WorkID, saved.ExecutionSelection.LaunchProfileID)
	}
	if saved.CanonicalSurface != req.URL || saved.DesiredOutcome != req.Goal || saved.SuccessPredicate != req.ExpectText {
		return result, errors.New("browser URL, goal, or expected text conflicts with saved work contract")
	}
	leases, err := store.ListLeases()
	if err != nil {
		return result, err
	}
	hasLease := false
	for _, lease := range leases {
		if lease.WorkID == req.WorkID && lease.By == req.Actor {
			hasLease = true
			break
		}
	}
	if !hasLease {
		return result, fmt.Errorf("browser work %s has no lease held by actor %q", req.WorkID, req.Actor)
	}
	// All predictable launch failures are checked before claimed -> running.
	if held, err := browserPreflight(req, settings, cfg); err != nil {
		held.Backend = "jev"
		return held, err
	}
	workspace := req.Workspace
	if workspace == "" {
		workspace, err = os.Getwd()
		if err != nil {
			return result, err
		}
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return result, fmt.Errorf("resolve browser workspace: %w", err)
	}
	homePath, err := filepath.EvalSymlinks(req.Home)
	if err != nil {
		return result, fmt.Errorf("resolve COMPOUND_HOME: %w", err)
	}
	if relative, relErr := filepath.Rel(homePath, workspace); relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return result, errors.New("browser workspace cannot be inside COMPOUND_HOME")
	}
	if stat, err := os.Stat(workspace); err != nil || !stat.IsDir() {
		return result, fmt.Errorf("browser workspace must be a directory: %w", err)
	}

	runStore, err := work.Open(req.Home)
	if err != nil {
		return result, err
	}
	req.NonGoals = append([]string(nil), saved.NonGoals...)
	req.ProviderModel = effectiveJevModel(settings)
	req.ProviderBound, err = providerCostBound(req, settings, req.ProviderModel)
	if err != nil {
		return result, err
	}
	contract, err := json.Marshal(struct {
		WorkID, Actor, URL, Goal, ExpectText, ProviderModel string
		NonGoals                                            []string
		MaxSteps                                            int
		MaxProviderUSD, ProviderBound                       float64
	}{req.WorkID, req.Actor, req.URL, req.Goal, req.ExpectText, req.ProviderModel, req.NonGoals, req.MaxSteps, req.MaxProviderUSD, req.ProviderBound})
	if err != nil {
		return result, err
	}
	digest := sha256.Sum256(contract)
	req.ContractDigest = hex.EncodeToString(digest[:])
	req.ContractJSON = base64.StdEncoding.EncodeToString(contract)
	adapter := &jevTaskAdapter{homePath: req.Home, request: req, config: cfg, launch: launch}
	now := time.Now().UTC()
	executed, err := actuator.ExecuteGovernedHarnessAdvance(ctx, req.Home, runStore, saved, adapter, harness.RunInput{
		WorkID: req.WorkID, Harness: adapter.Name(), Workspace: workspace, Command: "jev-bridge", Timeout: 90 * time.Second, Now: now,
		LaneID: req.Actor, ProofRefs: []string{"browser_task_sha256:" + req.ContractDigest},
	}, req.Actor, "running", "browser Jev task reserved browser_task_sha256:"+req.ContractDigest, saved.RunAttemptIDs, false, now,
		actuator.GovernedHarnessContinuationOptions{RetainFailedAttempt: true})
	result = adapter.result
	if result.ReceiptIDs == nil {
		result.ReceiptIDs = []string{}
	}
	if result.TransitionIDs == nil {
		result.TransitionIDs = []string{}
	}
	result.Backend = "jev"
	result.RunAttemptID = executed.Output.RunAttempt.ID
	result.WorkState = executed.Finalized.FinalState
	if result.RunAttemptID == "" {
		result.RunAttemptID = adapter.attemptID
	}
	if result.WorkState == "" {
		if current, getErr := runStore.Get(req.WorkID); getErr == nil {
			result.WorkState = current.State
		}
	}
	if err != nil {
		result.Verified = false
		return result, err
	}
	if executed.RunError != nil {
		result.Verified = false
		return result, executed.RunError
	}
	return result, nil
}

func browserPreflight(req TaskRequest, settings Settings, cfg policy.Config) (TaskResult, error) {
	result := TaskResult{ReceiptIDs: []string{}, TransitionIDs: []string{}}
	origin, err := publicOrigin(req.URL)
	if err != nil {
		return result, err
	}
	if SelectRoute(settings, req.ProfileID).Backend != "jev" {
		return result, errors.New("Jev route no longer enabled")
	}
	if err := jevCredentialReady(req.Home); err != nil {
		return result, err
	}
	model := settings.ProviderModel
	if model == "" {
		model = os.Getenv("TYPESAFE_MODEL")
	}
	if model == "" {
		model = "jev-1.13.0"
	}
	bound, err := providerCostBound(req, settings, model)
	if err != nil {
		return result, err
	}
	intent, _, _ := decideStep(req, origin, bridgeEvent{Step: 0, Kind: "navigate", URL: req.URL}, cfg)
	intent.Action = "start_browser_and_provider_predictions"
	intent.MutationClass, intent.Reversibility, intent.BlastRadius = "financial", "none", "account"
	intent.RollbackPlan = "stop before provider contact; provider charges already incurred cannot be reversed"
	intent.EstimatedCost.USD = &bound
	if err := intent.Validate(); err != nil {
		return result, err
	}
	decision := policy.Evaluate(intent, cfg, time.Now().UTC())
	if err := policy.Gate(decision); err != nil {
		events, openErr := eventstore.Open(req.Home)
		if openErr != nil {
			return result, errors.Join(err, openErr)
		}
		if writeErr := recordEvent(events, intent.ID, &intent); writeErr != nil {
			return result, errors.Join(err, writeErr)
		}
		if writeErr := recordEvent(events, decision.ID, &decision); writeErr != nil {
			return result, errors.Join(err, writeErr)
		}
		receipt, transition, writeErr := recordStep(events, intent, "blocked", "browser start held before launch")
		if writeErr != nil {
			return result, errors.Join(err, writeErr)
		}
		result.Status = "held"
		result.ReceiptIDs = append(result.ReceiptIDs, receipt)
		result.TransitionIDs = append(result.TransitionIDs, transition)
		return result, fmt.Errorf("browser start held by policy: %w", err)
	}
	return result, nil
}

func effectiveJevModel(settings Settings) string {
	if settings.ProviderModel != "" {
		return settings.ProviderModel
	}
	if model := os.Getenv("TYPESAFE_MODEL"); model != "" {
		return model
	}
	return "jev-1.13.0"
}

// Readiness checks the enrolled handle without opening the secret or writing
// credential usage. The guarded adapter resolves the value later.
func jevCredentialReady(homePath string) error {
	registryPath := filepath.Join(homePath, "registries", "capability-connection-surfaces.jsonl")
	info, statErr := os.Lstat(registryPath)
	if errors.Is(statErr, os.ErrNotExist) {
		if os.Getenv("TYPESAFE_API_KEY") != "" {
			return nil
		}
		return errors.New("Jev requires a registered TypeSafe credential")
	}
	if statErr != nil {
		return fmt.Errorf("Jev credential registry: %w", statErr)
	}
	if !info.Mode().IsRegular() {
		return errors.New("Jev credential registry is not a regular file")
	}
	store, err := capgraph.OpenReadOnly(homePath)
	if err != nil {
		return fmt.Errorf("Jev credential registry unavailable: %w", err)
	}
	surfaces, err := store.ConnectionSurfaces()
	if err != nil {
		return fmt.Errorf("Jev credential registry: %w", err)
	}
	for _, surface := range surfaces {
		if surface.NodeID == jevCredentialSurface {
			if surface.CredentialHandle != jevCredentialHandle() {
				return errors.New("Jev credential surface has unexpected handle")
			}
			return nil
		}
	}
	return errors.New("Jev credential surface absent from existing registry")
}

type jevTaskAdapter struct {
	homePath  string
	request   TaskRequest
	config    policy.Config
	launch    bridgeFactory
	result    TaskResult
	attemptID string
}

func (*jevTaskAdapter) Name() string { return "browser-jev" }

func (a *jevTaskAdapter) Run(ctx context.Context, in harness.RunInput) (out harness.RunOutput, err error) {
	raStore := runattempt.Open(a.homePath)
	attempt, err := raStore.MintWithActionIntent(in.WorkID, a.request.Actor, a.Name(), in.ActionIntentID, time.Now().UTC())
	if err != nil {
		return out, err
	}
	attemptID := attempt.ID
	a.attemptID = attemptID
	out.RunAttempt = attempt
	defer func() {
		status := runattempt.StatusSucceeded
		if err != nil {
			status = runattempt.StatusFailed
			out.ExitCode = -1
		}
		terminal, terminalErr := raStore.Complete(attemptID, status, time.Now().UTC())
		if terminalErr != nil {
			err = errors.Join(err, terminalErr)
		} else {
			out.RunAttempt = terminal
		}
	}()
	if _, err = raStore.SetOrigin(attemptID, runattempt.OriginHarnessExecution); err != nil {
		return out, err
	}
	if _, err = raStore.SetWorkspace(attemptID, in.Workspace); err != nil {
		return out, err
	}
	if _, err = raStore.SetCommand(attemptID, in.Command); err != nil {
		return out, err
	}
	if in.ExecutionPosture != nil {
		if _, err = raStore.SetExecutionPosture(attemptID, *in.ExecutionPosture); err != nil {
			return out, err
		}
	}
	if _, err = raStore.SetProofRefs(attemptID, in.ProofRefs); err != nil {
		return out, err
	}
	if _, err = raStore.SetAttribution(attemptID, a.request.Actor, "", ""); err != nil {
		return out, err
	}
	if in.StartGuard == nil {
		return out, errors.New("browser governed start guard is missing")
	}
	workStore, err := work.OpenReadOnly(a.homePath)
	if err != nil {
		return out, err
	}
	running, err := workStore.Get(in.WorkID)
	if err != nil {
		return out, err
	}
	if len(running.ActionIntentIDs) == 0 || running.ActionIntentIDs[len(running.ActionIntentIDs)-1] != in.ActionIntentID || running.State != "running" {
		return out, work.ErrExecutionReservationChanged
	}
	policyID := running.PolicyDecisionID
	if running.IDs.ContextPackID != "" {
		if _, err = raStore.SetContextPack(attemptID, running.IDs.ContextPackID); err != nil {
			return out, err
		}
	}
	// A heartbeat remains live while the bidirectional Jev protocol waits for
	// predictions and browser actions. Terminalization always stops it first.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	done := make(chan struct{})
	heartbeatErrors := make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, beatErr := raStore.Heartbeat(attemptID, time.Now().UTC()); beatErr != nil {
					heartbeatErrors <- beatErr
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
		select {
		case beatErr := <-heartbeatErrors:
			err = errors.Join(err, fmt.Errorf("browser attempt heartbeat: %w", beatErr))
			a.result.Verified = false
			a.result.Status = "failed"
		default:
		}
	}()
	start := func(startCtx context.Context, cmd *exec.Cmd) error {
		return in.StartGuard(startCtx, func() error {
			_, startErr := raStore.CommitLaunch(attemptID, time.Now().UTC(), func() error { return cmd.Start() })
			return startErr
		})
	}
	check := func(initialCredential string) error {
		if err := home.WithAutonomyAdmissionLock(a.homePath, func() error {
			return actuator.RevalidateGovernedExecutionReservation(a.homePath, in.WorkID, policyID, in.ActionIntentID)
		}); err != nil {
			return err
		}
		settings, err := LoadSettings(a.homePath)
		if err != nil {
			return err
		}
		model := effectiveJevModel(settings)
		bound, err := providerCostBound(a.request, settings, model)
		if err != nil {
			return err
		}
		if model != a.request.ProviderModel || bound != a.request.ProviderBound {
			return errors.New("Jev model or provider cost contract changed before prediction")
		}
		currentPolicy, _, err := policy.LoadConfig(a.homePath)
		if err != nil {
			return err
		}
		if _, err = browserPreflight(a.request, settings, currentPolicy); err != nil {
			return err
		}
		currentCredential, err := capgraph.ResolveCredentialForSurface(a.homePath, jevCredentialSurface, in.WorkID, jevCredentialHandle())
		if errors.Is(err, capgraph.ErrCredentialSurfaceNotRegistered) {
			if key := os.Getenv("TYPESAFE_API_KEY"); key != "" {
				currentCredential, err = "TYPESAFE_API_KEY="+key, nil
			}
		}
		if err != nil {
			return err
		}
		if initialCredential != "" && currentCredential != initialCredential {
			return errors.New("Jev credential assignment changed before prediction")
		}
		return nil
	}
	a.request.RunningActionID, a.request.RunAttemptID = in.ActionIntentID, attemptID
	a.result, err = runTaskProtocol(runCtx, a.request, a.config, a.launch, start, check)
	proofRefs := append([]string(nil), in.ProofRefs...)
	proofRefs = append(proofRefs, a.result.ReceiptIDs...)
	proofRefs = append(proofRefs, a.result.TransitionIDs...)
	if _, proofErr := raStore.SetProofRefs(attemptID, proofRefs); proofErr != nil {
		err = errors.Join(err, fmt.Errorf("browser attempt proof refs: %w", proofErr))
		a.result.Verified = false
		a.result.Status = "failed"
	}
	if err != nil {
		out.ExitCode = -1
	}
	return out, err
}

var _ harness.Adapter = (*jevTaskAdapter)(nil)
