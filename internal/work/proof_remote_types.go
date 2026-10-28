package work

import (
	"bytes"
	"compound/internal/authority/gitrepository"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

const RemoteProofReportSchemaVersion = "compound.proof_check_report.v0.7"
const remoteRequestSchema = "compound.remote_go_test_request.v0.1"
const remoteReceiptSchema = "compound.remote_go_test_receipt.v0.1"
const remoteStateSchema = "compound.remote_go_test_state.v0.1"

type RemoteProofRoute struct {
	SchemaVersion  string `json:"schema_version"`
	Host           string `json:"host"`
	RunnerPath     string `json:"runner_path"`
	RunnerSHA256   string `json:"runner_sha256"`
	GoVersion      string `json:"go_version"`
	ConfigBase64   string `json:"config_base64,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}
type RemoteProofInput struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	SHA256     string `json:"sha256,omitempty"`
	Executable *bool  `json:"executable,omitempty"`
	Target     string `json:"target,omitempty"`
}
type RemoteProofDescriptor struct {
	Route        RemoteProofRoute       `json:"route"`
	Source       gitrepository.Snapshot `json:"source"`
	InputRoots   []string               `json:"input_roots"`
	Inputs       []RemoteProofInput     `json:"inputs_manifest"`
	InputsDigest string                 `json:"inputs_digest"`
	GateDigest   string                 `json:"gate_digest"`
	Digest       string                 `json:"digest"`
}
type RemoteProofRequest struct {
	SchemaVersion         string             `json:"schema_version"`
	ID                    string             `json:"id"`
	WorkID                string             `json:"work_id"`
	ReservationID         string             `json:"reservation_id"`
	RunAttemptID          string             `json:"run_attempt_id"`
	RepositoryFingerprint string             `json:"repository_fingerprint"`
	BaseSHA               string             `json:"base_sha"`
	HeadSHA               string             `json:"head_sha"`
	SourceTree            string             `json:"source_tree"`
	GateDigest            string             `json:"gate_digest"`
	InputsDigest          string             `json:"inputs_digest"`
	RunnerSHA256          string             `json:"runner_sha256"`
	GoVersion             string             `json:"go_version"`
	TimeoutSeconds        int                `json:"timeout_seconds"`
	InputRoots            []string           `json:"input_roots"`
	Inputs                []RemoteProofInput `json:"inputs_manifest"`
}
type RemoteProofReceipt struct {
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	RequestDigest string              `json:"request_digest"`
	Request       *RemoteProofRequest `json:"request"`
	Status        string              `json:"status"`
	FinishedAt    string              `json:"finished_at"`
	Inherited     *struct {
		WorkID                string `json:"work_id"`
		ReservationID         string `json:"reservation_id"`
		RunAttemptID          string `json:"run_attempt_id"`
		RepositoryFingerprint string `json:"repository_fingerprint"`
	} `json:"inherited_identity"`
	Verified *struct {
		HeadSHA      string `json:"head_sha"`
		SourceTree   string `json:"source_tree"`
		BaseSHA      string `json:"base_sha"`
		InputsDigest string `json:"inputs_digest"`
		RunnerSHA256 string `json:"runner_sha256"`
		GoVersion    string `json:"go_version"`
		GateDigest   string `json:"gate_digest"`
	} `json:"verified_evidence"`
	Execution struct {
		Launched             *bool    `json:"launched"`
		Command              []string `json:"command"`
		ExitCode             *int     `json:"exit_code"`
		PhasesComplete       bool     `json:"phases_complete,omitempty"`
		Reason               string   `json:"reason,omitempty"`
		StageCommandsStarted *int     `json:"stage_commands_started,omitempty"`
	} `json:"execution"`
	Cleanup struct {
		FinishedAt string   `json:"finished_at,omitempty"`
		Status     string   `json:"status"`
		Removed    []string `json:"removed,omitempty"`
		Killed     []int    `json:"killed,omitempty"`
		Errors     []string `json:"errors"`
		Warnings   []string `json:"warnings,omitempty"`
		Leftovers  *int     `json:"leftovers"`
	} `json:"cleanup"`
	Quiescent bool `json:"quiescent"`
}

type RemoteProofRecovery struct {
	Status                string `json:"status"`
	OriginalAttemptStatus string `json:"original_attempt_status"`
	ResourceReleased      bool   `json:"resource_released"`
	EligibleAfter         string `json:"eligible_after,omitempty"`
	NextCommand           string `json:"next_command,omitempty"`
}

type RemoteProofEvidence struct {
	Recovery           *RemoteProofRecovery `json:"recovery,omitempty"`
	ProfileDigest      string               `json:"profile_digest"`
	DescriptorDigest   string               `json:"descriptor_digest"`
	RequestArtifactRef string               `json:"request_artifact_ref"`
	ReceiptArtifactRef string               `json:"receipt_artifact_ref"`
	Request            RemoteProofRequest   `json:"request"`
	Receipt            RemoteProofReceipt   `json:"receipt"`
	ReceiptJSON        json.RawMessage      `json:"receipt_json"`
	Timings            map[string]int64     `json:"timings_ms,omitempty"`
	Bytes              map[string]int64     `json:"bytes,omitempty"`
}

func remoteCanonicalJSON(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var object any
	if err = dec.Decode(&object); err != nil {
		return nil, err
	}
	var check func(any) error
	check = func(v any) error {
		switch v := v.(type) {
		case string:
			if strings.ContainsAny(v, "\u2028\u2029") {
				return fmt.Errorf("remote canonical JSON excludes U+2028/U+2029")
			}
		case []any:
			for _, x := range v {
				if err := check(x); err != nil {
					return err
				}
			}
		case map[string]any:
			for k, x := range v {
				if err := check(k); err != nil {
					return err
				}
				if err := check(x); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := check(object); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(object); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}
func remoteDigest(value any) (string, error) {
	b, e := remoteCanonicalJSON(value)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func remoteStrictJSON(body []byte, v any) error {
	if err := consumerUniqueJSON(body); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("remote JSON trailing content")
	}
	return nil
}
func remoteGateCommand(head string) []string {
	return []string{"go", "run", "./scripts/testbattery", "general-release", "--source-head", head}
}
func remoteGateDigest(head string) (string, error) {
	return remoteDigest(map[string]any{"gate_id": "go-test", "proof_ref": "go-test-count-1-general-release", "command": remoteGateCommand(head)})
}
func remoteInputsDigest(roots []string, entries []RemoteProofInput) (string, error) {
	return remoteDigest(map[string]any{"input_roots": roots, "entries": entries})
}
func remoteSame(a, b any) bool { return reflect.DeepEqual(a, b) }
