package work

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"compound/internal/envelope"
)

func TestDeclaredSessionExactInvalidRecordIsNotAbsent(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"malformed", "unreadable", "wrong-key", "wrong-session", "wrong-schema", "no-actor"} {
		t.Run(mode, func(t *testing.T) {
			s, cwd, _, claimAt := declaredFixture(t, "codex:holder")
			in, err := (PresenceSessionInput{Harness: "claude_code", SessionID: "declared-session", Cwd: cwd, Actor: "codex:holder"}).normalized()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(s.presenceSessionDir(), presenceSessionKey(in)+".json")
			record, err := readPresenceSession(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "malformed":
				err = os.WriteFile(path, []byte("{"), 0o600)
			case "unreadable":
				// A directory deterministically fails the owned file read even
				// when the test process can bypass permission bits.
				if err = os.Remove(path); err == nil {
					err = os.Mkdir(path, 0o700)
				}
			default:
				switch mode {
				case "wrong-key":
					record.Key = "unrelated-key"
				case "wrong-session":
					record.SessionID = "unrelated-session"
				case "wrong-schema":
					record.SchemaVersion = "unknown"
				case "no-actor":
					record.Actor = ""
				}
				blob, marshalErr := json.Marshal(record)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(path, blob, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			resolved, ok, err := s.resolveDeclaredCollaborationRecipient(declaredQuery(cwd), cwd, s.nonTerminalWorkByID(t), claimAt.Add(time.Minute))
			if ok || (err == nil && resolved.UnresolvedReason == "") {
				t.Fatalf("present invalid exact identity became absent or valid: ok=%v resolution=%+v err=%v", ok, resolved, err)
			}
		})
	}
}

func TestDeclaredSessionIgnoresUnrelatedInvalidRecord(t *testing.T) {
	t.Parallel()
	s, cwd, workID, claimAt := declaredFixture(t, "codex:holder")
	if err := os.WriteFile(filepath.Join(s.presenceSessionDir(), "unrelated.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := s.resolveDeclaredCollaborationRecipient(declaredQuery(cwd), cwd, s.nonTerminalWorkByID(t), claimAt.Add(time.Minute))
	if err != nil || !ok || resolved.WorkID != workID {
		t.Fatalf("unrelated invalid record blocked exact identity: ok=%v resolution=%+v err=%v", ok, resolved, err)
	}
	query := declaredQuery(cwd)
	query.SessionID = "never-declared"
	resolved, ok, err = s.resolveDeclaredCollaborationRecipient(query, cwd, s.nonTerminalWorkByID(t), claimAt.Add(time.Minute))
	if err != nil || ok || resolved.UnresolvedReason != "" {
		t.Fatalf("unrelated invalid record made an absent identity unresolved: ok=%v resolution=%+v err=%v", ok, resolved, err)
	}
}

// declaredFixture stands up an announced session whose actor holds one fresh
// lease -- the case the declared path is FOR.
func declaredFixture(t *testing.T, actor string) (*Store, string, string, time.Time) {
	t.Helper()
	s := newStore(t)
	cwd := t.TempDir()
	w, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "declared recipient"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	claimAt := fixedNow.Add(time.Minute)
	if _, err := s.Claim(w.ID, actor, claimAt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnnouncePresenceSession(PresenceSessionInput{
		Harness:   "claude_code",
		SessionID: "declared-session",
		Cwd:       cwd,
		Actor:     actor,
	}, claimAt); err != nil {
		t.Fatal(err)
	}
	return s, cwd, w.ID, claimAt
}

func declaredQuery(cwd string) CollaborationRecipientQuery {
	return CollaborationRecipientQuery{Harness: "claude_code", SessionID: "declared-session", Cwd: cwd}
}

// The point of the slice: a session Compound never spawned, with no delegate
// binding and no run attempt, resolves.
func TestDeclaredSessionResolvesWhenCorroborated(t *testing.T) {
	t.Parallel()
	s, cwd, workID, claimAt := declaredFixture(t, "claude-code:declared-agent")

	resolved, ok, err := s.resolveDeclaredCollaborationRecipient(
		declaredQuery(cwd), cwd, s.nonTerminalWorkByID(t), claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a corroborated declaration should resolve")
	}
	if resolved.Actor != "claude-code:declared-agent" {
		t.Fatalf("actor = %q", resolved.Actor)
	}
	if resolved.WorkID != workID {
		t.Fatalf("work = %q, want %q", resolved.WorkID, workID)
	}
	// Load-bearing: an empty RunAttemptID is what makes deliveryRelevant refuse
	// the interrupt class for this recipient, so it can only receive delivery at
	// a prompt boundary.
	if resolved.RunAttemptID != "" {
		t.Fatalf("declared recipient must carry no run attempt, got %q", resolved.RunAttemptID)
	}
}

// Declaration without corroboration adopts nothing. This is the same property
// presence enforces, now on the path that actually grants delivery.
func TestDeclaredSessionWithoutAnyLeaseNeverResolves(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	cwd := t.TempDir()
	if _, err := s.AnnouncePresenceSession(PresenceSessionInput{
		Harness:   "claude_code",
		SessionID: "declared-session",
		Cwd:       cwd,
		Actor:     "claude-code:claims-to-be-someone",
	}, fixedNow); err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := s.resolveDeclaredCollaborationRecipient(
		declaredQuery(cwd), cwd, s.nonTerminalWorkByID(t), fixedNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok || resolved.UnresolvedReason == "" {
		t.Fatal("an uncorroborated declaration must not resolve")
	}
}

// A lease that has gone cold stops corroborating. Without this an actor that
// stopped weeks ago would keep validating a session forever.
func TestDeclaredSessionWithStaleLeaseNeverResolves(t *testing.T) {
	t.Parallel()
	s, cwd, _, claimAt := declaredFixture(t, "codex:went-quiet")

	resolved, ok, err := s.resolveDeclaredCollaborationRecipient(
		declaredQuery(cwd), cwd, s.nonTerminalWorkByID(t),
		claimAt.Add(DefaultPresenceTTL+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ok || resolved.UnresolvedReason == "" {
		t.Fatal("a cold lease must not corroborate a session")
	}
}

// Two candidate works means we cannot say which context a message belongs to.
// Guessing would deliver one lane's coordination into another's, so refuse --
// mirroring the spawned path's exactly-one rule.
func TestDeclaredSessionWithTwoLeasesIsAmbiguousAndRefuses(t *testing.T) {
	t.Parallel()
	s, cwd, _, claimAt := declaredFixture(t, "claude-code:busy-agent")

	second, err := s.Create(CreateInput{Owner: "operator", SuccessPredicate: "second work"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(second.ID, "claude-code:busy-agent", claimAt); err != nil {
		t.Fatal(err)
	}

	resolved, ok, err := s.resolveDeclaredCollaborationRecipient(
		declaredQuery(cwd), cwd, s.nonTerminalWorkByID(t), claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok || resolved.UnresolvedReason == "" {
		t.Fatal("two candidate works must refuse rather than guess")
	}
}

// The declaration must match the EXACT session triple. A different session in
// the same directory is a different agent and must not inherit the identity --
// this is the "proximity never adopts" rule, on the new path.
func TestDeclaredResolutionRequiresTheExactSessionTriple(t *testing.T) {
	t.Parallel()
	s, cwd, _, claimAt := declaredFixture(t, "claude-code:declared-agent")

	other := declaredQuery(cwd)
	other.SessionID = "a-different-session-in-the-same-cwd"
	_, ok, err := s.resolveDeclaredCollaborationRecipient(
		other, cwd, s.nonTerminalWorkByID(t), claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a neighbouring session must not inherit another's declared identity")
	}
}

// A terminal work is not a delivery context. workByID excludes terminal states
// upstream; this pins that the declared path honours it rather than reaching
// around it via the lease, which outlives the close.
func TestDeclaredSessionIgnoresTerminalWork(t *testing.T) {
	t.Parallel()
	s, cwd, workID, claimAt := declaredFixture(t, "claude-code:finished-agent")

	byID := s.nonTerminalWorkByID(t)
	delete(byID, workID) // simulate the work having reached a terminal state

	_, ok, err := s.resolveDeclaredCollaborationRecipient(
		declaredQuery(cwd), cwd, byID, claimAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a lease on terminal work is not a delivery context")
	}
}

// nonTerminalWorkByID mirrors the map ResolveCollaborationRecipient builds.
func (s *Store) nonTerminalWorkByID(t *testing.T) map[string]envelope.WorkEnvelope {
	t.Helper()
	works, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]envelope.WorkEnvelope, len(works))
	for _, item := range works {
		if terminalStates[item.State] {
			continue
		}
		byID[item.ID] = item
	}
	return byID
}
