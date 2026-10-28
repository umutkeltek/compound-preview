package runtimebinding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"compound/internal/projectcontract"
)

type projectContract struct {
	Project struct {
		Slug     string `yaml:"slug"`
		Name     string `yaml:"name"`
		Class    string `yaml:"class"`
		Status   string `yaml:"status"`
		Priority string `yaml:"priority"`
		Owner    string `yaml:"owner"`
		Aliases  []any  `yaml:"aliases"`
	} `yaml:"project"`
	Source struct {
		Authority struct {
			Host string `yaml:"host"`
			Path string `yaml:"path"`
			Repo string `yaml:"repo"`
		} `yaml:"authority"`
	} `yaml:"source"`
	Runtime struct {
		Authority    string   `yaml:"authority"`
		ServiceNames []string `yaml:"service_names"`
		RuntimePaths []string `yaml:"runtime_paths"`
		URLs         []string `yaml:"urls"`
		Healthchecks []string `yaml:"healthchecks"`
		Deployment   struct {
			Method   string `yaml:"method"`
			Command  string `yaml:"command"`
			Rollback string `yaml:"rollback"`
		} `yaml:"deployment"`
	} `yaml:"runtime"`
	Data struct {
		Authority       string                   `yaml:"authority"`
		Classification  string                   `yaml:"classification"`
		NASPaths        []string                 `yaml:"nas_paths"`
		LocalPaths      []string                 `yaml:"local_paths"`
		Databases       projectcontract.NameList `yaml:"databases"`
		ExternalSources []string                 `yaml:"external_sources"`
	} `yaml:"data"`
}

// LoadProjectContract adapts an infra-os-style project.yaml into Compound's
// RuntimeBinding/ServiceContract bundle. It reads source-in-place and returns a
// generic contract object; it never makes infra-os a runtime authority.
func LoadProjectContract(path string) (Bundle, error) {
	var pc projectContract
	data, err := os.ReadFile(path)
	if err != nil {
		return Bundle{}, fmt.Errorf("read project contract %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &pc); err != nil {
		return Bundle{}, fmt.Errorf("parse project contract %s: %w", path, err)
	}
	return ProjectContractBundle(pc, filepath.Base(path))
}

func ProjectContractBundle(pc projectContract, sourceRef string) (Bundle, error) {
	slug := strings.TrimSpace(pc.Project.Slug)
	if slug == "" {
		return Bundle{}, fmt.Errorf("project.slug is required")
	}
	serviceName := firstNonEmpty(first(pc.Runtime.ServiceNames), slug)
	serviceID := "service:" + serviceName
	owner := "project:" + slug
	status := projectStatus(pc.Project.Status)
	risk := projectRisk(pc.Project.Priority, pc.Project.Class, pc.Project.Status)
	evidence := []string{"project-contract:" + firstNonEmpty(sourceRef, slug)}

	runtimeAuthority := strings.TrimSpace(pc.Runtime.Authority)
	host := runtimeAuthority
	if host == "" || host == "none" || host == "unknown" {
		host = strings.TrimSpace(pc.Source.Authority.Host)
	}
	deploy := strings.TrimSpace(pc.Runtime.Deployment.Method)
	if deploy == "" {
		deploy = "none"
	}

	dataBindings := prefixAll("database:", pc.Data.Databases)
	dataBindings = append(dataBindings, prefixAll("nas:", pc.Data.NASPaths)...)
	dataBindings = append(dataBindings, prefixAll("local:", pc.Data.LocalPaths)...)
	dataBindings = append(dataBindings, prefixAll("external:", pc.Data.ExternalSources)...)
	if pc.Data.Authority != "" {
		dataBindings = append([]string{"authority:" + pc.Data.Authority}, dataBindings...)
	}

	interfaceRefs := prefixAll("url:", pc.Runtime.URLs)
	interfaceRefs = append(interfaceRefs, prefixAll("health:", pc.Runtime.Healthchecks)...)

	return Bundle{
		RuntimeBinding: RuntimeBinding{
			ServiceID:         serviceID,
			OwnerEntity:       owner,
			HostBinding:       host,
			DeploymentBinding: deploy,
			DataBindings:      dataBindings,
			HealthCheckRefs:   pc.Runtime.Healthchecks,
			RiskTier:          risk,
			EvidenceRefs:      evidence,
			Status:            status,
		},
		ServiceContract: ServiceContract{
			ServiceID:     serviceID,
			InterfaceRefs: interfaceRefs,
			Dependencies:  dependencyRefs(pc.Runtime.ServiceNames, pc.Data.Databases),
			OwnerEntity:   owner,
			RiskTier:      risk,
			EvidenceRefs:  evidence,
			Status:        status,
		},
	}, nil
}

func projectStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active":
		return StatusActive
	case "paused", "active_needs_contract", "unclassified":
		return StatusStale
	case "archive", "archived":
		return StatusRetired
	default:
		return StatusCandidate
	}
}

func projectRisk(priority, class, status string) string {
	p := strings.ToLower(strings.TrimSpace(priority))
	c := strings.ToLower(strings.TrimSpace(class))
	s := strings.ToLower(strings.TrimSpace(status))
	if p == "p0" || p == "p1" || (c == "product" && s == "active") {
		return "tier2"
	}
	return "tier1"
}

func dependencyRefs(services, databases []string) []string {
	var deps []string
	if len(services) > 1 {
		for _, s := range services[1:] {
			if strings.TrimSpace(s) != "" {
				deps = append(deps, "service:"+s)
			}
		}
	}
	deps = append(deps, prefixAll("database:", databases)...)
	return deps
}

func prefixAll(prefix string, vals []string) []string {
	var out []string
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, prefix+v)
		}
	}
	return out
}

func first(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[0])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
