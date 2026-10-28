// Package authority imports external runtime/source authority rows into the spine memory
// family. It is an importer only: it emits mem entities and candidates, and the caller
// decides capture/adjudication through the normal mem store.
package authority

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"compound/internal/candidates"
	"compound/internal/mem"
)

// DefaultRegistry returns configured authority registry path. Public core has no private
// default; callers pass a path or set COMPOUND_AUTHORITY_REGISTRY.
func DefaultRegistry() string {
	return strings.TrimSpace(os.Getenv("COMPOUND_AUTHORITY_REGISTRY"))
}

type registryFile struct {
	Production map[string]projectRec `yaml:"production"`
}

type projectRec struct {
	Class           string `yaml:"class"`
	SourceAuthority struct {
		Host string `yaml:"host"`
		Repo string `yaml:"repo"`
	} `yaml:"source_authority"`
	Runtime struct {
		Host      string `yaml:"host"`
		Authority string `yaml:"authority"`
	} `yaml:"runtime"`
	DataAuthority struct {
		Database        string `yaml:"database"`
		PrimaryDatabase string `yaml:"primary_database"`
	} `yaml:"data_authority"`
	AWS struct {
		Profile string `yaml:"profile"`
	} `yaml:"aws"`
	URLs struct {
		App string `yaml:"app"`
	} `yaml:"urls"`
}

// Import parses a project registry into spine-native entities and candidates.
// Deterministic authority rows carry trusted_deterministic_import as promotion basis,
// but still flow through mem capture/adjudication; the importer never writes canon.
func Import(path string) (entities []mem.Entity, facts []mem.Candidate, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var rf registryFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, nil, err
	}
	src := "authority-registry:" + filepath.Base(path)
	for slug, p := range rf.Production {
		kind := "project"
		if strings.EqualFold(p.Class, "product") {
			kind = "product"
		}
		entities = append(entities, mem.Entity{
			Slug:       slug,
			Kind:       kind,
			Visibility: "operational",
		})
		add := func(pred, val string) {
			val = strings.TrimSpace(val)
			if val == "" {
				return
			}
			facts = append(facts, mem.Candidate{
				Entity:         slug,
				Predicate:      pred,
				Object:         val,
				Confidence:     1.0,
				Derivation:     "deterministic",
				ClaimType:      "status",
				Qualifiers:     map[string]string{"domain": pred},
				PromotionBasis: candidates.BasisTrustedDeterministic,
				SourceTraceRef: src,
				SnippetHash:    src + ":" + slug + ":" + pred,
				Visibility:     "operational",
			})
		}
		runtime := firstNonEmpty(p.Runtime.Host, p.Runtime.Authority)
		db := firstNonEmpty(p.DataAuthority.Database, p.DataAuthority.PrimaryDatabase)
		add("source_repo", p.SourceAuthority.Repo)
		add("source_host", p.SourceAuthority.Host)
		add("deployed_on", runtime)
		add("database", db)
		add("cloud_profile", p.AWS.Profile)
		add("domain", p.URLs.App)
	}
	return entities, facts, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
