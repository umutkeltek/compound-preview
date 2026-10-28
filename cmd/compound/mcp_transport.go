// mcp_transport.go owns the local stdio JSON-RPC MCP transport for compound.
// It is a THIN framing layer: it routes JSON-RPC 2.0 envelopes over stdio and
// dispatches at most two prefixed tools back onto the same governed libraries
// the CLI already uses.
//
// Boundary:
//   - No second memory authority: the transport never loads a memory store or
//     adjudicates a candidate. memory-derived state is read solely through the
//     orient runtime materializer (the existing `compound orient` path), so a
//     single memory owner stays the rule.
//   - No shell injection: arguments are decoded into typed Go structs with
//     `DisallowUnknownFields`; values never cross an exec boundary.
//   - No auto-promotion / auto-adjudication: orient_recall returns ambient
//     recall refs the materializer already adjudicated; it never promotes.
//   - orient_recall is NOT read-only/idempotent by design: it persists sealed
//     runtime artifacts under COMPOUND_HOME (one EffectiveContract + one
//     ContextPack pair), exactly like `compound orient`. The ack surfaces the
//     deterministic paths the materializer wrote so the caller can audit
//     persistence without re-reading the home.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/home"
)

const (
	mcpFindingAckSchema = "compound.finding.record-ack.v1"
	mcpOrientAckSchema  = "compound.mcp.orient-recall-ack.v1"
	mcpServerName       = "compound-mcp-transport"
	mcpServerVersion    = "0.3.0"
	// mcpProtocolVersion is the newest handshake-era revision this server
	// supports. Initialize negotiates an exact member of
	// mcpSupportedProtocolVersions.
	mcpProtocolVersion       = "2025-06-18"
	mcpLegacyProtocolVersion = "2024-11-05"
	mcpModernProtocolVersion = "2026-07-28"
	mcpModernCacheTTLMillis  = 3600000
	mcpModernProtocolMetaKey = "io.modelcontextprotocol/protocolVersion"
	mcpModernClientCapsKey   = "io.modelcontextprotocol/clientCapabilities"
	mcpModernClientInfoKey   = "io.modelcontextprotocol/clientInfo"
	mcpMaxVersionEchoChars   = 64
	mcpFindingToolName       = "compound_finding_record"
	mcpOrientToolName        = "compound_orient_recall"
	// mcpMaxLineBytes is the request-line byte budget enforced at the framing
	// layer. It is intentionally separate from the finding-body byte budget
	// and the per-field character caps below.
	mcpMaxLineBytes = 1 << 20
	// mcpMaxFindingBodyBytes limits the persisted body payload independently
	// of both request framing and JSON Schema's character-counting maxLength.
	mcpMaxFindingBodyBytes = 64 << 10
	// mcpMaxBodyPackTokens is the body_pack token budget surfaced to callers.
	mcpMaxBodyPackTokens = 32 << 10
	mcpMaxRecallRefs     = 256
	mcpMaxRecallRefBytes = 1024
	mcpMaxSummaryBytes   = 4096
	mcpMaxEvidenceRefs   = 32
	// The *Chars caps below are per-field character caps (runes) surfaced to
	// ordinary validators via the standard JSON Schema `maxLength` keyword.
	// Runtime enforcement uses utf8.RuneCountInString. The framing and finding
	// body byte budgets remain independent.
	mcpMaxFindingTopicChars      = 1024
	mcpMaxFindingBodyChars       = 64 << 10
	mcpMaxFindingWorkIDChars     = 1024
	mcpMaxFindingProducerChars   = 256
	mcpMaxFindingSupersedesChars = 17
	mcpMaxEvidenceRefChars       = 1024
	mcpMaxOrientTaskChars        = 4096
	mcpMaxOrientHarnessChars     = 64
	mcpMaxOrientTopicChars       = 256
	mcpMaxOrientRepoKindChars    = 64
	mcpMaxOrientModeChars        = 32
	mcpMaxOrientCwdChars         = 4096
	mcpMaxClientNameChars        = 128
	mcpMaxClientVersionChars     = 128
	mcpMaxClientTitleChars       = 128
)

var (
	mcpSupportedProtocolVersions = map[string]struct{}{
		mcpProtocolVersion:       {},
		mcpLegacyProtocolVersion: {},
	}
	mcpNonBlankPattern    = regexp.MustCompile(`(?s)^\S(?:.*\S)?$`)
	mcpWorkIDPattern      = regexp.MustCompile(`^work_[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	mcpProducerPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	mcpFindingIDPattern   = regexp.MustCompile(`^find_[a-f0-9]{12}$`)
	mcpEvidenceRefPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:\S+$`)
	mcpModePattern        = regexp.MustCompile(`^(observe|plan|execute|review)$`)
	mcpErrorCodePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// toolSet is the closed alphabet of MCP-prefixed tool names this transport
// exposes. Unknown names return -32001 (toolUnavailable) so a misconfigured
// host fails closed rather than racing against a wildcard.
var toolSet = map[string]struct{}{
	mcpFindingToolName: {},
	mcpOrientToolName:  {},
}

// ioIn/ioOut are test seams for newline-delimited framing. Production wires
// os.Stdin/Stdout.
var (
	ioIn  io.Reader = os.Stdin
	ioOut io.Writer = os.Stdout
)

// jsonRPCRequest is one JSON-RPC 2.0 envelope. A nil ID means the member was
// absent (a notification); a non-nil RawMessage containing `null` is a request
// whose ID must be echoed as JSON null.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCError is one JSON-RPC 2.0 error object. Message stays bounded (no
// body bytes, no private paths, no internal stack frames) so the transport
// cannot accidentally become a leak channel.
type jsonRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// mcpInitializeResult is the result of an MCP `initialize` request.
type mcpImplementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Title   string `json:"title,omitempty"`
}

type mcpInitializeParams struct {
	ProtocolVersion string            `json:"protocolVersion"`
	Capabilities    json.RawMessage   `json:"capabilities"`
	ClientInfo      mcpImplementation `json:"clientInfo"`
	Meta            json.RawMessage   `json:"_meta,omitempty"`
}

type mcpToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type mcpServerCapabilities struct {
	Tools mcpToolsCapability `json:"tools"`
}

type mcpInitializeResult struct {
	ProtocolVersion string                `json:"protocolVersion"`
	Capabilities    mcpServerCapabilities `json:"capabilities"`
	ServerInfo      mcpImplementation     `json:"serverInfo"`
}

type mcpModernMetaParams struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type mcpModernResponseMeta struct {
	ServerInfo mcpImplementation `json:"io.modelcontextprotocol/serverInfo"`
}

type mcpDiscoverResult struct {
	ResultType        string                `json:"resultType"`
	SupportedVersions []string              `json:"supportedVersions"`
	Capabilities      mcpServerCapabilities `json:"capabilities"`
	TTLMillis         int                   `json:"ttlMs"`
	CacheScope        string                `json:"cacheScope"`
	Meta              mcpModernResponseMeta `json:"_meta"`
}

type mcpModernToolsListResult struct {
	ResultType string                `json:"resultType"`
	Tools      []mcpToolDescriptor   `json:"tools"`
	TTLMillis  int                   `json:"ttlMs"`
	CacheScope string                `json:"cacheScope"`
	Meta       mcpModernResponseMeta `json:"_meta"`
}

type mcpModernCompleteResult struct {
	ResultType string `json:"resultType"`
}

type mcpModernRequestContext struct {
	ProtocolVersion string
	Meta            map[string]json.RawMessage
}

type mcpUnsupportedProtocolVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

// mcpToolDescriptor is one entry in `tools/list`. The JSON Schema field is
// typed structurally; the runtime validator is the same Go function the CLI
// uses (strictDecodeArgs) and refuses unknown top-level keys + non-string
// typed fields.
type mcpToolDescriptor struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// mcpToolCallParams is the typed shape of `tools/call` params.
type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type mcpModernToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

// mcpToolCallResult uses the CallToolResult shape shared by both supported MCP
// revisions. The typed acknowledgement is encoded in content[0].text; this
// server does not advertise an outputSchema and therefore does not emit the
// newer structuredContent extension.
type mcpToolCallResult struct {
	Content []mcpToolContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

type mcpModernToolCallResult struct {
	ResultType string                `json:"resultType"`
	Content    []mcpToolContent      `json:"content"`
	IsError    bool                  `json:"isError"`
	Meta       mcpModernResponseMeta `json:"_meta"`
}

type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpFindingRecordInput is the typed input for compound_finding_record. All
// fields are required EXCEPT the optional provenance bag. The body byte cap
// prevents an unbounded caller from saturating stdio.
type mcpFindingRecordInput struct {
	Topic        string   `json:"topic"`
	Body         string   `json:"body"`
	WorkID       *string  `json:"work_id,omitempty"`
	ProducedBy   *string  `json:"produced_by,omitempty"`
	Supersedes   *string  `json:"supersedes,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// mcpOrientRecallInput is the typed input for compound_orient_recall. task is
// the recall key; harness defaults to "codex" if absent. BodyBudget is an
// estimated-token ceiling for ContextPack bodies, not a finding-body byte cap.
type mcpOrientRecallInput struct {
	Task       string  `json:"task"`
	Harness    *string `json:"harness,omitempty"`
	Cwd        *string `json:"cwd,omitempty"`
	RepoKind   *string `json:"repo_kind,omitempty"`
	Mode       *string `json:"mode,omitempty"`
	Topic      *string `json:"topic,omitempty"`
	BodyBudget int     `json:"body_budget,omitempty"`
}

// mcpFindingRecordAck mirrors the body-free exact acknowledgement the
// `compound finding record --json` CLI emits. The transport must NOT add the
// body or provenance fields — the dedicated `show` surface is the body-bearing
// retrieval, and that boundary stays here.
type mcpFindingRecordAck struct {
	Schema     string `json:"schema"`
	Status     string `json:"status"`
	ID         string `json:"id"`
	Path       string `json:"path"`
	Privacy    string `json:"privacy"`
	Visibility string `json:"visibility"`
}

// mcpOrientRecallAck is the bounded task-recall result. active_decisions and
// active_memory are the ambient recall refs from the persisted pack, bounded by
// mcpMaxRecallRefs. body_pack_present mirrors whether the materializer emitted
// a body_pack section (bounded by body_budget). persisted_artifacts records the
// deterministic paths the runtime materializer wrote under COMPOUND_HOME — this
// is the bounded receipt the caller audits because orient_recall is
// intentionally non-idempotent.
type mcpOrientRecallAck struct {
	Schema             string                      `json:"schema"`
	PackID             string                      `json:"pack_id"`
	ContractID         string                      `json:"contract_id"`
	SummaryMD          string                      `json:"summary_md,omitempty"`
	ActiveDecisions    []string                    `json:"active_decisions"`
	ActiveMemory       []string                    `json:"active_memory"`
	BodyPackPresent    bool                        `json:"body_pack_present"`
	PersistedArtifacts mcpOrientPersistedArtifacts `json:"persisted_artifacts"`
	Warnings           []string                    `json:"warnings,omitempty"`
}

type mcpOrientPersistedArtifacts struct {
	EffectiveContractPath string `json:"effective_contract_path"`
	LatestContractPath    string `json:"latest_contract_path"`
	ContextPackPath       string `json:"context_pack_path"`
}

// JSON-RPC 2.0 standard error codes (spec §5.1) plus MCP-specific extension
// codes for bounded domain errors.
const (
	rpcParseError                 = -32700
	rpcInvalidRequest             = -32600
	rpcMethodNotFound             = -32601
	rpcInvalidParams              = -32602
	rpcInternalError              = -32603
	rpcUnsupportedProtocolVersion = -32022
	rpcToolUnavailable            = -32001
	rpcServerNotInitialized       = -32002
)

type mcpLifecycle uint8

const (
	mcpLifecycleNew mcpLifecycle = iota
	mcpLifecycleAwaitingInitialized
	mcpLifecycleReady
)

type mcpSession struct {
	lifecycle       mcpLifecycle
	protocolVersion string
}

// runMcpTransportLoop reads newline-delimited JSON-RPC 2.0 envelopes from
// ioIn and writes responses to ioOut. A read error closes the loop without
// spamming the host; a write error is fatal (the host went away).
func runMcpTransportLoop() error {
	reader := bufio.NewReaderSize(ioIn, mcpMaxLineBytes)
	writer := bufio.NewWriter(ioOut)
	session := &mcpSession{}
	for {
		line, err := readBoundedJSONLine(reader, mcpMaxLineBytes)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return writer.Flush()
			}
			// Non-EOF read error closes the loop quietly. Logging here would
			// smear the JSON-RPC channel — the MCP host owns the transport.
			return writer.Flush()
		}
		if len(bytesTrim(line)) == 0 {
			continue
		}
		resp := dispatchJSONRPC(session, line)
		if resp == nil {
			continue
		}
		if err := writeJSONLine(writer, resp); err != nil {
			return err
		}
	}
}

// readBoundedJSONLine reads ONE JSON-RPC line up to max+1 bytes; over-cap
// returns an error so an ill-behaved host cannot OOM the transport. A missing
// newline is fatal for this transport (no framing ambiguity allowed).
//
// Implementation note: bufio.Reader.ReadSlice returns ErrBufferFull when the
// internal buffer fills before a newline. That is a SIGNAL to keep reading,
// NOT the transport-level "too large" error — we keep appending until we hit
// the newline OR exceed `max`.
func readBoundedJSONLine(r *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		seg, err := r.ReadSlice('\n')
		// ReadSlice always returns the partial slice when err != nil.
		out = append(out, seg...)
		if err == nil {
			if len(out) > max {
				return nil, fmt.Errorf("json-rpc line exceeds %d bytes", max)
			}
			return out, nil
		}
		// Error path: only bubble up the transport-level cap. ErrBufferFull
		// means "the bufio slab filled before we found '\n'" — keep reading.
		if !errors.Is(err, bufio.ErrBufferFull) {
			if len(out) > max {
				return nil, fmt.Errorf("json-rpc line exceeds %d bytes", max)
			}
			return out, err
		}
		if len(out) > max {
			return nil, fmt.Errorf("json-rpc line exceeds %d bytes", max)
		}
	}
}

// dispatchJSONRPC returns nil only for a true notification (the id member is
// absent). It owns the MCP initialize -> initialized -> ready lifecycle for one
// stdio session.
func dispatchJSONRPC(session *mcpSession, raw []byte) []byte {
	var req jsonRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return encodeResponse(nil, &jsonRPCError{Code: rpcParseError, Message: "parse error"})
	}
	isNotification := req.ID == nil
	if strings.TrimSpace(req.JSONRPC) != "2.0" {
		if isNotification {
			return nil
		}
		return respondToID(req.ID, &jsonRPCError{Code: rpcInvalidRequest, Message: "invalid request"})
	}
	if strings.TrimSpace(req.Method) == "" {
		if isNotification {
			return nil
		}
		return respondToID(req.ID, &jsonRPCError{Code: rpcInvalidRequest, Message: "invalid request"})
	}
	if !isNotification && !validJSONRPCID(req.ID) {
		return encodeResponse(nil, &jsonRPCError{Code: rpcInvalidRequest, Message: "invalid request"})
	}
	if isNotification {
		switch req.Method {
		case "notifications/initialized":
			modern, metadataError := mcpModernRequest(req.Params)
			if metadataError == nil && modern == nil && session.lifecycle == mcpLifecycleAwaitingInitialized {
				session.lifecycle = mcpLifecycleReady
			}
		case "notifications/cancelled":
			// Cancellation is advisory for this synchronous two-tool server.
		}
		return nil
	}

	if req.Method == "server/discover" {
		return respondToID(req.ID, mcpHandleServerDiscover(req.Params))
	}
	modern, metadataError := mcpModernRequest(req.Params)
	if metadataError != nil {
		return respondToID(req.ID, metadataError)
	}
	if modern != nil {
		if modern.ProtocolVersion != mcpModernProtocolVersion {
			return respondToID(req.ID, mcpUnsupportedVersionError(modern.ProtocolVersion))
		}
		if err := validateMCPModernMetadata(modern.Meta); err != nil {
			return respondToID(req.ID, err)
		}
		return respondToID(req.ID, dispatchModernJSONRPC(req))
	}

	switch req.Method {
	case "initialize":
		return respondToID(req.ID, session.handleInitialize(req.Params))
	case "ping":
		return respondToID(req.ID, struct{}{})
	case "tools/list":
		if session.lifecycle != mcpLifecycleReady {
			return respondToID(req.ID, &jsonRPCError{Code: rpcServerNotInitialized, Message: "server not initialized"})
		}
		return respondToID(req.ID, mcpHandleToolsList())
	case "tools/call":
		if session.lifecycle != mcpLifecycleReady {
			return respondToID(req.ID, &jsonRPCError{Code: rpcServerNotInitialized, Message: "server not initialized"})
		}
		return respondToID(req.ID, mcpHandleToolsCall(req.Params))
	default:
		return respondToID(req.ID, &jsonRPCError{Code: rpcMethodNotFound, Message: "method not found"})
	}
}

func mcpModernRequest(rawParams json.RawMessage) (*mcpModernRequestContext, *jsonRPCError) {
	if len(rawParams) == 0 {
		return nil, nil
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(rawParams, &params); err != nil || params == nil {
		return nil, nil
	}
	rawMeta, hasMeta := params["_meta"]
	if !hasMeta {
		return nil, nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(rawMeta, &meta); err != nil || meta == nil {
		return nil, &jsonRPCError{Code: rpcInvalidParams, Message: "invalid request metadata"}
	}
	rawVersion, hasVersion := meta[mcpModernProtocolMetaKey]
	if !hasVersion {
		return nil, nil
	}
	var version string
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return nil, &jsonRPCError{Code: rpcInvalidParams, Message: "invalid request metadata"}
	}
	return &mcpModernRequestContext{ProtocolVersion: version, Meta: meta}, nil
}

func mcpUnsupportedVersionError(requested string) *jsonRPCError {
	return &jsonRPCError{
		Code:    rpcUnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data: mcpUnsupportedProtocolVersionData{
			Supported: []string{mcpModernProtocolVersion},
			Requested: boundedProtocolVersionEcho(requested),
		},
	}
}

func validateMCPModernMetadata(meta map[string]json.RawMessage) *jsonRPCError {
	capabilities, ok := meta[mcpModernClientCapsKey]
	if !ok || !validJSONObject(capabilities) {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid request metadata"}
	}
	rawClientInfo, hasClientInfo := meta[mcpModernClientInfoKey]
	if !hasClientInfo {
		return nil
	}
	var clientInfo map[string]json.RawMessage
	if err := json.Unmarshal(rawClientInfo, &clientInfo); err != nil || clientInfo == nil {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid request metadata"}
	}
	for _, field := range []struct {
		name     string
		maxChars int
	}{
		{name: "name", maxChars: mcpMaxClientNameChars},
		{name: "version", maxChars: mcpMaxClientVersionChars},
		{name: "title", maxChars: mcpMaxClientTitleChars},
	} {
		rawValue, present := clientInfo[field.name]
		if !present {
			continue
		}
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil ||
			!validBoundedNonBlank(value, field.maxChars, nil) {
			return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid request metadata"}
		}
	}
	return nil
}

func dispatchModernJSONRPC(req jsonRPCRequest) interface{} {
	switch req.Method {
	case "ping":
		if err := validateMCPModernMetaOnlyParams(req.Params); err != nil {
			return err
		}
		return mcpModernCompleteResult{ResultType: "complete"}
	case "tools/list":
		if err := validateMCPModernMetaOnlyParams(req.Params); err != nil {
			return err
		}
		return mcpModernToolsListResult{
			ResultType: "complete",
			Tools: []mcpToolDescriptor{
				mcpFindingRecordDescriptor(),
				mcpOrientRecallDescriptor(),
			},
			TTLMillis:  mcpModernCacheTTLMillis,
			CacheScope: "public",
			Meta:       mcpModernServerMeta(),
		}
	case "tools/call":
		return mcpHandleModernToolsCall(req.Params)
	default:
		return &jsonRPCError{Code: rpcMethodNotFound, Message: "method not found"}
	}
}

func validateMCPModernMetaOnlyParams(rawParams json.RawMessage) *jsonRPCError {
	var params mcpModernMetaParams
	if err := strictDecodeArgs(rawParams, &params); err != nil {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid params"}
	}
	return nil
}

func mcpHandleServerDiscover(rawParams json.RawMessage) interface{} {
	if len(rawParams) > 0 {
		var params mcpModernMetaParams
		if err := strictDecodeArgs(rawParams, &params); err != nil {
			return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid server/discover params"}
		}
	}
	return mcpDiscoverResult{
		ResultType:        "complete",
		SupportedVersions: []string{mcpModernProtocolVersion},
		Capabilities:      mcpServerCapabilities{Tools: mcpToolsCapability{}},
		TTLMillis:         mcpModernCacheTTLMillis,
		CacheScope:        "public",
		Meta:              mcpModernServerMeta(),
	}
}

func mcpModernServerMeta() mcpModernResponseMeta {
	return mcpModernResponseMeta{
		ServerInfo: mcpImplementation{Name: mcpServerName, Version: mcpServerVersion},
	}
}

func (session *mcpSession) handleInitialize(rawParams json.RawMessage) interface{} {
	if session.lifecycle != mcpLifecycleNew {
		return &jsonRPCError{Code: rpcInvalidRequest, Message: "already initialized"}
	}
	if len(rawParams) == 0 {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid initialize params"}
	}
	var params mcpInitializeParams
	if err := strictDecodeArgs(rawParams, &params); err != nil {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid initialize params"}
	}
	negotiatedVersion := params.ProtocolVersion
	if _, ok := mcpSupportedProtocolVersions[negotiatedVersion]; !ok {
		// MCP lifecycle negotiation requires the server to return a version it
		// supports when the client's proposed version is unsupported.
		negotiatedVersion = mcpProtocolVersion
	}
	if !validJSONObject(params.Capabilities) ||
		(len(params.Meta) > 0 && !validJSONObject(params.Meta)) ||
		!validBoundedNonBlank(params.ClientInfo.Name, mcpMaxClientNameChars, nil) ||
		!validBoundedNonBlank(params.ClientInfo.Version, mcpMaxClientVersionChars, nil) ||
		(params.ClientInfo.Title != "" && !validBoundedNonBlank(params.ClientInfo.Title, mcpMaxClientTitleChars, nil)) {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid initialize params"}
	}
	session.protocolVersion = negotiatedVersion
	session.lifecycle = mcpLifecycleAwaitingInitialized
	return mcpInitializeResult{
		ProtocolVersion: negotiatedVersion,
		Capabilities:    mcpServerCapabilities{Tools: mcpToolsCapability{}},
		ServerInfo:      mcpImplementation{Name: mcpServerName, Version: mcpServerVersion},
	}
}

func validJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}

func mcpHandleToolsList() interface{} {
	return map[string]interface{}{
		"tools": []mcpToolDescriptor{
			mcpFindingRecordDescriptor(),
			mcpOrientRecallDescriptor(),
		},
	}
}

func mcpHandleToolsCall(rawParams json.RawMessage) interface{} {
	if len(rawParams) == 0 {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "tools/call params required"}
	}
	var params mcpToolCallParams
	if err := strictDecodeArgs(rawParams, &params); err != nil {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid tools/call params"}
	}
	if _, ok := toolSet[params.Name]; !ok {
		return &jsonRPCError{Code: rpcToolUnavailable, Message: "tool unavailable"}
	}
	switch params.Name {
	case mcpFindingToolName:
		return mcpCallFindingRecord(params.Arguments)
	case mcpOrientToolName:
		return mcpCallOrientRecall(params.Arguments)
	default:
		return &jsonRPCError{Code: rpcToolUnavailable, Message: "tool dispatch unreachable"}
	}
}

func mcpHandleModernToolsCall(rawParams json.RawMessage) interface{} {
	if len(rawParams) == 0 {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "tools/call params required"}
	}
	var params mcpModernToolCallParams
	if err := strictDecodeArgs(rawParams, &params); err != nil {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid tools/call params"}
	}
	if _, ok := toolSet[params.Name]; !ok {
		return &jsonRPCError{Code: rpcInvalidParams, Message: "invalid tools/call params"}
	}
	var rawResult interface{}
	switch params.Name {
	case mcpFindingToolName:
		rawResult = mcpCallFindingRecord(params.Arguments)
	case mcpOrientToolName:
		rawResult = mcpCallOrientRecall(params.Arguments)
	default:
		return &jsonRPCError{Code: rpcInternalError, Message: "internal error"}
	}
	result, ok := rawResult.(mcpToolCallResult)
	if !ok {
		return &jsonRPCError{Code: rpcInternalError, Message: "internal error"}
	}
	return mcpModernToolCallResult{
		ResultType: "complete",
		Content:    result.Content,
		IsError:    result.IsError,
		Meta:       mcpModernServerMeta(),
	}
}

// ---------- Tool handlers ----------

func mcpCallFindingRecord(rawArgs json.RawMessage) interface{} {
	if len(rawArgs) == 0 {
		return toolErrorResult("missing_arguments")
	}
	var in mcpFindingRecordInput
	if err := strictDecodeArgs(rawArgs, &in); err != nil {
		return toolErrorResult("invalid_arguments")
	}
	for _, field := range []struct {
		name string
		code string
	}{
		{"work_id", "invalid_work_id"},
		{"produced_by", "invalid_produced_by"},
		{"supersedes", "invalid_supersedes"},
		{"evidence_refs", "invalid_evidence_ref"},
	} {
		if explicitJSONNull(rawArgs, field.name) {
			return toolErrorResult(field.code)
		}
	}
	if !validBoundedNonBlank(in.Topic, mcpMaxFindingTopicChars, mcpNonBlankPattern) {
		return toolErrorResult("invalid_topic")
	}
	if strings.TrimSpace(in.Body) == "" {
		return toolErrorResult("invalid_body")
	}
	if utf8.RuneCountInString(in.Body) > mcpMaxFindingBodyChars {
		return toolErrorResult("finding_body_budget_exceeded")
	}
	if len(in.Body) > mcpMaxFindingBodyBytes {
		return toolErrorResult("finding_body_budget_exceeded")
	}
	if !validOptionalString(in.WorkID, mcpMaxFindingWorkIDChars, mcpWorkIDPattern) {
		return toolErrorResult("invalid_work_id")
	}
	if !validOptionalString(in.ProducedBy, mcpMaxFindingProducerChars, mcpProducerPattern) {
		return toolErrorResult("invalid_produced_by")
	}
	if !validOptionalString(in.Supersedes, mcpMaxFindingSupersedesChars, mcpFindingIDPattern) {
		return toolErrorResult("invalid_supersedes")
	}
	if len(in.EvidenceRefs) > mcpMaxEvidenceRefs {
		return toolErrorResult("too_many_evidence_refs")
	}
	for _, ref := range in.EvidenceRefs {
		if !validBoundedNonBlank(ref, mcpMaxEvidenceRefChars, mcpEvidenceRefPattern) {
			return toolErrorResult("invalid_evidence_ref")
		}
	}
	homePath, err := home.Resolve("")
	if err != nil {
		return toolErrorResult("home_unavailable")
	}
	res, err := finding.RecordWithStatus(homePath, finding.Finding{
		Topic:        in.Topic,
		Body:         in.Body,
		WorkID:       optionalString(in.WorkID),
		ProducedBy:   optionalString(in.ProducedBy),
		Supersedes:   optionalString(in.Supersedes),
		EvidenceRefs: append([]string(nil), in.EvidenceRefs...),
	}, time.Now().UTC())
	if err != nil {
		return toolErrorResult("finding_record_failed")
	}
	ack := mcpFindingRecordAck{
		Schema:     mcpFindingAckSchema,
		Status:     res.Status,
		ID:         res.Finding.ID,
		Path:       filepath.ToSlash(filepath.Join(finding.Subdir, res.Finding.ID+".json")),
		Privacy:    firstNonEmpty(res.Finding.Privacy, finding.PrivacyPrivate),
		Visibility: firstNonEmpty(res.Finding.Visibility, finding.VisibilityOperational),
	}
	text, _ := json.Marshal(ack)
	return mcpToolCallResult{Content: []mcpToolContent{{Type: "text", Text: string(text)}}}
}

func mcpCallOrientRecall(rawArgs json.RawMessage) interface{} {
	if len(rawArgs) == 0 {
		return toolErrorResult("missing_arguments")
	}
	var in mcpOrientRecallInput
	if err := strictDecodeArgs(rawArgs, &in); err != nil {
		return toolErrorResult("invalid_arguments")
	}
	for _, field := range []struct {
		name string
		code string
	}{
		{"harness", "invalid_harness"},
		{"cwd", "invalid_cwd"},
		{"repo_kind", "invalid_repo_kind"},
		{"mode", "invalid_mode"},
		{"topic", "invalid_topic"},
		{"body_budget", "body_pack_token_budget_invalid"},
	} {
		if explicitJSONNull(rawArgs, field.name) {
			return toolErrorResult(field.code)
		}
	}
	if !validBoundedNonBlank(in.Task, mcpMaxOrientTaskChars, mcpNonBlankPattern) {
		return toolErrorResult("invalid_task")
	}
	for _, field := range []struct {
		value   *string
		max     int
		pattern *regexp.Regexp
		code    string
	}{
		{in.Harness, mcpMaxOrientHarnessChars, nil, "invalid_harness"},
		{in.Cwd, mcpMaxOrientCwdChars, nil, "invalid_cwd"},
		{in.RepoKind, mcpMaxOrientRepoKindChars, nil, "invalid_repo_kind"},
		{in.Mode, mcpMaxOrientModeChars, mcpModePattern, "invalid_mode"},
		{in.Topic, mcpMaxOrientTopicChars, nil, "invalid_topic"},
	} {
		if !validOptionalString(field.value, field.max, field.pattern) {
			return toolErrorResult(field.code)
		}
	}
	if in.BodyBudget < 0 || in.BodyBudget > mcpMaxBodyPackTokens {
		return toolErrorResult("body_pack_token_budget_invalid")
	}
	homePath, err := home.Resolve("")
	if err != nil {
		return toolErrorResult("home_unavailable")
	}
	restoreEngine, err := configureMemoryEngineFromHome(homePath, "")
	if err != nil {
		return toolErrorResult("memory_engine_unavailable")
	}
	defer restoreEngine()
	cwd := optionalString(in.Cwd)
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	kind, kindVia, err := contract.ResolveRepoKindWithHome(homePath, optionalString(in.RepoKind), cwd)
	if err != nil {
		return toolErrorResult("repo_kind_unavailable")
	}
	orient := contract.Input{
		Harness:     firstNonEmpty(optionalString(in.Harness), "codex"),
		Cwd:         cwd,
		Task:        in.Task,
		RepoKind:    kind,
		RepoKindVia: kindVia,
		Mode:        firstNonEmpty(optionalString(in.Mode), "execute"),
		Topic:       optionalString(in.Topic),
	}
	pack, err := materializeOrientContextPack(homePath, orient, in.BodyBudget, time.Now())
	if err != nil {
		return toolErrorResult("orient_materialization_failed")
	}
	artifacts := computePersistedOrientArtifacts(pack)
	boundedSummary, sourceTruncated := boundedUTF8(pack.SummaryMD, mcpMaxSummaryBytes)
	privacyFilteredSummary := privacyFilterMCPString(boundedSummary)
	summary, sanitizedTruncated := boundedUTF8(privacyFilteredSummary, mcpMaxSummaryBytes)
	summaryTruncated := sourceTruncated || sanitizedTruncated
	decisions, decisionsLimited := boundedRecallRefs(pack.ActiveDecisions, mcpMaxRecallRefs, mcpMaxRecallRefBytes)
	memory, memoryLimited := boundedRecallRefs(pack.ActiveMemory, mcpMaxRecallRefs, mcpMaxRecallRefBytes)
	warnings := make([]string, 0, 3)
	if summaryTruncated {
		warnings = append(warnings, "summary_truncated")
	}
	if decisionsLimited {
		warnings = append(warnings, "active_decisions_limited")
	}
	if memoryLimited {
		warnings = append(warnings, "active_memory_limited")
	}
	ack := mcpOrientRecallAck{
		Schema:             mcpOrientAckSchema,
		PackID:             pack.ID,
		ContractID:         pack.IDs.ContractID,
		SummaryMD:          summary,
		ActiveDecisions:    decisions,
		ActiveMemory:       memory,
		BodyPackPresent:    pack.BodyPack != nil,
		PersistedArtifacts: artifacts,
		Warnings:           warnings,
	}
	text, _ := json.Marshal(ack)
	return mcpToolCallResult{Content: []mcpToolContent{{Type: "text", Text: string(text)}}}
}

// computePersistedOrientArtifacts reconstructs the deterministic, home-relative
// artifact refs the runtime materializer wrote. The materializer derives the
// same paths from contractID, packID, and contractCreatedAt.
func computePersistedOrientArtifacts(pack envelope.ContextPack) mcpOrientPersistedArtifacts {
	if strings.TrimSpace(pack.ID) == "" || strings.TrimSpace(pack.IDs.ContractID) == "" {
		return mcpOrientPersistedArtifacts{}
	}
	createdAt, err := time.Parse(time.RFC3339, pack.CreatedAt)
	if err != nil {
		return mcpOrientPersistedArtifacts{}
	}
	dateDir := createdAt.UTC().Format("2006-01-02")
	return mcpOrientPersistedArtifacts{
		EffectiveContractPath: filepath.ToSlash(filepath.Join("contracts", "effective", dateDir, pack.IDs.ContractID+".json")),
		LatestContractPath:    filepath.ToSlash(filepath.Join("contracts", "effective", "latest.json")),
		ContextPackPath:       filepath.ToSlash(filepath.Join("context", "packs", pack.ID+".json")),
	}
}

// ---------- Helpers: encoding, validation, IO ----------

// toolErrorResult returns one bounded stable code. Internal error text, input
// bytes, and filesystem paths never cross the MCP boundary.
func toolErrorResult(code string) mcpToolCallResult {
	if !mcpErrorCodePattern.MatchString(code) {
		code = "internal_error"
	}
	payload := struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}{}
	payload.Error.Code = code
	text, _ := json.Marshal(payload)
	return mcpToolCallResult{
		IsError: true,
		Content: []mcpToolContent{{Type: "text", Text: string(text)}},
	}
}

func explicitJSONNull(raw json.RawMessage, field string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	value, ok := fields[field]
	return ok && bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func validOptionalString(value *string, maxChars int, pattern *regexp.Regexp) bool {
	if value == nil {
		return true
	}
	return validBoundedNonBlank(*value, maxChars, pattern)
}

func validBoundedNonBlank(value string, maxChars int, pattern *regexp.Regexp) bool {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
		return false
	}
	if utf8.RuneCountInString(value) > maxChars {
		return false
	}
	return pattern == nil || pattern.MatchString(value)
}

func boundedProtocolVersionEcho(value string) string {
	runes := []rune(value)
	if len(runes) <= mcpMaxVersionEchoChars {
		return value
	}
	return string(runes[:mcpMaxVersionEchoChars])
}

// boundedRecallRefs caps count and bytes, and rejects refs containing local
// absolute paths even when a path is embedded in Markdown or an assignment.
func boundedRecallRefs(refs []string, maxCount, maxBytes int) ([]string, bool) {
	out := make([]string, 0, minInt(len(refs), maxCount))
	limited := false
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" || len(ref) > maxBytes || containsLocalAbsolutePath(ref) {
			limited = true
			continue
		}
		if len(out) == maxCount {
			limited = true
			continue
		}
		out = append(out, ref)
	}
	return out, limited
}

func containsLocalAbsolutePath(value string) bool {
	_, _, ok := nextLocalAbsolutePath(value, 0)
	return ok
}

// privacyFilterMCPString sanitizes local machine paths while preserving the
// surrounding prose. It recognizes bare paths, file URIs, home shorthand,
// Windows paths, Markdown destinations, and assignment values. Network URLs
// are left intact.
func privacyFilterMCPString(value string) string {
	var filtered strings.Builder
	cursor := 0
	for {
		start, end, ok := nextLocalAbsolutePath(value, cursor)
		if !ok {
			if cursor == 0 {
				return value
			}
			filtered.WriteString(value[cursor:])
			return filtered.String()
		}
		filtered.WriteString(value[cursor:start])
		filtered.WriteString("[private-path]")
		cursor = end
	}
}

func nextLocalAbsolutePath(value string, from int) (int, int, bool) {
	for start := from; start < len(value); start++ {
		if end, ok := localAbsolutePathEnd(value, start); ok {
			return start, end, true
		}
	}
	return 0, 0, false
}

func localAbsolutePathEnd(value string, start int) (int, bool) {
	if start < 0 || start >= len(value) || !localPathBoundary(value, start) {
		return 0, false
	}

	rest := value[start:]
	pathStart := false
	switch {
	case len(rest) > len("file:") &&
		strings.EqualFold(rest[:len("file:")], "file:") &&
		!localPathTerminator(rest[len("file:")]):
		pathStart = true
	case localHomeShorthand(rest):
		pathStart = true
	case rest[0] == '/':
		if protocolRelativeNetworkURL(rest) {
			return 0, false
		}
		// The first slash in https:// is preceded by ':' and followed by a
		// second slash. A source:/tmp reference remains local and is blocked.
		if start > 0 && value[start-1] == ':' && len(rest) >= 2 && rest[1] == '/' {
			return 0, false
		}
		pathStart = true
	case len(rest) >= 2 && rest[0] == '\\' && rest[1] == '\\':
		pathStart = true
	case len(rest) >= 3 && isASCIIAlpha(rest[0]) && rest[1] == ':' && (rest[2] == '/' || rest[2] == '\\'):
		pathStart = true
	}
	if !pathStart {
		return 0, false
	}

	if end, ok := localEnclosedPathEnd(value, start); ok {
		return end, true
	}
	end := start
	for end < len(value) && !localPathTerminator(value[end]) {
		end++
	}
	if end == start {
		return 0, false
	}
	return end, true
}

// localEnclosedPathEnd permits spaces and balanced parentheses inside quoted
// assignments and Markdown destinations. The returned end excludes a closing
// delimiter so privacyFilterMCPString preserves surrounding syntax. A missing
// closer consumes the bounded remainder rather than exposing a path suffix.
func localEnclosedPathEnd(value string, start int) (int, bool) {
	if start == 0 {
		return 0, false
	}
	opener := value[start-1]
	var closer byte
	switch opener {
	case '"', '\'':
		closer = opener
	case '<':
		closer = '>'
	case '(':
		closer = ')'
	case '|':
		closer = '|'
	default:
		return 0, false
	}

	depth := 1
	for index := start; index < len(value); index++ {
		if localDelimiterEscaped(value, index) {
			continue
		}
		char := value[index]
		if opener == '(' && char == '(' {
			depth++
			continue
		}
		if char != closer {
			continue
		}
		depth--
		if depth == 0 {
			return index, true
		}
	}
	return len(value), true
}

func localDelimiterEscaped(value string, index int) bool {
	backslashes := 0
	for index--; index >= 0 && value[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func localPathBoundary(value string, start int) bool {
	if start == 0 {
		return true
	}
	previous := value[start-1]
	if previous == ' ' || previous == '\t' || previous == '\r' || previous == '\n' {
		return true
	}
	return strings.ContainsRune("=([{<,;:'\"`|>", rune(previous))
}

func localPathTerminator(char byte) bool {
	switch char {
	case ' ', '\t', '\r', '\n', ')', ']', '}', '>', '|', ',', ';', '\'', '"', '`':
		return true
	default:
		return false
	}
}

func localHomeShorthand(value string) bool {
	if len(value) < 2 || value[0] != '~' {
		return false
	}
	if value[1] == '/' || value[1] == '\\' {
		return true
	}
	separator := strings.IndexAny(value[1:], `/\`)
	if separator <= 0 {
		return false
	}
	for index := 1; index <= separator; index++ {
		char := value[index]
		if !isASCIIAlphaNum(char) && !strings.ContainsRune("._-+", rune(char)) {
			return false
		}
	}
	return true
}

func protocolRelativeNetworkURL(value string) bool {
	if len(value) < 3 || value[0] != '/' || value[1] != '/' {
		return false
	}
	return isASCIIAlphaNum(value[2]) || value[2] == '['
}

func isASCIIAlpha(char byte) bool {
	return char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func isASCIIAlphaNum(char byte) bool {
	return isASCIIAlpha(char) || char >= '0' && char <= '9'
}

func boundedUTF8(value string, maxBytes int) (string, bool) {
	if len(value) <= maxBytes {
		return value, false
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut], true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// strictDecodeArgs decodes json arguments into the target Go struct and
// REJECTS unknown top-level keys + structural mismatches. This is the same
// shape validation the CLI uses (the validateKnownFlags convention) —
// structural only, the typed contract is the JSON Schema side of the tool
// descriptor.
func strictDecodeArgs(raw json.RawMessage, target interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func boundedNonBlankSchema(maxChars int) map[string]interface{} {
	return map[string]interface{}{
		"type": "string", "minLength": 1, "maxLength": maxChars,
		"pattern": `^\S(?:[\s\S]*\S)?$`,
	}
}

// mcpFindingRecordDescriptor is the typed schema for compound_finding_record.
// Unknown top-level keys are refused by strictDecodeArgs at call time.
func mcpFindingRecordDescriptor() mcpToolDescriptor {
	return mcpToolDescriptor{
		Name: mcpFindingToolName,
		Description: "Records one private operational evidence finding under COMPOUND_HOME/observed/findings. " +
			"It is idempotent on (topic, body), returns a body-free acknowledgement, and never writes memory authority.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []string{"topic", "body"},
			"properties": map[string]interface{}{
				"topic": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": mcpMaxFindingTopicChars,
					"pattern": `^\S(?:[\s\S]*\S)?$`,
				},
				"body": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": mcpMaxFindingBodyChars,
					"pattern": `\S`, "x-maxBytes": mcpMaxFindingBodyBytes,
				},
				"work_id": map[string]interface{}{
					"type": "string", "minLength": 6, "maxLength": mcpMaxFindingWorkIDChars,
					"pattern": `^work_[A-Za-z0-9][A-Za-z0-9._:-]*$`,
				},
				"produced_by": map[string]interface{}{
					"type": "string", "minLength": 1, "maxLength": mcpMaxFindingProducerChars,
					"pattern": `^[A-Za-z0-9][A-Za-z0-9._:/-]*$`,
				},
				"supersedes": map[string]interface{}{
					"type": "string", "minLength": mcpMaxFindingSupersedesChars, "maxLength": mcpMaxFindingSupersedesChars,
					"pattern": `^find_[a-f0-9]{12}$`,
				},
				"evidence_refs": map[string]interface{}{
					"type": "array", "maxItems": mcpMaxEvidenceRefs,
					"items": map[string]interface{}{
						"type": "string", "minLength": 3, "maxLength": mcpMaxEvidenceRefChars,
						"pattern": `^[A-Za-z][A-Za-z0-9+.-]*:\S+$`,
					},
				},
			},
		},
	}
}

// mcpOrientRecallDescriptor is the typed schema for compound_orient_recall.
// This tool persists sealed runtime artifacts (not read-only), so the schema
// mirrors `compound orient` exactly: one task + bounded harness/repo_kind/mode
// hints. body_budget<=0 keeps the pack body-free.
func mcpOrientRecallDescriptor() mcpToolDescriptor {
	return mcpToolDescriptor{
		Name: mcpOrientToolName,
		Description: "Compiles a sealed EffectiveContract+ContextPack pair via the same `compound orient` runtime materializer and persists the artifacts " +
			"under COMPOUND_HOME. NOT read-only/idempotent — a successful call writes one EffectiveContract (and a `latest.json` pointer) " +
			"plus one ContextPack. Returns bounded task recall (active decisions + active memory) plus the deterministic persisted paths. " +
			"body_budget > 0 enables a body_pack section; 0 keeps the pack body-free.",
		InputSchema: map[string]interface{}{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"task"},
			"properties": map[string]interface{}{
				"task":      boundedNonBlankSchema(mcpMaxOrientTaskChars),
				"harness":   boundedNonBlankSchema(mcpMaxOrientHarnessChars),
				"cwd":       boundedNonBlankSchema(mcpMaxOrientCwdChars),
				"repo_kind": boundedNonBlankSchema(mcpMaxOrientRepoKindChars),
				"mode": map[string]interface{}{
					"type": "string", "enum": []string{"observe", "plan", "execute", "review"},
					"maxLength": mcpMaxOrientModeChars,
				},
				"topic":       boundedNonBlankSchema(mcpMaxOrientTopicChars),
				"body_budget": map[string]interface{}{"type": "integer", "minimum": 0, "maximum": mcpMaxBodyPackTokens},
			},
		},
	}
}

// ---------- JSON-RPC response helpers ----------

// respondToID wraps a result OR error into a JSON-RPC response envelope
// preserving the request ID unchanged. nil result → marshaled null.
func respondToID(id json.RawMessage, result interface{}) []byte {
	switch v := result.(type) {
	case *jsonRPCError:
		return encodeResponse(id, v)
	case jsonRPCError:
		return encodeResponse(id, &v)
	case error:
		return encodeResponse(id, &jsonRPCError{Code: rpcInternalError, Message: "internal error"})
	default:
		return encodeResponse(id, nil, result)
	}
}

func encodeResponse(id json.RawMessage, rpcErr *jsonRPCError, result ...interface{}) []byte {
	responseID := id
	if responseID == nil {
		responseID = json.RawMessage("null")
	}
	var memberName string
	var memberValue []byte
	var err error
	if rpcErr != nil {
		memberName = "error"
		memberValue, err = json.Marshal(rpcErr)
	} else {
		memberName = "result"
		var value interface{}
		if len(result) > 0 {
			value = result[0]
		}
		memberValue, err = json.Marshal(value)
	}
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"encode failure"}}`)
	}
	out := make([]byte, 0, len(responseID)+len(memberValue)+48)
	out = append(out, `{"jsonrpc":"2.0","id":`...)
	out = append(out, responseID...)
	out = append(out, `,"`...)
	out = append(out, memberName...)
	out = append(out, `":`...)
	out = append(out, memberValue...)
	out = append(out, '}')
	return out
}

func validJSONRPCID(id json.RawMessage) bool {
	if id == nil {
		return false
	}
	trimmed := bytes.TrimSpace(id)
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var text string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &text) == nil
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return false
	}
	var trailing interface{}
	return errors.Is(dec.Decode(&trailing), io.EOF)
}

// writeJSONLine writes one response to the buffered writer and flushes.
func writeJSONLine(w *bufio.Writer, payload []byte) error {
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}

// bytesTrim trims ASCII whitespace from both ends of a byte slice without
// allocating a new slice for the common no-op case.
func bytesTrim(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		c := b[start]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		start++
	}
	for end > start {
		c := b[end-1]
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' {
			break
		}
		end--
	}
	return b[start:end]
}

// cmdMCPTransport is the dispatch seam the cmd/compound/main.go switch calls
// when invoked as `compound mcp-transport`. It rewires ioIn/ioOut to stdio
// (the test seams above let tests swap to pipes) and runs the transport loop
// until EOF.
func cmdMCPTransport(args []string) {
	if hasUsageFlag(args) {
		fmt.Fprintln(os.Stderr, "usage: compound mcp-transport")
		return
	}
	ioIn = os.Stdin
	ioOut = os.Stdout
	if err := runMcpTransportLoop(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-transport:", err)
		os.Exit(1)
	}
}
