package work

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIProofExportsDiagnosticArtifacts(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "proof.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				If   string            `yaml:"if"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ job, report string }{
		{"proof-ci", "ci"}, {"proof-ci-console", "ci-console"},
	} {
		t.Run(tc.job, func(t *testing.T) {
			count := 0
			for _, step := range workflow.Jobs[tc.job].Steps {
				if !strings.HasPrefix(step.Uses, "actions/upload-artifact@") {
					continue
				}
				count++
				want := []string{
					"${{ runner.temp }}/" + tc.report + ".json",
					"${{ runner.temp }}/" + tc.report + ".stdout",
					"${{ runner.temp }}/" + tc.report + ".json.gates/",
				}
				if got := strings.Split(strings.TrimSpace(step.With["path"]), "\n"); !reflect.DeepEqual(got, want) {
					t.Errorf("exported diagnostics = %v; want exact report/stdout/gate logs %v (never a whole home or workspace)", got, want)
				}
				if step.If != "always()" {
					t.Error("diagnostics must survive a failed proof step")
				}
			}
			if count != 1 {
				t.Errorf("diagnostic upload steps = %d, want 1", count)
			}
		})
	}
}
