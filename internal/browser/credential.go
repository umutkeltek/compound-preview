package browser

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"compound/internal/capgraph"
	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
)

const (
	jevCredentialSurface = "tool:browser-jev-typesafe"
	jevCredentialRef     = "home-secret://typesafe/jev-api-key"
)

func jevCredentialHandle() capgraph.CredentialHandle {
	return capgraph.CredentialHandle{Ref: jevCredentialRef, Custodian: "compound-home", EnvName: "TYPESAFE_API_KEY"}
}

type CredentialChange struct {
	Stored       bool   `json:"stored"`
	Refused      bool   `json:"refused,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ActionID     string `json:"action_intent_id,omitempty"`
	DecisionID   string `json:"policy_decision_id,omitempty"`
	ReceiptID    string `json:"receipt_id,omitempty"`
	TransitionID string `json:"transition_id,omitempty"`
}

// StoreTypeSafeCredential is the closed, stdin-fed enrollment door for Jev's
// single provider slot. Neither the key nor a key-derived digest enters an
// envelope, registry, receipt, or error message.
func StoreTypeSafeCredential(home, workID string, key []byte, cfg policy.Config, now time.Time) (change CredentialChange, err error) {
	return setTypeSafeCredential(home, workID, key, cfg, now, false)
}

// RotateTypeSafeCredential replaces only the exact registered Jev slot through
// the same security-class policy gate; no credential bytes enter audit state.
func RotateTypeSafeCredential(home, workID string, key []byte, cfg policy.Config, now time.Time) (CredentialChange, error) {
	return setTypeSafeCredential(home, workID, key, cfg, now, true)
}

func setTypeSafeCredential(home, workID string, key []byte, cfg policy.Config, now time.Time, replace bool) (change CredentialChange, err error) {
	if !strings.HasPrefix(workID, "work_") || len(key) == 0 || len(key) > 4096 || strings.ContainsAny(string(key), "\r\n\x00") {
		return CredentialChange{}, errors.New("credential enrollment requires work_id and one bounded nonempty key")
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	operation := "enroll"
	if replace {
		operation = "rotate"
	}
	digest := sha256.Sum256([]byte(workID + "\x00" + jevCredentialSurface + "\x00" + operation + "\x00" + stamp))
	intent := envelope.ActionIntent{
		Header: envelope.Header{SchemaVersion: envelope.SVActionIntent, ID: "act_" + hex.EncodeToString(digest[:8]), CreatedAt: stamp,
			Producer: envelope.Producer{Kind: "cli", Name: "compound"}, IDs: envelope.IDs{WorkID: workID},
			Authority: envelope.Authority{SourceClass: "runtime", Status: "current"}, Privacy: "private", Risk: "tier1",
			EvidenceRefs: []string{}, Uncertainty: envelope.Uncertainty{Level: "known"}},
		Actor: "compound.browser", Capability: "browser.credential.store", Action: "enroll_typesafe_key",
		Target:        envelope.ActionTarget{Kind: "credential_slot", ID: jevCredentialSurface},
		MutationClass: "security", Reversibility: "hard", BlastRadius: "local",
		Preview:      "enroll Jev provider credential in Compound custody",
		RollbackPlan: "operator removes or replaces the exact Compound credential slot through a governed credential action",
	}
	if replace {
		intent.Action = "rotate_typesafe_key"
		intent.Preview = "replace the Jev provider credential in Compound custody"
	}
	if err := intent.Validate(); err != nil {
		return CredentialChange{}, err
	}
	decision := policy.Evaluate(intent, cfg, now)
	if err := decision.Validate(); err != nil {
		return CredentialChange{}, err
	}
	store, err := eventstore.Open(home)
	if err != nil {
		return CredentialChange{}, err
	}
	if err := recordEvent(store, intent.ID, &intent); err != nil {
		return CredentialChange{}, err
	}
	if err := recordEvent(store, decision.ID, &decision); err != nil {
		return CredentialChange{}, err
	}
	if err := policy.Gate(decision); err != nil {
		receiptID, transitionID, recordErr := recordStep(store, intent, "blocked", "Jev credential enrollment held by policy")
		if recordErr != nil {
			return CredentialChange{}, recordErr
		}
		return CredentialChange{Refused: true, Reason: err.Error(), ActionID: intent.ID, DecisionID: decision.ID, ReceiptID: receiptID, TransitionID: transitionID}, nil
	}
	defer func() {
		if err == nil || change.ReceiptID != "" {
			return
		}
		// A prepared mutation may already have changed the live slot. Its
		// original receipt is recovered from the journal, not contradicted by
		// a failed receipt for the same ActionIntent.
		if pending, readErr := readPendingCredentialProof(home); readErr == nil &&
			len(pending.Transition.ActionRefs) == 1 && pending.Transition.ActionRefs[0] == intent.ID {
			return // This exact action still owns a recoverable transaction.
		}
		receiptID, transitionID, recordErr := recordStep(store, intent, "failed", "Jev credential operation failed; inspect exact slot before retry")
		if recordErr != nil {
			err = errors.Join(err, recordErr)
			return
		}
		change.ActionID, change.DecisionID = intent.ID, decision.ID
		change.ReceiptID, change.TransitionID = receiptID, transitionID
	}()
	path := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	lock, err := lockCredentialSlot(home)
	if err != nil {
		return CredentialChange{}, err
	}
	defer unlockCredentialSlot(lock)
	if err := repairPendingCredentialProof(home, store); err != nil {
		return CredentialChange{}, err
	}
	graph, err := capgraph.Open(home)
	if err != nil {
		return CredentialChange{}, err
	}
	surfaces, err := graph.ConnectionSurfaces()
	if err != nil {
		return CredentialChange{}, err
	}
	registered := false
	for _, surface := range surfaces {
		if surface.NodeID == jevCredentialSurface {
			if surface.CredentialHandle != jevCredentialHandle() || surface.AuthMode != "api_key" || surface.AssertingOwner != "compound.browser" {
				return CredentialChange{}, errors.New("Jev credential surface already exists with an unexpected binding")
			}
			registered = true
		}
	}
	if replace && !registered {
		return CredentialChange{}, errors.New("credential rotation requires the exact registered Jev surface")
	}
	if err := secureCredentialParents(home); err != nil {
		return CredentialChange{}, err
	}
	info, statErr := os.Lstat(path)
	existing := statErr == nil
	var postCommitErr error
	if statErr != nil && !os.IsNotExist(statErr) {
		return CredentialChange{}, statErr
	}
	if existing {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return CredentialChange{}, errors.New("existing credential slot is not a regular mode-0600 file")
		}
		stored, readErr := os.ReadFile(path)
		if readErr != nil {
			return CredentialChange{}, errors.New("credential slot could not be read")
		}
		if subtle.ConstantTimeCompare(stored, key) != 1 && !replace {
			return CredentialChange{}, errors.New("credential slot already contains another value; enrollment does not replace secrets")
		}
		if replace && subtle.ConstantTimeCompare(stored, key) != 1 {
			if _, _, err := prepareCredentialStored(home, intent, decision.ID, now, key, stored); err != nil {
				return CredentialChange{}, err
			}
			committed, replaceErr := replaceCredentialFile(path, key)
			if replaceErr != nil && !committed {
				return CredentialChange{}, replaceErr
			}
			postCommitErr = replaceErr
			existing = false // A real state delta requires a completed receipt.
		}
	} else {
		if replace {
			return CredentialChange{}, errors.New("credential rotation requires an existing secret slot")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return CredentialChange{}, err
		}
		if err := secureCredentialParents(home); err != nil {
			return CredentialChange{}, err
		}
		if _, _, err := prepareCredentialStored(home, intent, decision.ID, now, key, nil); err != nil {
			return CredentialChange{}, err
		}
		pending, readErr := readPendingCredentialProof(home)
		if readErr != nil {
			return CredentialChange{}, readErr
		}
		if linkErr := linkStagedCredential(home, pending.NewSecret); linkErr != nil {
			return CredentialChange{}, linkErr
		}
	}
	if !registered {
		if err := capgraph.AddGoverned(home, capgraph.Node{ID: jevCredentialSurface, Kind: "tool", Name: "Jev TypeSafe provider", Triggers: []string{"browser", "jev"}}, now); err != nil {
			return CredentialChange{}, err
		}
		_, err = capgraph.AddConnectionSurfaceGoverned(home, capgraph.ConnectionSurface{
			NodeID: jevCredentialSurface, AuthMode: "api_key", AssertingOwner: "compound.browser",
			CredentialHandle: jevCredentialHandle(),
		}, now)
		if err != nil {
			return CredentialChange{}, err
		}
	}
	if existing && registered {
		receiptID, transitionID, recordErr := recordStep(store, intent, "no_op", "Jev credential slot already enrolled")
		return CredentialChange{Stored: recordErr == nil, ActionID: intent.ID, DecisionID: decision.ID, ReceiptID: receiptID, TransitionID: transitionID}, recordErr
	}
	if postCommitErr != nil {
		// Rename changed the live slot, but failed directory sync leaves crash
		// durability uncertain. Never publish a completed state-delta receipt.
		return CredentialChange{ActionID: intent.ID, DecisionID: decision.ID}, fmt.Errorf("credential slot changed but durability is unverified: %w", postCommitErr)
	}
	ids, err := readPendingCredentialProof(home)
	if err != nil {
		return CredentialChange{}, err
	}
	receiptID, transitionID := ids.Receipt.ID, ids.Transition.ID
	err = repairPendingCredentialProof(home, store)
	if err != nil {
		// The exact slot stays recoverable on retry if audit publication fails.
		return CredentialChange{Stored: true, ActionID: intent.ID, DecisionID: decision.ID, ReceiptID: receiptID, TransitionID: transitionID}, errors.Join(err, postCommitErr)
	}
	return CredentialChange{Stored: true, ActionID: intent.ID, DecisionID: decision.ID, ReceiptID: receiptID, TransitionID: transitionID}, postCommitErr
}

func replaceCredentialFile(path string, key []byte) (bool, error) {
	return replaceCredentialFileWithSync(path, key, func(dir *os.File) error { return dir.Sync() })
}

func replaceCredentialFileWithSync(path string, key []byte, syncDir func(*os.File) error) (bool, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".jev-api-key-*")
	if err != nil {
		return false, err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return false, err
	}
	if n, err := file.Write(key); err != nil || n != len(key) {
		_ = file.Close()
		return false, errors.New("credential replacement write failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return false, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return true, err
	}
	defer dir.Close()
	return true, syncDir(dir)
}

// The content-free lock also journals one incomplete Receipt -> Transition
// pair. A retry repairs the exact prior transition before changing the slot.
func lockCredentialSlot(home string) (*os.File, error) {
	if err := secureCredentialParents(home); err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "secrets", "typesafe")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := secureCredentialParents(home); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".jev-api-key.lock")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, errors.New("credential lock must be a regular mode-0600 file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return platformLockCredential(path)
}

func unlockCredentialSlot(file *os.File) {
	platformUnlockCredential(file)
	_ = file.Close()
}

type pendingCredentialProof struct {
	Receipt    envelope.Receipt    `json:"receipt"`
	Transition envelope.Transition `json:"transition"`
	DecisionID string              `json:"decision_id"`
	NewSecret  string              `json:"new_secret"`
	OldSecret  string              `json:"old_secret,omitempty"`
	Abandoned  bool                `json:"abandoned,omitempty"`
}

func credentialJournalPath(home string) string {
	return filepath.Join(home, "registries", "browser-credential-pending.json")
}

func writePendingCredentialProof(home string, receipt envelope.Receipt, transition envelope.Transition, decisionID, newSecret, oldSecret string) error {
	return persistPendingCredentialProof(home, pendingCredentialProof{Receipt: receipt, Transition: transition, DecisionID: decisionID, NewSecret: newSecret, OldSecret: oldSecret}, false)
}

func persistPendingCredentialProof(home string, pending pendingCredentialProof, replace bool) error {
	path := credentialJournalPath(home)
	if !replace {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("credential proof journal already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	data, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".browser-credential-pending-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if n, err := file.Write(data); err != nil || n != len(data) {
		_ = file.Close()
		return errors.New("credential proof journal write failed")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func clearPendingCredentialProof(home string) error {
	path := credentialJournalPath(home)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func finishPendingCredentialProof(home string, pending pendingCredentialProof) error {
	for _, name := range []string{pending.NewSecret, pending.OldSecret} {
		if name == "" {
			continue
		}
		path, err := stagedCredentialPath(home, name)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	secretDir, err := os.Open(filepath.Join(home, "secrets", "typesafe"))
	if err != nil {
		return err
	}
	syncErr := secretDir.Sync()
	closeErr := secretDir.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return clearPendingCredentialProof(home)
}

func readPendingCredentialProof(home string) (pendingCredentialProof, error) {
	path := credentialJournalPath(home)
	info, err := os.Lstat(path)
	if err != nil {
		return pendingCredentialProof{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 32768 {
		return pendingCredentialProof{}, errors.New("credential proof journal is not a private bounded file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pendingCredentialProof{}, err
	}
	var pending pendingCredentialProof
	if err := json.Unmarshal(data, &pending); err != nil {
		return pendingCredentialProof{}, errors.New("credential proof journal is malformed")
	}
	return pending, nil
}

func stagedCredentialPath(home, name string) (string, error) {
	if !strings.HasPrefix(name, ".jev-stage-") || filepath.Base(name) != name || strings.Contains(name, "..") {
		return "", errors.New("credential proof journal has invalid secret reference")
	}
	return filepath.Join(home, "secrets", "typesafe", name), nil
}

func readStagedCredential(home, name string) ([]byte, error) {
	path, err := stagedCredentialPath(home, name)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, os.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 4096 {
		return nil, errors.New("credential proof stage is not a private bounded file")
	}
	return os.ReadFile(path)
}

func stageCredentialSecret(home string, key []byte) (string, error) {
	dir := filepath.Join(home, "secrets", "typesafe")
	file, err := os.CreateTemp(dir, ".jev-stage-")
	if err != nil {
		return "", err
	}
	name := filepath.Base(file.Name())
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0o600); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	if n, err := file.Write(key); err != nil || n != len(key) {
		_ = os.Remove(file.Name())
		return "", errors.New("credential stage write failed")
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	d, err := os.Open(dir)
	if err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		_ = os.Remove(file.Name())
		return "", err
	}
	return name, nil
}

func linkStagedCredential(home, stageName string) error {
	stagePath, err := stagedCredentialPath(home, stageName)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	if err := os.Link(stagePath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func repairPendingCredentialProof(home string, store *eventstore.Store) error {
	return repairPendingCredentialProofWithJournalSync(home, store, syncCredentialJournal)
}

func syncCredentialJournal(home string) error {
	dir, err := os.Open(filepath.Dir(credentialJournalPath(home)))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func repairPendingCredentialProofWithJournalSync(home string, store *eventstore.Store, syncJournal func(string) error) error {
	pending, err := readPendingCredentialProof(home)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	receipt, transition := pending.Receipt, pending.Transition
	if receipt.Validate() != nil || transition.Validate() != nil ||
		len(transition.AfterRefs) != 1 || transition.AfterRefs[0] != receipt.ID ||
		len(transition.ActionRefs) != 1 ||
		(pending.Abandoned && receipt.Outcome != "failed") ||
		(!pending.Abandoned && receipt.Outcome != "completed") {
		return errors.New("credential proof journal is invalid")
	}
	actionRecord, err := store.Read(transition.ActionRefs[0])
	if err != nil {
		return err
	}
	var intent envelope.ActionIntent
	if err := json.Unmarshal(actionRecord.Bytes, &intent); err != nil ||
		intent.Capability != "browser.credential.store" || intent.Target.ID != jevCredentialSurface {
		return errors.New("credential proof journal names an unrelated action")
	}
	decisionRecord, err := store.Read(pending.DecisionID)
	if err != nil {
		return err
	}
	var decision envelope.PolicyDecision
	if err := json.Unmarshal(decisionRecord.Bytes, &decision); err != nil || decision.ID != pending.DecisionID || decision.ActionIntentID != intent.ID || policy.Gate(decision) != nil {
		return errors.New("credential proof journal lacks its allowing policy decision")
	}
	receiptExists, err := store.Exists(receipt.ID)
	if err != nil {
		return err
	}
	transitionExists, err := store.Exists(transition.ID)
	if err != nil {
		return err
	}
	if receipt.IDs.WorkID != intent.IDs.WorkID || transition.IDs.WorkID != intent.IDs.WorkID ||
		(intent.Action == "rotate_typesafe_key") != (pending.OldSecret != "") {
		return errors.New("credential proof journal action lineage is invalid")
	}
	// The prior writer may have renamed the journal and then failed the
	// directory sync. A retry must make that preparation durable before it
	// can change the live credential slot.
	if err := syncJournal(home); err != nil {
		return err
	}
	if receiptExists && transitionExists {
		return recordPendingCredentialEvents(home, store, pending)
	}
	if pending.Abandoned {
		return recordPendingCredentialEvents(home, store, pending)
	}
	newKey, err := readStagedCredential(home, pending.NewSecret)
	if err != nil {
		if os.IsNotExist(err) && !receiptExists && !transitionExists {
			if _, statErr := os.Lstat(filepath.Join(home, "secrets", "typesafe", "jev-api-key")); os.IsNotExist(statErr) {
				pending.Receipt.Outcome = "failed"
				pending.Receipt.MoveType = "audit_only"
				pending.Receipt.StateDelta = envelope.StateDelta{Changed: false, Summary: "Jev credential preparation was absent after restore; no live slot changed"}
				pending.Receipt.ProofRefs = []string{}
				pending.Transition.StateDelta = pending.Receipt.StateDelta.Summary
				pending.Transition.SecondOrder.FutureBehaviorChanged = false
				pending.Abandoned = true
				if err := pending.Receipt.Validate(); err != nil {
					return err
				}
				if err := pending.Transition.Validate(); err != nil {
					return err
				}
				if err := persistPendingCredentialProof(home, pending, true); err != nil {
					return err
				}
				return recordPendingCredentialEvents(home, store, pending)
			}
		}
		return err
	}
	secretPath := filepath.Join(home, "secrets", "typesafe", "jev-api-key")
	secretInfo, err := os.Lstat(secretPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var live []byte
	if err == nil {
		if !secretInfo.Mode().IsRegular() || secretInfo.Mode().Perm() != 0o600 {
			return errors.New("credential proof repair needs a mode-0600 live slot")
		}
		live, err = os.ReadFile(secretPath)
		if err != nil {
			return err
		}
	}
	if subtle.ConstantTimeCompare(live, newKey) != 1 {
		if pending.OldSecret == "" {
			if secretInfo != nil {
				return errors.New("credential proof live slot conflicts with prepared enrollment")
			}
			if err := linkStagedCredential(home, pending.NewSecret); err != nil {
				return err
			}
		} else {
			oldKey, readErr := readStagedCredential(home, pending.OldSecret)
			if readErr != nil {
				return readErr
			}
			if subtle.ConstantTimeCompare(live, oldKey) != 1 {
				return errors.New("credential proof live slot conflicts with prepared rotation")
			}
			if _, replaceErr := replaceCredentialFile(secretPath, newKey); replaceErr != nil {
				return replaceErr
			}
		}
	}
	secretDir, err := os.Open(filepath.Dir(secretPath))
	if err != nil {
		return err
	}
	syncErr := secretDir.Sync()
	closeErr := secretDir.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	graph, err := capgraph.Open(home)
	if err != nil {
		return err
	}
	surfaces, err := graph.ConnectionSurfaces()
	if err != nil {
		return err
	}
	var bound bool
	for _, surface := range surfaces {
		if surface.NodeID == jevCredentialSurface &&
			surface.CredentialHandle == jevCredentialHandle() &&
			surface.AuthMode == "api_key" && surface.AssertingOwner == "compound.browser" {
			bound = true
		}
	}
	if !bound {
		if err := capgraph.AddGoverned(home, capgraph.Node{ID: jevCredentialSurface, Kind: "tool", Name: "Jev TypeSafe provider", Triggers: []string{"browser", "jev"}}, time.Now()); err != nil {
			return err
		}
		if _, err := capgraph.AddConnectionSurfaceGoverned(home, capgraph.ConnectionSurface{NodeID: jevCredentialSurface, AuthMode: "api_key", AssertingOwner: "compound.browser", CredentialHandle: jevCredentialHandle()}, time.Now()); err != nil {
			return err
		}
	}
	return recordPendingCredentialEvents(home, store, pending)
}

func recordPendingCredentialEvents(home string, store *eventstore.Store, pending pendingCredentialProof) error {
	receipt, transition := pending.Receipt, pending.Transition
	for _, item := range []struct {
		id    string
		value any
		equal func([]byte) bool
	}{
		{receipt.ID, &receipt, func(raw []byte) bool {
			var stored envelope.Receipt
			return json.Unmarshal(raw, &stored) == nil && reflect.DeepEqual(stored, receipt)
		}},
		{transition.ID, &transition, func(raw []byte) bool {
			var stored envelope.Transition
			return json.Unmarshal(raw, &stored) == nil && reflect.DeepEqual(stored, transition)
		}},
	} {
		exists, err := store.Exists(item.id)
		if err != nil {
			return err
		}
		if exists {
			record, err := store.Read(item.id)
			if err != nil || !item.equal(record.Bytes) {
				return errors.New("credential proof journal conflicts with stored event")
			}
		} else if err := recordEvent(store, item.id, item.value); err != nil {
			return err
		}
	}
	return finishPendingCredentialProof(home, pending)
}

func secureCredentialParents(home string) error {
	for _, path := range []string{filepath.Join(home, "secrets"), filepath.Join(home, "secrets", "typesafe")} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return errors.New("credential custody directories must be private real directories")
		}
	}
	return nil
}

func prepareCredentialStored(home string, intent envelope.ActionIntent, decisionID string, now time.Time, key, old []byte) (string, string, error) {
	digest := sha256.Sum256([]byte(intent.ID + "\x00stored"))
	summary := "Jev TypeSafe credential slot enrolled in Compound custody"
	if intent.Action == "rotate_typesafe_key" {
		summary = "Jev TypeSafe credential slot replaced in Compound custody"
	}
	receipt := envelope.Receipt{
		Header: intent.Header, Outcome: "completed", MoveType: "control_plane_repair",
		StateDelta: envelope.StateDelta{Changed: true, Summary: summary},
		Cost:       envelope.ReceiptCost{ModelCalls: []string{}},
		Skills:     envelope.ReceiptSkills{Used: []string{"browser.credential.store"}, Missed: []string{}, Candidate: []string{}, UsedDeclared: true},
		Memory:     envelope.ReceiptMemory{Candidates: []string{}, Promoted: []string{}, Parked: []string{}},
		Next:       envelope.ReceiptNext{SuggestedWork: []string{}},
		ProofRefs:  []string{"check:jev-credential-slot-mode-0600"},
	}
	receipt.SchemaVersion, receipt.ID, receipt.CreatedAt = envelope.SVReceipt, "rec_"+hex.EncodeToString(digest[:8]), now.UTC().Format(time.RFC3339Nano)
	if err := receipt.Validate(); err != nil {
		return "", "", err
	}
	transition := envelope.Transition{
		Header: intent.Header, BeforeRefs: []string{}, ActionRefs: []string{intent.ID}, AfterRefs: []string{receipt.ID},
		StateDelta:  summary,
		Projections: envelope.Projections{Memory: []string{}, Skill: []string{}, Product: []string{}, Cost: []string{}, Risk: []string{}, Taste: []string{}, Routing: []string{}, HumanAttention: []string{}},
		SecondOrder: envelope.SecondOrder{FutureBehaviorChanged: true},
	}
	transition.SchemaVersion, transition.ID, transition.CreatedAt = envelope.SVTransition, "tr_"+hex.EncodeToString(digest[:8]), now.UTC().Format(time.RFC3339Nano)
	if err := transition.Validate(); err != nil {
		return "", "", err
	}
	newStage, err := stageCredentialSecret(home, key)
	if err != nil {
		return "", "", err
	}
	oldStage := ""
	if old != nil {
		oldStage, err = stageCredentialSecret(home, old)
		if err != nil {
			if path, pathErr := stagedCredentialPath(home, newStage); pathErr == nil {
				_ = os.Remove(path)
			}
			return "", "", err
		}
	}
	if err := writePendingCredentialProof(home, receipt, transition, decisionID, newStage, oldStage); err != nil {
		// A failed directory sync may have committed the journal rename.
		// Leave both stages intact if its existence is uncertain.
		if _, statErr := os.Lstat(credentialJournalPath(home)); os.IsNotExist(statErr) {
			for _, name := range []string{newStage, oldStage} {
				if name == "" {
					continue
				}
				if path, pathErr := stagedCredentialPath(home, name); pathErr == nil {
					_ = os.Remove(path)
				}
			}
		}
		return "", "", err
	}
	return receipt.ID, transition.ID, nil
}
