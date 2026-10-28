package runtimebinding

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSwitchbackPulseFailedHopReport(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": "fast",
  "overall": "fail",
  "summary": {"ok": 2, "warn": 1, "fail": 1},
  "duration_ms": 42,
  "checks": [
    {"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"},
    {"name": "headroom", "area": "headroom", "level": "ok", "detail": "synthetic optimizer is healthy"},
    {"name": "config-drift", "area": "config", "level": "warn", "detail": "synthetic config differs"},
    {"name": "tap:synthetic capture/one", "area": "taps", "level": "fail", "detail": "synthetic-capture.invalid is not listening\u001b[31m\u202e"}
  ]
}`, now.Add(-time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || report.ServiceID != "service:switchback" || report.Owner != "project:switchback" {
		t.Fatalf("unexpected report identity/status: %+v", report)
	}
	if got := checkStatus(report, "live:config-drift"); got != "warn" {
		t.Fatalf("warning status = %q, checks=%+v", got, report.Checks)
	}
	if got := checkStatus(report, "live:tap:synthetic capture/one"); got != "fail" {
		t.Fatalf("failed hop status = %q, checks=%+v", got, report.Checks)
	}
	detail := checkDetail(report, "live:tap:synthetic capture/one")
	if !strings.Contains(detail, "area=taps") || !strings.Contains(detail, "synthetic-capture.invalid is not listening") {
		t.Fatalf("failed hop detail was not preserved safely: %q", detail)
	}
	if strings.ContainsRune(detail, '\x1b') || strings.ContainsRune(detail, '\u202e') {
		t.Fatalf("failed hop detail retained terminal/control formatting: %q", detail)
	}
	if !NeedsWriteback(report) {
		t.Fatal("failed pulse should enter runtime writeback")
	}

	plan, err := BuildRepairPlan(report, RepairRefs{WritebackGateID: "owg_synthetic_pulse"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if plan.FailedCount != 1 || len(plan.FailedChecks) != 1 || plan.FailedChecks[0].Name != "live:tap:synthetic capture/one" {
		t.Fatalf("repair plan did not preserve the exact failed hop: %+v", plan)
	}
}

func TestSwitchbackPulseHealthyReport(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 2, "warn": 0, "fail": 0},
  "duration_ms": 21,
  "checks": [
    {"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"},
    {"name": "headroom", "area": "headroom", "level": "ok", "detail": "synthetic optimizer is healthy"}
  ]
}`, now.Add(-time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, checks=%+v", report.Status, report.Checks)
	}
	for _, name := range []string{"live:schema", "live:freshness", "live:engine", "live:headroom"} {
		if got := checkStatus(report, name); got != "ok" {
			t.Fatalf("%s = %q, checks=%+v", name, got, report.Checks)
		}
	}
}

func TestSwitchbackPulseAcceptsV2Schema(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@2",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 2, "warn": 0, "fail": 0},
  "duration_ms": 12,
  "checks": [
    {"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"},
    {"name": "headroom", "area": "headroom", "level": "ok", "detail": "synthetic optimizer is healthy"}
  ]
}`, now.Add(-time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || checkStatus(report, "live:schema") != "ok" {
		t.Fatalf("v2 schema should be accepted: %+v", report)
	}
	if got := checkDetail(report, "live:schema"); got != "switchback/pulse@2" {
		t.Fatalf("expected schema detail to reflect observed version, got %q", got)
	}
}

func TestSwitchbackPulseRejectsSchemaMismatch(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@999",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 1, "warn": 0, "fail": 0},
  "checks": [{"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"}]
}`, now.Add(-time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || checkStatus(report, "live:schema") != "fail" || !NeedsWriteback(report) {
		t.Fatalf("schema mismatch did not fail closed: %+v", report)
	}
}

func TestSwitchbackPulseRejectsStaleReceipt(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 2, "warn": 0, "fail": 0},
  "duration_ms": 20,
  "checks": [
    {"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"},
    {"name": "headroom", "area": "headroom", "level": "ok", "detail": "synthetic optimizer is healthy"}
  ]
}`, now.Add(-21*time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || checkStatus(report, "live:freshness") != "fail" || !NeedsWriteback(report) {
		t.Fatalf("stale receipt did not fail closed: %+v", report)
	}
}

func TestSwitchbackPulseAllowsScheduledJitter(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 2, "warn": 0, "fail": 0},
  "duration_ms": 20,
  "checks": [
    {"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"},
    {"name": "headroom", "area": "headroom", "level": "ok", "detail": "synthetic optimizer is healthy"}
  ]
}`, now.Add(-20*time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || checkStatus(report, "live:freshness") != "ok" {
		t.Fatalf("one scheduled interval plus bounded jitter should remain fresh: %+v", report)
	}
}

func TestSwitchbackPulseRequiresCoreCoverage(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 1, "warn": 0, "fail": 0},
  "duration_ms": 10,
  "checks": [{"name": "arbitrary", "area": "synthetic", "level": "ok", "detail": "unrelated check"}]
}`, now.Add(-time.Minute).Format(time.RFC3339))

	report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "fail" || checkStatus(report, "live:coverage") != "fail" || !NeedsWriteback(report) {
		t.Fatalf("partial receipt coverage did not fail closed: %+v", report)
	}
}

func TestSwitchbackPulseRejectsInvalidReceiptMetadata(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		mode         string
		durationJSON string
	}{
		{name: "missing mode", mode: "", durationJSON: "10"},
		{name: "unknown mode", mode: "slow", durationJSON: "10"},
		{name: "missing duration", mode: "fast", durationJSON: "null"},
		{name: "negative duration", mode: "fast", durationJSON: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": %q,
  "overall": "ok",
  "summary": {"ok": 1, "warn": 0, "fail": 0},
  "duration_ms": %s,
  "checks": [{"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"}]
}`, now.Add(-time.Minute).Format(time.RFC3339), tt.mode, tt.durationJSON)

			report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != "fail" || checkStatus(report, "live:receipt") != "fail" || !NeedsWriteback(report) {
				t.Fatalf("invalid receipt metadata did not fail closed: %+v", report)
			}
		})
	}
}

func TestSwitchbackPulseRejectsInvalidOrFutureTimestamp(t *testing.T) {
	now := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		timestamp string
	}{
		{name: "invalid RFC3339", timestamp: "not-a-timestamp"},
		{name: "future beyond skew", timestamp: now.Add(2 * time.Minute).Format(time.RFC3339)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{
  "schema": "switchback/pulse@1",
  "ts": %q,
  "mode": "fast",
  "overall": "ok",
  "summary": {"ok": 1, "warn": 0, "fail": 0},
  "duration_ms": 10,
  "checks": [{"name": "engine", "area": "engine", "level": "ok", "detail": "synthetic engine is healthy"}]
}`, tt.timestamp)

			report, err := ReadSwitchbackPulseReport(strings.NewReader(body), now)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != "fail" || checkStatus(report, "live:freshness") != "fail" || !NeedsWriteback(report) {
				t.Fatalf("invalid receipt timestamp did not fail closed: %+v", report)
			}
		})
	}
}

func checkDetail(report DoctorReport, name string) string {
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Detail
		}
	}
	return ""
}
