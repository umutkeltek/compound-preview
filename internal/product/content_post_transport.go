package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"compound/internal/act"
	"compound/internal/home"
	"reflect"
)

const (
	SocialAccountRegistrySchemaVersion       = "compound.social_account_registry.v0.1"
	ContentPostTransportRequestSchemaVersion = act.ContentTransportContract
)

// SocialAccount is the distribution-facing subset of the private social-account registry.
// Authentication material is deliberately absent; the transport owns its credential handle.
type SocialAccount struct {
	ID                  string `json:"id"`
	ProductID           string `json:"product_id"`
	Platform            string `json:"platform"`
	Transport           string `json:"transport"`
	PostizIntegrationID string `json:"postiz_integration_id"`
	Status              string `json:"status"`
}

type socialAccountRegistry struct {
	SchemaVersion string          `json:"schema_version"`
	Accounts      []SocialAccount `json:"accounts"`
}

// ContentPostTransportRequest is the complete body a transport receives. The request is
// vendor-neutral: IntegrationID is the selected transport's enrolled channel identity.
type ContentPostTransportRequest = act.ContentTransportRequest

// BuildContentPostTransportRequest joins the operator-reviewed draft authorization to the
// currently enrolled account and exact body bytes. It is read-only and refuses before an
// ActionIntent is minted when any join is missing, stale, duplicated, or inconsistent.
func BuildContentPostTransportRequest(homePath, draftID, transport string, scheduleAt, now time.Time) (ContentPostTransportRequest, ContentPostAuthorization, error) {
	draft, err := ReadContentDraft(homePath, draftID)
	if err != nil {
		return ContentPostTransportRequest{}, ContentPostAuthorization{}, err
	}
	if draft.Outbound != nil {
		return ContentPostTransportRequest{}, ContentPostAuthorization{}, fmt.Errorf("draft already owns outbound attempt %s (%s); %s", draft.Outbound.ID, draft.Outbound.Status, contentReconcileCommand(draft))
	}
	if contentDraftExpired(draft, now) {
		return ContentPostTransportRequest{}, ContentPostAuthorization{}, fmt.Errorf("draft expired before publication")
	}
	auth, err := LoadContentPostAuthorization(homePath, draftID)
	if err != nil {
		return ContentPostTransportRequest{}, auth, fmt.Errorf("load content post authorization: %w", err)
	}
	if err := validateContentPostAuthorization(homePath, auth); err != nil {
		return ContentPostTransportRequest{}, auth, err
	}
	transport = strings.TrimSpace(transport)
	if transport == "" {
		return ContentPostTransportRequest{}, auth, fmt.Errorf("content post transport is required")
	}
	if scheduleAt.IsZero() || !scheduleAt.After(now) {
		return ContentPostTransportRequest{}, auth, fmt.Errorf("content post schedule must be in the future")
	}
	expiresAt, err := time.Parse(time.RFC3339, draft.ExpiresAt)
	if err != nil || !scheduleAt.Before(expiresAt) {
		return ContentPostTransportRequest{}, auth, fmt.Errorf("content post schedule must precede the reviewed package expiry")
	}
	account, err := ResolveActiveSocialAccount(homePath, auth.ProductID, auth.Platform, transport)
	if err != nil {
		return ContentPostTransportRequest{}, auth, err
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return ContentPostTransportRequest{}, auth, err
	}
	body, err := store.Get(auth.BodyRef)
	if err != nil {
		return ContentPostTransportRequest{}, auth, fmt.Errorf("resolve content body %s: %w", auth.BodyRef, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return ContentPostTransportRequest{}, auth, fmt.Errorf("content post body is empty")
	}
	binding, err := act.PinContentTransport(homePath, transport)
	if err != nil {
		return ContentPostTransportRequest{}, auth, err
	}
	var media []act.ContentTransportMedia
	for _, m := range draft.Media {
		b, err := store.Get(m.Ref)
		if err != nil {
			return ContentPostTransportRequest{}, auth, err
		}
		media = append(media, act.ContentTransportMedia{Kind: m.Kind, Ref: m.Ref, MIMEType: m.MIMEType, AltText: m.AltText, Bytes: b})
	}
	attemptID := stableID("cpost_", draft.ID, auth.ActionIntentID, auth.PackageDigest, account.ID, transport, scheduleAt.UTC().Format(time.RFC3339), binding.ConfigDigest, binding.ExecutableDigest)
	return ContentPostTransportRequest{
		SchemaVersion: ContentPostTransportRequestSchemaVersion,
		AccountID:     account.ID, PackageDigest: auth.PackageDigest, Media: media, AttemptID: attemptID, IdempotencyKey: attemptID, Binding: binding,
		DraftID:       auth.DraftID,
		ProductID:     auth.ProductID,
		Platform:      auth.Platform,
		Transport:     transport,
		IntegrationID: account.PostizIntegrationID,
		PostType:      "schedule",
		ScheduleAt:    scheduleAt.UTC().Format(time.RFC3339),
		ExpiresAt:     draft.ExpiresAt,
		Body:          string(body),
	}, auth, nil
}

func ResolveActiveSocialAccount(homePath, productID, platform, transport string) (SocialAccount, error) {
	path := filepath.Join(homePath, "registries", "social-accounts.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return SocialAccount{}, fmt.Errorf("read social-account registry: %w", err)
	}
	var registry socialAccountRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return SocialAccount{}, fmt.Errorf("decode social-account registry: %w", err)
	}
	if registry.SchemaVersion != SocialAccountRegistrySchemaVersion {
		return SocialAccount{}, fmt.Errorf("social-account registry schema %q is unsupported", registry.SchemaVersion)
	}
	productID = strings.TrimSpace(productID)
	platform = strings.ToLower(strings.TrimSpace(platform))
	transport = strings.TrimSpace(transport)
	var exact []SocialAccount
	for _, account := range registry.Accounts {
		if strings.TrimSpace(account.ProductID) == productID &&
			strings.ToLower(strings.TrimSpace(account.Platform)) == platform &&
			strings.TrimSpace(account.Transport) == transport {
			exact = append(exact, account)
		}
	}
	if len(exact) == 0 {
		return SocialAccount{}, fmt.Errorf("no social account for product=%s platform=%s transport=%s", productID, platform, transport)
	}
	if len(exact) != 1 {
		return SocialAccount{}, fmt.Errorf("social account join is ambiguous for product=%s platform=%s transport=%s: matches=%d", productID, platform, transport, len(exact))
	}
	account := exact[0]
	if account.Status != "active" {
		return SocialAccount{}, fmt.Errorf("social account %s is %s; human enrollment is required", account.ID, firstNonEmpty(account.Status, "unconfigured"))
	}
	if strings.TrimSpace(account.PostizIntegrationID) == "" {
		return SocialAccount{}, fmt.Errorf("social account %s has no transport integration id", account.ID)
	}
	return account, nil
}

func validateContentPostAuthorization(homePath string, auth ContentPostAuthorization) error {
	if auth.DraftID == "" || auth.ProductID == "" || auth.Platform == "" || auth.BodyRef == "" {
		return fmt.Errorf("content post authorization is incomplete")
	}
	if auth.ActionIntentID != auth.ActionIntent.ID || auth.PolicyDecisionID != auth.PolicyDecision.ID {
		return fmt.Errorf("content post authorization envelope ids do not match embedded records")
	}
	if err := auth.ActionIntent.Validate(); err != nil {
		return fmt.Errorf("content post authorization action intent: %w", err)
	}
	if err := auth.PolicyDecision.Validate(); err != nil {
		return fmt.Errorf("content post authorization policy decision: %w", err)
	}
	if auth.PolicyOutcome != auth.PolicyDecision.Outcome {
		return fmt.Errorf("content post authorization policy outcome does not match embedded decision")
	}
	if auth.PolicyDecision.ActionIntentID != auth.ActionIntent.ID {
		return fmt.Errorf("content post authorization policy is bound to a different action intent")
	}
	if auth.ActionIntent.Capability != ContentPostCapability || auth.ActionIntent.Target.Kind != "content_post" || auth.ActionIntent.Target.ID != auth.DraftID {
		return fmt.Errorf("content post authorization action target does not match draft %s", auth.DraftID)
	}
	if auth.ActionIntent.Scope.ProductID != "" && auth.ActionIntent.Scope.ProductID != auth.ProductID {
		return fmt.Errorf("content post authorization product id does not match embedded action")
	}
	if !containsString(auth.ActionIntent.EvidenceRefs, auth.BodyRef) {
		return fmt.Errorf("content post authorization action is not bound to body ref %s", auth.BodyRef)
	}
	draft, err := ReadContentDraft(homePath, auth.DraftID)
	if err != nil {
		return fmt.Errorf("read authorized content draft: %w", err)
	}
	digest, err := ContentPackageDigest(homePath, draft)
	if err != nil {
		return err
	}
	if !contentPackageMatches(draft, digest) || digest != auth.PackageDigest || !reflect.DeepEqual(draft.Approval.Authorization, auth) {
		return fmt.Errorf("authorization no longer matches exact reviewed content package")
	}
	if draft.State != ContentDraftStateOperationalized || draft.ProductID != auth.ProductID || draft.Platform != auth.Platform || draft.BodyRef != auth.BodyRef {
		return fmt.Errorf("content post authorization no longer matches the operationalized draft")
	}
	return nil
}
