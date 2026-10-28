package routeevidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	homepkg "compound/internal/home"
)

const RequestClaimSchemaVersion = "compound.execution_route_request_claim.v0.1"

var requestClaimBeforeReleaseRemove = func() {}

// RequestClaim is a durable, identity-bound right to record one request_id.
// It closes the route-ledger check-to-call gap without holding the shared
// authority lock across provider I/O.
type RequestClaim struct {
	SchemaVersion  string `json:"schema_version"`
	ID             string `json:"id"`
	RequestID      string `json:"request_id"`
	WorkID         string `json:"work_id,omitempty"`
	ClientProfile  string `json:"client_profile"`
	Protocol       string `json:"protocol"`
	SelectedTarget Target `json:"selected_target"`
}

func BuildRequestClaim(requestID, workID, clientProfile, protocol string, target Target) (RequestClaim, error) {
	claim := RequestClaim{
		SchemaVersion:  RequestClaimSchemaVersion,
		RequestID:      strings.TrimSpace(requestID),
		WorkID:         strings.TrimSpace(workID),
		ClientProfile:  strings.TrimSpace(clientProfile),
		Protocol:       strings.TrimSpace(protocol),
		SelectedTarget: target,
	}
	claim.SelectedTarget.Provider = strings.TrimSpace(claim.SelectedTarget.Provider)
	claim.SelectedTarget.Model = strings.TrimSpace(claim.SelectedTarget.Model)
	claim.SelectedTarget.Protocol = strings.TrimSpace(claim.SelectedTarget.Protocol)
	claim.ID = requestClaimID(claim)
	if err := claim.Validate(); err != nil {
		return RequestClaim{}, err
	}
	return claim, nil
}

func (c RequestClaim) Validate() error {
	var problems []string
	if c.SchemaVersion != RequestClaimSchemaVersion {
		problems = append(problems, "schema_version must be "+RequestClaimSchemaVersion)
	}
	if strings.TrimSpace(c.RequestID) == "" || c.RequestID != strings.TrimSpace(c.RequestID) {
		problems = append(problems, "request_id required and canonical")
	}
	if strings.TrimSpace(c.ClientProfile) == "" {
		problems = append(problems, "client_profile required")
	}
	if strings.TrimSpace(c.Protocol) == "" {
		problems = append(problems, "protocol required")
	}
	if strings.TrimSpace(c.SelectedTarget.Provider) == "" || strings.TrimSpace(c.SelectedTarget.Model) == "" {
		problems = append(problems, "selected_target provider and model required")
	}
	if c.SelectedTarget.Protocol != c.Protocol {
		problems = append(problems, "selected_target protocol must match protocol")
	}
	if c.ID != requestClaimID(c) {
		problems = append(problems, "id does not match claim identity")
	}
	if len(problems) > 0 {
		return fmt.Errorf("execution route request claim invalid: %s", strings.Join(problems, "; "))
	}
	return nil
}

// ClaimRequestID atomically claims an unused request id under route evidence's
// shared writer authority. The durable claim remains until its owner records
// matching evidence; a crash therefore fails closed instead of replaying spend.
func ClaimRequestID(homePath string, claim RequestClaim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	return homepkg.WithAutonomyAdmissionLock(homePath, func() error {
		if existing, ok, err := uniqueEvidenceForRequestID(homePath, claim.RequestID); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("execution route evidence request_id %q already recorded as %s", claim.RequestID, existing.ID)
		}
		if existing, ok, err := loadRequestClaim(homePath, claim.RequestID); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("execution route evidence request_id %q already claimed as %s", claim.RequestID, existing.ID)
		}
		return writeRequestClaimDurable(homePath, requestClaimPath(homePath, claim.RequestID), claim)
	})
}

func authorizeRequestClaimLocked(homePath string, ev Evidence, supplied *RequestClaim) (bool, error) {
	stored, present, err := loadRequestClaim(homePath, ev.RequestID)
	if err != nil {
		return false, err
	}
	if !present {
		if supplied != nil {
			return false, fmt.Errorf("execution route evidence request_id %q has no active claim", ev.RequestID)
		}
		return false, nil
	}
	if supplied == nil {
		return false, fmt.Errorf("execution route evidence request_id %q is claimed as %s", ev.RequestID, stored.ID)
	}
	if err := supplied.Validate(); err != nil {
		return false, err
	}
	if stored != *supplied {
		return false, fmt.Errorf("execution route evidence request_id %q claim identity mismatch", ev.RequestID)
	}
	if stored.RequestID != ev.RequestID || stored.WorkID != ev.WorkID ||
		stored.ClientProfile != ev.ClientProfile || stored.Protocol != ev.Protocol ||
		stored.SelectedTarget != ev.SelectedTarget {
		return false, fmt.Errorf("execution route evidence request_id %q does not match its claim", ev.RequestID)
	}
	return true, nil
}

func releaseRequestClaimLocked(homePath string, claim RequestClaim) error {
	path := requestClaimPath(homePath, claim.RequestID)
	return homepkg.RemoveRootedRegularFileDurableValidated(homePath, path, "request claim", func(data []byte) error {
		stored, err := decodeRequestClaim(data, path, claim.RequestID)
		if err != nil {
			return err
		}
		if stored != claim {
			return fmt.Errorf("execution route evidence request_id %q claim changed before release", claim.RequestID)
		}
		requestClaimBeforeReleaseRemove()
		return nil
	})
}

func loadRequestClaim(homePath, requestID string) (RequestClaim, bool, error) {
	path := requestClaimPath(homePath, requestID)
	data, err := homepkg.ReadRootedRegularFile(homePath, path, "request claim")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RequestClaim{}, false, nil
		}
		return RequestClaim{}, false, err
	}
	claim, err := decodeRequestClaim(data, path, requestID)
	if err != nil {
		return RequestClaim{}, false, err
	}
	return claim, true, nil
}

func decodeRequestClaim(data []byte, path, requestID string) (RequestClaim, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var claim RequestClaim
	if err := dec.Decode(&claim); err != nil {
		return RequestClaim{}, fmt.Errorf("decode execution route request claim %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return RequestClaim{}, fmt.Errorf("decode execution route request claim %s: trailing JSON", path)
		}
		return RequestClaim{}, fmt.Errorf("decode execution route request claim %s trailing JSON: %w", path, err)
	}
	if err := claim.Validate(); err != nil {
		return RequestClaim{}, err
	}
	if claim.RequestID != requestID {
		return RequestClaim{}, fmt.Errorf("execution route request claim path conflicts with request_id %q", requestID)
	}
	return claim, nil
}

func requestClaimPath(homePath, requestID string) string {
	sum := sha256.Sum256([]byte(requestID))
	return filepath.Join(homePath, "ledger", "execution-route-request-claims", hex.EncodeToString(sum[:])+".json")
}

func requestClaimID(claim RequestClaim) string {
	claim.ID = ""
	data, _ := json.Marshal(claim)
	return "routeclaim_" + digestHex(data)[:12]
}

func writeRequestClaimDurable(homePath, path string, claim RequestClaim) error {
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return homepkg.WriteRootedNewFileDurable(homePath, path, "request claim", data, 0o644)
}
