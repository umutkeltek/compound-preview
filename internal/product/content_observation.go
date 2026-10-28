package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"

	"compound/internal/act"
	"compound/internal/home"
)

const ContentObservationSchemaVersion = "compound.content_transport_observation.v0.2"

type ContentTransportObservation struct {
	SchemaVersion  string                      `json:"schema_version"`
	Operation      string                      `json:"operation"`
	AttemptID      string                      `json:"attempt_id"`
	IdempotencyKey string                      `json:"idempotency_key"`
	RequestDigest  string                      `json:"request_digest"`
	PackageDigest  string                      `json:"package_digest"`
	Transport      string                      `json:"transport"`
	AccountID      string                      `json:"account_id"`
	IntegrationID  string                      `json:"integration_id"`
	Binding        act.ContentTransportBinding `json:"binding"`
	ObjectType     string                      `json:"object_type"`
	Status         string                      `json:"status"`
	ScheduleID     string                      `json:"schedule_id,omitempty"`
	ScheduledAt    string                      `json:"scheduled_at,omitempty"`
	RemoteID       string                      `json:"remote_id,omitempty"`
	PublishedAt    string                      `json:"published_at,omitempty"`
	URL            string                      `json:"url,omitempty"`
}

func parseBoundContentObservation(body []byte, req ContentPostTransportRequest, operation, requestDigest string, now time.Time) (ContentTransportObservation, error) {
	var obs ContentTransportObservation
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obs); err != nil {
		return obs, fmt.Errorf("invalid typed transport observation: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return obs, fmt.Errorf("transport observation must end after one object")
	}
	if obs.SchemaVersion != ContentObservationSchemaVersion || obs.Operation != operation || obs.AttemptID != req.AttemptID || obs.IdempotencyKey != req.IdempotencyKey || obs.RequestDigest != requestDigest || obs.PackageDigest != req.PackageDigest || obs.Transport != req.Transport || obs.AccountID != req.AccountID || obs.IntegrationID != req.IntegrationID || !reflect.DeepEqual(obs.Binding, req.Binding) {
		return obs, fmt.Errorf("transport observation is not bound to the original request, package, account and pinned transport")
	}
	switch obs.Status {
	case "published":
		at, err := time.Parse(time.RFC3339, obs.PublishedAt)
		if obs.ObjectType != "publication" || obs.RemoteID == "" || err != nil || at.After(now) {
			return obs, fmt.Errorf("publication requires a positive publication object, remote id and observed nonfuture publication time")
		}
	case "scheduled":
		if obs.ObjectType != "schedule" || obs.ScheduleID == "" || obs.ScheduledAt != req.ScheduleAt || obs.RemoteID != "" {
			return obs, fmt.Errorf("scheduling requires the exact accepted schedule identity and time")
		}
	case "processing", "unknown", "rejected", "cancelled", "error":
		if obs.ObjectType != "attempt" && obs.ObjectType != "schedule" {
			return obs, fmt.Errorf("nonpublication outcome must name an attempt or schedule object")
		}
	default:
		return obs, fmt.Errorf("unknown typed transport status %q", obs.Status)
	}
	return obs, nil
}

func recordBoundContentObservation(homePath, id string, req ContentPostTransportRequest, operation string, body []byte, receiptID string, now time.Time) (ContentDraft, error) {
	r, d, err := readContentOutbound(homePath, id)
	if err != nil {
		return d, err
	}
	if d.Outbound == nil || d.Outbound.ID != req.AttemptID {
		return d, fmt.Errorf("outbound ownership changed before observation")
	}
	obs, err := parseBoundContentObservation(body, req, operation, d.Outbound.RequestDigest, now)
	if err != nil {
		return d, err
	}
	if d.Outbound.Status == "posted" {
		if obs.Status == "published" && d.Post != nil && d.Post.RemoteID == obs.RemoteID {
			return d, nil
		}
		return d, fmt.Errorf("observation conflicts with terminal publication")
	}
	if d.Outbound.Status == "failed" {
		return d, fmt.Errorf("observation conflicts with terminal failed attempt")
	}
	if d.Schedule != nil {
		// Some providers publish the same object they previously scheduled.
		// An omitted schedule ID is safe only when the positive publication
		// names that exact object; different IDs still need an explicit link.
		sameObjectPublication := obs.Status == "published" && obs.ScheduleID == "" && obs.RemoteID == d.Schedule.ScheduleID
		if obs.ScheduleID != d.Schedule.ScheduleID && !sameObjectPublication {
			return d, fmt.Errorf("observation schedule identity changed")
		}
	}
	store, err := home.NewArtifactStore(homePath)
	if err != nil {
		return d, err
	}
	ref, err := store.Put(body)
	if err != nil {
		return d, err
	}
	d.Outbound.ResponseRef = ref
	d.Outbound.Detail = ""
	if receiptID != "" {
		d.Outbound.ReceiptID = receiptID
	}
	switch obs.Status {
	case "published":
		d.State = ContentDraftStatePosted
		d.Outbound.Status = "posted"
		d.Post = &ContentDraftPost{Platform: d.Platform, RemoteID: obs.RemoteID, URL: obs.URL, PostedAt: obs.PublishedAt, ReceiptRef: d.Outbound.ReceiptID}
	case "scheduled":
		d.State = ContentDraftStateScheduled
		d.Outbound.Status = "scheduled"
		d.Schedule = &ContentDraftSchedule{Platform: d.Platform, ScheduleID: obs.ScheduleID, ScheduledAt: obs.ScheduledAt, AckedAt: now.UTC().Format(time.RFC3339), ReceiptRef: d.Outbound.ReceiptID}
	case "rejected", "cancelled", "error":
		d.Outbound.Status = "failed"
		d.Outbound.Detail = obs.Status
	case "processing":
		d.Outbound.Status = "processing"
	default:
		d.Outbound.Status = "reconciliation_required"
	}
	res, err := replaceContentOutbound(homePath, r, d)
	if err != nil {
		return d, err
	}
	if !res.Replaced {
		return d, fmt.Errorf("newer draft generation preserved while recording observation; reconcile again")
	}
	return d, nil
}
