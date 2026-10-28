package tooltopology

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// LoadMCPorterCatalog adapts `mcporter list --json` output into the generic
// ToolTopology contract. It observes the catalog only; it never invokes MCP
// tools or mutates mcporter config.
func LoadMCPorterCatalog(path string, now time.Time) (Topology, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Topology{}, err
	}
	var src mcporterList
	if err := json.Unmarshal(data, &src); err != nil {
		return Topology{}, fmt.Errorf("parse mcporter catalog %s: %w", path, err)
	}
	if len(src.Servers) == 0 {
		return Topology{}, fmt.Errorf("mcporter catalog %s has no servers", path)
	}
	if now.IsZero() {
		now = time.Now()
	}
	sum := sha256.Sum256(data)
	revision := "sha256:" + hex.EncodeToString(sum[:])
	evidenceRef := "mcporter_json:" + revision[:19]
	topo := Topology{
		SchemaVersion: SchemaVersion,
		ID:            "topo_mcporter_" + revision[7:19],
		AdapterID:     "mcporter.list-json",
		SourceSystem:  "mcporter",
		ObservedAt:    now.UTC().Format(time.RFC3339),
		SourceRef: SourceRef{
			Path:        path,
			Kind:        "catalog",
			Owner:       "tool-topology",
			SourceClass: "mcporter-json",
			Revision:    revision,
		},
		EvidenceRefs: []string{evidenceRef},
	}
	sort.Slice(src.Servers, func(i, j int) bool { return src.Servers[i].Name < src.Servers[j].Name })
	for _, server := range src.Servers {
		serverName := strings.TrimSpace(server.Name)
		if serverName == "" {
			continue
		}
		serverID := "mcp:" + slug(serverName)
		status := mcporterStatus(server.Status)
		serverConn := mcporterConnection(server)
		topo.Entries = append(topo.Entries, Entry{
			ID:           serverID,
			Kind:         "mcp",
			Name:         serverName,
			Interface:    "mcp",
			Status:       status,
			Triggers:     limitTokens(normalizedTokens(serverName, server.Transport), 24),
			Capabilities: limitTokens(mcporterToolNames(server.Tools), 40),
			Constraints:  mcporterConstraints(server),
			Weight:       mcporterWeight(status, false),
			// NOT Retired. mcporter reports how a server responded to a probe, and
			// a probe is a health observation -- a stdio server that was not running
			// at that moment, a remote that timed out. Retiring on it made the graph
			// forget a capability over a transient blip, and because retirement also
			// removes the node from surfacing, nothing downstream could report the
			// loss. Degradation is carried by Health (visible) and by the low weight
			// mcporterWeight already assigns (it ranks down), which is the whole job
			// retirement was wrongly doing.
			Health:       mcporterHealth(status),
			EvidenceRefs: []string{evidenceRef},
			Connection:   serverConn,
		})
		sort.Slice(server.Tools, func(i, j int) bool { return server.Tools[i].Name < server.Tools[j].Name })
		for _, tool := range server.Tools {
			toolName := strings.TrimSpace(tool.Name)
			if toolName == "" {
				continue
			}
			topo.Entries = append(topo.Entries, Entry{
				ID:           "tool:" + slug(serverName) + "." + slug(toolName),
				Kind:         "tool",
				Name:         serverName + "." + toolName,
				Interface:    "mcp_tool",
				Status:       status,
				Triggers:     limitTokens(normalizedTokens(serverName, toolName, tool.Description), 32),
				Capabilities: limitTokens(normalizedTokens(toolName, tool.Description), 32),
				Constraints:  append(mcporterConstraints(server), "mcp_server:"+slug(serverName)),
				Weight:       mcporterWeight(status, true),
				Health:       mcporterHealth(status),
				EvidenceRefs: []string{evidenceRef},
				// A tool is not reached the way its server is. You do not dial an
				// individual tool, and you do not spawn the server yourself to use
				// one -- you go through the gateway, which is what mcporter IS. So
				// the tool's launch command is the gateway invocation, and the
				// server's endpoint is deliberately NOT inherited: copying it down
				// would tell a harness to dial an address that does not serve this
				// tool. Config paths and auth posture DO carry down, because those
				// are properties of the server the tool lives in.
				Connection: &EntryConnection{
					ConfigPaths:   serverConn.ConfigPaths,
					AuthMode:      serverConn.AuthMode,
					LaunchCommand: "mcporter call " + serverName + "." + toolName,
				},
			})
		}
	}
	if len(topo.Entries) == 0 {
		return Topology{}, fmt.Errorf("mcporter catalog %s produced no topology entries", path)
	}
	return topo, nil
}

type mcporterList struct {
	Mode    string           `json:"mode"`
	Counts  map[string]int   `json:"counts"`
	Servers []mcporterServer `json:"servers"`
}

type mcporterServer struct {
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	Transport   string           `json:"transport"`
	Source      mcporterSource   `json:"source"`
	Sources     []mcporterSource `json:"sources"`
	Tools       []mcporterTool   `json:"tools"`
	Issue       map[string]any   `json:"issue"`
	AuthCommand string           `json:"authCommand"`
	Error       string           `json:"error"`
}

type mcporterSource struct {
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	ImportKind string `json:"importKind"`
}

type mcporterTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// mcporterConnection projects one catalog server onto the access facts a
// consumer needs to actually reach it.
//
// mcporter reports transport as ONE compound string -- "HTTP https://host/mcp"
// or "STDIO npx -y pkg" -- where the first field is the class and the remainder
// is the address or the command. The previous import kept only the first field
// (as a "transport:http" constraint) and dropped the remainder, so a harness was
// told an MCP server existed, and told what protocol it spoke, and never told
// where it was. That is the exact shape of "named but unreachable": the fact was
// read from the source, then discarded one step before it became useful.
func mcporterConnection(server mcporterServer) *EntryConnection {
	conn := &EntryConnection{
		ConfigPaths: mcporterConfigPaths(server),
		AuthMode:    mcporterAuthMode(server),
	}
	kind, address := splitTransport(server.Transport)
	switch {
	case address == "":
		// A class with no address teaches nothing; record no address rather than
		// echoing the class into a field that is supposed to hold a location.
	case isDialable(kind, address):
		conn.Endpoint = address
	default:
		conn.LaunchCommand = address
	}
	return conn
}

// splitTransport cuts mcporter's compound transport string into class and
// remainder. It splits on the FIRST whitespace run only: stdio commands contain
// spaces (arguments, and paths with spaces like "/Applications/Open Design.app/..."),
// so anything greedier truncates the command it is supposed to preserve.
func splitTransport(transport string) (kind, address string) {
	trimmed := strings.TrimSpace(transport)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.SplitN(trimmed, " ", 2)
	kind = strings.ToUpper(strings.TrimSpace(parts[0]))
	if len(parts) == 2 {
		address = strings.TrimSpace(parts[1])
	}
	return kind, address
}

// isDialable decides endpoint-vs-launch-command. It trusts the declared class
// first and falls back to the address shape for classes this adapter has not
// seen, so an mcporter release that adds a transport name does not silently
// reclassify a URL as a command (or a command as something to dial).
func isDialable(kind, address string) bool {
	switch kind {
	case "HTTP", "HTTPS", "SSE", "WS", "WSS":
		return true
	case "STDIO":
		return false
	}
	return strings.Contains(address, "://")
}

// mcporterConfigPaths collects where this server is DEFINED, deduped and order
// preserved. Answering "where is it configured" is what makes a broken server
// fixable without a filesystem hunt.
func mcporterConfigPaths(server mcporterServer) []string {
	var paths []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	add(server.Source.Path)
	for _, source := range server.Sources {
		add(source.Path)
	}
	return paths
}

// mcporterAuthMode reports auth posture ONLY where the catalog gives evidence of
// it. An unknown posture stays empty rather than defaulting to "none": a
// confident wrong "none" sends an agent to a server without the credential it
// needed, which is worse than an admitted gap.
func mcporterAuthMode(server mcporterServer) string {
	if strings.TrimSpace(server.AuthCommand) != "" {
		return "command"
	}
	if mcporterStatus(server.Status) == "auth_required" {
		return "required"
	}
	return ""
}

func mcporterStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok":
		return "on_demand"
	case "auth":
		return "auth_required"
	case "offline":
		return "offline"
	case "error":
		return "error"
	default:
		return firstNonEmpty(strings.ToLower(strings.TrimSpace(status)), "unknown")
	}
}

// mcporterHealth reports the observed operational state, empty when the server
// is fine. "on_demand" is mcporter's healthy state for a server that is started
// when needed -- it is the NORMAL condition for stdio servers, not a degradation,
// which is precisely why treating anything-but-on_demand as retirement was wrong
// in both directions.
func mcporterHealth(status string) string {
	if status == "on_demand" {
		return ""
	}
	return status
}

func mcporterWeight(status string, tool bool) float64 {
	if status != "on_demand" {
		return 0.1
	}
	if tool {
		return 1.2
	}
	return 1.0
}

func mcporterToolNames(tools []mcporterTool) []string {
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return limitTokens(normalizedTokens(names...), 40)
}

func mcporterConstraints(server mcporterServer) []string {
	values := []string{"status:" + mcporterStatus(server.Status)}
	if fields := strings.Fields(server.Transport); len(fields) > 0 {
		values = append(values, "transport:"+strings.ToLower(fields[0]))
	}
	if server.Source.Kind != "" {
		values = append(values, "source:"+server.Source.Kind)
	}
	for _, source := range server.Sources {
		if source.ImportKind != "" {
			values = append(values, "import:"+source.ImportKind)
		}
	}
	if server.AuthCommand != "" {
		values = append(values, "auth_required")
	}
	return uniqueStrings(values)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func limitTokens(values []string, max int) []string {
	if max > 0 && len(values) > max {
		return values[:max]
	}
	return values
}
