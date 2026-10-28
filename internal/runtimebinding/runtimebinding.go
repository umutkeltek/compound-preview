// Package runtimebinding models where a service actually runs and how to check it.
// It is the first RuntimeBinding/ServiceContract slice: public contract + fixture
// doctor now, real infra-os adapters later.
package runtimebinding

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"compound/internal/writeback"

	"gopkg.in/yaml.v3"
)

const (
	StatusCandidate = "candidate"
	StatusActive    = "active"
	StatusStale     = "stale"
	StatusRetired   = "retired"
)

var statuses = []string{StatusCandidate, StatusActive, StatusStale, StatusRetired}

type Bundle struct {
	RuntimeBinding  RuntimeBinding  `json:"runtime_binding" yaml:"runtime_binding"`
	ServiceContract ServiceContract `json:"service_contract" yaml:"service_contract"`
	RuntimeProbes   []RuntimeProbe  `json:"runtime_probes,omitempty" yaml:"runtime_probes,omitempty"`
}

type RuntimeBinding struct {
	ServiceID         string   `json:"service_id" yaml:"service_id"`
	OwnerEntity       string   `json:"owner_entity" yaml:"owner_entity"`
	HostBinding       string   `json:"host_binding" yaml:"host_binding"`
	DeploymentBinding string   `json:"deployment_binding" yaml:"deployment_binding"`
	DataBindings      []string `json:"data_bindings" yaml:"data_bindings"`
	CredentialRefs    []string `json:"credential_refs,omitempty" yaml:"credential_refs,omitempty"`
	HealthCheckRefs   []string `json:"health_check_refs" yaml:"health_check_refs"`
	RiskTier          string   `json:"risk_tier" yaml:"risk_tier"`
	EvidenceRefs      []string `json:"evidence_refs" yaml:"evidence_refs"`
	Status            string   `json:"status" yaml:"status"`
}

type ServiceContract struct {
	ServiceID     string   `json:"service_id" yaml:"service_id"`
	InterfaceRefs []string `json:"interface_refs" yaml:"interface_refs"`
	Dependencies  []string `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	OwnerEntity   string   `json:"owner_entity" yaml:"owner_entity"`
	RiskTier      string   `json:"risk_tier" yaml:"risk_tier"`
	EvidenceRefs  []string `json:"evidence_refs" yaml:"evidence_refs"`
	Status        string   `json:"status" yaml:"status"`
}

type RuntimeProbe struct {
	ID             string `json:"id" yaml:"id"`
	Kind           string `json:"kind" yaml:"kind"`
	Target         string `json:"target" yaml:"target"`
	Method         string `json:"method,omitempty" yaml:"method,omitempty"`
	ExpectedStatus int    `json:"expected_status,omitempty" yaml:"expected_status,omitempty"`
	TimeoutMS      int    `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type DoctorReport struct {
	ServiceID string  `json:"service_id"`
	Owner     string  `json:"owner"`
	Status    string  `json:"status"`
	Checks    []Check `json:"checks"`
}

// NeedsWriteback reports whether a runtime observation should enter the owner
// writeback gate. OK reports are proof; failed reports are evidence that a
// runtime binding/service contract owner may need review or regeneration.
func NeedsWriteback(r DoctorReport) bool {
	return r.Status != "ok"
}

// WritebackGate converts a failing runtime doctor report into the generic owner
// writeback contract. It is evidence and gate input, never owner mutation by
// itself.
func WritebackGate(r DoctorReport, actionIntentRef, policyDecisionRef string, now time.Time) writeback.OwnerWritebackGate {
	return writeback.OwnerWritebackGate{
		SchemaVersion:     writeback.GateSchemaVersion,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		CandidateRef:      "runtime:" + r.ServiceID + ":" + r.Status,
		OwnerTarget:       writeback.OwnerTarget{Kind: "runtime_binding", Ref: r.ServiceID},
		GateBasis:         fmt.Sprintf("runtime doctor for %s is %s; owner binding may need writeback", r.ServiceID, r.Status),
		ActionIntentRef:   actionIntentRef,
		MutationClass:     "local_reversible",
		PolicyDecisionRef: policyDecisionRef,
		EvidenceRefs:      EvidenceRefs(r),
	}
}

// EvidenceRefs returns bounded metadata refs for a runtime doctor report.
func EvidenceRefs(r DoctorReport) []string {
	refs := []string{
		"runtime:" + r.ServiceID,
		"owner:" + r.Owner,
		"status:" + r.Status,
	}
	for _, check := range r.Checks {
		refs = append(refs, "check:"+check.Name+":"+check.Status+":"+check.Detail)
	}
	return refs
}

func Load(path string) (Bundle, error) {
	var b Bundle
	data, err := os.ReadFile(path)
	if err != nil {
		return b, fmt.Errorf("read runtime binding %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("parse runtime binding %s: %w", path, err)
	}
	return b, nil
}

func Doctor(b Bundle) DoctorReport {
	rb := b.RuntimeBinding
	sc := b.ServiceContract
	report := DoctorReport{ServiceID: rb.ServiceID, Owner: rb.OwnerEntity, Status: "ok"}
	add := func(name string, ok bool, detail string) {
		status := "ok"
		if !ok {
			status = "fail"
			report.Status = "fail"
		}
		report.Checks = append(report.Checks, Check{Name: name, Status: status, Detail: detail})
	}

	add("service", rb.ServiceID != "" && sc.ServiceID != "" && rb.ServiceID == sc.ServiceID, joinNonEmpty(rb.ServiceID, sc.ServiceID))
	add("owner", rb.OwnerEntity != "" && sc.OwnerEntity != "" && rb.OwnerEntity == sc.OwnerEntity, joinNonEmpty(rb.OwnerEntity, sc.OwnerEntity))
	add("host", rb.HostBinding != "", rb.HostBinding)
	add("deploy", rb.DeploymentBinding != "", rb.DeploymentBinding)
	add("data", len(rb.DataBindings) > 0, strings.Join(rb.DataBindings, ", "))
	add("health", len(rb.HealthCheckRefs) > 0, strings.Join(rb.HealthCheckRefs, ", "))
	add("interface", len(sc.InterfaceRefs) > 0, strings.Join(sc.InterfaceRefs, ", "))
	add("risk", rb.RiskTier != "" && sc.RiskTier != "" && rb.RiskTier == sc.RiskTier, joinNonEmpty(rb.RiskTier, sc.RiskTier))
	add("evidence", len(rb.EvidenceRefs) > 0 && len(sc.EvidenceRefs) > 0, joinNonEmpty(strings.Join(rb.EvidenceRefs, ", "), strings.Join(sc.EvidenceRefs, ", ")))
	add("status", validStatus(rb.Status) && validStatus(sc.Status), joinNonEmpty(rb.Status, sc.Status))

	return report
}

func DoctorWithLiveProbes(ctx context.Context, b Bundle, now time.Time) DoctorReport {
	report := Doctor(b)
	if len(b.RuntimeProbes) == 0 {
		report.Status = "fail"
		report.Checks = append(report.Checks, Check{
			Name:   "live:coverage",
			Status: "fail",
			Detail: "live verification requested but no runtime probes are configured",
		})
		return report
	}
	for i, probe := range b.RuntimeProbes {
		check := runProbe(ctx, probe, i, now)
		if check.Status != "ok" {
			report.Status = "fail"
		}
		report.Checks = append(report.Checks, check)
	}
	return report
}

// ProbeResults runs every runtime probe in the bundle concurrently and returns
// a map keyed by probe id (falling back to a positional "<kind>-N" id when a
// probe has no explicit id). It is the reachability read behind projections that
// treat hosts as topology nodes: load a bundle once, fan the probes out, and let
// the caller decide status per probe or in aggregate. Each probe still enforces
// its own timeout, so a single unreachable host cannot stall the others.
func ProbeResults(ctx context.Context, b Bundle, now time.Time) map[string]Check {
	results := make(map[string]Check, len(b.RuntimeProbes))
	if len(b.RuntimeProbes) == 0 {
		return results
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, probe := range b.RuntimeProbes {
		i, probe := i, probe
		wg.Add(1)
		go func() {
			defer wg.Done()
			check := runProbe(ctx, probe, i, now)
			mu.Lock()
			results[probeID(probe, i)] = check
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

func runProbe(ctx context.Context, probe RuntimeProbe, idx int, now time.Time) Check {
	_ = now
	id := probeID(probe, idx)
	if strings.TrimSpace(probe.Kind) == "" || strings.TrimSpace(probe.Target) == "" {
		return Check{Name: "live:" + id, Status: "fail", Detail: "probe kind and target required"}
	}
	timeout := probeTimeout(probe)
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	switch strings.ToLower(strings.TrimSpace(probe.Kind)) {
	case "http":
		return runHTTPProbe(probeCtx, probe, id, start)
	case "tcp":
		return runTCPProbe(probeCtx, probe, id, start)
	case "file":
		return runFileProbe(probe, id, start)
	case "env":
		return runEnvProbe(probe, id, start)
	default:
		return Check{Name: "live:" + id, Status: "fail", Detail: "unsupported probe kind " + probe.Kind}
	}
}

func runHTTPProbe(ctx context.Context, probe RuntimeProbe, id string, start time.Time) Check {
	target := strings.TrimSpace(probe.Target)
	if _, err := url.ParseRequestURI(target); err != nil {
		return Check{Name: "live:" + id, Status: "fail", Detail: "invalid http target " + target}
	}
	method := strings.ToUpper(strings.TrimSpace(probe.Method))
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return Check{Name: "live:" + id, Status: "fail", Detail: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Check{Name: "live:" + id, Status: "fail", Detail: err.Error()}
	}
	defer resp.Body.Close()

	expected := probe.ExpectedStatus
	ok := false
	if expected > 0 {
		ok = resp.StatusCode == expected
	} else {
		ok = resp.StatusCode >= 200 && resp.StatusCode < 400
	}
	return probeCheck(id, ok, fmt.Sprintf("http %s status=%d elapsed_ms=%d", target, resp.StatusCode, elapsedMS(start)))
}

func runTCPProbe(ctx context.Context, probe RuntimeProbe, id string, start time.Time) Check {
	target := strings.TrimSpace(probe.Target)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return Check{Name: "live:" + id, Status: "fail", Detail: err.Error()}
	}
	_ = conn.Close()
	return probeCheck(id, true, fmt.Sprintf("tcp %s reachable elapsed_ms=%d", target, elapsedMS(start)))
}

func runFileProbe(probe RuntimeProbe, id string, start time.Time) Check {
	target := expandUser(strings.TrimSpace(probe.Target))
	info, err := os.Stat(target)
	if err != nil {
		return Check{Name: "live:" + id, Status: "fail", Detail: "file missing " + target}
	}
	if info.IsDir() {
		return Check{Name: "live:" + id, Status: "fail", Detail: "file target is directory " + target}
	}
	return probeCheck(id, true, fmt.Sprintf("file %s exists bytes=%d elapsed_ms=%d", target, info.Size(), elapsedMS(start)))
}

func runEnvProbe(probe RuntimeProbe, id string, start time.Time) Check {
	target := strings.TrimSpace(probe.Target)
	if target == "" {
		return Check{Name: "live:" + id, Status: "fail", Detail: "env target required"}
	}
	_, ok := os.LookupEnv(target)
	return probeCheck(id, ok, fmt.Sprintf("env %s present=%v elapsed_ms=%d", target, ok, elapsedMS(start)))
}

func probeCheck(id string, ok bool, detail string) Check {
	status := "ok"
	if !ok {
		status = "fail"
	}
	return Check{Name: "live:" + id, Status: status, Detail: detail}
}

func probeID(probe RuntimeProbe, idx int) string {
	if strings.TrimSpace(probe.ID) != "" {
		return strings.TrimSpace(probe.ID)
	}
	return fmt.Sprintf("%s-%d", strings.ToLower(strings.TrimSpace(probe.Kind)), idx+1)
}

func probeTimeout(probe RuntimeProbe) time.Duration {
	if probe.TimeoutMS > 0 {
		return time.Duration(probe.TimeoutMS) * time.Millisecond
	}
	return 2 * time.Second
}

func elapsedMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

func expandUser(path string) string {
	if strings.HasPrefix(path, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func validStatus(s string) bool {
	for _, allowed := range statuses {
		if s == allowed {
			return true
		}
	}
	return false
}

func joinNonEmpty(vals ...string) string {
	var out []string
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, " / ")
}
