package product

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"compound/internal/act"
	"compound/internal/candidates"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/home"
	"compound/internal/policy"
)

// ContentOutboundAttempt is the exclusive outbound owner on one draft. It is
// published with candidate CAS BEFORE invoking the adapter. Even a crash in
// the next instruction leaves a durable barrier against another create.
type ContentOutboundAttempt struct {
	ID               string `json:"id"`
	Transport        string `json:"transport"`
	PackageDigest    string `json:"package_digest"`
	RequestRef       string `json:"request_ref"`
	RequestDigest    string `json:"request_digest"`
	ActionIntentID   string `json:"action_intent_id"`
	PolicyDecisionID string `json:"policy_decision_id"`
	Status           string `json:"status"`
	CreatedAt        string `json:"created_at"`
	ResponseRef      string `json:"response_ref,omitempty"`
	ReceiptID        string `json:"receipt_id,omitempty"`
	Detail           string `json:"detail,omitempty"`
}

type ContentPostRun struct {
	Mode          string                       `json:"mode"`
	Request       *ContentPostTransportRequest `json:"request,omitempty"`
	PolicyOutcome string                       `json:"policy_outcome,omitempty"`
	AttemptID     string                       `json:"attempt_id,omitempty"`
	NextCommand   string                       `json:"next_command,omitempty"`
	ReceiptID     string                       `json:"receipt_id,omitempty"`
	PostImageRef  string                       `json:"post_image_ref,omitempty"`
	Draft         *ContentDraft                `json:"draft,omitempty"`
}

// contentPostExecute permits failure injection after reservation; production
// always reaches act.Execute, the sole outbound mutation owner.
var contentPostExecute = act.Execute
var contentObservationNow = time.Now
var replaceContentOutbound = candidates.ReplaceCandidateIfCurrent

func ExecuteContentPost(homePath, draftID, transport, workID string, at time.Time, confirm, reconcile bool, now time.Time) (ContentPostRun, error) {
	if err := RequireDistributionExperiment("act post --draft"); err != nil {
		return ContentPostRun{}, err
	}
	if reconcile {
		return reconcileContentPost(homePath, draftID, transport, now)
	}
	request, auth, err := BuildContentPostTransportRequest(homePath, draftID, transport, at, now)
	if err != nil {
		return ContentPostRun{}, err
	}
	if workID == "" {
		workID = auth.ActionIntent.IDs.WorkID
	}
	forward, err := json.Marshal(request)
	if err != nil {
		return ContentPostRun{}, err
	}
	ai := contentOutboundIntent(workID, request, auth, forward, now)
	events, err := eventstore.Open(homePath)
	if err != nil {
		return ContentPostRun{}, err
	}
	// A crash after recording the intent but before reserving the candidate
	// must not collide with a timestamp-derived retry of the same identity.
	// Reuse only an otherwise byte-equivalent owner record, then evaluate
	// policy afresh below; the old PolicyDecision is never reused as a grant.
	if recorded, readErr := events.Read(ai.ID); readErr == nil {
		var original envelope.ActionIntent
		if err := json.Unmarshal(recorded.Bytes, &original); err != nil {
			return ContentPostRun{}, err
		}
		ai.CreatedAt = original.CreatedAt
		if !reflect.DeepEqual(ai, original) {
			return ContentPostRun{}, fmt.Errorf("persisted outbound intent differs from this exact request or work identity")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return ContentPostRun{}, readErr
	}
	cfg, _, err := policy.LoadConfig(homePath)
	if err != nil {
		return ContentPostRun{}, err
	}
	pd := policy.Evaluate(ai, cfg, now)
	out := ContentPostRun{Mode: "dry-run", Request: &request, PolicyOutcome: pd.Outcome}
	if !confirm {
		return out, nil
	}
	if !policy.Allowed(pd) {
		out.Mode = "held"
		return out, nil
	}
	if err := act.Gate(ai, pd); err != nil {
		return out, err
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return out, err
	}
	ref, err := store.Put(forward)
	if err != nil {
		return out, err
	}
	if _, err := events.PutJSON(ai.ID, &ai, " "); err != nil {
		return out, err
	}
	if _, err := events.PutJSON(pd.ID, &pd, " "); err != nil {
		return out, err
	}
	record, draft, err := readContentOutbound(homePath, draftID)
	if err != nil {
		return out, err
	}
	digest, err := ContentPackageDigest(homePath, draft)
	if err != nil {
		return out, err
	}
	if draft.Outbound != nil || draft.State != ContentDraftStateOperationalized || !contentPackageMatches(draft, request.PackageDigest) || digest != request.PackageDigest {
		return out, fmt.Errorf("draft or outbound ownership changed before reservation")
	}
	draft.Outbound = &ContentOutboundAttempt{ID: request.AttemptID, Transport: transport, PackageDigest: request.PackageDigest, RequestRef: ref, RequestDigest: act.ContentTransportDigest(forward), ActionIntentID: ai.ID, PolicyDecisionID: pd.ID, Status: "reconciliation_required", CreatedAt: now.UTC().Format(time.RFC3339)}
	changed, err := replaceContentOutbound(homePath, record, draft)
	if err != nil {
		return out, err
	}
	if !changed.Replaced {
		return out, fmt.Errorf("another writer reserved this draft; no transport invoked")
	}
	out = contentAttemptResult(draft)
	admission := func() error {
		_, current, err := readContentOutbound(homePath, draftID)
		if err != nil {
			return err
		}
		if current.Outbound == nil || current.Outbound.ID != request.AttemptID || current.Outbound.RequestRef != ref || current.State != ContentDraftStateOperationalized {
			return fmt.Errorf("outbound reservation changed before start")
		}
		if contentDraftExpired(current, time.Now()) {
			return fmt.Errorf("draft expired immediately before transport start")
		}
		digest, err := ContentPackageDigest(homePath, current)
		if err != nil {
			return err
		}
		if digest != request.PackageDigest {
			return fmt.Errorf("content package changed before start")
		}
		account, err := ResolveActiveSocialAccount(homePath, request.ProductID, request.Platform, transport)
		if err != nil {
			return err
		}
		if account.ID != request.AccountID || account.PostizIntegrationID != request.IntegrationID {
			return fmt.Errorf("account changed before start")
		}
		cfg, _, err := policy.LoadConfig(homePath)
		if err != nil {
			return err
		}
		if !policy.Allowed(policy.Evaluate(ai, cfg, time.Now())) {
			return fmt.Errorf("policy no longer allows outbound attempt")
		}
		return nil
	}
	result, executeErr := contentPostExecute(store, act.NewContentTransportAdapter(homePath, request.Binding, forward, admission), ai, pd, forward, now)
	if executeErr != nil {
		responseRef := ""
		var recovery *act.RecoveryRequiredError
		if errors.As(executeErr, &recovery) {
			responseRef = recovery.PostImageRef
		}
		_, saveErr := recordContentUncertainty(homePath, draftID, request.AttemptID, responseRef, "", executeErr.Error())
		return out, errors.Join(executeErr, saveErr)
	}
	// A receipted invocation still needs a positive domain observation. The
	// persisted reservation survives a parser, artifact, or candidate failure.
	draft, err = recordBoundContentObservation(homePath, draftID, request, "schedule", result.PostImage, result.Receipt.ID, contentObservationNow())
	if err != nil {
		_, saveErr := recordContentUncertainty(homePath, draftID, request.AttemptID, result.PostImageRef, result.Receipt.ID, err.Error())
		return out, errors.Join(err, saveErr)
	}
	return contentAttemptResult(draft), nil
}

func contentOutboundIntent(workID string, request ContentPostTransportRequest, auth ContentPostAuthorization, forward []byte, now time.Time) envelope.ActionIntent {
	ai := auth.ActionIntent
	ai.ID = stableID("act_", request.AttemptID, act.ContentTransportDigest(forward))
	ai.CreatedAt = now.UTC().Format(time.RFC3339)
	ai.IDs.WorkID = workID
	ai.Action = "schedule_content"
	ai.Target = envelope.ActionTarget{Kind: "transport", ID: request.Transport, Path: "actuator/transports/" + request.Transport + ".json"}
	ai.ArgumentsHash = act.ContentTransportDigest(forward)
	ai.SchemaRevision = act.ContentTransportContract
	ai.Reversibility = "irreversible"
	ai.RollbackPlan = "No automatic restore; observe this exact attempt before any further external action"
	ai.Preview = "schedule reviewed content package " + request.PackageDigest + " on account " + request.AccountID
	ai.EvidenceRefs = append(ai.EvidenceRefs, auth.ActionIntentID, auth.PolicyDecisionID, request.PackageDigest)
	return ai
}

func readContentOutbound(homePath, id string) (candidates.Record, ContentDraft, error) {
	r, err := candidates.ReadRecord(homePath, candidates.ContentDraft, id)
	if err != nil {
		return r, ContentDraft{}, err
	}
	var d ContentDraft
	err = json.Unmarshal(r.Body, &d)
	return r, d, err
}

func contentAttemptResult(d ContentDraft) ContentPostRun {
	out := ContentPostRun{Draft: &d}
	if d.Outbound != nil {
		out.Mode = d.Outbound.Status
		out.AttemptID = d.Outbound.ID
		out.ReceiptID = d.Outbound.ReceiptID
		out.PostImageRef = d.Outbound.ResponseRef
		if out.Mode != "posted" && out.Mode != "failed" {
			out.NextCommand = contentReconcileCommand(d)
		}
	}
	return out
}

func recordContentUncertainty(homePath, id, attemptID, responseRef, receiptID, detail string) (ContentDraft, error) {
	r, d, err := readContentOutbound(homePath, id)
	if err != nil {
		return d, err
	}
	if d.Outbound == nil || d.Outbound.ID != attemptID {
		return d, fmt.Errorf("outbound attempt changed")
	}
	if d.Outbound.Status == "posted" || d.Outbound.Status == "failed" {
		return d, nil
	}
	d.Outbound.Detail = detail
	d.Outbound.ResponseRef = responseRef
	d.Outbound.ReceiptID = receiptID
	res, err := replaceContentOutbound(homePath, r, d)
	if err != nil {
		return d, err
	}
	if !res.Replaced {
		return d, fmt.Errorf("newer outbound observation preserved")
	}
	return d, nil
}

func reconcileContentPost(homePath, id, transport string, now time.Time) (ContentPostRun, error) {
	_, d, err := readContentOutbound(homePath, id)
	if err != nil {
		return ContentPostRun{}, err
	}
	if d.Outbound == nil || d.Outbound.Transport != transport {
		return ContentPostRun{}, fmt.Errorf("no matching outbound attempt to reconcile")
	}
	out := contentAttemptResult(d)
	if d.Outbound.Status == "posted" || d.Outbound.Status == "failed" {
		return out, nil
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return out, err
	}
	body, err := store.Get(d.Outbound.RequestRef)
	if err != nil {
		return out, err
	}
	if act.ContentTransportDigest(body) != d.Outbound.RequestDigest {
		return out, fmt.Errorf("original request artifact changed")
	}
	var request ContentPostTransportRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return out, err
	}
	if request.AttemptID != d.Outbound.ID || request.PackageDigest != d.Outbound.PackageDigest || request.Transport != transport {
		return out, fmt.Errorf("request identity differs from owned attempt")
	}
	observed, err := act.ObserveContentTransport(homePath, request)
	if err != nil {
		return out, err
	}
	d, err = recordBoundContentObservation(homePath, id, request, "observe", observed, "", contentObservationNow())
	if err != nil {
		return out, err
	}
	return contentAttemptResult(d), nil
}
