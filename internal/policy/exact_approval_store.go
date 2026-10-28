package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	homepkg "compound/internal/home"
)

const (
	ExactActionApprovalPlanSchema = "compound.exact_action_approval_plan.v0.1"

	ExactApprovalStatusReady    = "ready"
	ExactApprovalStatusConsumed = "consumed"
	ExactApprovalStatusExpired  = "expired"
)

// ErrExactApprovalExhausted distinguishes valid consumed/expired grants from
// malformed authority or store failures. Callers may rotate a plan generation
// only for this expected lifecycle result.
var ErrExactApprovalExhausted = errors.New("matching exact approvals exhausted")

// ErrExactApprovalAuthorityUnavailable is the bounded fail-closed result for
// matching compatibility records that do not carry consumable authority.
var ErrExactApprovalAuthorityUnavailable = errors.New("exact approval authority unavailable")

// Historical plans can be validated as evidence without carrying the complete
// intent binding required to grant authority. Only inventory scans handle this
// distinction; approval and consumption continue to refuse it.
var errLegacyExactApprovalPlan = errors.New("exact approval plan lacks full intent binding")

type ExactActionApprovalPlan struct {
	SchemaVersion  string        `json:"schema_version"`
	ID             string        `json:"id"`
	ActionIntentID string        `json:"action_intent_id"`
	Binding        ActionBinding `json:"binding"`
	BindingDigest  string        `json:"binding_digest"`
	CreatedAt      string        `json:"created_at"`
	ExpiresAt      string        `json:"expires_at"`
}

type ExactActionApprovalStatus struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Status        string `json:"status"`
	BindingDigest string `json:"binding_digest,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	ConsumedAt    string `json:"consumed_at,omitempty"`
}

type ExactActionApprovalStore struct {
	homePath string
}

func NewExactActionApprovalStore(homePath string) *ExactActionApprovalStore {
	return &ExactActionApprovalStore{homePath: strings.TrimSpace(homePath)}
}

func (s *ExactActionApprovalStore) Plan(
	actionIntentID string,
	cfg Config,
	ttl time.Duration,
	now time.Time,
) (ExactActionApprovalPlan, string, error) {
	if s == nil || s.homePath == "" {
		return ExactActionApprovalPlan{}, "", fmt.Errorf("exact action approval store requires compound home")
	}
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return ExactActionApprovalPlan{}, "", err
	}
	record, err := store.Read(strings.TrimSpace(actionIntentID))
	if err != nil {
		return ExactActionApprovalPlan{}, "", err
	}
	var intent envelope.ActionIntent
	if err := json.Unmarshal(record.Bytes, &intent); err != nil {
		return ExactActionApprovalPlan{}, "", fmt.Errorf("parse action intent: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return ExactActionApprovalPlan{}, "", err
	}
	plan, ref, _, err := s.EnsurePlanForIntent(intent, cfg, ttl, now)
	return plan, ref, err
}

// EnsurePlanForIntent returns the one live undecided plan generation for an
// exact action binding. Retry ActionIntent ids deliberately do not affect the
// binding digest, so repeated enforced holds converge instead of creating
// approval debt.
func (s *ExactActionApprovalStore) EnsurePlanForIntent(
	intent envelope.ActionIntent,
	cfg Config,
	ttl time.Duration,
	now time.Time,
) (ExactActionApprovalPlan, string, bool, error) {
	if s == nil || s.homePath == "" {
		return ExactActionApprovalPlan{}, "", false, fmt.Errorf("exact action approval store requires compound home")
	}
	var (
		plan   ExactActionApprovalPlan
		ref    string
		reused bool
	)
	err := homepkg.WithAutonomyAdmissionLock(s.homePath, func() error {
		var innerErr error
		plan, ref, reused, innerErr = s.ensurePlanForIntentLocked(intent, cfg, ttl, now)
		return innerErr
	})
	return plan, ref, reused, err
}

func (s *ExactActionApprovalStore) ensurePlanForIntentLocked(
	intent envelope.ActionIntent,
	cfg Config,
	ttl time.Duration,
	now time.Time,
) (ExactActionApprovalPlan, string, bool, error) {
	if ttl <= 0 || ttl > 15*time.Minute {
		return ExactActionApprovalPlan{}, "", false, fmt.Errorf("exact approval ttl must be greater than zero and at most 15m")
	}
	if err := intent.Validate(); err != nil {
		return ExactActionApprovalPlan{}, "", false, err
	}
	if strings.TrimSpace(intent.SecurityFloorOutcome) != "" {
		return ExactActionApprovalPlan{}, "", false, fmt.Errorf("action security floor is not approvable")
	}
	binding := ActionBindingForIntent(intent, cfg)
	digest := ActionBindingDigest(binding)
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return ExactActionApprovalPlan{}, "", false, err
	}
	if err := requirePersistedActionIntent(store, intent, cfg); err != nil {
		return ExactActionApprovalPlan{}, "", false, err
	}
	plans, err := exactApprovalPlansForBinding(store, digest)
	if err != nil {
		return ExactActionApprovalPlan{}, "", false, err
	}
	grants, err := exactApprovalPlanGrants(store)
	if err != nil {
		return ExactActionApprovalPlan{}, "", false, err
	}
	for i := len(plans) - 1; i >= 0; i-- {
		candidate := plans[i]
		expiresAt, _ := time.Parse(time.RFC3339Nano, candidate.ExpiresAt)
		if !now.Before(expiresAt) {
			continue
		}
		ref, _ := eventstore.ReferenceForID(candidate.ID)
		grant, approved := grants[ref.String()]
		if !approved {
			return candidate, ref.String(), true, nil
		}
		status, statusErr := exactApprovalStatusFromRecord(store, grant.approval, now)
		if statusErr != nil {
			return ExactActionApprovalPlan{}, "", false, statusErr
		}
		if status.Status == ExactApprovalStatusReady {
			return candidate, ref.String(), true, nil
		}
	}

	expiresAt := now.UTC().Add(ttl)
	plan := ExactActionApprovalPlan{
		SchemaVersion:  ExactActionApprovalPlanSchema,
		ID:             "approval_plan_" + shortActionHash(digest, strconv.Itoa(len(plans)+1)),
		ActionIntentID: intent.ID,
		Binding:        binding,
		BindingDigest:  digest,
		CreatedAt:      now.UTC().Format(time.RFC3339Nano),
		ExpiresAt:      expiresAt.Format(time.RFC3339Nano),
	}
	result, err := store.PutJSONExclusive(plan.ID, plan, "  ")
	if err != nil {
		return ExactActionApprovalPlan{}, "", false, err
	}
	return plan, result.Reference.String(), false, nil
}

func requirePersistedActionIntent(store *eventstore.Store, intent envelope.ActionIntent, cfg Config) error {
	record, err := store.Read(intent.ID)
	if err != nil {
		return fmt.Errorf("source action intent %s is not persisted: %w", intent.ID, err)
	}
	var persisted envelope.ActionIntent
	if err := json.Unmarshal(record.Bytes, &persisted); err != nil {
		return fmt.Errorf("invalid source action intent %s: %w", intent.ID, err)
	}
	if err := persisted.Validate(); err != nil {
		return fmt.Errorf("invalid source action intent %s: %w", intent.ID, err)
	}
	if persisted.ID != intent.ID ||
		ActionBindingDigest(ActionBindingForIntent(persisted, cfg)) != ActionBindingDigest(ActionBindingForIntent(intent, cfg)) ||
		persisted.SecurityFloorOutcome != intent.SecurityFloorOutcome {
		return fmt.Errorf("persisted source action intent %s does not match supplied intent binding", intent.ID)
	}
	return nil
}

func exactApprovalPlansForBinding(store *eventstore.Store, digest string) ([]ExactActionApprovalPlan, error) {
	entries, err := store.Scan(eventstore.ScanOptions{
		Prefixes:    []string{"approval_plan_"},
		RegularOnly: true,
	})
	if err != nil {
		return nil, err
	}
	plans := make([]ExactActionApprovalPlan, 0, len(entries))
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return nil, err
		}
		plan, err := readExactApprovalPlanRecord(store, entry.ID, record.Bytes)
		if err != nil {
			return nil, err
		}
		if plan.Binding.FullIntentDigest == "" {
			continue
		}
		if plan.BindingDigest == digest {
			plans = append(plans, plan)
		}
	}
	sort.Slice(plans, func(i, j int) bool {
		if plans[i].CreatedAt == plans[j].CreatedAt {
			return plans[i].ID < plans[j].ID
		}
		return plans[i].CreatedAt < plans[j].CreatedAt
	})
	return plans, nil
}

type exactApprovalPlanGrant struct {
	approval ExactActionApproval
	ref      string
}

func exactApprovalPlanGrants(store *eventstore.Store) (map[string]exactApprovalPlanGrant, error) {
	entries, err := store.Scan(eventstore.ScanOptions{
		Prefixes:    []string{"approval_grant_"},
		RegularOnly: true,
	})
	if err != nil {
		return nil, err
	}
	approved := make(map[string]exactApprovalPlanGrant, len(entries))
	for _, entry := range entries {
		record, err := entry.Read()
		if err != nil {
			return nil, err
		}
		approval, err := validateExactActionApprovalRecord(entry.ID, record.Bytes)
		if err != nil {
			return nil, err
		}
		planRef, hasPlan, err := validateExactPlanGrant(store, approval)
		if errors.Is(err, errLegacyExactApprovalPlan) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !hasPlan {
			continue
		}
		if _, exists := approved[planRef]; exists {
			return nil, fmt.Errorf("exact approval plan %s has multiple grants", planRef)
		}
		approved[planRef] = exactApprovalPlanGrant{approval: approval, ref: record.Reference.String()}
	}
	return approved, nil
}

func validateExactApprovalPlanRecord(store *eventstore.Store, entryID string, body []byte) (ExactActionApprovalPlan, error) {
	plan, err := readExactApprovalPlanRecord(store, entryID, body)
	if err != nil {
		return ExactActionApprovalPlan{}, err
	}
	if plan.Binding.FullIntentDigest == "" {
		return ExactActionApprovalPlan{}, fmt.Errorf("%w: %s", errLegacyExactApprovalPlan, entryID)
	}
	return plan, nil
}

// readExactApprovalPlanRecord verifies integrity using the binding format that
// was recorded. A successful historical read alone never authorizes an action.
func readExactApprovalPlanRecord(store *eventstore.Store, entryID string, body []byte) (ExactActionApprovalPlan, error) {
	var plan ExactActionApprovalPlan
	if err := json.Unmarshal(body, &plan); err != nil {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s: %w", entryID, err)
	}
	if plan.SchemaVersion != ExactActionApprovalPlanSchema || plan.ID != entryID ||
		!validGeneratedAuthorityID(plan.ID, "approval_plan_") ||
		strings.TrimSpace(plan.ActionIntentID) == "" ||
		plan.Binding.SchemaVersion != ActionBindingSchema ||
		plan.Binding.ActionIntentID != plan.ActionIntentID ||
		ActionBindingDigest(plan.Binding) != plan.BindingDigest {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s", entryID)
	}
	createdAt, createdErr := time.Parse(time.RFC3339Nano, plan.CreatedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, plan.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expiresAt.After(createdAt) || expiresAt.Sub(createdAt) > 15*time.Minute {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s lifetime", entryID)
	}
	actionRecord, err := store.Read(plan.ActionIntentID)
	if err != nil {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s source action intent: %w", entryID, err)
	}
	var source envelope.ActionIntent
	if err := json.Unmarshal(actionRecord.Bytes, &source); err != nil {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s source action intent: %w", entryID, err)
	}
	if err := source.Validate(); err != nil || source.ID != plan.ActionIntentID {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s source action intent", entryID)
	}
	sourceBinding := ActionBindingForIntent(source, Config{})
	sourceBinding.PolicyRevision = plan.Binding.PolicyRevision
	if plan.Binding.FullIntentDigest == "" {
		sourceBinding.FullIntentDigest = ""
	}
	if ActionBindingDigest(sourceBinding) != plan.BindingDigest {
		return ExactActionApprovalPlan{}, fmt.Errorf("invalid exact approval plan %s source binding", entryID)
	}
	return plan, nil
}

func validateExactActionApprovalRecord(entryID string, body []byte) (ExactActionApproval, error) {
	var approval ExactActionApproval
	if err := json.Unmarshal(body, &approval); err != nil {
		return ExactActionApproval{}, fmt.Errorf("invalid exact action approval %s: %w", entryID, err)
	}
	if approval.SchemaVersion != ExactActionApprovalSchema || approval.ID != entryID ||
		!strings.HasPrefix(approval.ID, "approval_") ||
		strings.HasPrefix(approval.ID, "approval_plan_") ||
		strings.HasPrefix(approval.ID, "approval_use_") ||
		approval.MaxUses != 1 || !HasOperatorProvenance(approval.GrantedBy) ||
		!validSHA256Digest(approval.BindingDigest) {
		return ExactActionApproval{}, fmt.Errorf("invalid exact action approval %s", entryID)
	}
	if _, err := time.Parse(time.RFC3339Nano, approval.ExpiresAt); err != nil {
		return ExactActionApproval{}, fmt.Errorf("invalid exact action approval %s expiry: %w", entryID, err)
	}
	wantBindingRef := "action-binding:" + approval.BindingDigest
	foundBindingRef := false
	seen := make(map[string]bool, len(approval.EvidenceRefs))
	for _, ref := range approval.EvidenceRefs {
		if strings.TrimSpace(ref) == "" || seen[ref] {
			return ExactActionApproval{}, fmt.Errorf("invalid exact action approval %s evidence", entryID)
		}
		seen[ref] = true
		if strings.HasPrefix(ref, "action-binding:") {
			if ref != wantBindingRef {
				return ExactActionApproval{}, fmt.Errorf("invalid exact action approval %s binding evidence", entryID)
			}
			foundBindingRef = true
		}
	}
	if !foundBindingRef {
		return ExactActionApproval{}, fmt.Errorf("invalid exact action approval %s missing binding evidence", entryID)
	}
	return approval, nil
}

func validateExactPlanGrant(store *eventstore.Store, approval ExactActionApproval) (string, bool, error) {
	planRef := ""
	planID := ""
	for _, ref := range approval.EvidenceRefs {
		if !strings.HasPrefix(ref, "compound://events/approval_plan_") {
			continue
		}
		id := strings.TrimPrefix(ref, "compound://events/")
		if !validGeneratedAuthorityID(id, "approval_plan_") || planRef != "" {
			return "", false, fmt.Errorf("invalid exact action approval %s plan evidence", approval.ID)
		}
		planRef, planID = ref, id
	}
	if planRef == "" {
		return "", false, nil
	}
	planRecord, err := store.Read(planID)
	if err != nil {
		return "", false, fmt.Errorf("invalid exact action approval %s plan evidence: %w", approval.ID, err)
	}
	plan, err := readExactApprovalPlanRecord(store, planID, planRecord.Bytes)
	if err != nil {
		return "", false, err
	}
	wantID := "approval_grant_" + shortActionHash(plan.ID, plan.BindingDigest)
	actionRef, _ := eventstore.ReferenceForID(plan.ActionIntentID)
	if approval.ID != wantID || approval.BindingDigest != plan.BindingDigest ||
		approval.ExpiresAt != plan.ExpiresAt || !containsExactString(approval.EvidenceRefs, actionRef.String()) {
		return "", false, fmt.Errorf("invalid exact action approval %s plan binding", approval.ID)
	}
	if plan.Binding.FullIntentDigest == "" {
		return "", false, fmt.Errorf("%w: %s", errLegacyExactApprovalPlan, plan.ID)
	}
	return planRef, true, nil
}

func validGeneratedAuthorityID(id, prefix string) bool {
	suffix := strings.TrimPrefix(id, prefix)
	if suffix == id || len(suffix) != 12 {
		return false
	}
	for _, ch := range suffix {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func validSHA256Digest(value string) bool {
	suffix := strings.TrimPrefix(value, "sha256:")
	if suffix == value || len(suffix) != 64 {
		return false
	}
	for _, ch := range suffix {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func (s *ExactActionApprovalStore) Approve(
	planID, grantedBy string,
	cfg Config,
	now time.Time,
) (ExactActionApproval, string, error) {
	if s == nil || s.homePath == "" {
		return ExactActionApproval{}, "", fmt.Errorf("exact action approval store requires compound home")
	}
	var (
		approval ExactActionApproval
		ref      string
	)
	err := homepkg.WithAutonomyAdmissionLock(s.homePath, func() error {
		var innerErr error
		approval, ref, innerErr = s.approveLocked(planID, grantedBy, cfg, now)
		return innerErr
	})
	return approval, ref, err
}

func (s *ExactActionApprovalStore) approveLocked(
	planID, grantedBy string,
	cfg Config,
	now time.Time,
) (ExactActionApproval, string, error) {
	grantedBy = strings.TrimSpace(grantedBy)
	if !HasOperatorProvenance(grantedBy) {
		return ExactActionApproval{}, "", fmt.Errorf("exact action approval requires operator provenance")
	}
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	record, err := store.Read(strings.TrimSpace(planID))
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	plan, err := validateExactApprovalPlanRecord(store, strings.TrimSpace(planID), record.Bytes)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	expiry, err := time.Parse(time.RFC3339Nano, plan.ExpiresAt)
	if err != nil || !now.Before(expiry) {
		return ExactActionApproval{}, "", fmt.Errorf("exact approval plan expired")
	}
	actionRecord, err := store.Read(plan.ActionIntentID)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	var intent envelope.ActionIntent
	if err := json.Unmarshal(actionRecord.Bytes, &intent); err != nil {
		return ExactActionApproval{}, "", fmt.Errorf("parse planned action intent: %w", err)
	}
	if err := intent.Validate(); err != nil {
		return ExactActionApproval{}, "", fmt.Errorf("invalid planned action intent: %w", err)
	}
	currentBinding := ActionBindingForIntent(intent, cfg)
	if ActionBindingDigest(currentBinding) != plan.BindingDigest {
		return ExactActionApproval{}, "", fmt.Errorf("exact approval plan binding changed")
	}
	planRef := record.Reference.String()
	existing, existingRef, found, err := exactApprovalForPlan(store, planRef)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	if found {
		return s.reuseApprovedPlan(plan, existing, existingRef, grantedBy, now)
	}
	approval, err := NewExactActionApproval(
		"approval_grant_"+shortActionHash(plan.ID, plan.BindingDigest),
		grantedBy,
		currentBinding,
		expiry,
	)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	approval.EvidenceRefs = mergeStrings(approval.EvidenceRefs, planRef, actionRecord.Reference.String())
	result, err := store.PutJSONExclusive(approval.ID, approval, "  ")
	if err == nil {
		return approval, result.Reference.String(), nil
	}
	if !errors.Is(err, os.ErrExist) {
		return ExactActionApproval{}, "", err
	}
	// A concurrent approver won the actor-independent grant identity. Read the
	// winner and apply the same one-plan/one-grant rule.
	existingRecord, readErr := store.Read(approval.ID)
	if readErr != nil {
		return ExactActionApproval{}, "", readErr
	}
	existing, validateErr := validateExactActionApprovalRecord(existingRecord.ID, existingRecord.Bytes)
	if validateErr != nil {
		return ExactActionApproval{}, "", validateErr
	}
	if validatedPlanRef, hasPlan, validateErr := validateExactPlanGrant(store, existing); validateErr != nil ||
		!hasPlan || validatedPlanRef != planRef {
		return ExactActionApproval{}, "", fmt.Errorf("invalid existing exact approval for plan %s", plan.ID)
	}
	return s.reuseApprovedPlan(plan, existing, existingRecord.Reference.String(), grantedBy, now)
}

func exactApprovalForPlan(store *eventstore.Store, planRef string) (ExactActionApproval, string, bool, error) {
	grants, err := exactApprovalPlanGrants(store)
	if err != nil {
		return ExactActionApproval{}, "", false, err
	}
	grant, found := grants[planRef]
	if !found {
		return ExactActionApproval{}, "", false, nil
	}
	return grant.approval, grant.ref, true, nil
}

func (s *ExactActionApprovalStore) reuseApprovedPlan(
	plan ExactActionApprovalPlan,
	approval ExactActionApproval,
	ref, grantedBy string,
	now time.Time,
) (ExactActionApproval, string, error) {
	if approval.GrantedBy != grantedBy {
		return ExactActionApproval{}, "", fmt.Errorf("exact approval plan %s already approved by %s", plan.ID, approval.GrantedBy)
	}
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	status, err := exactApprovalStatusFromRecord(store, approval, now)
	if err != nil {
		return ExactActionApproval{}, "", err
	}
	switch status.Status {
	case ExactApprovalStatusReady:
		return approval, ref, nil
	case ExactApprovalStatusConsumed:
		return ExactActionApproval{}, "", fmt.Errorf("exact approval plan %s approval already consumed", plan.ID)
	case ExactApprovalStatusExpired:
		return ExactActionApproval{}, "", fmt.Errorf("exact approval plan %s approval expired", plan.ID)
	default:
		return ExactActionApproval{}, "", fmt.Errorf("exact approval plan %s has invalid approval status %q", plan.ID, status.Status)
	}
}

func containsExactString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (s *ExactActionApprovalStore) Check(id string, now time.Time) (ExactActionApprovalStatus, error) {
	if s == nil || s.homePath == "" {
		return ExactActionApprovalStatus{}, fmt.Errorf("exact action approval store requires compound home")
	}
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return ExactActionApprovalStatus{}, err
	}
	record, err := store.Read(strings.TrimSpace(id))
	if err != nil {
		return ExactActionApprovalStatus{}, err
	}
	approval, err := validateExactActionApprovalRecord(record.ID, record.Bytes)
	if err != nil {
		return ExactActionApprovalStatus{}, err
	}
	if _, _, err := validateExactPlanGrant(store, approval); err != nil {
		return ExactActionApprovalStatus{}, err
	}
	return exactApprovalStatusFromRecord(store, approval, now)
}

func exactApprovalStatusFromRecord(store *eventstore.Store, approval ExactActionApproval, now time.Time) (ExactActionApprovalStatus, error) {
	status := ExactActionApprovalStatus{
		SchemaVersion: "compound.exact_action_approval_status.v0.1",
		ID:            approval.ID,
		Status:        ExactApprovalStatusReady,
		BindingDigest: approval.BindingDigest,
		ExpiresAt:     approval.ExpiresAt,
	}
	consumptionID := "approval_use_" + shortActionHash(approval.ID)
	if consumed, err := store.Read(consumptionID); err == nil {
		var row struct {
			SchemaVersion string `json:"schema_version"`
			ID            string `json:"id"`
			ApprovalID    string `json:"approval_id"`
			ConsumedAt    string `json:"consumed_at"`
		}
		if json.Unmarshal(consumed.Bytes, &row) != nil ||
			row.SchemaVersion != "compound.exact_action_approval_consumption.v0.1" ||
			row.ID != consumptionID || row.ApprovalID != approval.ID {
			return ExactActionApprovalStatus{}, fmt.Errorf("invalid exact approval consumption %s", consumptionID)
		}
		if _, err := time.Parse(time.RFC3339Nano, row.ConsumedAt); err != nil {
			return ExactActionApprovalStatus{}, fmt.Errorf("invalid exact approval consumption %s timestamp", consumptionID)
		}
		status.Status = ExactApprovalStatusConsumed
		status.ConsumedAt = row.ConsumedAt
		return status, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ExactActionApprovalStatus{}, err
	}
	expiry, err := time.Parse(time.RFC3339Nano, approval.ExpiresAt)
	if err != nil {
		return ExactActionApprovalStatus{}, err
	}
	if !now.Before(expiry) {
		status.Status = ExactApprovalStatusExpired
	}
	return status, nil
}

// Save preserves legacy/fixture storage compatibility. It does not establish
// consumable authority: ApplyMatching requires an Approve-created grant with a
// fully validated plan and source ActionIntent lineage.
func (s *ExactActionApprovalStore) Save(approval ExactActionApproval) (string, error) {
	if s == nil || s.homePath == "" {
		return "", fmt.Errorf("exact action approval store requires compound home")
	}
	body, err := json.Marshal(approval)
	if err != nil {
		return "", err
	}
	if _, err := validateExactActionApprovalRecord(approval.ID, body); err != nil {
		return "", err
	}
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return "", err
	}
	result, err := store.PutJSON(approval.ID, approval, "  ")
	if err != nil {
		return "", err
	}
	return result.Reference.String(), nil
}

func (s *ExactActionApprovalStore) ApplyMatching(
	intent envelope.ActionIntent,
	cfg Config,
	base envelope.PolicyDecision,
	now time.Time,
) (envelope.PolicyDecision, bool, error) {
	if s == nil || s.homePath == "" {
		return base, false, fmt.Errorf("exact action approval store requires compound home")
	}
	var (
		decision envelope.PolicyDecision
		matched  bool
	)
	err := homepkg.WithAutonomyAdmissionLock(s.homePath, func() error {
		var innerErr error
		decision, matched, innerErr = s.applyMatchingLocked(intent, cfg, base, now)
		return innerErr
	})
	if decision.ID == "" {
		decision = base
	}
	return decision, matched, err
}

func (s *ExactActionApprovalStore) applyMatchingLocked(
	intent envelope.ActionIntent,
	cfg Config,
	base envelope.PolicyDecision,
	now time.Time,
) (envelope.PolicyDecision, bool, error) {
	binding := ActionBindingForIntent(intent, cfg)
	if binding.FullIntentDigest == "" {
		return base, false, fmt.Errorf("action intent cannot form an exact JSON binding")
	}
	digest := ActionBindingDigest(binding)
	store, err := eventstore.Open(s.homePath)
	if err != nil {
		return base, false, err
	}
	entries, err := store.Scan(eventstore.ScanOptions{
		Prefixes:    []string{"approval_"},
		RegularOnly: true,
	})
	if err != nil {
		return base, false, err
	}
	matched := false
	diagnostics := []string{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.ID, "approval_plan_") || strings.HasPrefix(entry.ID, "approval_use_") {
			continue
		}
		record, err := entry.Read()
		if err != nil {
			return base, false, err
		}
		approval, err := validateExactActionApprovalRecord(entry.ID, record.Bytes)
		if err != nil {
			return base, false, err
		}
		if approval.BindingDigest != digest {
			continue
		}
		matched = true
		// ApplyMatching is the enforced hook consumption edge. Planless Save
		// compatibility is deliberately non-authoritative here; only a grant
		// whose plan and source ActionIntent fully validate may be consumed.
		if _, hasPlan, err := validateExactPlanGrant(store, approval); err != nil {
			return base, true, err
		} else if !hasPlan {
			return base, true, ErrExactApprovalAuthorityUnavailable
		}
		status, err := exactApprovalStatusFromRecord(store, approval, now)
		if err != nil {
			return base, true, err
		}
		switch status.Status {
		case ExactApprovalStatusConsumed:
			diagnostics = append(diagnostics, fmt.Sprintf("approval %s already consumed", approval.ID))
			continue
		case ExactApprovalStatusExpired:
			diagnostics = append(diagnostics, fmt.Sprintf("approval %s expired", approval.ID))
			continue
		case ExactApprovalStatusReady:
		default:
			return base, true, fmt.Errorf("approval %s has invalid status %q", approval.ID, status.Status)
		}
		monitored := MonitoredAction{
			Intent:   intent,
			Decision: base,
			Binding:  binding,
		}
		decision, err := ApplyExactActionApproval(monitored, approval, eventApprovalConsumer{store: store}, now)
		if err == nil {
			decision.EvidenceRefs = mergeStrings(decision.EvidenceRefs, entry.Reference.String())
			return decision, true, nil
		}
		if errors.Is(err, ErrExactApprovalExhausted) {
			diagnostics = append(diagnostics, err.Error())
			continue
		}
		return decision, true, err
	}
	if !matched {
		return base, false, nil
	}
	return base, true, fmt.Errorf("%w: %s", ErrExactApprovalExhausted, strings.Join(uniqueSortedStrings(diagnostics), "; "))
}

// ApplyMatchingLocked is for work-owned admission transactions that already
// hold home.WithAutonomyAdmissionLock. Callers must validate current work and
// persist the immutable intent before invoking it, then record under that same
// lock. Calling ApplyMatching instead would recursively acquire a flock.
func (s *ExactActionApprovalStore) ApplyMatchingLocked(intent envelope.ActionIntent, cfg Config, base envelope.PolicyDecision, now time.Time) (envelope.PolicyDecision, bool, error) {
	if s == nil || s.homePath == "" {
		return base, false, fmt.Errorf("exact action approval store requires compound home")
	}
	return s.applyMatchingLocked(intent, cfg, base, now)
}

func uniqueSortedStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func ActionBindingForIntent(intent envelope.ActionIntent, cfg Config) ActionBinding {
	inputRefs := make([]string, 0, len(intent.InputSources))
	for _, source := range intent.InputSources {
		inputRefs = append(inputRefs, source.SourceID+"@"+source.ContentHash)
	}
	sort.Strings(inputRefs)
	domain := intent.Capability
	if before, _, ok := strings.Cut(intent.Capability, "."); ok {
		domain = before
	}
	return ActionBinding{
		SchemaVersion:      ActionBindingSchema,
		ActionIntentID:     intent.ID,
		Tool:               intent.Capability,
		Domain:             domain,
		Operation:          intent.Action,
		ArgumentsHash:      intent.ArgumentsHash,
		Destination:        intent.Destination,
		InputRefs:          inputRefs,
		ToolSchemaRevision: intent.SchemaRevision,
		PolicyRevision:     actionPolicyRevision(cfg),
		FullIntentDigest:   CanonicalFullIntentDigest(intent),
	}
}

type eventApprovalConsumer struct {
	store *eventstore.Store
}

func (c eventApprovalConsumer) Consume(approvalID string, now time.Time) error {
	if c.store == nil {
		return fmt.Errorf("approval consumption store is required")
	}
	id := "approval_use_" + shortActionHash(approvalID)
	_, err := c.store.PutJSONExclusive(id, struct {
		SchemaVersion string `json:"schema_version"`
		ID            string `json:"id"`
		ApprovalID    string `json:"approval_id"`
		ConsumedAt    string `json:"consumed_at"`
	}{
		SchemaVersion: "compound.exact_action_approval_consumption.v0.1",
		ID:            id,
		ApprovalID:    approvalID,
		ConsumedAt:    now.UTC().Format(time.RFC3339Nano),
	}, "  ")
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: approval %s already consumed", ErrExactApprovalExhausted, approvalID)
	}
	if err != nil {
		return fmt.Errorf("consume approval %s: %w", approvalID, err)
	}
	return nil
}
