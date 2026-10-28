package main

import (
	"strings"
	"testing"
	"time"

	"compound/internal/home"
)

func TestBuildHomeDoctorReportIncludesParseableGeneratedAt(t *testing.T) {
	t.Parallel()
	report := buildHomeDoctorReport(home.Report{Home: t.TempDir()})
	if strings.TrimSpace(report.GeneratedAt) == "" {
		t.Fatal("generated_at must be non-empty")
	}
	if _, err := time.Parse(time.RFC3339, report.GeneratedAt); err != nil {
		t.Fatalf("generated_at must be RFC3339: %q: %v", report.GeneratedAt, err)
	}
}
