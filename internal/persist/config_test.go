package persist

import (
	"errors"
	"strings"
	"testing"
)

func syntheticURLDSN(secret string) string {
	return strings.Join([]string{"postgres", "://", "operator", ":", secret, "@", "db.example.invalid", "/", "compound"}, "")
}

func syntheticKeywordDSN(secret string) string {
	return strings.Join([]string{"host", "=db.example.invalid", " user", "=operator", " pass", "word", "=", secret, " dbname", "=compound"}, "")
}

func clearStoreEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"COMPOUND_STORE",
		"COMPOUND_DSN",
		ExperimentalPostgresEnv,
		PostgresProfileEnv,
		"COMPOUND_VECTOR_DIMS",
	} {
		t.Setenv(key, "")
	}
}

func TestConfigFromEnvDefaultsToSupportedFileBackend(t *testing.T) {
	clearStoreEnv(t)

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Backend != BackendFile {
		t.Fatalf("backend = %q, want %q", cfg.Backend, BackendFile)
	}
	if cfg.DSN != "" || cfg.PostgresProfile != "" || cfg.ExperimentalPostgres {
		t.Fatalf("file config carried experimental credentials: %+v", cfg)
	}
}

func TestConfigFromEnvRejectsUnknownBackendWithoutFileFallback(t *testing.T) {
	clearStoreEnv(t)
	t.Setenv("COMPOUND_STORE", "postgre")

	cfg, err := ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "unsupported COMPOUND_STORE") {
		t.Fatalf("error = %v, want unsupported-backend refusal", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("refused unknown backend returned usable config: %+v", cfg)
	}
}

func TestConfigFromEnvRequiresExactBackendSelection(t *testing.T) {
	for _, value := range []string{" postgres ", "POSTGRES", " file ", "FILE"} {
		t.Run(value, func(t *testing.T) {
			clearStoreEnv(t)
			t.Setenv("COMPOUND_STORE", value)

			_, err := ConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), "unsupported COMPOUND_STORE") {
				t.Fatalf("COMPOUND_STORE=%q error = %v, want exact-value refusal", value, err)
			}
		})
	}
}

func TestConfigFromEnvRejectsPartialPostgresConfigurationWithoutSelection(t *testing.T) {
	for _, key := range []string{ExperimentalPostgresEnv, PostgresProfileEnv, "COMPOUND_DSN", "COMPOUND_VECTOR_DIMS"} {
		t.Run(key, func(t *testing.T) {
			clearStoreEnv(t)
			t.Setenv(key, "configured")

			cfg, err := ConfigFromEnv()
			if err == nil || !strings.Contains(err.Error(), "partial Postgres configuration") {
				t.Fatalf("ConfigFromEnv() = %+v, %v; want partial-selection refusal", cfg, err)
			}
			if cfg != (Config{}) {
				t.Fatalf("refused partial selection returned usable config: %+v", cfg)
			}
		})
	}
}

func TestConfigFromEnvRejectsInvalidVectorDimensions(t *testing.T) {
	for _, value := range []string{"0", "-1", "+1536", "not-a-number", "1536junk", " 1536 ", "١٥٣٦", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			clearStoreEnv(t)
			t.Setenv("COMPOUND_STORE", "postgres")
			t.Setenv(ExperimentalPostgresEnv, "1")
			t.Setenv(PostgresProfileEnv, "local-lab")
			t.Setenv("COMPOUND_DSN", syntheticURLDSN("dimension-sentinel"))
			t.Setenv("COMPOUND_VECTOR_DIMS", value)

			_, err := ConfigFromEnv()
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Field != "COMPOUND_VECTOR_DIMS" {
				t.Fatalf("COMPOUND_VECTOR_DIMS=%q error = %T %v, want typed dimension refusal", value, err, err)
			}
		})
	}
}

func TestConfigFromEnvRequiresSeparateExperimentalPostgresOptIn(t *testing.T) {
	clearStoreEnv(t)
	secret := "credential" + "-sentinel"
	t.Setenv("COMPOUND_STORE", "postgres")
	t.Setenv("COMPOUND_DSN", syntheticURLDSN(secret))
	t.Setenv(PostgresProfileEnv, "local-lab")

	_, err := ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), ExperimentalPostgresEnv+"=1") {
		t.Fatalf("error = %v, want explicit experimental opt-in refusal", err)
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Field != ExperimentalPostgresEnv {
		t.Fatalf("error = %T %+v, want typed ConfigError for %s", err, err, ExperimentalPostgresEnv)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "db.example.invalid") {
		t.Fatalf("configuration error leaked DSN material: %v", err)
	}

	t.Setenv(ExperimentalPostgresEnv, " 1 ")
	_, err = ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), ExperimentalPostgresEnv+"=1") {
		t.Fatalf("whitespace-padded opt-in was accepted: %v", err)
	}
}

func TestConfigFromEnvRequiresNonsecretPostgresProfile(t *testing.T) {
	clearStoreEnv(t)
	secret := "credential" + "-sentinel"
	t.Setenv("COMPOUND_STORE", "postgres")
	t.Setenv(ExperimentalPostgresEnv, "1")
	t.Setenv("COMPOUND_DSN", syntheticURLDSN(secret))

	_, err := ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), PostgresProfileEnv) {
		t.Fatalf("error = %v, want missing-profile refusal", err)
	}

	profileSecret := "profile" + "-sentinel"
	t.Setenv(PostgresProfileEnv, syntheticURLDSN(profileSecret))
	_, err = ConfigFromEnv()
	if err == nil || !strings.Contains(err.Error(), "safe profile label") {
		t.Fatalf("error = %v, want unsafe-profile refusal", err)
	}
	if strings.Contains(err.Error(), profileSecret) || strings.Contains(err.Error(), "db.example.invalid") {
		t.Fatalf("profile validation leaked rejected value: %v", err)
	}
}

func TestConfigFromEnvBuildsOpaquePostgresTarget(t *testing.T) {
	clearStoreEnv(t)
	secret := "credential" + "-sentinel"
	t.Setenv("COMPOUND_STORE", "postgres")
	t.Setenv(ExperimentalPostgresEnv, "1")
	t.Setenv(PostgresProfileEnv, "local-lab")
	t.Setenv("COMPOUND_DSN", syntheticKeywordDSN(secret))

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if got := cfg.PostgresTargetRef(); got != "postgres-profile:local-lab" {
		t.Fatalf("target ref = %q", got)
	}
	if strings.Contains(cfg.PostgresTargetRef(), secret) || strings.Contains(cfg.PostgresTargetRef(), "db.example.invalid") {
		t.Fatalf("target ref disclosed DSN material: %q", cfg.PostgresTargetRef())
	}
}

func TestValidatePostgresExperimentProtectsDirectAdapterConstruction(t *testing.T) {
	secret := "credential" + "-sentinel"
	cfg := Config{
		Backend:          BackendPostgres,
		DSN:              syntheticURLDSN(secret),
		PostgresProfile:  "local-lab",
		VectorDimensions: 1536,
	}
	if err := cfg.ValidatePostgresExperiment(); err == nil || !strings.Contains(err.Error(), ExperimentalPostgresEnv) {
		t.Fatalf("direct config validation = %v, want opt-in refusal", err)
	}
	cfg.ExperimentalPostgres = true
	if err := cfg.ValidatePostgresExperiment(); err != nil {
		t.Fatalf("valid experimental config rejected: %v", err)
	}
}
