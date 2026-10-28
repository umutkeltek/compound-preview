package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestCanonicalProofRequiresSealedNoSkipCredentialAlignmentConsumer(t *testing.T) {
	head := strings.Repeat("a", 40)
	steps, err := proofCheckSteps(strings.Repeat("b", 40), head)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, step := range steps {
		if step.Name != "credential-password-alignment-native" {
			continue
		}
		count++
		want := []string{"go", "run", "./scripts/testbattery", "credential-password-alignment-native", "--source-head", head}
		if !reflect.DeepEqual(step.Command, want) || step.SkipIfMissing != "" || step.CIPortable || step.Dir != "" || step.ProofRef != "credential-password-alignment-native-pg17-no-skips" {
			t.Fatal("required native consumer lost source binding, fail-closed prerequisites or exact inventory", step)
		}
	}
	if count != 1 {
		t.Fatal("canonical full proof does not contain exactly one required native credential consumer")
	}
}
