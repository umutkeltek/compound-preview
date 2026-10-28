// self_entity_config.go owns the resolved self-entity set both
// internal/contract.selfModelClaim and this package's digestSelfModelClaim
// classify a memory claim's Entity against. "operator" is the one
// self-entity every home carries whether or not it configures anything;
// COMPOUND_HOME/config/identity.yaml lets a home EXTEND that set with
// additional aliases (e.g. a nickname or handle the operator's memory
// corpus also tags claims with), following the same
// filepath.Join(homePath, "config", "<name>.yaml") + yaml.Unmarshal idiom
// cmd/compound/memory_engine_config.go already uses for
// config/memory-engine.yaml. Kept fail-safe on purpose: a missing or
// malformed config never blocks pack/digest compilation — it just falls
// back to the unconfigured default set.
package mem

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultSelfEntity is the one self-model entity every home recognizes,
// configured or not.
const DefaultSelfEntity = "operator"

// selfEntityConfig is the on-disk shape at COMPOUND_HOME/config/identity.yaml.
type selfEntityConfig struct {
	SelfEntities []string `yaml:"self_entities"`
}

// SelfEntityConfigPath returns the identity config path under homePath.
func SelfEntityConfigPath(homePath string) string {
	return filepath.Join(homePath, "config", "identity.yaml")
}

// ResolveSelfEntitySet returns the normalized self-entity membership set for
// homePath, keyed by the same normalizer callers apply to claim.Entity
// (digestMemoryKey / internal/contract.memoryKey — lowercase, last ':'
// segment, [a-z0-9] only) so lookups compose directly:
// set[normalizedEntity]. The returned set always contains DefaultSelfEntity.
// Fail-safe: an absent file, an unreadable file, or malformed YAML all fall
// back to the default set with NO error returned — self-entity resolution
// must never break pack or digest compilation.
func ResolveSelfEntitySet(homePath string) map[string]bool {
	set := defaultSelfEntitySet()
	homePath = strings.TrimSpace(homePath)
	if homePath == "" {
		return set
	}
	data, err := os.ReadFile(SelfEntityConfigPath(homePath))
	if err != nil {
		return set
	}
	var cfg selfEntityConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return defaultSelfEntitySet()
	}
	for _, entity := range cfg.SelfEntities {
		if key := digestMemoryKey(entity); key != "" {
			set[key] = true
		}
	}
	return set
}

func defaultSelfEntitySet() map[string]bool {
	return map[string]bool{DefaultSelfEntity: true}
}
