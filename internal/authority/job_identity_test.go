package authority

import (
	"testing"
)

func TestParseJobRosterCarriesExplicitExecutionIdentity(t *testing.T) {
	data := []byte(`{"jobs":[{"id":"job-1","name":"release certifier","enabled":true,"owner":"compound:release","harness":"hermes","model":"minimax-m3","provider":"switchback"}]}`)
	jobs, err := parseJobRoster(data, "/tmp/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	job := jobs[0]
	if job.AccountableOwner != "compound:release" || job.Harness != "hermes" || job.RequestedModel != "minimax-m3" || job.ProviderRoute != "switchback" {
		t.Fatalf("explicit job identity = %+v", job)
	}
	if job.IdentitySource != "explicit" {
		t.Fatalf("identity source = %q, want explicit", job.IdentitySource)
	}
}

func TestParseJobRosterDerivesSpecificIdentityForAutonomousJobs(t *testing.T) {
	data := []byte(`{"jobs":[{"id":"script-1","name":"disk guard","enabled":true,"no_agent":true,"script":"disk-guard.sh"},{"id":"agent-1","name":"daily synthesis","enabled":true}]}`)
	jobs, err := parseJobRoster(data, "/Users/example/.hermes/cron/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	byID := map[string]JobSummary{}
	for _, job := range jobs {
		byID[job.JobID] = job
	}
	script := byID["script-1"]
	if script.AccountableOwner != "hermes-job:script-1" || script.Harness != "script" || script.RequestedModel != "none" || script.ProviderRoute != "local" || script.IdentitySource != "derived" {
		t.Fatalf("derived script identity = %+v", script)
	}
	agent := byID["agent-1"]
	if agent.AccountableOwner != "hermes-job:agent-1" || agent.Harness != "hermes" || agent.RequestedModel != "router-default" || agent.ProviderRoute != "router-default" || agent.IdentitySource != "derived" {
		t.Fatalf("derived agent identity = %+v", agent)
	}
}
