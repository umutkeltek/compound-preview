package mem

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Transport carries one protocol round-trip. The wire is the swap seam: a held coprocess (warm
// index, the solo default), a per-op exec (simplest), or a socket are interchangeable impls
// behind identical marshalling. compound owns the protocol; the engine is rented behind it.
type Transport interface {
	RoundTrip(req Request) (Response, error)
	Close() error
}

// HeldCoprocessTransport spawns the engine binary ONCE and holds framed JSON over its stdio, so
// the RAM-resident index stays warm across queries (no reload per call). Serialized by a mutex
// (single-writer through the held pipe). stderr is the engine's log channel; stdout is data only.
type HeldCoprocessTransport struct {
	argv    []string
	env     []string
	timeout time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	enc     *json.Encoder
	dec     *json.Decoder
	started bool
}

func NewHeldCoprocess(argv, extraEnv []string) *HeldCoprocessTransport {
	return &HeldCoprocessTransport{argv: argv, env: extraEnv, timeout: engineRoundTripTimeout()}
}

// engineRoundTripTimeout bounds one engine round-trip so a hung sidecar can
// never block lock-holders forever. COMPOUND_MEM_ENGINE_TIMEOUT (whole
// seconds) overrides the 10s default; both transports share the knob.
func engineRoundTripTimeout() time.Duration {
	if v := os.Getenv("COMPOUND_MEM_ENGINE_TIMEOUT"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return 10 * time.Second
}

// DefaultBreadthEngine selects the rented breadth engine the same way the CLI does: a held
// protocol sidecar when COMPOUND_MEM_ENGINE_BIN is set (e.g. the owned compound-breadth-engine),
// otherwise the in-process LexicalEngine. homePath is the already-resolved COMPOUND_HOME; it is
// forwarded to the sidecar so its vector cache lands in the right home even when the caller relied
// on the default without exporting COMPOUND_HOME (measured 2026-07-07: an empty home took the whole
// engine down). This is the ONE factory both the CLI and the orient/ContextPack path share, so the
// agent-facing recall inherits the exact engine that gives `memory query` its measured recall.
func DefaultBreadthEngine(homePath string) (Engine, func()) {
	bin := strings.TrimSpace(os.Getenv("COMPOUND_MEM_ENGINE_BIN"))
	if bin == "" {
		return &LexicalEngine{}, func() {}
	}
	var extraEnv []string
	if strings.TrimSpace(os.Getenv("COMPOUND_HOME")) == "" && strings.TrimSpace(homePath) != "" {
		extraEnv = append(extraEnv, "COMPOUND_HOME="+homePath)
	}
	tr := NewHeldCoprocess(strings.Fields(bin), extraEnv)
	eng := NewProtocolEngine(tr, os.Getenv("COMPOUND_MEM_ENGINE_EMBEDDER"))
	return eng, func() { _ = eng.Close() }
}

func (t *HeldCoprocessTransport) start() error {
	if len(t.argv) == 0 {
		return fmt.Errorf("no engine command")
	}
	cmd := exec.Command(t.argv[0], t.argv[1:]...)
	cmd.Stderr = os.Stderr
	if len(t.env) > 0 {
		cmd.Env = append(os.Environ(), t.env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close() // don't leak the stdin fd if StdoutPipe fails
		return err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return err
	}
	t.cmd, t.enc, t.dec, t.started = cmd, json.NewEncoder(stdin), json.NewDecoder(stdout), true
	return nil
}

// teardownLocked kills+reaps the held process and clears the warm state, so the NEXT call re-spawns
// cleanly and a desynced json.Decoder is discarded. Caller holds t.mu.
func (t *HeldCoprocessTransport) teardownLocked() {
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}
	t.cmd, t.enc, t.dec, t.started = nil, nil, nil, false
}

func (t *HeldCoprocessTransport) RoundTrip(req Request) (Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.started {
		if err := t.start(); err != nil {
			return Response{}, err
		}
	}
	// Run the framed exchange with a deadline so a hung sidecar can't block forever under the lock.
	// On ANY error (encode/decode/timeout/id-desync) we tear the transport down and reset, so the
	// next call re-spawns instead of staying wedged on a broken pipe or a desynced decoder.
	enc, dec := t.enc, t.dec
	type result struct {
		resp Response
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		if err := enc.Encode(req); err != nil {
			ch <- result{err: err}
			return
		}
		var resp Response
		err := dec.Decode(&resp)
		ch <- result{resp: resp, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.teardownLocked()
			return Response{}, r.err
		}
		if r.resp.RequestID != req.RequestID {
			// framing desync: the engine wrote an extra/duplicate object to the data channel.
			t.teardownLocked()
			return Response{}, fmt.Errorf("engine response id %q != request id %q (framing desync)", r.resp.RequestID, req.RequestID)
		}
		return r.resp, nil
	case <-time.After(t.timeout):
		t.teardownLocked() // kill so the blocked Decode returns and the pipe/child are released
		return Response{}, fmt.Errorf("engine round-trip timed out after %s", t.timeout)
	}
}

func (t *HeldCoprocessTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.teardownLocked()
	return nil
}

// ExecTransport spawns the engine per op (one JSON request in, one response out). Simplest, but
// cold — it is the conformance baseline / fallback, not the warm path.
type ExecTransport struct {
	Argv []string
	Env  []string
	// Timeout bounds one round-trip; zero resolves COMPOUND_MEM_ENGINE_TIMEOUT
	// or the 10s default, the same knob the warm path uses. A hung sidecar
	// must never block lock-holders forever on this path either.
	Timeout time.Duration
}

func (t ExecTransport) RoundTrip(req Request) (Response, error) {
	if len(t.Argv) == 0 {
		return Response{}, fmt.Errorf("no engine command")
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = engineRoundTripTimeout()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Argv[0], t.Argv[1:]...)
	cmd.Stderr = os.Stderr
	if len(t.Env) > 0 {
		cmd.Env = append(os.Environ(), t.Env...)
	}
	in, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	cmd.Stdin = bytes.NewReader(append(in, '\n'))
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return Response{}, fmt.Errorf("engine round-trip timed out after %s", timeout)
		}
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimSpace(out), &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}

func (ExecTransport) Close() error { return nil }

// ProtocolEngine is a mem.Engine that drives ANY breadth engine over the protocol via a Transport.
// turbovec plugs in here (as the sidecar binary behind a held coprocess); the pure-Go reference
// engine proves it with zero Rust. It enforces the identity guard (fail-CLOSED on an embedder
// mismatch), idempotent Index (no re-embed when the generation matches), and explicit error
// classes (unavailable → degrade; mismatch → refuse).
type ProtocolEngine struct {
	transport        Transport
	expectedEmbedder string // "" = adopt the engine's stamp on first use (bootstrap)

	probed   bool
	probeErr error
	caps     EngineCapabilities
	reqN     int
}

func NewProtocolEngine(t Transport, expectedEmbedder string) *ProtocolEngine {
	return &ProtocolEngine{transport: t, expectedEmbedder: expectedEmbedder}
}

func (e *ProtocolEngine) nextReqID() string {
	e.reqN++
	return "req_" + strconv.Itoa(e.reqN)
}

// ensureProbe asks the engine for capabilities once and runs the identity guard.
func (e *ProtocolEngine) ensureProbe() error {
	if e.probed {
		return e.probeErr
	}
	e.probed = true
	resp, err := e.transport.RoundTrip(Request{SchemaVersion: ProtocolVersion, RequestID: e.nextReqID(), Op: OpCapabilities})
	if err != nil || !resp.OK {
		e.probeErr = ErrEngineUnavailable
		return e.probeErr
	}
	if resp.SchemaVersion != 0 && resp.SchemaVersion != ProtocolVersion {
		e.probeErr = fmt.Errorf("%w: engine speaks v%d, core speaks v%d", ErrProtocolVersion, resp.SchemaVersion, ProtocolVersion)
		return e.probeErr
	}
	if e.expectedEmbedder != "" && resp.Embedder != e.expectedEmbedder {
		e.probeErr = fmt.Errorf("%w: indexed with %q, engine reports %q", ErrEmbedderMismatch, e.expectedEmbedder, resp.Embedder)
		return e.probeErr
	}
	e.caps = EngineCapabilities{Name: resp.Name, Breadth: resp.Breadth, Embedder: resp.Embedder, CorpusGen: resp.CorpusGen}
	return nil
}

func (e *ProtocolEngine) Capabilities() EngineCapabilities {
	if err := e.ensureProbe(); err != nil {
		// Optimistic: claim breadth so QueryBreadth proceeds; the real error (unavailable /
		// mismatch) surfaces loudly on Index/Query rather than masquerading as ErrUnsupported.
		return EngineCapabilities{Name: "protocol", Breadth: true}
	}
	return e.caps
}

func (e *ProtocolEngine) Index(claims []Claim, gen int64) error {
	if err := e.ensureProbe(); err != nil {
		return err
	}
	if e.caps.CorpusGen == gen { // idempotent: engine already at this generation
		return nil
	}
	cts := make([]ClaimText, 0, len(claims))
	for _, c := range claims {
		cts = append(cts, ClaimText{ClaimID: c.ID, Text: ClaimIndexText(c)})
	}
	resp, err := e.transport.RoundTrip(Request{SchemaVersion: ProtocolVersion, RequestID: e.nextReqID(), Op: OpIndex, CorpusGen: gen, Claims: cts})
	if err != nil {
		return ErrEngineUnavailable
	}
	if !resp.OK {
		return wireErr(resp)
	}
	e.caps.CorpusGen = gen
	return nil
}

func (e *ProtocolEngine) IndexWithContext(claims []Claim, edges []WireEdge, gen int64) error {
	if err := e.ensureProbe(); err != nil {
		return err
	}
	if e.caps.CorpusGen == gen { // idempotent: engine already at this generation
		return nil
	}
	cts := make([]ClaimText, 0, len(claims))
	for _, c := range claims {
		cts = append(cts, ClaimText{
			ClaimID:    c.ID,
			Text:       ClaimIndexText(c),
			Entity:     c.Entity,
			ClaimType:  c.ClaimType,
			Seq:        c.Seq,
			CreatedAt:  c.CreatedAt,
			Confidence: c.Confidence,
			Derivation: c.Derivation,
			EvidenceN:  len(c.EvidenceRefs),
		})
	}
	resp, err := e.transport.RoundTrip(Request{SchemaVersion: ProtocolVersion, RequestID: e.nextReqID(), Op: OpIndex, CorpusGen: gen, Claims: cts, Edges: edges})
	if err != nil {
		return ErrEngineUnavailable
	}
	if !resp.OK {
		return wireErr(resp)
	}
	e.caps.CorpusGen = gen
	return nil
}

func (e *ProtocolEngine) Query(text, gate string, allowlist []string, k int) ([]Hit, error) {
	hits, _, err := e.QueryWithDiagnostics(text, gate, allowlist, k)
	return hits, err
}

func (e *ProtocolEngine) QueryWithDiagnostics(text, gate string, allowlist []string, k int) ([]Hit, []string, error) {
	if err := e.ensureProbe(); err != nil {
		return nil, nil, err
	}
	resp, err := e.transport.RoundTrip(Request{SchemaVersion: ProtocolVersion, RequestID: e.nextReqID(), Op: OpQuery, Text: text, Gate: gate, Allowlist: allowlist, K: k})
	if err != nil {
		return nil, nil, ErrEngineUnavailable
	}
	if !resp.OK {
		return nil, nil, wireErr(resp)
	}
	return resp.Hits, appendDiagnosticLanes(nil, resp.DegradedLanes...), nil
}

func (e *ProtocolEngine) Close() error { return e.transport.Close() }

func wireErr(resp Response) error {
	if resp.Error == nil {
		return fmt.Errorf("engine error (no detail)")
	}
	switch resp.Error.Code {
	case CodeEmbedderMismatch, CodeDimMismatch:
		// both are identity-class: wrong vectors → HARD refuse, never a silent degrade.
		return fmt.Errorf("%w: [%s] %s", ErrEmbedderMismatch, resp.Error.Code, resp.Error.Message)
	case CodeEngineUnavailable:
		return fmt.Errorf("%w: [%s] %s", ErrEngineUnavailable, resp.Error.Code, resp.Error.Message)
	default:
		// index_missing / stale_generation / bad_request: surfaced with their code (callers can branch).
		return fmt.Errorf("engine error [%s]: %s", resp.Error.Code, resp.Error.Message)
	}
}
