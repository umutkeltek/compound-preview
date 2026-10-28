package capgraph

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// ConnectionSurfaceSchemaVersion tags the connection-surface sidecar records.
// The sidecar lives UNDER the existing compound://registries/capability-graph
// authority (no new registry ref string); this constant is a record marker only.
const ConnectionSurfaceSchemaVersion = "compound.capability_connection_surface.v0.1"

// ConnectionSurface is a typed "how do I reach this node" fact for a capability
// graph node: where its config lives, an optional endpoint/auth mode, the
// settings schema it conforms to, who asserts it, and the source revision the
// facts were read at. Endpoint/AuthMode/SettingsSchemaRef are optional free
// strings (empty OK) -- a harness has config paths but no endpoint; a
// gateway-class node gets an endpoint from a future import. Persisted as an
// append-only sidecar (registries/capability-connection-surfaces.jsonl),
// last-write-wins by NodeID on load, mirroring nodes/edges.
type ConnectionSurface struct {
	NodeID      string   `json:"node_id"`
	ConfigPaths []string `json:"config_paths,omitempty"`
	Endpoint    string   `json:"endpoint,omitempty"`
	// LaunchCommand is how you reach a capability that has no address because it
	// does not exist until you start it -- a stdio MCP server, a CLI-backed tool.
	// It is deliberately NOT folded into Endpoint: an endpoint is somewhere you
	// connect, a launch command is something you run, and a consumer that cannot
	// tell them apart will try to dial a command string. Keeping them separate is
	// what lets a harness pick the right verb without guessing from syntax.
	LaunchCommand     string `json:"launch_command,omitempty"`
	AuthMode          string `json:"auth_mode,omitempty"`
	SettingsSchemaRef string `json:"settings_schema_ref,omitempty"`
	AssertingOwner    string `json:"asserting_owner,omitempty"`
	SourceRevision    string `json:"source_revision,omitempty"`
	// CredentialHandle answers "and what unlocks it" -- an address into whichever
	// store already custodies the credential, never the credential itself.
	// Embedded so the fields flatten into the same record: a surface that tells
	// you where to connect but not what authenticates you is half an answer, and
	// the half that gets worked around by reading a wrapper script.
	CredentialHandle
}

func (s *Store) connectionSurfacesLog() string {
	return filepath.Join(s.home, "registries", "capability-connection-surfaces.jsonl")
}

// AddConnectionSurface upserts a connection-surface record (append-only; last
// write wins by NodeID on load). It returns true when a new record line was
// appended and false when an identical record already exists -- so a re-import
// with unchanged sources is a no-op at the file level (DeepEqual dedupe,
// mirroring Store.Add for nodes).
func (s *Store) AddConnectionSurface(cs ConnectionSurface) (added bool, retErr error) {
	if strings.TrimSpace(cs.NodeID) == "" {
		return false, fmt.Errorf("connection surface needs a node id")
	}
	// Validate at the write door, not the read door. This log is append-only and
	// is joined into orient offers, so a credential value accepted here reaches
	// agent context and cannot be unwritten -- there is no later place to catch it.
	if err := ValidateCredentialHandle(cs.CredentialHandle); err != nil {
		return false, fmt.Errorf("connection surface %s: %w", cs.NodeID, err)
	}
	retErr = s.withGraphWriteLock(func() error {
		existing, err := s.loadConnectionSurfaces()
		if err != nil {
			return err
		}
		for _, prev := range existing {
			if prev.NodeID == cs.NodeID && reflect.DeepEqual(prev, cs) {
				return nil
			}
		}
		if err := appendJSONL(s.connectionSurfacesLog(), &cs); err != nil {
			return err
		}
		added = true
		return nil
	})
	return added, retErr
}

// SetConnectionSurfaceCredential attaches (or replaces) a CredentialHandle on an
// existing connection surface in place, so a surface minted by an import door
// can be bound to its custody without re-importing. The handle is validated at
// the write door (pointer-not-value). Append-only last-write-wins on load, so
// this appends a new record line carrying the updated handle.
func (s *Store) SetConnectionSurfaceCredential(nodeID string, h CredentialHandle) (bool, error) {
	if strings.TrimSpace(nodeID) == "" {
		return false, fmt.Errorf("connection surface needs a node id")
	}
	if err := ValidateCredentialHandle(h); err != nil {
		return false, fmt.Errorf("connection surface %s: %w", nodeID, err)
	}
	var updated bool
	err := s.withGraphWriteLock(func() error {
		existing, err := s.loadConnectionSurfaces()
		if err != nil {
			return err
		}
		var current *ConnectionSurface
		for i := range existing {
			if existing[i].NodeID == nodeID {
				current = &existing[i]
				break
			}
		}
		if current == nil {
			return fmt.Errorf("no connection surface for node %q (create it first)", nodeID)
		}
		next := *current
		next.CredentialHandle = h
		if current.CredentialHandle == h {
			return nil // already bound identically — no-op
		}
		if err := appendJSONL(s.connectionSurfacesLog(), &next); err != nil {
			return err
		}
		updated = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return updated, nil
}

// loadConnectionSurfaces reads the sidecar, last-write-wins by NodeID with
// insertion order preserved (mirrors loadNodes). A missing file is empty, never
// an error, so read-only consumers on a home without the sidecar stay clean.
func (s *Store) loadConnectionSurfaces() ([]ConnectionSurface, error) {
	byID := map[string]ConnectionSurface{}
	var order []string
	err := readJSONL(s.connectionSurfacesLog(), func(line []byte) error {
		var cs ConnectionSurface
		if err := json.Unmarshal(line, &cs); err != nil {
			return nil // skip a torn/malformed line rather than bricking the sidecar
		}
		if strings.TrimSpace(cs.NodeID) == "" {
			return nil
		}
		if _, seen := byID[cs.NodeID]; !seen {
			order = append(order, cs.NodeID)
		}
		byID[cs.NodeID] = cs
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]ConnectionSurface, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}

// ConnectionSurfaces returns the loaded connection-surface sidecar records
// (last-wins by NodeID). Read-only front door for import doors and tests.
func (s *Store) ConnectionSurfaces() ([]ConnectionSurface, error) {
	return s.loadConnectionSurfaces()
}

// connectionSurfaceMap keys the loaded sidecar by node id for the render join.
// It returns nil (not an empty map) when there are no records so the offer join
// stays byte-identical to base behavior on a home with no connection surfaces.
func (s *Store) connectionSurfaceMap() (map[string]ConnectionSurface, error) {
	records, err := s.loadConnectionSurfaces()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	out := make(map[string]ConnectionSurface, len(records))
	for _, cs := range records {
		out[cs.NodeID] = cs
	}
	return out, nil
}

// connectionSurfaceConstraints renders a ConnectionSurface as kind/ref
// SurfacingConstraints so "how do I reach this" flows through the SAME offer
// join as every other constraint entry. Empty optional fields are skipped.
func connectionSurfaceConstraints(cs ConnectionSurface) []SurfacingConstraint {
	var out []SurfacingConstraint
	add := func(kind, ref, detail string) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return
		}
		out = append(out, SurfacingConstraint{Kind: kind, Ref: ref, Detail: detail, Confidence: 1})
	}
	for _, path := range cs.ConfigPaths {
		add("config_path", path, "connection surface config path")
	}
	add("endpoint", cs.Endpoint, "connection surface endpoint")
	add("launch_command", cs.LaunchCommand, "command that starts this capability (run it; do not dial it)")
	add("auth_mode", cs.AuthMode, "connection surface auth mode")
	// Credential facts are addresses and mechanism, never values -- see
	// ValidateCredentialHandle, which refuses value-shaped input at the write
	// door so nothing value-shaped can reach this render.
	add("credential_ref", cs.CredentialHandle.Ref, "credential address (pointer, never a value)")
	add("credential_custodian", cs.CredentialHandle.Custodian, "system that holds and rotates this credential")
	add("credential_env_name", cs.CredentialHandle.EnvName, "environment variable the broker sets in the child")
	add("broker_cmd", cs.CredentialHandle.Broker, "command that injects the credential into a child process")
	add("settings_schema", cs.SettingsSchemaRef, "connection surface settings schema")
	add("asserting_owner", cs.AssertingOwner, "connection surface asserting owner")
	add("source_revision", cs.SourceRevision, "connection surface source revision")
	return out
}
