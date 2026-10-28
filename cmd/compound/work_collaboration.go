package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"compound/internal/work"
)

func runWorkCollaborate(store *work.Store, args []string, now time.Time) {
	if len(args) > 0 && args[0] == "body" {
		runWorkCollaborationBody(store, args[1:])
		return
	}
	usage := workUsage("collaborate")
	var (
		workID          string
		actor           string
		idempotencyKey  string
		sessionID       string
		runAttemptID    string
		targetActor     string
		bodyRef         string
		visibility      string
		deliveryClass   string
		delta           work.CollaborationStateDelta
		affects         work.CollaborationAffects
		causalParentIDs []string
		jsonOutput      bool
	)
	value := func(name string, i *int) string {
		if *i+1 >= len(args) || strings.TrimSpace(args[*i+1]) == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		*i++
		return strings.TrimSpace(args[*i])
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--actor":
			actor = value("--actor", &i)
		case "--idempotency-key":
			idempotencyKey = value("--idempotency-key", &i)
		case "--session":
			sessionID = value("--session", &i)
		case "--run-attempt":
			runAttemptID = value("--run-attempt", &i)
		case "--target-actor":
			targetActor = value("--target-actor", &i)
		case "--causal-parent":
			causalParentIDs = append(causalParentIDs, value("--causal-parent", &i))
		case "--body-ref":
			bodyRef = value("--body-ref", &i)
		case "--visibility":
			visibility = value("--visibility", &i)
		case "--delivery-class":
			deliveryClass = value("--delivery-class", &i)
		case "--delta-kind":
			delta.Kind = value("--delta-kind", &i)
		case "--delta-subject":
			delta.SubjectRef = value("--delta-subject", &i)
		case "--delta-before":
			delta.Before = value("--delta-before", &i)
		case "--delta-after":
			delta.After = value("--delta-after", &i)
		case "--affects-actor":
			affects.Actors = append(affects.Actors, value("--affects-actor", &i))
		case "--affects-work":
			affects.WorkIDs = append(affects.WorkIDs, value("--affects-work", &i))
		case "--affects-run-attempt":
			affects.RunAttemptIDs = append(affects.RunAttemptIDs, value("--affects-run-attempt", &i))
		case "--scope-ref":
			affects.ScopeRefs = append(affects.ScopeRefs, value("--scope-ref", &i))
		case "--dependency-ref":
			affects.DependencyRefs = append(affects.DependencyRefs, value("--dependency-ref", &i))
		case "--policy-ref":
			affects.PolicyRefs = append(affects.PolicyRefs, value("--policy-ref", &i))
		case "--base-revision":
			affects.BaseRevisions = append(affects.BaseRevisions, value("--base-revision", &i))
		case "--resource-ref":
			affects.ResourceRefs = append(affects.ResourceRefs, value("--resource-ref", &i))
		case "--json":
			jsonOutput = true
		default:
			if strings.HasPrefix(args[i], "-") || workID != "" {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			workID = strings.TrimSpace(args[i])
		}
	}
	if workID == "" || actor == "" || idempotencyKey == "" || bodyRef == "" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	result, err := store.RecordCollaboration(workID, work.CollaborationInput{
		Actor:           actor,
		IdempotencyKey:  idempotencyKey,
		SessionID:       sessionID,
		RunAttemptID:    runAttemptID,
		TargetActor:     targetActor,
		CausalParentIDs: causalParentIDs,
		BodyRef:         bodyRef,
		Visibility:      visibility,
		DeliveryClass:   deliveryClass,
		Delta:           delta,
		Affects:         affects,
	}, now)
	die(err)
	if jsonOutput {
		printJSON(result)
		return
	}
	fmt.Printf("%s collaboration=%s actor=%s created=%v body_ref=%s\n", result.Work.ID, result.Event.ID, result.Event.Actor, result.Created, result.Event.BodyRef)
}

func runWorkCollaborationBody(store *work.Store, args []string) {
	usage := workUsage("collaborate")
	var workID, eventID, actor, runAttemptID, sessionID string
	jsonOutput := false
	value := func(i *int) string {
		if *i+1 >= len(args) || strings.TrimSpace(args[*i+1]) == "" {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		*i++
		return strings.TrimSpace(args[*i])
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--event":
			eventID = value(&i)
		case "--actor":
			actor = value(&i)
		case "--run-attempt":
			runAttemptID = value(&i)
		case "--session":
			sessionID = value(&i)
		case "--json":
			jsonOutput = true
		default:
			if strings.HasPrefix(args[i], "-") || workID != "" {
				fmt.Fprintln(os.Stderr, usage)
				os.Exit(2)
			}
			workID = strings.TrimSpace(args[i])
		}
	}
	if workID == "" || eventID == "" || actor == "" || runAttemptID == "" || sessionID == "" {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	result, err := store.RetrieveCollaborationBody(work.CollaborationBodyRequest{
		WorkID: workID, EventID: eventID, Actor: actor,
		RunAttemptID: runAttemptID, SessionID: sessionID,
	})
	die(err)
	if jsonOutput {
		printJSON(struct {
			EventID    string `json:"event_id"`
			BodyRef    string `json:"body_ref"`
			BodyBase64 []byte `json:"body_base64"`
		}{EventID: result.EventID, BodyRef: result.BodyRef, BodyBase64: result.Body})
		return
	}
	if _, err := os.Stdout.Write(result.Body); err != nil {
		die(err)
	}
}
