package product

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/candidates"
	"compound/internal/home"
)

// The provider keeps one object ID when its scheduled object becomes published.
// Publication responses omit schedule_id, unlike the older combined fixture.
func TestContentRecoveryTransportProcess(t *testing.T) {
	args := os.Args
	if len(args) < 2 || args[len(args)-2] != "--content-recovery-transport" {
		return
	}
	dir := args[len(args)-1]
	var req ContentPostTransportRequest
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(10)
	}
	f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(11)
	}
	fmt.Fprintln(f, req.PostType)
	f.Close()
	original := req
	original.PostType = "schedule"
	raw, _ := json.Marshal(original)
	obs := ContentTransportObservation{
		SchemaVersion: ContentObservationSchemaVersion, Operation: req.PostType,
		AttemptID: req.AttemptID, IdempotencyKey: req.IdempotencyKey,
		RequestDigest: act.ContentTransportDigest(raw), PackageDigest: req.PackageDigest,
		Transport: req.Transport, AccountID: req.AccountID, IntegrationID: req.IntegrationID,
		Binding: req.Binding,
	}
	if req.PostType == "schedule" {
		obs.ObjectType, obs.Status = "schedule", "scheduled"
		obs.ScheduleID, obs.ScheduledAt = "object-synth", req.ScheduleAt
	} else if req.PostType == "observe" {
		obs.ObjectType, obs.Status = "publication", "published"
		obs.RemoteID = "object-synth"
		obs.PublishedAt = time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
		obs.URL = "https://social.example/post/post-synth"
		mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
		switch string(mode) {
		case "explicit":
			obs.ScheduleID = "object-synth"
		case "separate-post":
			obs.ScheduleID, obs.RemoteID = "object-synth", "publication-synth"
		case "wrong-schedule":
			obs.ScheduleID = "other-schedule"
		case "wrong-post":
			obs.RemoteID = "other-publication"
		case "wrong-account":
			obs.AccountID = "other-account"
		case "wrong-integration":
			obs.IntegrationID = "other-integration"
		case "wrong-attempt":
			obs.AttemptID = "other-attempt"
		case "wrong-request":
			obs.RequestDigest = "sha256:other"
		case "wrong-binding":
			obs.Binding.ExecutableDigest = "sha256:other"
		case "missing-post":
			obs.RemoteID = ""
		case "future-publication":
			obs.PublishedAt = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
		case "unknown", "processing", "error":
			obs.ObjectType, obs.Status = "attempt", string(mode)
			obs.PublishedAt, obs.URL = "", ""
		}
	} else {
		os.Exit(12)
	}
	json.NewEncoder(os.Stdout).Encode(obs)
	os.Exit(0)
}

func recoveryScheduledFixture(t *testing.T) (string, ContentDraft, time.Time, string) {
	t.Helper()
	h, draft, now, _ := b2Fixture(t)
	dir := filepath.Join(h, "recovery-transport")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{
		"argv":             []string{exe, "-test.run=TestContentRecoveryTransportProcess", "--", "--content-recovery-transport", dir},
		"content_contract": act.ContentTransportContract,
	})
	if err := os.WriteFile(filepath.Join(h, "actuator", "transports", "fixture.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := ExecuteContentPost(h, draft.ID, "fixture", "", now.Add(time.Minute), true, false, now)
	if err != nil || out.Mode != "scheduled" || out.Draft == nil {
		t.Fatalf("schedule fixture: mode=%s err=%v", out.Mode, err)
	}
	return h, *out.Draft, now, dir
}

func TestContentRecoveryPublicationIdentity(t *testing.T) {
	for _, mode := range []string{"omitted", "explicit", "separate-post"} {
		t.Run(mode, func(t *testing.T) {
			h, before, now, dir := recoveryScheduledFixture(t)
			if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			store, _ := home.NewArtifactStore(h)
			originalResponse, err := store.Get(before.Outbound.ResponseRef)
			if err != nil {
				t.Fatal(err)
			}
			out, err := ExecuteContentPost(h, before.ID, "fixture", "", time.Time{}, false, true, now)
			if err != nil || out.Mode != "posted" || out.Draft == nil {
				t.Fatalf("positive bound publication did not reconcile: mode=%s err=%v", out.Mode, err)
			}
			after := *out.Draft
			wantID := "object-synth"
			if mode == "separate-post" {
				wantID = "publication-synth"
			}
			if after.Post.RemoteID != wantID || after.Post.URL != "https://social.example/post/post-synth" || after.Post.PostedAt == "" {
				t.Fatalf("publication identity changed: %+v", after.Post)
			}
			if !reflect.DeepEqual(before.Schedule, after.Schedule) || after.Post.ReceiptRef != before.Outbound.ReceiptID {
				t.Fatal("original schedule or receipt lost")
			}
			b, a := *before.Outbound, *after.Outbound
			a.Status, a.ResponseRef = b.Status, b.ResponseRef
			if !reflect.DeepEqual(a, b) {
				t.Fatal("pinned outbound identity or original policy/receipt changed")
			}
			retained, err := store.Get(before.Outbound.ResponseRef)
			if err != nil || !bytes.Equal(originalResponse, retained) {
				t.Fatal("original scheduling response was overwritten")
			}
			observation, err := store.Get(after.Outbound.ResponseRef)
			if err != nil {
				t.Fatal(err)
			}
			var obs ContentTransportObservation
			if err := json.Unmarshal(observation, &obs); err != nil {
				t.Fatal(err)
			}
			if mode == "omitted" && obs.ScheduleID != "" {
				t.Fatal("recovery invented a schedule ID in the provider observation")
			}
			if _, err := ExecuteContentPost(h, before.ID, "fixture", "", time.Time{}, false, true, now); err != nil {
				t.Fatalf("terminal replay: %v", err)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if string(calls) != "schedule\nobserve\n" {
				t.Fatalf("duplicate or send during recovery: %q", calls)
			}
			if _, _, err := BuildContentPostTransportRequest(h, before.ID, "fixture", now.Add(2*time.Minute), now); err == nil {
				t.Fatal("posted draft allowed another send")
			}
			plan, err := BuildContentQueuePlan(h, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range plan.Rows {
				if row.DraftID == before.ID {
					t.Fatal("posted draft still asks the consumer to reconcile or resend")
				}
			}
		})
	}
}

func TestContentRecoveryRefusesUnboundPublication(t *testing.T) {
	for _, mode := range []string{"wrong-schedule", "wrong-post", "wrong-account", "wrong-integration", "wrong-attempt", "wrong-request", "wrong-binding", "missing-post", "future-publication", "unknown", "processing", "error"} {
		t.Run(mode, func(t *testing.T) {
			h, before, now, dir := recoveryScheduledFixture(t)
			if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			original, _ := candidates.ReadRecord(h, candidates.ContentDraft, before.ID)
			if _, err := ExecuteContentPost(h, before.ID, "fixture", "", time.Time{}, false, true, now); err == nil {
				t.Fatalf("accepted %s observation without exact schedule continuity", mode)
			}
			after, _ := candidates.ReadRecord(h, candidates.ContentDraft, before.ID)
			if !bytes.Equal(original.Body, after.Body) {
				t.Fatal("refused observation changed the owned draft")
			}
			if _, _, err := BuildContentPostTransportRequest(h, before.ID, "fixture", now.Add(2*time.Minute), now); err == nil || !strings.Contains(err.Error(), "already owns outbound attempt") {
				t.Fatalf("uncertain attempt lost its no-resend barrier: %v", err)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if string(calls) != "schedule\nobserve\n" {
				t.Fatalf("unexpected transport calls: %q", calls)
			}
		})
	}
}
