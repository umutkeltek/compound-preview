package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"compound/internal/home"
	"compound/internal/persist"
)

func TestMemoryBreadthEngineRefusesExternalStoreSelectionWithoutFileFallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store string
		valid bool
	}{
		{name: "unknown backend", store: "postgre"},
		{name: "valid experimental postgres", store: "postgres", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{
				"COMPOUND_STORE",
				"COMPOUND_DSN",
				persist.ExperimentalPostgresEnv,
				persist.PostgresProfileEnv,
				"COMPOUND_VECTOR_DIMS",
				"COMPOUND_MEM_ENGINE_BIN",
			} {
				t.Setenv(key, "")
			}
			t.Setenv("COMPOUND_STORE", tc.store)
			secret := "memory-fence-" + "sentinel"
			if tc.valid {
				t.Setenv(persist.ExperimentalPostgresEnv, "1")
				t.Setenv(persist.PostgresProfileEnv, "local-lab")
				t.Setenv("COMPOUND_DSN", strings.Join([]string{
					"postgres", "://operator:", secret, "@db.example.invalid/compound",
				}, ""))
			}

			eng, closeEngine := memBreadthEngine()
			defer closeEngine()
			_, err := eng.Query("synthetic retrieval", "", nil, 10)
			if err == nil {
				t.Fatal("selected external store fell through to the file breadth engine")
			}
			var configErr *persist.ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("error = %T %v, want typed external-store refusal", err, err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "db.example.invalid") {
				t.Fatalf("external-store refusal disclosed connection material: %v", err)
			}
		})
	}
}

func TestMemoryDoctorReportsExternalStoreRefusalInsteadOfFileHealth(t *testing.T) {
	homePath := t.TempDir()
	if err := home.Init(homePath, "global", time.Unix(0, 0)); err != nil {
		t.Fatalf("init home: %v", err)
	}

	cmd := exec.Command(compoundTestBinary(t), "memory", "doctor", "--json")
	cmd.Env = parityTestEnv(os.Environ(), map[string]string{
		"COMPOUND_HOME":                   homePath,
		"COMPOUND_STORE":                  "postgre",
		"COMPOUND_DSN":                    "",
		persist.ExperimentalPostgresEnv:   "",
		persist.PostgresProfileEnv:        "",
		"COMPOUND_VECTOR_DIMS":            "",
		"COMPOUND_MEM_ENGINE_BIN":         "",
		"COMPOUND_MEMORY_CLI_PROFILE":     "",
		"COMPOUND_MEM_ENGINE_EMBEDDER":    "",
		"COMPOUND_BREADTH_EMBED_PROVIDER": "",
	})
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("memory doctor reported file-backed health under an unknown external store: %s", out)
	}
	if !strings.Contains(string(out), "unsupported COMPOUND_STORE") {
		t.Fatalf("memory doctor omitted categorical external-store refusal: %s", out)
	}
}

func TestMemoryBreadthCLIsFailClosedOnExternalStoreSelection(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes the compound binary")
	}

	secret := "memory-cli-fence-" + "sentinel"
	host := "memory-db.example.invalid"
	configurations := []struct {
		name       string
		overrides  map[string]string
		wantOutput string
	}{
		{
			name: "unknown backend",
			overrides: map[string]string{
				"COMPOUND_STORE": "postgre",
			},
			wantOutput: "unsupported COMPOUND_STORE value",
		},
		{
			name: "partial postgres selection",
			overrides: map[string]string{
				"COMPOUND_STORE": "postgres",
			},
			wantOutput: persist.ExperimentalPostgresEnv,
		},
		{
			name: "valid experimental postgres selection",
			overrides: map[string]string{
				"COMPOUND_STORE":                "postgres",
				persist.ExperimentalPostgresEnv: "1",
				persist.PostgresProfileEnv:      "local-lab",
				"COMPOUND_DSN": strings.Join([]string{
					"postgres", "://operator:", secret, "@", host, "/compound",
				}, ""),
			},
			wantOutput: "not wired to the memory breadth CLI",
		},
	}
	commands := []struct {
		name string
		args []string
	}{
		{name: "query", args: []string{"memory", "query", "synthetic retrieval"}},
		{name: "engine doctor", args: []string{"memory", "engine-doctor", "synthetic retrieval"}},
	}

	for _, configuration := range configurations {
		for _, command := range commands {
			t.Run(configuration.name+"/"+command.name, func(t *testing.T) {
				homePath := t.TempDir()
				if err := home.Init(homePath, "global", time.Unix(0, 0)); err != nil {
					t.Fatalf("init home: %v", err)
				}
				overrides := map[string]string{
					"COMPOUND_HOME":                   homePath,
					"COMPOUND_STORE":                  "",
					"COMPOUND_DSN":                    "",
					persist.ExperimentalPostgresEnv:   "",
					persist.PostgresProfileEnv:        "",
					"COMPOUND_VECTOR_DIMS":            "",
					"COMPOUND_MEM_ENGINE_BIN":         "",
					"COMPOUND_MEMORY_CLI_PROFILE":     "",
					"COMPOUND_MEM_ENGINE_EMBEDDER":    "",
					"COMPOUND_BREADTH_EMBED_PROVIDER": "",
				}
				for key, value := range configuration.overrides {
					overrides[key] = value
				}

				cmd := exec.Command(compoundTestBinary(t), command.args...)
				cmd.Env = parityTestEnv(os.Environ(), overrides)
				out, err := cmd.CombinedOutput()
				if err == nil {
					t.Fatalf("%s succeeded under %s selection: %s", command.name, configuration.name, out)
				}
				if !strings.Contains(string(out), configuration.wantOutput) {
					t.Fatalf("%s omitted categorical refusal %q: %s", command.name, configuration.wantOutput, out)
				}
				for _, forbidden := range []string{secret, host} {
					if strings.Contains(string(out), forbidden) {
						t.Fatalf("%s disclosed connection material %q: %s", command.name, forbidden, out)
					}
				}
			})
		}
	}
}
