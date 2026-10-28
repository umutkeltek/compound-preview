package browser

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
)

const jevRevision = "1231850a0bf1a0c0341fe408ef1668dbbfdfac46"

var ErrRoutedToPlaywright = errors.New("browser goal routed to Playwright; execute through the enrolled Browser skill")

//go:embed jev_bridge.py
var bridgeSource embed.FS

type TaskRequest struct {
	Actor           string   `json:"-"`
	Workspace       string   `json:"-"`
	NonGoals        []string `json:"-"`
	RunningActionID string   `json:"-"`
	RunAttemptID    string   `json:"-"`
	ContractDigest  string   `json:"-"`
	ContractJSON    string   `json:"-"`
	ProviderModel   string   `json:"-"`
	ProviderBound   float64  `json:"-"`
	Home            string   `json:"-"`
	WorkID          string   `json:"work_id"`
	URL             string   `json:"url"`
	Goal            string   `json:"goal"`
	ExpectText      string   `json:"expect_text"`
	MaxSteps        int      `json:"max_steps"`
	// MaxProviderUSD is the caller's per-task spending ceiling. A separate
	// operator-supplied per-request provider ceiling is required before launch.
	MaxProviderUSD float64 `json:"max_provider_usd"`
	ProfileID      string  `json:"profile_id,omitempty"`
}

type TaskResult struct {
	RunAttemptID  string   `json:"run_attempt_id,omitempty"`
	WorkState     string   `json:"work_state,omitempty"`
	Backend       string   `json:"backend"`
	Status        string   `json:"status"`
	Verified      bool     `json:"verified"`
	Steps         int      `json:"steps"`
	Reason        string   `json:"reason,omitempty"`
	ReceiptIDs    []string `json:"receipt_ids"`
	TransitionIDs []string `json:"transition_ids"`
}

type bridgeEvent struct {
	Event     string `json:"event"`
	Step      int    `json:"step"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	URL       string `json:"url"`
	TargetURL string `json:"target_url"`
	Status    string `json:"status"`
	Verified  bool   `json:"verified"`
	Steps     int    `json:"steps"`
	Code      string `json:"code"`
}

type bridgeFactory func(context.Context) *exec.Cmd

func jevCommand(ctx context.Context) *exec.Cmd {
	script, _ := bridgeSource.ReadFile("jev_bridge.py")
	cmd := exec.CommandContext(ctx, "uv", "run", "--no-project", "--with", "git+https://github.com/browser-use/jev-ultrafast.git@"+jevRevision, "python", "-c", string(script))
	configureJevProcess(cmd)
	return cmd
}

// RunTask owns routing and the Jev protocol. The bridge is never started for
// disabled or profile-bound work. Each proposed action is decided before act.
func RunTask(ctx context.Context, req TaskRequest, cfg policy.Config) (TaskResult, error) {
	return runTask(ctx, req, cfg, jevCommand)
}

func runTaskProtocol(ctx context.Context, req TaskRequest, cfg policy.Config, launch bridgeFactory, startBridge func(context.Context, *exec.Cmd) error, checkReservation func(string) error) (result TaskResult, err error) {
	result.ReceiptIDs = []string{}
	result.TransitionIDs = []string{}
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
	if !strings.HasPrefix(req.WorkID, "work_") || len(req.Goal) == 0 || len(req.Goal) > 1000 || len(req.ExpectText) == 0 || len(req.ExpectText) > 200 || req.MaxSteps < 1 || req.MaxSteps > 6 {
		return result, errors.New("browser goal requires work_id, bounded goal, expected text, and 1-6 steps")
	}
	origin, err := publicOrigin(req.URL)
	if err != nil {
		return result, err
	}
	providerModel := settings.ProviderModel
	if providerModel == "" {
		providerModel = os.Getenv("TYPESAFE_MODEL")
	}
	if providerModel == "" {
		providerModel = "jev-1.13.0"
	}
	providerBound, err := providerCostBound(req, settings, providerModel)
	if err != nil {
		return result, err
	}
	store, err := eventstore.Open(req.Home)
	if err != nil {
		return result, err
	}
	start := bridgeEvent{Step: 0, Kind: "navigate", URL: req.URL}
	startIntent, startDecision, _ := decideStep(req, origin, start, cfg)
	startIntent.Action = "start_browser_and_provider_predictions"
	startIntent.MutationClass = "financial"
	startIntent.Reversibility = "none"
	startIntent.BlastRadius = "account"
	startIntent.RollbackPlan = "stop before provider contact; provider charges already incurred cannot be reversed"
	startIntent.EstimatedCost.USD = &providerBound
	startDecision = policy.Evaluate(startIntent, cfg, time.Now().UTC())
	if err := startIntent.Validate(); err != nil {
		return result, err
	}
	if err := recordEvent(store, startIntent.ID, &startIntent); err != nil {
		return result, err
	}
	if err := recordEvent(store, startDecision.ID, &startDecision); err != nil {
		return result, err
	}
	if err := policy.Gate(startDecision); err != nil {
		receipt, transition, writeErr := recordStep(store, startIntent, "blocked", "browser start held before launch")
		if writeErr != nil {
			return result, writeErr
		}
		result.Status = "held"
		result.ReceiptIDs = append(result.ReceiptIDs, receipt)
		result.TransitionIDs = append(result.TransitionIDs, transition)
		return result, fmt.Errorf("browser start held by policy: %w", err)
	}
	var pending *envelope.ActionIntent
	defer func() {
		if err == nil || result.Status == "failed" || result.Status == "scope_violation" {
			return
		}
		result.Status, result.Verified = "failed", false
		failedIntent := startIntent
		if pending != nil {
			failedIntent = *pending
		}
		receipt, transition, recordErr := recordStep(store, failedIntent, "failed", "browser task stopped without verified result")
		if recordErr != nil {
			err = errors.Join(err, recordErr)
			return
		}
		result.ReceiptIDs = append(result.ReceiptIDs, receipt)
		result.TransitionIDs = append(result.TransitionIDs, transition)
	}()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	sessionDir, err := newJevSession()
	if err != nil {
		return result, err
	}
	defer func() {
		if cleanupErr := cleanupJevSession(sessionDir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean Jev session: %w", cleanupErr))
			result.Verified = false
			result.Status = "failed"
		}
	}()
	cmd := launch(ctx)
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(providerChildEnv(cmd.Env), "COMPOUND_JEV_SESSION_DIR="+sessionDir, "TYPESAFE_MODEL="+providerModel)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	// Upstream diagnostics can contain page content. Never forward stderr.
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{dst: &stderr, remaining: 4096}
	if err := startBridge(ctx, cmd); err != nil {
		return result, fmt.Errorf("start Jev bridge: %w", err)
	}
	defer func() {
		if err != nil {
			cancel()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 16*1024)
	if !scanner.Scan() {
		return result, errors.New("Jev bridge did not become ready")
	}
	var ready bridgeEvent
	if json.Unmarshal(scanner.Bytes(), &ready) != nil || ready.Event != "ready" {
		return result, errors.New("Jev bridge readiness protocol failed")
	}
	if err := checkReservation(""); err != nil {
		return result, err
	}
	credentialEnv, err := capgraph.ResolveCredentialForSurface(req.Home, jevCredentialSurface, req.WorkID, jevCredentialHandle())
	if errors.Is(err, capgraph.ErrCredentialSurfaceNotRegistered) {
		if key := os.Getenv("TYPESAFE_API_KEY"); key != "" {
			credentialEnv, err = "TYPESAFE_API_KEY="+key, nil
		}
	}
	if err != nil {
		return result, fmt.Errorf("Jev requires a registered TypeSafe credential: %w", err)
	}
	request := map[string]any{"work_id": req.WorkID, "actor": req.Actor, "url": req.URL, "goal": req.Goal, "expect_text": req.ExpectText, "non_goals": req.NonGoals, "provider_model": req.ProviderModel, "provider_bound": req.ProviderBound, "max_provider_usd": req.MaxProviderUSD, "contract_json": req.ContractJSON, "contract_digest": req.ContractDigest, "max_steps": req.MaxSteps, "typesafe_api_key": strings.TrimPrefix(credentialEnv, "TYPESAFE_API_KEY=")}
	if err := json.NewEncoder(stdin).Encode(request); err != nil {
		return result, err
	}
	var lastIntent *envelope.ActionIntent
	terminalDone := false
	predictAuthorized := false
	providerAuthorized := false
	for scanner.Scan() {
		var event bridgeEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return result, errors.New("invalid Jev bridge event")
		}
		switch event.Event {
		case "predict_request":
			if pending != nil || predictAuthorized || event.Step != result.Steps+1 || event.Step > req.MaxSteps {
				return result, errors.New("out-of-order Jev prediction request")
			}
			if err := checkReservation(credentialEnv); err != nil {
				_ = json.NewEncoder(stdin).Encode(map[string]bool{"allow": false})
				return result, fmt.Errorf("Jev prediction held before provider contact: %w", err)
			}
			if err := json.NewEncoder(stdin).Encode(map[string]bool{"allow": true}); err != nil {
				return result, err
			}
			predictAuthorized = true
			providerAuthorized = true
		case "proposal":
			if !predictAuthorized {
				return result, errors.New("Jev proposal lacked provider authorization")
			}
			predictAuthorized = false
			if err := checkReservation(credentialEnv); err != nil {
				return result, err
			}
			currentPolicy, _, err := policy.LoadConfig(req.Home)
			if err != nil {
				return result, err
			}
			if pending != nil || event.Step != result.Steps+1 || event.Step > req.MaxSteps {
				return result, errors.New("out-of-order Jev proposal")
			}
			intent, decision, allowed := decideStep(req, origin, event, currentPolicy)
			if err := recordEvent(store, intent.ID, &intent); err != nil {
				return result, err
			}
			if err := recordEvent(store, decision.ID, &decision); err != nil {
				return result, err
			}
			if !allowed {
				receipt, transition, writeErr := recordStep(store, intent, "blocked", "proposal held before browser action")
				if writeErr != nil {
					return result, writeErr
				}
				result.ReceiptIDs = append(result.ReceiptIDs, receipt)
				result.TransitionIDs = append(result.TransitionIDs, transition)
				result.Status = "held"
			}
			if err := json.NewEncoder(stdin).Encode(map[string]bool{"allow": allowed}); err != nil {
				return result, err
			}
			if allowed {
				pending = &intent
				lastIntent = &intent
				if event.Kind == "done" {
					terminalDone = true
				}
			}
		case "acted":
			if pending == nil || event.Step != result.Steps+1 {
				return result, errors.New("unexpected Jev act")
			}
			status := "failed"
			switch {
			case pending.Action == "done" && event.Status == "done":
				status = "no_op" // model claim only; result readback decides task success
			case pending.Action == "blocked" && event.Status == "blocked":
				status = "blocked"
			case (pending.Action == "scroll" || pending.Action == "wait") && event.Status == "ready":
				status = "completed"
			case (pending.Action == "scroll" || pending.Action == "wait") && event.Status == "blocked":
				status = "blocked"
			}
			if _, urlErr := scopedURL(origin, event.URL); urlErr != nil {
				status = "failed"
			}
			receipt, transition, writeErr := recordStep(store, *pending, status, "Jev browser action observed")
			if writeErr != nil {
				return result, writeErr
			}
			result.ReceiptIDs = append(result.ReceiptIDs, receipt)
			result.TransitionIDs = append(result.TransitionIDs, transition)
			result.Steps++
			pending = nil
			if status == "failed" {
				result.Status = "failed"
				return result, errors.New("Jev act was invalid or left the allowed origin")
			}
		case "result":
			if pending != nil {
				return result, errors.New("Jev result without act")
			}
			if event.Status != "held" && event.Status != "verified" && event.Status != "unverified" && event.Status != "blocked" && event.Status != "budget_exhausted" {
				return result, errors.New("invalid Jev result status")
			}
			if result.Status == "held" && event.Status != "held" {
				return result, errors.New("Jev ignored held action")
			}
			if event.Status == "verified" && (!terminalDone || !event.Verified || event.Steps != result.Steps) {
				return result, errors.New("invalid Jev verification")
			}
			if event.Status == "verified" {
				if _, urlErr := scopedURL(origin, event.URL); urlErr != nil {
					return result, urlErr
				}
			}
			result.Status, result.Verified = event.Status, event.Status == "verified" && event.Verified
			if lastIntent != nil && event.Status != "held" {
				outcome := "blocked"
				summary := "browser goal lacked independent readback proof"
				if result.Verified {
					outcome, summary = "completed", "browser readback matched expected text"
				}
				receipt, transition, writeErr := recordStep(store, *lastIntent, outcome, summary)
				if writeErr != nil {
					return result, writeErr
				}
				result.ReceiptIDs = append(result.ReceiptIDs, receipt)
				result.TransitionIDs = append(result.TransitionIDs, transition)
			}
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				return result, errors.New("Jev bridge failed after result")
			}
			if !providerAuthorized {
				receipt, transition, writeErr := recordStep(store, startIntent, "blocked", "provider prediction refused before contact")
				if writeErr != nil {
					return result, writeErr
				}
				result.ReceiptIDs = append(result.ReceiptIDs, receipt)
				result.TransitionIDs = append(result.TransitionIDs, transition)
				return result, errors.New("Jev finished without authorized prediction")
			}
			receipt, transition, writeErr := recordStep(store, startIntent, "completed", "bounded provider prediction authorized; actual contact and charge unobserved")
			if writeErr != nil {
				return result, writeErr
			}
			result.ReceiptIDs = append(result.ReceiptIDs, receipt)
			result.TransitionIDs = append(result.TransitionIDs, transition)
			return result, nil
		case "error":
			result.Status = "failed"
			failedIntent := startIntent
			if pending != nil {
				failedIntent = *pending
			}
			receipt, transition, writeErr := recordStep(store, failedIntent, "failed", "Jev bridge failed without verified browser result")
			if writeErr != nil {
				return result, writeErr
			}
			result.ReceiptIDs = append(result.ReceiptIDs, receipt)
			result.TransitionIDs = append(result.TransitionIDs, transition)
			return result, fmt.Errorf("Jev bridge error (%s)", safeCode(event.Code))
		default:
			return result, errors.New("unknown Jev bridge event")
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("Jev bridge read: %w", err)
	}
	return result, errors.New("Jev bridge ended without result")
}

// Upstream can retry a TypeSafe POST three times for each predict. The bound
// must be backed by a provider/account contract; a guessed price is unsafe.
func providerCostBound(req TaskRequest, settings Settings, model string) (float64, error) {
	price := ""
	if settings.ProviderMaxUSDPerRequest > 0 {
		price = strconv.FormatFloat(settings.ProviderMaxUSDPerRequest, 'f', -1, 64)
	} else {
		price = os.Getenv("TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST")
	}
	perRequest, err := strconv.ParseFloat(price, 64)
	if err != nil || !finitePositive(perRequest) || !finitePositive(req.MaxProviderUSD) {
		return 0, errors.New("Jev requires a provider per-request cost ceiling and --provider-budget-usd")
	}
	if perRequest < minPricedJevUSDPerRequest {
		return 0, errors.New("Jev provider ceiling is below the published pinned-model maximum")
	}
	bound := perRequest * float64(req.MaxSteps*3)
	if !finitePositive(bound) || bound > req.MaxProviderUSD {
		return 0, errors.New("Jev provider request bound exceeds task budget")
	}
	if model != "jev-1.13.0" {
		return 0, errors.New("Jev cost contract covers only pinned Jev 1.13")
	}
	return bound, nil
}

func providerChildEnv(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base))
	for _, item := range base {
		if strings.HasPrefix(item, "TYPESAFE_API_KEY=") || strings.HasPrefix(item, "TYPESAFE_MODEL=") || strings.HasPrefix(item, "TYPESAFE_PROVIDER_MAX_USD_PER_REQUEST=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

type limitedWriter struct {
	dst       *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining > 0 {
		take := len(p)
		if take > w.remaining {
			take = w.remaining
		}
		_, _ = w.dst.Write(p[:take])
		w.remaining -= take
	}
	return n, nil
}

func safeCode(code string) string {
	for _, r := range code {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return "unknown"
		}
	}
	if len(code) > 60 {
		return "unknown"
	}
	return code
}

func publicOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") {
		return "", errors.New("Jev requires a plain public HTTPS URL without credentials, query, or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil || host == "localhost" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".invalid") {
		return "", errors.New("Jev requires a public DNS hostname")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func scopedURL(origin, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || strings.ToLower(u.Host) != strings.TrimPrefix(origin, "https://") || u.User != nil {
		return "", errors.New("browser URL left allowed origin")
	}
	return u.String(), nil
}

func decideStep(req TaskRequest, origin string, event bridgeEvent, cfg policy.Config) (envelope.ActionIntent, envelope.PolicyDecision, bool) {
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte(req.WorkID + "\x00" + origin + "\x00" + fmt.Sprint(event.Step) + "\x00" + now.Format(time.RFC3339Nano)))
	intent := envelope.ActionIntent{Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_" + hex.EncodeToString(digest[:8]), CreatedAt: now.Format(time.RFC3339Nano), Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: req.WorkID}, Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1", EvidenceRefs: []string{}, Uncertainty: envelope.Uncertainty{Level: "known"}}, Actor: "compound.browser", Capability: "browser.goal.jev", Action: event.Kind, Target: envelope.ActionTarget{Kind: "browser_page", ID: origin}, MutationClass: "read", Reversibility: "easy", BlastRadius: "local", Preview: "bounded public browser step"}
	if req.RunningActionID != "" {
		intent.EvidenceRefs = append(intent.EvidenceRefs, req.RunningActionID)
	}
	if req.RunAttemptID != "" {
		intent.EvidenceRefs = append(intent.EvidenceRefs, req.RunAttemptID)
	}
	intent.ArgumentsHash = req.ContractDigest
	safe := false
	if _, err := scopedURL(origin, event.URL); err == nil {
		switch {
		case event.Kind == "navigate" && event.Step == 0:
			safe = true
		case event.Kind == "done" || event.Kind == "blocked" || event.Kind == "scroll" || event.Kind == "wait":
			safe = true
		}
	}
	if !safe {
		intent.MutationClass = "external_side_effect"
		intent.Reversibility = "manual"
		intent.BlastRadius = "public"
		intent.RollbackPlan = "stop before act; operator must separately reconcile any external effect"
	}
	decision := policy.Evaluate(intent, cfg, now)
	allowed := intent.Validate() == nil && decision.Validate() == nil && policy.Gate(decision) == nil && safe
	return intent, decision, allowed
}

func recordEvent(store *eventstore.Store, id string, value any) error {
	_, err := store.PutJSON(id, value, "  ")
	return err
}

func recordStep(store *eventstore.Store, intent envelope.ActionIntent, outcome, summary string) (string, string, error) {
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte(intent.ID + "\x00" + outcome + "\x00" + now.Format(time.RFC3339Nano)))
	receipt := envelope.Receipt{Header: intent.Header, Outcome: outcome, MoveType: "audit_only", StateDelta: envelope.StateDelta{Changed: false, Summary: summary}, Cost: envelope.ReceiptCost{ModelCalls: []string{}, USDEstimated: intent.EstimatedCost.USD}, Skills: envelope.ReceiptSkills{Used: []string{intent.Capability}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true}, Memory: envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}}, Next: envelope.ReceiptNext{SuggestedWork: []string{}}}
	receipt.SchemaVersion, receipt.ID, receipt.CreatedAt = envelope.SVReceipt, "rec_"+hex.EncodeToString(digest[:8]), now.Format(time.RFC3339Nano)
	if err := receipt.Validate(); err != nil {
		return "", "", err
	}
	transition := envelope.Transition{Header: intent.Header, BeforeRefs: []string{}, ActionRefs: []string{intent.ID}, AfterRefs: []string{receipt.ID}, StateDelta: summary, Projections: envelope.Projections{Memory: []string{}, Skill: []string{}, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{}}, SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: false}}
	transition.SchemaVersion, transition.ID, transition.CreatedAt = envelope.SVTransition, "tr_"+hex.EncodeToString(digest[:8]), now.Format(time.RFC3339Nano)
	if err := transition.Validate(); err != nil {
		return "", "", err
	}
	if err := recordEvent(store, receipt.ID, &receipt); err != nil {
		return "", "", err
	}
	if err := recordEvent(store, transition.ID, &transition); err != nil {
		return receipt.ID, "", err
	}
	return receipt.ID, transition.ID, nil
}
