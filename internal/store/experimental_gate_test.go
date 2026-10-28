package store

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"compound/internal/persist"

	"github.com/jackc/pgx/v5/pgconn"
)

func syntheticStoreTestURLDSN(secret string) string {
	return strings.Join([]string{"postgres", "://", "operator", ":", secret, "@", "db.example.invalid", "/", "compound"}, "")
}

func malformedStoreTestDSN(secret string) string {
	return strings.Join([]string{"host", "='unterminated user", "=operator pass", "word", "=", secret}, "")
}

func storeTestSecret() string {
	return "credential" + "-sentinel"
}

func setSelectedPostgresWithoutExperiment(t *testing.T) {
	t.Helper()
	secret := storeTestSecret()
	t.Setenv("COMPOUND_STORE", "postgres")
	t.Setenv("COMPOUND_DSN", syntheticStoreTestURLDSN(secret))
	t.Setenv(persist.PostgresProfileEnv, "local-lab")
	t.Setenv(persist.ExperimentalPostgresEnv, "")
}

func TestStoreStatusRefusesSelectionWithoutExperimentalOptInAndNeverDisplaysDSN(t *testing.T) {
	setSelectedPostgresWithoutExperiment(t)
	var stdout, stderr bytes.Buffer

	exit := runStoreStatus([]string{"--json"}, &stdout, &stderr)
	if exit == 0 {
		t.Fatalf("store status succeeded without experimental opt-in: %s", stdout.String())
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, persist.ExperimentalPostgresEnv+"=1") {
		t.Fatalf("missing explicit opt-in remedy: %s", combined)
	}
	for _, forbidden := range []string{storeTestSecret(), "db.example.invalid", "dsn_masked", "postgres://"} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("store status disclosed %q: %s", forbidden, combined)
		}
	}
}

func TestEveryStoreVerbSharesExperimentalGateBeforeExternalWork(t *testing.T) {
	setSelectedPostgresWithoutExperiment(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "doctor", args: []string{"doctor", "--json"}},
		{name: "init", args: []string{"init", "--json"}},
		{name: "status", args: []string{"status", "--json"}},
		{name: "migrate", args: []string{"migrate", "--dry-run", "--json"}},
		{name: "sync", args: []string{"sync", "--json"}},
		{name: "embed", args: []string{"embed", "--json"}},
		{name: "recall", args: []string{"recall", "synthetic query", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := Run(tc.args, &stdout, &stderr)
			if exit == 0 {
				t.Fatalf("Run(%v) succeeded without experimental opt-in: %s", tc.args, stdout.String())
			}
			combined := stdout.String() + stderr.String()
			if !strings.Contains(combined, persist.ExperimentalPostgresEnv+"=1") {
				t.Fatalf("Run(%v) did not use the common gate: %s", tc.args, combined)
			}
			for _, forbidden := range []string{storeTestSecret(), "db.example.invalid", "postgres://"} {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("Run(%v) disclosed %q: %s", tc.args, forbidden, combined)
				}
			}
		})
	}
}

func TestStoreGovernedIntentPersistsOnlyOpaqueProfileTarget(t *testing.T) {
	ai, err := storeGovernedIntent("work_profile", "sync_apply", validExperimentalPostgresConfig("local-lab"), "preview", time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("build intent: %v", err)
	}
	data, err := json.Marshal(ai)
	if err != nil {
		t.Fatalf("marshal intent: %v", err)
	}
	encoded := string(data)
	if !strings.Contains(encoded, "postgres-profile:local-lab") {
		t.Fatalf("profile target missing: %s", encoded)
	}
	for _, forbidden := range []string{"dsn", "postgres://", "password", "****@"} {
		if strings.Contains(strings.ToLower(encoded), forbidden) {
			t.Fatalf("governed intent contains DSN-shaped material %q: %s", forbidden, encoded)
		}
	}
}

func TestStoreGovernedIntentRefusesUnvalidatedPostgresConfig(t *testing.T) {
	_, err := storeGovernedIntent(
		"work_profile",
		"sync_apply",
		persist.Config{PostgresProfile: "local-lab"},
		"preview",
		time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	)
	if err == nil {
		t.Fatal("unvalidated Postgres config minted an authority object")
	}
}

func TestStoreDoctorCleanupNeverEchoesConnectionMaterial(t *testing.T) {
	secret := storeTestSecret()
	report := storeDoctorReport{}
	checkCleanup(
		context.Background(),
		malformedStoreTestDSN(secret),
		"postgres-profile:local-lab",
		newStoreDoctorProbe(),
		&report,
	)
	if len(report.Checks) != 1 || report.Checks[0].Status != "fail" {
		t.Fatalf("cleanup report = %+v, want one failed categorical check", report.Checks)
	}
	detail := report.Checks[0].Detail
	if !strings.Contains(detail, "postgres-profile:local-lab") {
		t.Fatalf("cleanup error omitted opaque profile: %q", detail)
	}
	for _, forbidden := range []string{secret, "unterminated", "operator", "host="} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("cleanup error disclosed %q: %q", forbidden, detail)
		}
	}
}

func TestStoreReportsSanitizePostgresConnectionTopology(t *testing.T) {
	raw := "failed to connect to user=operator database=compound: dial tcp db.example.invalid:5432"
	profile := "postgres-profile:local-lab"

	var status bytes.Buffer
	emitStoreStatus(&status, storeStatusReport{
		Backend: "postgres",
		Profile: profile,
		Error:   raw,
	}, true)

	var doctor bytes.Buffer
	emitStoreDoctor(&doctor, storeDoctorReport{
		Backend: "postgres",
		Profile: profile,
		Checks: []storeDoctorCheck{{
			Name: "schema", Status: "fail", Detail: raw,
		}},
	}, true)

	for name, output := range map[string]string{
		"status": status.String(),
		"doctor": doctor.String(),
	} {
		if !strings.Contains(output, profile) {
			t.Fatalf("%s report omitted opaque profile: %s", name, output)
		}
		for _, forbidden := range []string{"operator", "database=", "dial tcp", "db.example.invalid", "5432"} {
			if strings.Contains(output, forbidden) {
				t.Fatalf("%s report disclosed %q: %s", name, forbidden, output)
			}
		}
	}
}

func TestPostgresFailureWriterSanitizesTopologyAndPreservesSQLState(t *testing.T) {
	profile := "postgres-profile:local-lab"
	raw := &pgconn.PgError{
		Code:    "42501",
		Message: "permission denied for operator at db.example.invalid:5432/compound",
	}
	var output bytes.Buffer
	writePostgresFailure(&output, "store sync", profile, "bulk claims", raw)

	got := output.String()
	for _, required := range []string{"store sync", profile, "bulk claims", "SQLSTATE 42501"} {
		if !strings.Contains(got, required) {
			t.Fatalf("sanitized failure omitted %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{"operator", "db.example.invalid", "5432", "compound"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("sanitized failure disclosed %q: %s", forbidden, got)
		}
	}
}
