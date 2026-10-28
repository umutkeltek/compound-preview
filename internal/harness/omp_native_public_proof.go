package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"compound/internal/runattempt"
)

// ompNativePublicProofSchemaVersion is the exact schema_version the OMP
// native verifier adapter emits in finalizeEvidence. The decoder is a
// strict structural peer of that emitter; drift on this string fails
// closed.
const ompNativePublicProofSchemaVersion = "compound.omp_prompt_only_native_proof.v0.3"

// ompNativePublicProofEvidenceRefPrefix mirrors the artifact ref grammar
// for entries in evidence_refs. The literal MUST match the emitter's
// homepkg.ArtifactRefPrefix; the decoder does not import internal/home.
const ompNativePublicProofEvidenceRefPrefix = "artifact:sha256:"

// ompNativePublicProofEvidenceRefCount is the exact cardinality the
// emitter produces: [recipe_ref, report_ref].
const ompNativePublicProofEvidenceRefCount = 2

// ompNativePublicProofDigestHexLen is the byte length of a lowercase
// hex-encoded SHA-256 digest, matching the verifier's hex.EncodeToString
// output.
const ompNativePublicProofDigestHexLen = 64

// errOMPNativePublicProofSchema is the single content-free failure
// surface DecodeOMPNativePublicProof returns. The raw input is never
// reflected.
var errOMPNativePublicProofSchema = errors.New("OMP_PUBLIC_PROOF_SCHEMA")

// ompNativePublicProofTopLevelKeys is the exact set the emitter writes.
// The decoder rejects unknown, missing, case-variant, and duplicate keys
// (including Unicode-escaped aliases that resolve to a member).
var ompNativePublicProofTopLevelKeys = map[string]struct{}{
	"schema_version":         {},
	"work_id":                {},
	"run_attempt_id":         {},
	"action_intent_id":       {},
	"report_sha256":          {},
	"private_custody_sha256": {},
	"evidence_refs":          {},
}

// ompNativePublicProofCustodyRoles is the exact set of role keys the
// emitter populates in private_custody_sha256. Any other key (including
// case variants) is rejected.
var ompNativePublicProofCustodyRoles = map[string]struct{}{
	"capture":       {},
	"interpreter":   {},
	"wrapper":       {},
	"models_config": {},
	"executable":    {},
}

// OMPNativePublicProof is the strict, fully-decoded public proof envelope
// emitted by the OMP native verifier adapter's finalizeEvidence. The zero
// value is the only valid representation of "absent / error".
type OMPNativePublicProof struct {
	SchemaVersion        string
	WorkID               string
	RunAttemptID         string
	ActionIntentID       string
	ReportSHA256         string
	PrivateCustodySHA256 map[string]string
	EvidenceRefs         []string
}

// DecodeOMPNativePublicProof strictly decodes a public proof body
// emitted by the OMP native verifier adapter. It is a content-free
// structural peer: every failure returns OMP_PUBLIC_PROOF_SCHEMA and the
// raw input is never reflected.
//
// Required shape:
//   - well-formed UTF-8 input bytes;
//   - exactly one JSON object at the root (no null/scalar/array root);
//   - exactly seven top-level keys, case-sensitive, no duplicates
//     (including Unicode-escaped aliases);
//   - schema_version == "compound.omp_prompt_only_native_proof.v0.3";
//   - nonempty opaque work_id and action_intent_id;
//   - run_attempt_id accepted by runattempt.Valid;
//   - report_sha256 is exactly 64 lowercase-hex bytes;
//   - private_custody_sha256 is an object with exactly the five roles
//     capture, interpreter, wrapper, models_config, executable — each
//     value a 64 lowercase-hex digest;
//   - evidence_refs is an array of exactly two artifact:sha256:<64 hex>
//     refs in emitter order (recipe first, report second), with the
//     second ref equal to artifact:sha256: + report_sha256;
//   - no trailing JSON.
//
// Recipe content validation and producer authority are explicitly NOT
// this decoder's job.
func DecodeOMPNativePublicProof(body []byte) (OMPNativePublicProof, error) {
	if !utf8.Valid(body) {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	proof, err := decodeOMPNativePublicProofTopLevel(decoder)
	if err != nil {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	return proof, nil
}

func decodeOMPNativePublicProofTopLevel(decoder *json.Decoder) (OMPNativePublicProof, error) {
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return OMPNativePublicProof{}, errOMPNativePublicProofSchema
		}
		if _, allowed := ompNativePublicProofTopLevelKeys[key]; !allowed {
			return OMPNativePublicProof{}, errOMPNativePublicProofSchema
		}
		if _, duplicate := fields[key]; duplicate {
			return OMPNativePublicProof{}, errOMPNativePublicProofSchema
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return OMPNativePublicProof{}, errOMPNativePublicProofSchema
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != len(ompNativePublicProofTopLevelKeys) {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	var proof OMPNativePublicProof
	if err := json.Unmarshal(fields["schema_version"], &proof.SchemaVersion); err != nil || proof.SchemaVersion != ompNativePublicProofSchemaVersion {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	if err := json.Unmarshal(fields["work_id"], &proof.WorkID); err != nil || strings.TrimSpace(proof.WorkID) == "" {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	if err := json.Unmarshal(fields["run_attempt_id"], &proof.RunAttemptID); err != nil || !runattempt.Valid(proof.RunAttemptID) {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	if err := json.Unmarshal(fields["action_intent_id"], &proof.ActionIntentID); err != nil || strings.TrimSpace(proof.ActionIntentID) == "" {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	if err := json.Unmarshal(fields["report_sha256"], &proof.ReportSHA256); err != nil || !isOMPNativePublicProofLowerHex(proof.ReportSHA256, ompNativePublicProofDigestHexLen) {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	custody, err := decodeOMPNativePublicProofCustody(fields["private_custody_sha256"])
	if err != nil {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	evidence, err := decodeOMPNativePublicProofEvidence(fields["evidence_refs"], proof.ReportSHA256)
	if err != nil {
		return OMPNativePublicProof{}, errOMPNativePublicProofSchema
	}
	proof.PrivateCustodySHA256 = custody
	proof.EvidenceRefs = evidence
	return proof, nil
}

func decodeOMPNativePublicProofCustody(raw json.RawMessage) (map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errOMPNativePublicProofSchema
	}
	roles := map[string]string{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errOMPNativePublicProofSchema
		}
		if _, allowed := ompNativePublicProofCustodyRoles[key]; !allowed {
			return nil, errOMPNativePublicProofSchema
		}
		if _, duplicate := roles[key]; duplicate {
			return nil, errOMPNativePublicProofSchema
		}
		var digest string
		if err := decoder.Decode(&digest); err != nil {
			return nil, errOMPNativePublicProofSchema
		}
		if !isOMPNativePublicProofLowerHex(digest, ompNativePublicProofDigestHexLen) {
			return nil, errOMPNativePublicProofSchema
		}
		roles[key] = digest
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errOMPNativePublicProofSchema
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errOMPNativePublicProofSchema
	}
	if len(roles) != len(ompNativePublicProofCustodyRoles) {
		return nil, errOMPNativePublicProofSchema
	}
	return roles, nil
}

func decodeOMPNativePublicProofEvidence(raw json.RawMessage, reportDigest string) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return nil, errOMPNativePublicProofSchema
	}
	refs := []string{}
	for decoder.More() {
		var ref string
		if err := decoder.Decode(&ref); err != nil {
			return nil, errOMPNativePublicProofSchema
		}
		refs = append(refs, ref)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, errOMPNativePublicProofSchema
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errOMPNativePublicProofSchema
	}
	if len(refs) != ompNativePublicProofEvidenceRefCount {
		return nil, errOMPNativePublicProofSchema
	}
	for index, ref := range refs {
		if !isOMPNativePublicProofEvidenceRef(ref) {
			return nil, errOMPNativePublicProofSchema
		}
		if index == 1 && ref != ompNativePublicProofEvidenceRefPrefix+reportDigest {
			return nil, errOMPNativePublicProofSchema
		}
	}
	return refs, nil
}

func isOMPNativePublicProofLowerHex(s string, want int) bool {
	if len(s) != want {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f')) {
			return false
		}
	}
	return true
}

func isOMPNativePublicProofEvidenceRef(s string) bool {
	if !strings.HasPrefix(s, ompNativePublicProofEvidenceRefPrefix) {
		return false
	}
	return isOMPNativePublicProofLowerHex(s[len(ompNativePublicProofEvidenceRefPrefix):], ompNativePublicProofDigestHexLen)
}
