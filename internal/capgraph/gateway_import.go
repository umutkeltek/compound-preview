package capgraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// GatewayNodeKind is the capgraph node kind for gateway/provider connectivity
// nodes minted by the import-gateway door. Gateway nodes carry ConnectionSurface
// connectivity facts (endpoint, auth mode, schema, revision) as EVIDENCE; they
// have no triggers and a low weight, so they never surface by task-match alone.
const GatewayNodeKind = "gateway"

// gatewayNodeWeight is the low base weight for gateway nodes. Combined with the
// absence of triggers it keeps them out of task-conditioned surfacing entirely:
// surfaceTriggerScore returns 0 (no matched tokens) so their score is 0 and the
// surface loop drops them.
const gatewayNodeWeight = 0.1

// ProviderRegistrySchemaPrefix is the accepted schema family for the switchback
// provider registry. A file whose "schema" field lacks this prefix is rejected
// (fail-closed) rather than minting connectivity from an unknown shape.
const ProviderRegistrySchemaPrefix = "switchback/provider-registry@"

// GatewayProvider is one provider descriptor read from a switchback provider
// registry: CONNECTIVITY METADATA ONLY (id, name, base_url, auth_scheme). No
// pricing, cost class, tier, health, or routing semantics -- those belong to the
// model-router authority (compound://registries/model-router), a disjoint object
// family this door must not duplicate or fight.
type GatewayProvider struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	AuthScheme string `json:"auth_scheme"`
}

// ProviderRegistry is the parsed connectivity view of a switchback provider
// registry file. SourceRevision is sha256 of the RAW FILE BYTES so a changed
// file yields a new revision (last-write-wins on the connection-surface sidecar,
// mirroring the import-connections revision contract).
type ProviderRegistry struct {
	Schema         string
	Providers      []GatewayProvider
	SourceRevision string
}

// LoadProviderRegistry reads and validates a switchback provider-registry JSON
// file for the import-gateway door. It parses connectivity descriptors ONLY and
// fails closed -- returns an error and nothing to persist -- on an unreadable
// file, a non-JSON body, or a missing/unknown schema field. It never reads env
// values or secrets (the registry carries descriptors only).
func LoadProviderRegistry(path string) (ProviderRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ProviderRegistry{}, fmt.Errorf("read provider registry %s: %w", path, err)
	}
	var raw struct {
		Schema    string            `json:"schema"`
		Providers []GatewayProvider `json:"providers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ProviderRegistry{}, fmt.Errorf("parse provider registry %s: %w", path, err)
	}
	schema := strings.TrimSpace(raw.Schema)
	if schema == "" {
		return ProviderRegistry{}, fmt.Errorf("provider registry %s missing \"schema\" field", path)
	}
	if !strings.HasPrefix(schema, ProviderRegistrySchemaPrefix) {
		return ProviderRegistry{}, fmt.Errorf("provider registry %s has unknown schema %q (want %s*)", path, schema, ProviderRegistrySchemaPrefix)
	}
	sum := sha256.Sum256(data)
	return ProviderRegistry{
		Schema:         schema,
		Providers:      raw.Providers,
		SourceRevision: "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}

// GatewayRootNodeID is the deterministic id of the gateway root node: gateway:<name>.
func GatewayRootNodeID(gatewayName string) string {
	return "gateway:" + strings.TrimSpace(gatewayName)
}

// GatewayProviderNodeID is the deterministic id of a provider node:
// gateway:<name>/provider/<provider_id>.
func GatewayProviderNodeID(gatewayName, providerID string) string {
	return "gateway:" + strings.TrimSpace(gatewayName) + "/provider/" + strings.TrimSpace(providerID)
}

// GatewayNode constructs a gateway-class capgraph node. It centralizes the
// invariant: kind "gateway", NO triggers, low weight -- so the node carries
// connectivity evidence without ever surfacing by task-match alone.
func GatewayNode(id, name string) Node {
	return Node{ID: id, Kind: GatewayNodeKind, Name: name, Weight: gatewayNodeWeight}
}
