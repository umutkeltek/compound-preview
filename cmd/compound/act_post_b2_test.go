package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"compound/internal/act"
	"compound/internal/mem"
	"compound/internal/policy"
	"compound/internal/product"
)

func b2Grant(t *testing.T, homePath string) {
	t.Helper()
	p := filepath.Join(homePath, "standing-grants.yaml")
	body := "generated_at: \"2026-09-21T12:00:00Z\"\nauthority:\n  decision_ref: \"grant:fixture-content\"\n  reviewed_by: \"operator:fixture\"\ngrants:\n  - grant_id: grant_content\n    capabilities: [\"" + product.ContentPostCapability + "\"]\n    target_scope: \"actuator/transports/fixture.json\"\n    mutation_class: external_side_effect\n    reversibility_ceiling: irreversible\n    blast_radius: local\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.ImportStandingGrants(homePath, p, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// The enrolled synthetic executable consumes the actual wire request. It
// never talks to a provider, and records create versus observe separately.
func TestB2TransportProcess(t *testing.T) {
	if os.Getenv("COMPOUND_B2_TRANSPORT") != "1" {
		return
	}
	dir := os.Args[len(os.Args)-1]
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	var req product.ContentPostTransportRequest
	if json.Unmarshal(body, &req) != nil {
		os.Exit(4)
	}
	f, err := os.OpenFile(filepath.Join(dir, "calls"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(5)
	}
	_, _ = fmt.Fprintln(f, req.PostType)
	_ = f.Close()
	_ = os.WriteFile(filepath.Join(dir, "request"), body, 0600)
	original := req
	original.PostType = "schedule"
	originalBytes, _ := json.Marshal(original)
	obs := product.ContentTransportObservation{SchemaVersion: product.ContentObservationSchemaVersion, Operation: req.PostType, AttemptID: req.AttemptID, IdempotencyKey: req.IdempotencyKey, RequestDigest: act.ContentTransportDigest(originalBytes), PackageDigest: req.PackageDigest, Transport: req.Transport, AccountID: req.AccountID, IntegrationID: req.IntegrationID, Binding: req.Binding, ObjectType: "schedule", Status: "scheduled", ScheduleID: "schedule-synth", ScheduledAt: req.ScheduleAt}
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	switch string(mode) {
	case "published", "lost_response", "truncated_response":
		time.Sleep(1100 * time.Millisecond)
		obs.ObjectType = "publication"
		obs.Status = "published"
		obs.RemoteID = "post-synth"
		obs.PublishedAt = time.Now().UTC().Format(time.RFC3339)
	case "processing", "unknown", "rejected", "cancelled", "error":
		obs.ObjectType = "attempt"
		obs.Status = string(mode)
	case "untyped":
		fmt.Print(`{"id":"job-synth"}`)
		os.Exit(0)
	}
	encoded, _ := json.Marshal(obs)
	_, _ = os.Stdout.Write(encoded)
	if string(mode) == "truncated_response" {
		fmt.Print(strings.Repeat(" ", 70*1024))
	}
	if string(mode) == "lost_response" {
		os.Exit(7)
	}
	if string(mode) == "persist_failure" {
		_ = os.Rename(filepath.Join(os.Getenv("COMPOUND_HOME"), "inbox", "content-drafts"), filepath.Join(os.Getenv("COMPOUND_HOME"), "inbox", "content-drafts-held"))
	}
	os.Exit(0)
}

func b2Transport(t *testing.T, h string) string {
	t.Helper()
	t.Setenv("COMPOUND_B2_TRANSPORT", "1")
	t.Setenv("COMPOUND_HOME", h)
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"argv": []string{exe, "-test.run=TestB2TransportProcess", "--", dir}, "content_contract": act.ContentTransportContract, "timeout_s": 30})
	p := filepath.Join(h, "actuator", "transports")
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "fixture.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestB2CLIProcess(t *testing.T) {
	if os.Getenv("COMPOUND_B2_CLI") == "" {
		return
	}
	var args []string
	if json.Unmarshal([]byte(os.Getenv("COMPOUND_B2_CLI")), &args) != nil {
		os.Exit(9)
	}
	os.Exit(runActWithInput(args, strings.NewReader(""), os.Stdout, os.Stderr))
}

func b2Run(t *testing.T, h, id, at string, reconcile, fresh bool) (product.ContentPostRun, int, string) {
	t.Helper()
	args := []string{"post", "--draft", id, "--transport", "fixture", "--home", h, "--json"}
	if reconcile {
		args = append(args, "--reconcile")
	} else {
		args = append(args, "--at", at, "--confirm")
	}
	var out, errOut bytes.Buffer
	code := 0
	if fresh {
		exe, _ := os.Executable()
		argJSON, _ := json.Marshal(args)
		cmd := exec.Command(exe, "-test.run=^TestB2CLIProcess$")
		cmd.Env = append(os.Environ(), "COMPOUND_B2_CLI="+string(argJSON))
		cmd.Stdout = &out
		cmd.Stderr = &errOut
		if err := cmd.Run(); err != nil {
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else {
				t.Fatal(err)
			}
		}
	} else {
		code = runActWithInput(args, strings.NewReader(""), &out, &errOut)
	}
	var result product.ContentPostRun
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("result not JSON: %v: %s / %s", err, out.String(), errOut.String())
	}
	return result, code, errOut.String()
}

func TestB2ReviewedMediaScheduleRestartAndPublication(t *testing.T) {
	t.Setenv(product.DistributionExperimentEnv, "1")
	h := t.TempDir()
	_ = seedAuthorizedPostDraft(t, h, "active", "integration-synth")
	b2Grant(t, h)
	dir := b2Transport(t, h)
	now := time.Now().UTC()
	image := []byte("synthetic image bytes")
	d, err := product.RecordContentDraft(h, product.ContentDraftInput{ProductID: "product:prod_synth_a", Platform: "x", Body: "exact reviewed body\nwith second line", ExpiresAt: now.Add(time.Hour), Media: []product.ContentDraftMediaInput{{Kind: "image", Bytes: image, MIMEType: "image/png", AltText: "exact reviewed alternative text"}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := product.BuildContentQueuePlan(h, now)
	if err != nil {
		t.Fatal(err)
	}
	var row product.ContentQueueRow
	for _, r := range plan.Rows {
		if r.DraftID == d.ID {
			row = r
		}
	}
	if row.BodyText != "exact reviewed body\nwith second line" || len(row.Media) != 1 || row.Media[0].AltText != "exact reviewed alternative text" || row.PackageDigest == "" {
		t.Fatalf("review does not show package: %+v", row)
	}
	review := product.ContentQueueReview{SchemaVersion: product.ContentQueueReviewSchemaVersion, ID: "review_b2_exact", Drafts: []product.ContentQueueReviewRow{{DraftID: d.ID, PackageDigest: row.PackageDigest, Verdict: "approve"}}}
	report, err := product.ApplyContentQueueReview(h, review, "", now)
	if err != nil || report.Counts.Approved != 1 {
		t.Fatalf("review: %+v %v", report, err)
	}
	at := now.Add(time.Minute).Format(time.RFC3339)
	scheduled, code, detail := b2Run(t, h, d.ID, at, false, false)
	if code != 0 || scheduled.Mode != "scheduled" {
		t.Fatalf("schedule: %+v %d %s", scheduled, code, detail)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "request"))
	var req product.ContentPostTransportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req.Body != row.BodyText || req.PackageDigest != row.PackageDigest || len(req.Media) != 1 || !bytes.Equal(req.Media[0].Bytes, image) || req.Media[0].AltText != row.Media[0].AltText {
		t.Fatalf("wire package differs from review: %+v", req)
	}
	if _, code, _ := b2Run(t, h, d.ID, now.Add(2*time.Minute).Format(time.RFC3339), false, true); code == 0 {
		t.Fatal("changed schedule retried create")
	}
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	posted, code, detail := b2Run(t, h, d.ID, "", true, true)
	if code != 0 || posted.Mode != "posted" || posted.Draft.Post.RemoteID != "post-synth" {
		t.Fatalf("fresh reconciliation: %+v %d %s", posted, code, detail)
	}
	if posted.AttemptID != scheduled.AttemptID {
		t.Fatal("reconciliation changed attempt identity")
	}
	if _, code, detail := b2Run(t, h, d.ID, "", true, true); code != 0 {
		t.Fatalf("replay: %s", detail)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if string(calls) != "schedule\nobserve\n" {
		t.Fatalf("duplicate external calls: %q", calls)
	}
	b2MeasurePublishedOutcome(t, h, *posted.Draft, now)
}

// Continue the real published attempt into the existing measurement owner. The
// synthetic operator supplies the causal sentence; publication alone supplies
// neither causal lift nor a supported hypothesis verdict.
func b2MeasurePublishedOutcome(t *testing.T, h string, d product.ContentDraft, now time.Time) {
	t.Helper()
	store, err := mem.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{"content_draft:" + d.ID, d.Outbound.RequestRef, d.Outbound.ResponseRef, d.Outbound.ReceiptID}
	change := "synthetic publication " + d.Outbound.ID
	id, err := product.MintHypothesisDraftCandidate(h, d.ProductID, change, []string{"posthog_activation_rate"}, nil, refs, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := product.HypothesisApprove(h, product.HypothesisApproveInput{CandidateID: id, ExpectedEffect: "The reviewed synthetic publication increases activation in the measured fixture cohort", Metrics: []string{"posthog_activation_rate"}, ObservationWindow: "7d after synthetic publication", ReviewedBy: "operator:fixture"}, now); err != nil {
		t.Fatal(err)
	}
	signal := product.SignalDocument{SchemaVersion: product.SignalSchemaVersion, Source: product.SignalSource{Kind: "posthog", Ref: "fixture:posthog:content"}, Signal: product.Signal{ID: "sig_content_publication", ProductID: d.ProductID, Surface: "activation", ChangeEvent: change, MetricName: "posthog_activation_rate", Before: 0.20, After: 0.25, Window: "7d after synthetic publication", Confidence: 0.8, Confounders: []string{"synthetic controlled cohort"}, EvidenceRefs: refs, ObservedAt: now.Add(7 * 24 * time.Hour).Format(time.RFC3339)}, EvidenceRefs: refs}
	if _, err := product.ApplySignal(h, store, signal, "fixture:content-signal", now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan, err := product.BuildReviewPlan(h, id, signal.Signal.ID, "", now.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	review := product.Review{SchemaVersion: product.ReviewSchemaVersion, ID: plan.ReviewDraft.ReviewID, ProductID: d.ProductID, HypothesisID: id, Result: "supported", Rationale: "Synthetic activation measurement increased after this exact publication", Confidence: 0.8, NextAction: "review the next bounded content package", EvidenceRefs: plan.EvidenceRefs, ReviewedBy: "operator:fixture"}
	if report, err := product.ApplyReview(h, store, review, "fixture:content-review", now.Add(8*24*time.Hour)); err != nil || report.Status != "updated" {
		t.Fatalf("measurement review: %+v %v", report, err)
	}
	model, err := product.BuildLoopModel(h, product.LoopOptions{ProductID: d.ProductID, Now: now.Add(9 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range model.Products {
		for _, open := range p.OpenHypotheses {
			if open.ID == id {
				t.Fatal("measured publication loop still open")
			}
		}
		for _, r := range p.Reviews {
			if r.HypothesisID == id && r.SignalID == signal.Signal.ID && r.Verdict == "supported" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("published-to-measured review missing from loop projection")
	}
}

func TestB2UncertaintyAndLocalRecordFailureRecoverWithoutResend(t *testing.T) {
	for _, mode := range []string{"lost_response", "persist_failure", "truncated_response"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(product.DistributionExperimentEnv, "1")
			h := t.TempDir()
			d := seedAuthorizedPostDraft(t, h, "active", "integration-synth")
			b2Grant(t, h)
			dir := b2Transport(t, h)
			if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			_, code, _ := b2Run(t, h, d.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), false, false)
			if code == 0 {
				t.Fatal("failure was hidden")
			}
			if mode == "persist_failure" {
				if err := os.Rename(filepath.Join(h, "inbox", "content-drafts-held"), filepath.Join(h, "inbox", "content-drafts")); err != nil {
					t.Fatal(err)
				}
			}
			uncertain, err := product.ReadContentDraft(h, d.ID)
			if err != nil || uncertain.Post != nil {
				t.Fatalf("failed/truncated response became publication: %+v %v", uncertain, err)
			}
			if mode != "persist_failure" && uncertain.Outbound.ResponseRef == "" {
				t.Fatal("partial response evidence was discarded")
			}
			if _, code, _ := b2Run(t, h, d.ID, time.Now().Add(2*time.Hour).UTC().Format(time.RFC3339), false, true); code == 0 {
				t.Fatal("uncertain attempt was resent")
			}
			plan, err := product.BuildContentQueuePlan(h, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Rows) != 1 || plan.Rows[0].State != "reconciliation_required" || !strings.Contains(plan.Rows[0].NextCommand, "--reconcile") {
				t.Fatalf("uncertainty not visible: %+v", plan)
			}
			if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("published"), 0600); err != nil {
				t.Fatal(err)
			}
			posted, code, detail := b2Run(t, h, d.ID, "", true, true)
			if code != 0 || posted.Mode != "posted" {
				t.Fatalf("reconciliation failed: %+v %s", posted, detail)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if string(calls) != "schedule\nobserve\n" {
				t.Fatalf("failure caused resend: %q", calls)
			}
		})
	}
}

func TestB2TypedNonpublicationStatesNeverPostOrResend(t *testing.T) {
	for _, status := range []string{"processing", "unknown", "rejected", "cancelled", "error"} {
		t.Run(status, func(t *testing.T) {
			t.Setenv(product.DistributionExperimentEnv, "1")
			h := t.TempDir()
			d := seedAuthorizedPostDraft(t, h, "active", "integration-synth")
			b2Grant(t, h)
			dir := b2Transport(t, h)
			if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(status), 0600); err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
			got, code, detail := b2Run(t, h, d.ID, at, false, false)
			want := "failed"
			if status == "processing" {
				want = "processing"
			}
			if status == "unknown" {
				want = "reconciliation_required"
			}
			if code != 0 || got.Mode != want || got.Draft.Post != nil {
				t.Fatalf("%s: %+v %d %s", status, got, code, detail)
			}
			if _, code, _ := b2Run(t, h, d.ID, time.Now().Add(2*time.Minute).UTC().Format(time.RFC3339), false, true); code == 0 {
				t.Fatal("nonpublication outcome allowed second create")
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if string(calls) != "schedule\n" {
				t.Fatalf("unexpected creates: %q", calls)
			}
		})
	}
}

func TestB2BareSchedulingIDNeverPostsOrRetries(t *testing.T) {
	t.Setenv(product.DistributionExperimentEnv, "1")
	h := t.TempDir()
	d := seedAuthorizedPostDraft(t, h, "active", "integration-synth")
	b2Grant(t, h)
	marker := filepath.Join(h, "calls")
	script := filepath.Join(h, "synthetic-transport")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf called >> \"$1\"\nprintf '{\"id\":\"scheduler-job\"}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"argv": []string{script, marker}, "content_contract": "compound.content_transport.v0.2"})
	if err := os.MkdirAll(filepath.Join(h, "actuator", "transports"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "actuator", "transports", "fixture.json"), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var out, errOut bytes.Buffer
		code := runActWithInput([]string{"post", "--draft", d.ID, "--at", time.Now().Add(time.Duration(i+1) * time.Hour).UTC().Format(time.RFC3339), "--work", "work_fixture_b2", "--transport", "fixture", "--home", h, "--confirm", "--json"}, strings.NewReader(""), &out, &errOut)
		if code == 0 {
			t.Errorf("untyped scheduling ID classified success: %s", out.String())
		}
	}
	after, err := product.ReadContentDraft(h, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State == product.ContentDraftStatePosted {
		t.Error("scheduler ID became posted")
	}
	called, err := os.ReadFile(marker)
	if err != nil || string(called) != "called" {
		t.Fatalf("want exactly one outbound create, got %q, %v", called, err)
	}
}
