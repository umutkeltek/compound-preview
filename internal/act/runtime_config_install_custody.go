package act

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"compound/internal/envelope"
	"compound/internal/eventstore"
	"compound/internal/policy"
	"compound/internal/runattempt"
	"compound/internal/work"
	"golang.org/x/sys/unix"
)

// RuntimeConfigCaptureQualification is a metadata artifact produced by a real
// source qualification action. Its completed Work/Receipt/Transition must bind
// this exact artifact and the original capture Action/Policy. It cannot create
// source acceptance by presenting an unrelated successful Receipt.
type RuntimeConfigCaptureQualification struct {
	SchemaVersion     string                              `json:"schema_version"`
	WorkID            string                              `json:"work_id"`
	SourceInputSHA256 string                              `json:"source_input_sha256"`
	Action            policy.RuntimeConfigInstallProofRef `json:"action"`
	Policy            policy.RuntimeConfigInstallProofRef `json:"policy"`
	SourceAction      policy.RuntimeConfigInstallProofRef `json:"source_action"`
	SourcePolicy      policy.RuntimeConfigInstallProofRef `json:"source_policy"`
	SourceAttempt     policy.RuntimeConfigInstallProofRef `json:"source_attempt"`
	SourceResult      policy.RuntimeConfigInstallProofRef `json:"source_result"`
	SourcePublic      policy.RuntimeConfigInstallProofRef `json:"source_public"`
	SourceBinding     string                              `json:"source_binding"`
	ReceiverSHA256    string                              `json:"receiver_sha256"`
	RequestSHA256     string                              `json:"request_sha256"`
	SamplerSHA256     string                              `json:"sampler_sha256"`
	CapturePath       string                              `json:"capture_path"`
	CaptureStat       policy.RuntimeConfigInstallStat     `json:"capture_stat"`
	ProfileSHA256     string                              `json:"profile_sha256"`
	ProjectionSHA256  string                              `json:"projection_sha256"`
}

func runtimeConfigBytesHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func runtimeConfigStrict(raw []byte, value any) error {
	if len(raw) == 0 || len(raw) > 131072 || !credentialVerifyUniqueJSON(raw) {
		return runtimeConfigInstallError("proof_shape")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return runtimeConfigInstallError("proof_shape")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return runtimeConfigInstallError("proof_shape")
	}
	return nil
}
func (v *RuntimeConfigInstaller) proof(ref policy.RuntimeConfigInstallProofRef, artifact bool, value any) ([]byte, error) {
	var raw []byte
	var e error
	if artifact {
		var s *ArtifactStore
		s, e = NewArtifactStore(v.home)
		if e == nil {
			raw, e = s.Get(ref.Ref)
		}
	} else {
		var s *eventstore.Store
		s, e = eventstore.Open(v.home)
		if e == nil {
			var rec eventstore.Record
			rec, e = s.Read(ref.Ref)
			raw = rec.Bytes
		}
	}
	if e != nil || runtimeConfigBytesHash(raw) != ref.SHA256 {
		return nil, runtimeConfigInstallError("proof_digest")
	}
	if value != nil {
		if e = runtimeConfigStrict(raw, value); e != nil {
			return nil, e
		}
	}
	return raw, nil
}
func runtimeConfigEqualJSON(a, b any) bool {
	return runtimeConfigInstallHash(a) == runtimeConfigInstallHash(b)
}

func (v *RuntimeConfigInstaller) qualification(r policy.RuntimeConfigInstallRequest, now time.Time) error {
	q := r.Qualification
	var p CredentialVerificationPlan
	if _, e := v.proof(q.Plan, true, &p); e != nil {
		return e
	}
	verifier, e := NewCredentialVerifier(v.home)
	if e != nil {
		return e
	}
	original, ai, e := verifier.loadPlan(q.Plan.Ref, p.Actor)
	if e != nil || !reflect.DeepEqual(original, p) {
		return runtimeConfigInstallError("credential_plan")
	}
	var rec envelope.Receipt
	var tr envelope.Transition
	if _, e = v.proof(q.Receipt, false, &rec); e != nil {
		return e
	}
	if _, e = v.proof(q.Transition, false, &tr); e != nil {
		return e
	}
	attempt, e := runattempt.Open(v.home).Get(q.Attempt.Ref)
	if e != nil || runtimeConfigInstallHash(attempt) != q.Attempt.SHA256 || attempt.Status != runattempt.StatusSucceeded || attempt.ActionIntentID != ai.ID || attempt.WorkID != p.WorkID || attempt.TerminalizationOwner != credentialVerifyOwner || attempt.TerminalizationReceiptID != rec.ID || attempt.TerminalizationTransitionID != tr.ID || attempt.Launch == nil || attempt.Launch.CommittedAt == nil || attempt.Launch.StartErrorAt != nil {
		return runtimeConfigInstallError("credential_attempt")
	}
	if rec.Validate() != nil || tr.Validate() != nil || rec.Outcome != "completed" || rec.IDs.WorkID != p.WorkID || tr.IDs.WorkID != p.WorkID || !slices.Contains(tr.ActionRefs, ai.ID) || !slices.Contains(tr.AfterRefs, rec.ID) || !slices.Contains(rec.ProofRefs, q.Observation.Ref) || !slices.Contains(rec.ProofRefs, attempt.ID) {
		return runtimeConfigInstallError("credential_chain")
	}
	if now.Sub(attempt.CompletedAt) > policy.RuntimeConfigInstallApprovalTTL || attempt.CompletedAt.After(now) {
		return runtimeConfigInstallError("credential_stale")
	}
	raw, e := v.proof(q.Observation, true, nil)
	if e != nil {
		return e
	}
	if _, e = decodeCredentialVerificationObservation(raw, p.Request); e != nil {
		return runtimeConfigInstallError("credential_observation")
	}
	pr := p.Request
	f := r.Files[0]
	s := f.Stat
	if s == nil || !runtimeConfigEqualJSON(pr.Host, r.Host) || !runtimeConfigEqualJSON(pr.Resource, r.Resource) || !runtimeConfigEqualJSON(pr.SSH, r.SSH) || !runtimeConfigEqualJSON(pr.Roles, r.Roles) || pr.Database.Name != r.Database.Name || pr.Database.AllowConnections != r.Database.AllowConnections || pr.File.Path != f.Path || pr.File.Device != s.Device || pr.File.Inode != s.Inode || pr.File.Bytes != s.Bytes || pr.File.MtimeNS != s.MtimeNS || pr.File.CtimeNS != s.CtimeNS || pr.File.SHA256 != s.SHA256 {
		return runtimeConfigInstallError("credential_binding")
	}
	_, e = v.alignmentAcceptances(r)
	return e
}

func (v *RuntimeConfigInstaller) alignmentAcceptances(r policy.RuntimeConfigInstallRequest) ([]credentialPasswordAlignmentAcceptance, error) {
	var receipt envelope.Receipt
	if _, e := v.proof(r.Qualification.Receipt, false, &receipt); e != nil {
		return nil, e
	}
	created, e := time.Parse(time.RFC3339Nano, receipt.CreatedAt)
	if e != nil {
		return nil, runtimeConfigInstallError("credential_receipt_time")
	}
	if v.alignment == nil {
		v.alignment = &CredentialPasswordAligner{home: v.home}
	}
	if v.alignment.home != v.home {
		return nil, runtimeConfigInstallError("alignment_home_drift")
	}
	return v.alignment.installerFence(r.Files[0].Path,
		policy.CredentialVerificationDestination(policy.CredentialVerificationRequest{SSH: policy.CredentialVerifySSH{User: r.SSH.User, Alias: r.SSH.Alias}}), created)
}

func (v *RuntimeConfigInstaller) captureAuthority(r policy.RuntimeConfigInstallRequest) error {
	c := r.Capture
	var q RuntimeConfigCaptureQualification
	if _, e := v.proof(c.Qualification, true, &q); e != nil {
		return e
	}
	if q.SchemaVersion != "compound.runtime_config_capture_qualification.v0.2" || q.CapturePath != c.Path || q.CaptureStat != c.Stat || q.ProjectionSHA256 != c.ProjectionSHA256 || q.ProfileSHA256 != r.Profile.SHA256 {
		return runtimeConfigInstallError("capture_binding")
	}
	var rec envelope.Receipt
	var tr envelope.Transition
	var ai, sourceAI envelope.ActionIntent
	var pd, sourcePD envelope.PolicyDecision
	for _, item := range []struct {
		ref policy.RuntimeConfigInstallProofRef
		out any
	}{{c.Receipt, &rec}, {c.Transition, &tr}, {q.Action, &ai}, {q.Policy, &pd}, {q.SourceAction, &sourceAI}, {q.SourcePolicy, &sourcePD}} {
		if _, e := v.proof(item.ref, false, item.out); e != nil {
			return e
		}
	}
	wstore, e := work.OpenReadOnly(v.home)
	if e != nil {
		return runtimeConfigInstallError("capture_work")
	}
	w, e := wstore.Get(q.WorkID)
	if ai.Validate() != nil || pd.Validate() != nil || ai.ID != q.Action.Ref || pd.ID != q.Policy.Ref || sourceAI.ID != q.SourceAction.Ref || sourcePD.ID != q.SourcePolicy.Ref || rec.ID != c.Receipt.Ref || tr.ID != c.Transition.Ref {
		return runtimeConfigInstallError("capture_proof_identity")
	}
	if e != nil || w.State != "completed" || w.IDs.TransitionID != tr.ID || ai.IDs.WorkID != w.ID || pd.ActionIntentID != ai.ID || !policy.Allowed(pd) || ai.Capability != "runtime.capture_qualification.exact" || ai.ArgumentsHash != "sha256:"+RuntimeConfigCaptureQualificationDigest(q) || rec.IDs.WorkID != w.ID || rec.Outcome != "completed" || rec.Validate() != nil || tr.Validate() != nil || tr.IDs.WorkID != w.ID || !slices.Contains(rec.ProofRefs, c.Qualification.Ref) || !slices.Contains(tr.ActionRefs, ai.ID) || !slices.Contains(tr.AfterRefs, rec.ID) {
		return runtimeConfigInstallError("capture_chain")
	}
	terminal, e := wstore.ReadTerminalCloseProof(q.WorkID)
	if e != nil || terminal.ReceiptID != rec.ID || terminal.TransitionID != tr.ID || !runtimeCaptureSHA(q.SourceInputSHA256) || !ai.RequiresExactApproval || policy.NewExactActionApprovalStore(v.home).ValidateConsumedDecision(ai, pd) != nil {
		return runtimeConfigInstallError("capture_native_qualification")
	}
	if sourceAI.Validate() != nil || sourcePD.Validate() != nil || sourcePD.ActionIntentID != sourceAI.ID || !policy.Allowed(sourcePD) || !slices.Contains(rec.ProofRefs, sourceAI.ID) || !slices.Contains(rec.ProofRefs, sourcePD.ID) {
		return runtimeConfigInstallError("source_capture_chain")
	}
	// These are frozen metadata bodies only; never store the private capture as
	// an artifact. The parent qualification verifies receiver semantics once;
	// this owner rechecks every metadata binding and the exact private bytes.
	var originalAttempt struct {
		SchemaVersion string `json:"schema_version"`
		State         string `json:"state"`
		Binding       string `json:"binding_cid"`
		Action        string `json:"native_action_intent_id"`
		Policy        string `json:"native_policy_decision_id"`
		Receiver      string `json:"receiver_sha256"`
		Request       string `json:"request_sha256"`
		Sampler       string `json:"sampler_sha256"`
	}
	if _, e = v.proof(q.SourceAttempt, true, &originalAttempt); e != nil {
		return e
	}
	if originalAttempt.Binding != q.SourceBinding || originalAttempt.Action != sourceAI.ID || originalAttempt.Policy != sourcePD.ID || originalAttempt.Receiver != q.ReceiverSHA256 || originalAttempt.Request != q.RequestSHA256 || originalAttempt.Sampler != q.SamplerSHA256 {
		return runtimeConfigInstallError("source_attempt_binding")
	}
	var result map[string]json.RawMessage
	if _, e = v.proof(q.SourceResult, true, &result); e != nil {
		return e
	}
	var resultAI, resultPD, binding string
	_ = json.Unmarshal(result["native_action_intent_id"], &resultAI)
	_ = json.Unmarshal(result["native_policy_decision_id"], &resultPD)
	_ = json.Unmarshal(result["binding_cid"], &binding)
	var rawStat map[string]json.RawMessage
	if json.Unmarshal(result["private_artifact"], &rawStat) != nil {
		return runtimeConfigInstallError("source_result")
	}
	delete(rawStat, "name")
	// Source metadata used integral nanoseconds; the request retains decimal
	// strings to avoid float conversion. Normalize without a float round-trip.
	for _, key := range []string{"mtime_ns", "ctime_ns"} {
		var n json.Number
		if json.Unmarshal(rawStat[key], &n) == nil {
			rawStat[key], _ = json.Marshal(n.String())
		}
	}
	statRaw, _ := json.Marshal(rawStat)
	var sourceStat policy.RuntimeConfigInstallStat
	if runtimeConfigStrict(statRaw, &sourceStat) != nil || sourceStat != c.Stat || resultAI != sourceAI.ID || resultPD != sourcePD.ID || binding != q.SourceBinding {
		return runtimeConfigInstallError("source_result_binding")
	}
	var public struct {
		Binding string              `json:"binding_cid"`
		Fields  map[string][]string `json:"service_selected_fields"`
	}
	publicRaw, e := v.proof(q.SourcePublic, true, nil)
	if e != nil {
		return e
	}
	if json.Unmarshal(publicRaw, &public) != nil || public.Binding != q.SourceBinding || !slices.Equal(public.Fields["api"], r.Profile.APIFields) {
		return runtimeConfigInstallError("source_roster")
	}
	profileRaw, e := v.proof(policy.RuntimeConfigInstallProofRef{Ref: r.Profile.Ref, SHA256: r.Profile.SHA256}, true, nil)
	if e != nil {
		return e
	}
	// The frozen artifact has exactly the profile fields except self-reference.
	expected := r.Profile
	expected.Ref = ""
	expected.SHA256 = ""
	var frozen policy.RuntimeConfigInstallProfile
	if runtimeConfigStrict(profileRaw, &frozen) != nil || !reflect.DeepEqual(frozen, expected) {
		return runtimeConfigInstallError("profile_binding")
	}
	return nil
}

func runtimeConfigLocalStat(info os.FileInfo, raw []byte) (policy.RuntimeConfigInstallStat, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return policy.RuntimeConfigInstallStat{}, runtimeConfigInstallError("capture_stat")
	}
	ctime := ""
	rv := reflect.ValueOf(st).Elem()
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := rv.FieldByName(name)
		if field.IsValid() {
			sec := field.FieldByName("Sec").Int()
			ns := field.FieldByName("Nsec").Int()
			ctime = strconv.FormatInt(sec*1_000_000_000+ns, 10)
		}
	}
	return policy.RuntimeConfigInstallStat{Device: uint64(st.Dev), Inode: st.Ino, Bytes: info.Size(), MtimeNS: strconv.FormatInt(info.ModTime().UnixNano(), 10), CtimeNS: ctime, Mode: fmt.Sprintf("%04o", info.Mode().Perm()), UID: st.Uid, GID: st.Gid, Links: uint64(st.Nlink), SHA256: runtimeConfigBytesHash(raw)}, nil
}

// runtimeConfigReadCapture opens every parent descriptor without following a
// symlink. The same anchored chain is re-opened and compared after bounded read.
func runtimeConfigReadCapture(c policy.RuntimeConfigInstallCapture) ([]byte, error) {
	bad := runtimeConfigInstallError("capture_custody")
	open := func() (int, []string, error) {
		fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if e != nil {
			return -1, nil, bad
		}
		var chain []string
		parts := strings.Split(strings.TrimPrefix(c.Path, "/"), "/")
		for _, part := range parts[:len(parts)-1] {
			next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			unix.Close(fd)
			if e != nil {
				return -1, nil, bad
			}
			fd = next
			var st unix.Stat_t
			if unix.Fstat(fd, &st) != nil || (st.Uid != 0 && st.Uid != c.Stat.UID) || st.Mode&0022 != 0 {
				unix.Close(fd)
				return -1, nil, bad
			}
			chain = append(chain, fmt.Sprintf("%d:%d:%d:%d:%d", st.Dev, st.Ino, st.Uid, st.Gid, st.Mode))
		}
		file, e := unix.Openat(fd, filepath.Base(c.Path), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, nil, bad
		}
		return file, chain, nil
	}
	fd, chain, e := open()
	if e != nil {
		return nil, e
	}
	file := os.NewFile(uintptr(fd), "capture")
	defer file.Close()
	before, e := file.Stat()
	if e != nil || !before.Mode().IsRegular() || before.Size() != c.Stat.Bytes || before.Size() > 131072 {
		return nil, bad
	}
	raw, e := io.ReadAll(io.LimitReader(file, 131073))
	if e != nil || len(raw) > 131072 {
		return nil, bad
	}
	first, e := runtimeConfigLocalStat(before, raw)
	if e != nil || first != c.Stat {
		return nil, bad
	}
	after, e := file.Stat()
	if e != nil {
		return nil, bad
	}
	last, e := runtimeConfigLocalStat(after, raw)
	if e != nil || first != last {
		return nil, bad
	}
	check, again, e := open()
	if e != nil {
		return nil, bad
	}
	checkFile := os.NewFile(uintptr(check), "capture-check")
	defer checkFile.Close()
	info, e := checkFile.Stat()
	if e != nil || !slices.Equal(chain, again) {
		return nil, bad
	}
	same, e := runtimeConfigLocalStat(info, raw)
	if e != nil || same != first {
		return nil, bad
	}
	return raw, nil
}
func (v *RuntimeConfigInstaller) captureProjection(r policy.RuntimeConfigInstallRequest) (map[string]string, error) {
	if e := v.captureAuthority(r); e != nil {
		return nil, e
	}
	raw, e := runtimeConfigReadCapture(r.Capture)
	if e != nil {
		return nil, e
	}
	defer clear(raw)
	if !credentialVerifyUniqueJSON(raw) {
		return nil, runtimeConfigInstallError("capture_ambiguous")
	}
	var source struct {
		Binding  string `json:"binding_cid"`
		Services map[string]struct {
			Record struct {
				Selected map[string]string `json:"selected"`
			} `json:"record"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &source) != nil {
		return nil, runtimeConfigInstallError("capture_shape")
	}
	var q RuntimeConfigCaptureQualification
	if _, e = v.proof(r.Capture.Qualification, true, &q); e != nil {
		return nil, e
	}
	selected := source.Services["api"].Record.Selected
	if source.Binding != q.SourceBinding || len(selected) != len(r.Profile.APIFields) {
		return nil, runtimeConfigInstallError("capture_roster")
	}
	for _, key := range r.Profile.APIFields {
		value, ok := selected[key]
		if !ok || !runtimeConfigDotenvValue(value) {
			return nil, runtimeConfigInstallError("capture_value")
		}
	}
	if runtimeConfigProjectionHash(selected) != r.Capture.ProjectionSHA256 {
		return nil, runtimeConfigInstallError("capture_projection")
	}
	return selected, nil
}
func runtimeConfigDotenvValue(value string) bool {
	if len(value) == 0 || len(value) > 4096 {
		return false
	}
	for _, c := range value {
		if c < 32 || c >= 127 || strings.ContainsRune("#$\\\"'`", c) {
			return false
		}
	}
	return true
}

// RuntimeConfigCaptureQualificationDigest excludes the two backreferences to
// the action/decision that attest this manifest, avoiding circular digests.
func RuntimeConfigCaptureQualificationDigest(q RuntimeConfigCaptureQualification) string {
	q.Action = policy.RuntimeConfigInstallProofRef{}
	q.Policy = policy.RuntimeConfigInstallProofRef{}
	return runtimeConfigInstallHash(q)
}
