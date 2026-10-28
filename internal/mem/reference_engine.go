package mem

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
)

// RunReferenceEngine is the pure-Go reference breadth engine: it speaks the protocol over the
// given streams using lexical matching + a fixed embedder stamp, holding its corpus warm in
// memory across requests. It IS the conformance baseline the real turbovec sidecar must satisfy
// (same wire), it lets the whole adapter be proven with zero Rust, and cmd/compound-mock-engine
// wraps it. Logs (if any) must go to a separate writer — stdout here is the data channel only.
func RunReferenceEngine(in io.Reader, out io.Writer, embedder string) error {
	dec := json.NewDecoder(in)
	enc := json.NewEncoder(out)
	var (
		claims []ClaimText
		gen    int64
		ready  bool
	)
	for {
		var req Request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		var resp Response
		resp.RequestID = req.RequestID
		resp.SchemaVersion = ProtocolVersion
		if req.SchemaVersion != 0 && req.SchemaVersion != ProtocolVersion {
			resp.OK, resp.Error = false, &WireError{Code: CodeBadRequest, Message: "unsupported schema_version"}
			if err := enc.Encode(&resp); err != nil {
				return err
			}
			continue
		}
		switch req.Op {
		case OpCapabilities:
			resp.OK, resp.Name, resp.Breadth, resp.Embedder, resp.Ready, resp.CorpusGen = true, "reference", true, embedder, ready, gen
		case OpIndex:
			if ready && req.CorpusGen == gen { // idempotent: already at this generation
				resp.OK, resp.Indexed, resp.CorpusGen = true, 0, gen
			} else {
				claims, gen, ready = req.Claims, req.CorpusGen, true
				resp.OK, resp.Indexed, resp.CorpusGen = true, len(claims), gen
			}
		case OpQuery:
			resp.OK, resp.Hits, resp.CorpusGen = true, refMatch(claims, req.Text, req.Gate, req.Allowlist, req.K), gen
		default:
			resp.OK, resp.Error = false, &WireError{Code: CodeBadRequest, Message: "unknown op " + req.Op}
		}
		if err := enc.Encode(&resp); err != nil {
			return err
		}
	}
}

// refMatch is lexical (no embeddings) but honors the SAME gate contract as the real engine:
// GateList + an empty allowlist matches NOTHING (fail-closed); GateAll skips the gate.
func refMatch(claims []ClaimText, text, gate string, allowlist []string, k int) []Hit {
	allow := map[string]bool{}
	for _, id := range allowlist {
		allow[id] = true
	}
	terms := strings.Fields(strings.ToLower(text))
	var hits []Hit
	for _, c := range claims {
		if gate != GateAll && !allow[c.ClaimID] {
			continue
		}
		hay := strings.ToLower(c.Text)
		score := 0.0
		for _, t := range terms {
			if strings.Contains(hay, t) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, Hit{ClaimID: c.ClaimID, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	return hits
}
