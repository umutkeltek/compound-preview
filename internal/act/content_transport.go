package act

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	"compound/internal/envelope"
)

// ContentTransportContract is opt-in at the enrolled executable. The protocol
// has two operations: schedule creates idempotently; observe only reads that
// exact attempt. Generic executable transports never gain a read door.
const ContentTransportContract = "compound.content_transport.v0.2"

type ContentTransportBinding struct {
	Name             string   `json:"name"`
	Contract         string   `json:"contract"`
	ConfigDigest     string   `json:"config_digest"`
	Executable       string   `json:"executable"`
	ExecutableDigest string   `json:"executable_digest"`
	Argv             []string `json:"argv"`
	TimeoutS         int      `json:"timeout_s"`
}

type ContentTransportMedia struct {
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	MIMEType string `json:"mime_type"`
	AltText  string `json:"alt_text"`
	Bytes    []byte `json:"bytes"`
}

type ContentTransportRequest struct {
	SchemaVersion  string                  `json:"schema_version"`
	DraftID        string                  `json:"draft_id"`
	ProductID      string                  `json:"product_id"`
	Platform       string                  `json:"platform"`
	Transport      string                  `json:"transport"`
	AccountID      string                  `json:"account_id"`
	IntegrationID  string                  `json:"integration_id"`
	PostType       string                  `json:"type"`
	ScheduleAt     string                  `json:"date"`
	ExpiresAt      string                  `json:"expires_at"`
	Body           string                  `json:"body"`
	Media          []ContentTransportMedia `json:"media"`
	PackageDigest  string                  `json:"package_digest"`
	AttemptID      string                  `json:"attempt_id"`
	IdempotencyKey string                  `json:"idempotency_key"`
	Binding        ContentTransportBinding `json:"binding"`
}

func ContentTransportDigest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func PinContentTransport(homePath, name string) (ContentTransportBinding, error) {
	a := &ExecTransportAdapter{Name: name, Home: homePath}
	cfg, err := a.loadConfig()
	if err != nil {
		return ContentTransportBinding{}, err
	}
	if cfg.ContentContract != ContentTransportContract {
		return ContentTransportBinding{}, fmt.Errorf("transport %q must explicitly enroll contract %s", name, ContentTransportContract)
	}
	config, err := os.ReadFile(filepath.Join(homePath, "actuator", "transports", name+".json"))
	if err != nil {
		return ContentTransportBinding{}, err
	}
	// Decode the exact hashed bytes and validate them again, so the digest and
	// resolved executable cannot come from different config generations.
	cfg = execTransportConfig{}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return ContentTransportBinding{}, err
	}
	if cfg.ContentContract != ContentTransportContract || len(cfg.Argv) == 0 {
		return ContentTransportBinding{}, fmt.Errorf("content transport contract changed")
	}
	if err := validateExecArgv(name, cfg.Argv); err != nil {
		return ContentTransportBinding{}, err
	}
	exe, err := exec.LookPath(cfg.Argv[0])
	if err != nil {
		return ContentTransportBinding{}, err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return ContentTransportBinding{}, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return ContentTransportBinding{}, err
	}
	body, err := os.ReadFile(exe)
	if err != nil {
		return ContentTransportBinding{}, err
	}
	return ContentTransportBinding{Name: name, Contract: cfg.ContentContract, ConfigDigest: ContentTransportDigest(config), Executable: exe, ExecutableDigest: ContentTransportDigest(body), Argv: cfg.Argv, TimeoutS: cfg.TimeoutS}, nil
}

func CheckContentTransport(homePath string, want ContentTransportBinding) error {
	got, err := PinContentTransport(homePath, want.Name)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("content transport config or executable changed; reconcile with the pinned contract")
	}
	return nil
}

// NewContentTransportAdapter retains the one policy-gated mutation owner.
func NewContentTransportAdapter(homePath string, binding ContentTransportBinding, request []byte, admission func() error) Adapter {
	return &ExecTransportAdapter{Name: binding.Name, Home: homePath, contentBinding: &binding, requestDigest: ContentTransportDigest(request), admissionCheck: admission}
}

func (a *ExecTransportAdapter) validateIntent(_ string, ai envelope.ActionIntent, _ envelope.PolicyDecision, body []byte) error {
	if a.contentBinding == nil {
		return nil
	}
	if ai.ArgumentsHash != a.requestDigest || ContentTransportDigest(body) != a.requestDigest || ai.SchemaRevision != ContentTransportContract || ai.Target.ID != a.Name {
		return fmt.Errorf("content transport intent does not bind exact request and contract")
	}
	var req ContentTransportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return err
	}
	if req.PostType != "schedule" || req.AttemptID == "" || req.IdempotencyKey != req.AttemptID || !reflect.DeepEqual(req.Binding, *a.contentBinding) {
		return fmt.Errorf("invalid bound schedule request")
	}
	return nil
}

// ObserveContentTransport is the explicit read operation of the pinned v0.2
// contract, not an arbitrary executable request relabeled read by a caller.
// A failed/truncated response is never returned as a complete observation.
func ObserveContentTransport(homePath string, original ContentTransportRequest) ([]byte, error) {
	if original.SchemaVersion != ContentTransportContract || original.PostType != "schedule" || original.AttemptID == "" || original.IdempotencyKey != original.AttemptID || original.Transport != original.Binding.Name {
		return nil, fmt.Errorf("observation requires the exact original typed schedule request")
	}
	request := original
	request.PostType = "observe"
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	a := &ExecTransportAdapter{Name: original.Transport, Home: homePath, contentBinding: &original.Binding, requestDigest: ContentTransportDigest(body)}
	if err := a.write(envelope.ActionTarget{}, body); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(a.lastOutput)) == 0 {
		return nil, fmt.Errorf("transport observation is empty")
	}
	return append([]byte(nil), a.lastOutput...), nil
}
